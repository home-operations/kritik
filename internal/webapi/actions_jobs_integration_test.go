//go:build integration

package webapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/home-operations/kritika/internal/auth"
	"github.com/home-operations/kritika/internal/configfile"
	"github.com/home-operations/kritika/internal/configfile/configfiletest"
	"github.com/home-operations/kritika/internal/jobs"
	"github.com/home-operations/kritika/internal/store"
	"github.com/home-operations/kritika/internal/store/storetest"
)

const actionsConfig = `
auth:
  oidc:
    issuer: https://idp.example
    clientId: kritika
    clientSecret: { env: KRITIKA_TEST_TOKEN }
    roleMappingExpr: '"kritika-admin" in roles ? "admin" : ""'
apps:
  aj-bot:
    accounts: [aj]
    clientId: Iv1.test
    privateKey: { env: KRITIKA_TEST_TOKEN }
    webhookSecret: { env: KRITIKA_TEST_TOKEN }
repositories:
  aj/one: {}
`

// actionsEnv exercises the dashboard actions (rerun, cancel, reindex)
// through HTTP against a real, insert-only River client, the way startWeb
// wires webapi.JobActions in production.
type actionsEnv struct {
	t         *testing.T
	st        *store.Store
	owner     *pgxpool.Pool
	queue     *river.Client[pgx.Tx]
	srv       *Server
	http      *httptest.Server
	cookie    *http.Cookie
	accountID string
	repoID    string
	prID      string
}

func newActionsEnv(t *testing.T) *actionsEnv {
	t.Helper()
	ctx := context.Background()
	logger := slog.New(slog.DiscardHandler)
	st := storetest.Open(t)
	owner, err := pgxpool.New(ctx, storetest.Env(t, "KRITIKA_TEST_OWNER_URL"))
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(owner.Close)
	t.Setenv("KRITIKA_TEST_TOKEN", "tok")
	file := configfiletest.Load(t, actionsConfig)
	if err := st.ApplyConfig(ctx, file); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	cur := configfile.NewCurrent(file)
	webURL, _ := url.Parse("https://kritika.example")
	h, err := auth.New(auth.Config{Store: st, Current: cur, WebURL: webURL, Logger: logger})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	queue, err := river.NewClient(riverpgxv5.New(st.App()), &river.Config{})
	if err != nil {
		t.Fatalf("river.NewClient: %v", err)
	}
	e := &actionsEnv{t: t, st: st, owner: owner, queue: queue}
	e.srv = New(Config{Store: st, Current: cur, Auth: h, Actions: JobActions{Queue: queue}, WebURL: webURL, Logger: logger})
	e.http = httptest.NewServer(e.srv.Handler())
	t.Cleanup(e.http.Close)

	a, _ := file.Account(configfile.ForgeGitHub, "aj")
	e.accountID = a.ID()
	e.repoID = configfile.RepositoryID(a.ID(), "aj/one")
	e.prID = e.scalar(`INSERT INTO pull_requests (account_id, repository_id, number, title, author, head_sha)
		VALUES ($1, $2, 11, 'rerun me', 'ada', 'headA') RETURNING id::text`, e.accountID, e.repoID)

	e.signIn("admin", "aj-op", store.SessionGrant{Role: store.RoleAdmin})
	return e
}

func (e *actionsEnv) scalar(sql string, args ...any) string {
	e.t.Helper()
	var v string
	if err := e.owner.QueryRow(context.Background(), sql, args...).Scan(&v); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
	return v
}

