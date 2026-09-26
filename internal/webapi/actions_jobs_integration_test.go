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
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/home-operations/kritik/internal/auth"
	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/jobs"
	"github.com/home-operations/kritik/internal/store"
)

const actionsConfig = `
web:
  signIn:
    - name: corp
      type: oidc
      issuer: https://idp.example
      clientId: kritik
      clientSecret: { env: KRITIK_TEST_TOKEN }
  operators: ["corp:aj-op"]
tenants:
  - slug: aj-tenant
    installations:
      - name: aj-bot
        forge: forgejo
        host: git.example
        account: aj
        token: { env: KRITIK_TEST_TOKEN }
        webhookSecret: { env: KRITIK_TEST_TOKEN }
    repositories:
      - name: aj/one
`

// actionsEnv exercises the dashboard actions (rerun, cancel, reindex)
// through HTTP against a real, insert-only River client, the way startWeb
// wires webapi.JobActions in production.
type actionsEnv struct {
	t        *testing.T
	st       *store.Store
	owner    *pgxpool.Pool
	queue    *river.Client[pgx.Tx]
	srv      *Server
	http     *httptest.Server
	cookie   *http.Cookie
	tenantID string
	repoID   string
	prID     string
}

func newActionsEnv(t *testing.T) *actionsEnv {
	t.Helper()
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(ctx, store.Options{
		AppURL: testEnv(t, "KRITIK_TEST_APP_URL"), OwnerURL: testEnv(t, "KRITIK_TEST_OWNER_URL"),
		AppRole: "kritik_app", RunnerRole: "kritik_runner", Logger: logger,
	})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx, "kritik_app", "kritik_runner"); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	owner, err := pgxpool.New(ctx, testEnv(t, "KRITIK_TEST_OWNER_URL"))
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(owner.Close)
	t.Setenv("KRITIK_TEST_TOKEN", "tok")
	file, err := configfile.Parse([]byte(actionsConfig))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := st.ApplyConfig(ctx, file, "actions-jobs-test"); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	cur := configfile.NewCurrent(file)
	webURL, _ := url.Parse("https://kritik.example")
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

	tn, _ := file.Tenant("aj-tenant")
	e.tenantID = tn.ID()
	e.repoID = configfile.RepositoryID(tn.Installations[0].ID(), "aj/one")
	e.prID = e.scalar(`INSERT INTO pull_requests (tenant_id, repository_id, number, title, author, head_sha)
		VALUES ($1, $2, 11, 'rerun me', 'ada', 'headA') RETURNING id::text`, e.tenantID, e.repoID)

	e.signIn("operator", "aj-op", nil)
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

func (e *actionsEnv) signIn(name, subject string, grants []store.Grant) {
	e.t.Helper()
	ctx := context.Background()
	now := time.Now()
	origin := "oidc:https://idp.example"
	acct, err := e.st.UpsertIdentity(ctx, store.SignInIdentity{Provider: "corp", Origin: origin, Subject: subject, DisplayName: name}, now)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.st.ReplaceForgeMemberships(ctx, acct.ID, grants, now); err != nil {
		e.t.Fatal(err)
	}
	token, err := e.st.CreateSession(ctx, acct.ID, "corp", origin, now, now.Add(time.Hour))
	if err != nil {
		e.t.Fatal(err)
	}
	e.cookie = &http.Cookie{Name: auth.CookieName, Value: token}
}

func (e *actionsEnv) do(path string) (int, []byte) {
	e.t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.http.URL+path, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	req.AddCookie(e.cookie)
	req.Header.Set("Origin", "https://kritik.example")
	req.Header.Set("X-Kritik", "1")
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
	t.Run("cancel of a non-running review is 409", func(t *testing.T) { testCancelNotCancelable(t, e) })
	t.Run("cancel stops a running review's job", func(t *testing.T) { testCancelRunning(t, e) })
	t.Run("reindex enqueues a full index job, then dedupes", func(t *testing.T) { testReindexJob(t, e) })
}

func testRerunJob(t *testing.T, e *actionsEnv) {
	status, body := e.do("/api/v1/tenants/aj-tenant/pulls/aj/one/11/rerun")
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

func testCancelNotCancelable(t *testing.T, e *actionsEnv) {
	review := e.scalar(`INSERT INTO reviews (tenant_id, pull_request_id, head_sha, status)
		VALUES ($1, $2, 'headA', 'completed') RETURNING id::text`, e.tenantID, e.prID)

	status, body := e.do("/api/v1/tenants/aj-tenant/reviews/" + review + "/cancel")
	e.expect(status, body, http.StatusConflict, CodeNotCancelable)
	if n := e.audits(AuditReviewCancel, review); n != 0 {
		t.Errorf("review.cancel audit rows = %d, want 0", n)
	}
}

func testCancelRunning(t *testing.T, e *actionsEnv) {
	ctx := context.Background()
	res, err := e.queue.Insert(ctx, jobs.ReviewArgs{
		TenantID: e.tenantID, RepositoryID: e.repoID, Number: 11, HeadSHA: "headA",
		Trigger: jobs.TriggerManual, Request: "cancel-me",
	}, nil)
	if err != nil {
		t.Fatalf("insert job to cancel: %v", err)
	}
	review := e.scalar(`INSERT INTO reviews (tenant_id, pull_request_id, head_sha, status, river_job_id)
		VALUES ($1, $2, 'headA', 'running', $3) RETURNING id::text`, e.tenantID, e.prID, res.Job.ID)

	status, body := e.do("/api/v1/tenants/aj-tenant/reviews/" + review + "/cancel")
	e.expect(status, body, http.StatusAccepted, "")

	if j := e.job(res.Job.ID); j.state != "cancelled" {
		t.Errorf("job state = %q, want cancelled", j.state)
	}
	if n := e.audits(AuditReviewCancel, review); n != 1 {
		t.Errorf("review.cancel audit rows = %d, want 1", n)
	}
}

func testReindexJob(t *testing.T, e *actionsEnv) {
	status, body := e.do("/api/v1/tenants/aj-tenant/repos/aj/one/reindex")
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
	status, body = e.do("/api/v1/tenants/aj-tenant/repos/aj/one/reindex")
	e.expect(status, body, http.StatusConflict, CodeAlreadyQueued)
	if n := e.audits(AuditRepoReindex, "aj/one"); n != 1 {
		t.Errorf("repo.reindex audit rows after dedup = %d, want still 1", n)
	}
}