func (e *actionsEnv) signIn(name, subject string, g store.SessionGrant) {
	e.t.Helper()
	ctx, now, origin := context.Background(), time.Now(), "oidc:https://idp.example"
	user, err := e.st.UpsertIdentity(ctx, store.SignInIdentity{
		Provider: "oidc", Origin: origin, Subject: subject, DisplayName: name,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	g.Key, _ = auth.GrantKey(e.srv.current.Get().Auth, "oidc")
	token, err := e.st.CreateSession(ctx, user.ID, "oidc", origin, g, now, now.Add(time.Hour))
	if err != nil {
		e.t.Fatal(err)
	}
	e.cookie = &http.Cookie{Name: auth.SessionCookieName(&url.URL{Scheme: "https", Host: "kritika.example"}), Value: token}
}

func (e *actionsEnv) do(path string) (int, []byte) {
	e.t.Helper()
	return e.send(http.MethodPost, path, "")
}

func (e *actionsEnv) send(method, path, body string) (int, []byte) {
	e.t.Helper()
	req, err := http.NewRequest(method, e.http.URL+path, strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	req.AddCookie(e.cookie)
	req.Header.Set("Origin", "https://kritika.example")
	req.Header.Set("X-Kritika", "1")
	resp, err := e.http.Client().Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	return resp.StatusCode, out
}

func (e *actionsEnv) expect(status int, body []byte, wantStatus int, wantCode ErrorCode) {
	e.t.Helper()
	if status != wantStatus {
		e.t.Fatalf("status = %d, want %d: %s", status, wantStatus, body)
	}
	if wantCode == "" {
		return
	}
	var eb ErrorBody
	if err := json.Unmarshal(body, &eb); err != nil || eb.Code != wantCode {
		e.t.Fatalf("body = %s, want code %s", body, wantCode)
	}
}

func (e *actionsEnv) audits(action AuditAction, target string) int {
	e.t.Helper()
	var n int
	if err := e.owner.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE action = $1 AND target = $2`,
		string(action), target).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// jobRow is one river_job's kind, args and state.
type jobRow struct {
	kind, args, state string
}

func (e *actionsEnv) job(id int64) jobRow {
	e.t.Helper()
	var j jobRow
	if err := e.owner.QueryRow(context.Background(), `SELECT kind, args::text, state FROM river_job WHERE id = $1`, id).
		Scan(&j.kind, &j.args, &j.state); err != nil {
		e.t.Fatalf("job %d: %v", id, err)
	}
	return j
}

func TestActionsJobs(t *testing.T) {
	e := newActionsEnv(t)

	t.Run("rerun enqueues a review job", func(t *testing.T) { testRerunJob(t, e) })
	t.Run("rerun refuses while that head is queued or running", func(t *testing.T) { testRerunDedupes(t, e) })
	t.Run("concurrent reruns of one pull request queue one job", func(t *testing.T) { testRerunConcurrent(t, e) })
	t.Run("cancel of a non-running review is 409", func(t *testing.T) { testCancelNotCancelable(t, e) })
	t.Run("cancel stops a running review's job", func(t *testing.T) { testCancelRunning(t, e) })
	t.Run("cancel of a review whose job already ended is 409", func(t *testing.T) { testCancelEndedJob(t, e) })
	t.Run("reindex enqueues a full index job, then dedupes", func(t *testing.T) { testReindexJob(t, e) })
	t.Run("a repository is turned off and on, audited", func(t *testing.T) { testTurnOn(t, e) })
}

func testTurnOn(t *testing.T, e *actionsEnv) {
	const path = "/api/v1/accounts/github/aj/repos/aj/one/turned-on"
	for _, tt := range []struct {
		body, want string
		action     AuditAction
	}{{`{"on":false}`, "false", AuditRepoTurnOff}, {`{"on":true}`, "true", AuditRepoTurnOn}} {
		status, body := e.send(http.MethodPut, path, tt.body)
		e.expect(status, body, http.StatusNoContent, "")
		if got := e.scalar(`SELECT coalesce(turned_on::text, 'unset') FROM repositories WHERE id = $1`, e.repoID); got != tt.want {
			t.Errorf("after %s turned_on = %s, want %s", tt.body, got, tt.want)
		}
		if n := e.audits(tt.action, "aj/one"); n != 1 {
			t.Errorf("%s audit rows = %d, want 1", tt.action, n)
		}
	}
	status, body := e.send(http.MethodPut, "/api/v1/accounts/github/aj/repos/aj/none/turned-on", `{"on":true}`)
	e.expect(status, body, http.StatusNotFound, CodeNotFound)
}

func testRerunJob(t *testing.T, e *actionsEnv) {
	status, body := e.do("/api/v1/accounts/github/aj/pulls/aj/one/11/rerun")
	e.expect(status, body, http.StatusAccepted, "")

	var accepted Accepted
	if err := json.Unmarshal(body, &accepted); err != nil || accepted.JobID == 0 {
		t.Fatalf("rerun body = %s: %v", body, err)
	}
	j := e.job(accepted.JobID)
	if j.kind != "review" {
		t.Errorf("kind = %q, want review", j.kind)
	}
	var args jobs.ReviewArgs
	if err := json.Unmarshal([]byte(j.args), &args); err != nil {
		t.Fatal(err)
	}
	if args.Trigger != jobs.TriggerManual || args.Request == "" || args.HeadSHA != "headA" {
		t.Errorf("args = %+v, want manual trigger, a request id, head headA", args)
	}
	if n := e.audits(AuditReviewRerun, "aj/one#11"); n != 1 {
		t.Errorf("review.rerun audit rows = %d, want 1", n)
	}
}

func testRerunDedupes(t *testing.T, e *actionsEnv) {
	const path = "/api/v1/accounts/github/aj/pulls/aj/one/11/rerun"
	status, body := e.do(path)
	e.expect(status, body, http.StatusConflict, CodeAlreadyQueued)
	if !strings.Contains(string(body), "review job #") || !strings.Contains(string(body), "is queued") {
		t.Errorf("body = %s, want it to name the queued job", body)
	}

	e.scalar(`UPDATE river_job SET state = 'completed', finalized_at = now()
		WHERE kind = 'review' AND args->>'repository_id' = $1 RETURNING 'done'`, e.repoID)
	review := e.scalar(`INSERT INTO reviews (account_id, pull_request_id, head_sha, status)
		VALUES ($1, $2, 'headA', 'running') RETURNING id::text`, e.accountID, e.prID)
	status, body = e.do(path)
	e.expect(status, body, http.StatusConflict, CodeAlreadyQueued)
	if !strings.Contains(string(body), "already queued or running") {
		t.Errorf("body = %s, want the plain refusal when no job is left to name", body)
	}

	e.scalar(`UPDATE reviews SET status = 'completed' WHERE id = $1 RETURNING 'done'`, review)
	status, body = e.do(path)
	e.expect(status, body, http.StatusAccepted, "")
	if n := e.audits(AuditReviewRerun, "aj/one#11"); n != 2 {
		t.Errorf("review.rerun audit rows = %d, want 2: refused re-runs are not audited", n)
	}
	e.scalar(`UPDATE river_job SET state = 'completed', finalized_at = now()
		WHERE kind = 'review' AND args->>'repository_id' = $1 AND finalized_at IS NULL RETURNING 'done'`, e.repoID)
}

func testRerunConcurrent(t *testing.T, e *actionsEnv) {
	e.scalar(`INSERT INTO pull_requests (account_id, repository_id, number, title, author, head_sha)
		VALUES ($1, $2, 12, 'race me', 'ada', 'headR') RETURNING id::text`, e.accountID, e.repoID)
	for round := range 5 {
		statuses := make([]int, 2)
		var wg sync.WaitGroup
		for i := range statuses {
			wg.Go(func() { statuses[i], _ = e.do("/api/v1/accounts/github/aj/pulls/aj/one/12/rerun") })
		}
		wg.Wait()
		if !slices.Contains(statuses, http.StatusAccepted) || !slices.Contains(statuses, http.StatusConflict) {
			t.Fatalf("round %d: statuses = %v, want one 202 and one 409", round, statuses)
		}
		e.scalar(`UPDATE river_job SET state = 'completed', finalized_at = now()
			WHERE kind = 'review' AND args->>'head_sha' = 'headR' AND finalized_at IS NULL RETURNING 'done'`)
	}
}

func testCancelNotCancelable(t *testing.T, e *actionsEnv) {
	review := e.scalar(`INSERT INTO reviews (account_id, pull_request_id, head_sha, status)
		VALUES ($1, $2, 'headA', 'completed') RETURNING id::text`, e.accountID, e.prID)

	status, body := e.do("/api/v1/accounts/github/aj/reviews/" + review + "/cancel")
	e.expect(status, body, http.StatusConflict, CodeNotCancelable)
	if n := e.audits(AuditReviewCancel, review); n != 0 {
		t.Errorf("review.cancel audit rows = %d, want 0", n)
	}
}

func testCancelRunning(t *testing.T, e *actionsEnv) {
	ctx := context.Background()
	res, err := e.queue.Insert(ctx, jobs.ReviewArgs{
		AccountID: e.accountID, RepositoryID: e.repoID, Number: 11, HeadSHA: "headA",
		Trigger: jobs.TriggerManual, Request: "cancel-me",
	}, nil)
	if err != nil {
		t.Fatalf("insert job to cancel: %v", err)
	}
	review := e.scalar(`INSERT INTO reviews (account_id, pull_request_id, head_sha, status, river_job_id)
		VALUES ($1, $2, 'headA', 'running', $3) RETURNING id::text`, e.accountID, e.prID, res.Job.ID)

	status, body := e.do("/api/v1/accounts/github/aj/reviews/" + review + "/cancel")
	e.expect(status, body, http.StatusAccepted, "")

	if j := e.job(res.Job.ID); j.state != "cancelled" {
		t.Errorf("job state = %q, want cancelled", j.state)
	}
	if n := e.audits(AuditReviewCancel, review); n != 1 {
		t.Errorf("review.cancel audit rows = %d, want 1", n)
	}
}

func testCancelEndedJob(t *testing.T, e *actionsEnv) {
	res, err := e.queue.Insert(context.Background(), jobs.ReviewArgs{
		AccountID: e.accountID, RepositoryID: e.repoID, Number: 11, HeadSHA: "headA",
		Trigger: jobs.TriggerManual, Request: "ended",
	}, nil)
	if err != nil {
		t.Fatalf("insert job: %v", err)
	}
	e.scalar(`UPDATE river_job SET state = 'completed', finalized_at = now() WHERE id = $1 RETURNING 'done'`, res.Job.ID)
	review := e.scalar(`INSERT INTO reviews (account_id, pull_request_id, head_sha, status, river_job_id)
		VALUES ($1, $2, 'headA', 'running', $3) RETURNING id::text`, e.accountID, e.prID, res.Job.ID)

	status, body := e.do("/api/v1/accounts/github/aj/reviews/" + review + "/cancel")
	e.expect(status, body, http.StatusConflict, CodeNotCancelable)
	if got := e.scalar(`SELECT (cancel_requested_at IS NULL)::text FROM reviews WHERE id = $1`, review); got != "true" {
		t.Error("a refused cancel still marked the review cancel-requested")
	}
	if n := e.audits(AuditReviewCancel, review); n != 0 {
		t.Errorf("review.cancel audit rows = %d, want 0", n)
	}
}

func testReindexJob(t *testing.T, e *actionsEnv) {
	status, body := e.do("/api/v1/accounts/github/aj/repos/aj/one/reindex")
	e.expect(status, body, http.StatusAccepted, "")

	var accepted Accepted
	if err := json.Unmarshal(body, &accepted); err != nil || accepted.JobID == 0 {
		t.Fatalf("reindex body = %s: %v", body, err)
	}
	j := e.job(accepted.JobID)
	if j.kind != "index" {
		t.Errorf("kind = %q, want index", j.kind)
	}
	var args jobs.IndexArgs
	if err := json.Unmarshal([]byte(j.args), &args); err != nil {
		t.Fatal(err)
	}
	if !args.Full || args.Trigger != jobs.TriggerReindex || args.CommitSHA != "" {
		t.Errorf("args = %+v, want a full forced reindex with no pinned commit", args)
	}
	if n := e.audits(AuditRepoReindex, "aj/one"); n != 1 {
		t.Errorf("repo.reindex audit rows = %d, want 1", n)
	}

	// The job above is still available (nothing runs it here), so a second
	// forced reindex of the same repository dedupes onto it.
	status, body = e.do("/api/v1/accounts/github/aj/repos/aj/one/reindex")
	e.expect(status, body, http.StatusConflict, CodeAlreadyQueued)
	if n := e.audits(AuditRepoReindex, "aj/one"); n != 1 {
		t.Errorf("repo.reindex audit rows after dedup = %d, want still 1", n)
	}
}
