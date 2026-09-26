//go:build integration

package webapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/home-operations/kritik/internal/auth"
	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/model"
	"github.com/home-operations/kritik/internal/store"
	"github.com/home-operations/kritik/internal/transcript"
)

func testEnv(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Skipf("%s not set", key)
	}
	return v
}

const integrationConfig = `
web:
  signIn:
    - name: corp
      type: oidc
      issuer: https://idp.example
      clientId: kritik
      clientSecret: { env: KRITIK_TEST_TOKEN }
  operators: ["corp:op-sub"]
tenants:
  - slug: webapi-a
    installations:
      - name: webapi-a-bot
        forge: forgejo
        host: git.example
        account: wa
        token: { env: KRITIK_TEST_TOKEN }
        webhookSecret: { env: KRITIK_TEST_TOKEN }
    repositories:
      - name: wa/one
      - name: wa/two
  - slug: webapi-b
    installations:
      - name: webapi-b-bot
        forge: forgejo
        host: git.example
        account: wb
        token: { env: KRITIK_TEST_TOKEN }
        webhookSecret: { env: KRITIK_TEST_TOKEN }
    repositories:
      - name: wb/one
`

// followupComment is answered in both tenants, as comment ids from two
// forges may collide; onlyBComment is answered only in tenant B.
const (
	followupComment = 4242
	onlyBComment    = 4343
)

// seeded is what seedTenant wrote for one tenant.
type seeded struct {
	tenantID, repoID, prID, reviewID, runID string
}

type apiEnv struct {
	t      *testing.T
	st     *store.Store
	owner  *pgxpool.Pool
	file   *configfile.File
	srv    *Server
	http   *httptest.Server
	a, b   seeded
	cookie map[string]*http.Cookie
}

func newAPIEnv(t *testing.T) *apiEnv {
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
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	t.Setenv("KRITIK_TEST_TOKEN", "tok")
	file, err := configfile.Parse([]byte(integrationConfig))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := st.ApplyConfig(ctx, file, "webapi-test"); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	cur := configfile.NewCurrent(file)
	webURL, _ := url.Parse("https://kritik.example")
	h, err := auth.New(auth.Config{Store: st, Current: cur, WebURL: webURL, Logger: logger})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	e := &apiEnv{t: t, st: st, owner: owner, file: file, cookie: map[string]*http.Cookie{}}
	e.srv = New(Config{Store: st, Current: cur, Auth: h, WebURL: webURL, Logger: logger})
	e.http = httptest.NewServer(e.srv.Handler())
	t.Cleanup(e.http.Close)
	e.a, e.b = e.seedTenant("webapi-a", "wa/one"), e.seedTenant("webapi-b", "wb/one")
	e.exec(`INSERT INTO followups (tenant_id, pull_request_id, comment_id, author, status)
		VALUES ($1, $2, $3, 'carol', 'answered')`, e.b.tenantID, e.b.prID, onlyBComment)
	e.signIn("member-a", "alice", seededGrant(e.a.tenantID))
	e.signIn("member-b", "bob", seededGrant(e.b.tenantID))
	e.signIn("operator", "op-sub", nil)
	return e
}

func seededGrant(tenantID string) []store.Grant {
	return []store.Grant{{TenantID: tenantID, Role: store.RoleMember}}
}

func (e *apiEnv) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.owner.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatalf("%s: %v", strings.Fields(sql)[0:3], err)
	}
}

func (e *apiEnv) scalar(sql string, args ...any) string {
	e.t.Helper()
	var v string
	if err := e.owner.QueryRow(context.Background(), sql, args...).Scan(&v); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
	return v
}

// seedTenant writes a pull request with a finished agentic review, its
// runner and agent runs, context pack, findings, usage, model calls, an
// index run, a follow-up and a queued job, all as the owner so no policy
// stands in the way.
func (e *apiEnv) seedTenant(slug, repo string) seeded {
	e.t.Helper()
	tn, _ := e.file.Tenant(slug)
	s := seeded{tenantID: tn.ID()}
	s.repoID = configfile.RepositoryID(tn.Installations[0].ID(), repo)
	e.exec(`UPDATE repositories SET default_branch = 'main' WHERE id = $1`, s.repoID)
	s.prID = e.scalar(`INSERT INTO pull_requests (tenant_id, repository_id, number, title, author, head_sha, labels)
		VALUES ($1, $2, 7, $3, 'ada', 'head7', '[{"name":"bug","color":"f00"}]') RETURNING id::text`, s.tenantID, s.repoID, "PR of "+slug)
	s.reviewID = e.scalar(`INSERT INTO reviews (tenant_id, pull_request_id, head_sha, status, trigger, mode, model, finished_at, summary)
		VALUES ($1, $2, 'head7', 'completed', 'push', 'agentic', 'acme/large', now(), '{"take":"ok","praise":["tests"]}')
		RETURNING id::text`, s.tenantID, s.prID)
	e.exec(`INSERT INTO findings (tenant_id, review_id, path, line, severity, title, explanation)
		VALUES ($1, $2, 'a.go', 3, 'blocking', 'nil deref', 'x'), ($1, $2, 'b.go', 9, 'nit', 'naming', 'y')`, s.tenantID, s.reviewID)
	s.runID = e.scalar(`INSERT INTO runner_runs (tenant_id, review_id, kind, phase, log_tail)
		VALUES ($1, $2, 'review', 'done', $3) RETURNING id::text`, s.tenantID, s.reviewID, "tail of "+slug)
	e.exec(`INSERT INTO context_packs (runner_run_id, tenant_id, head_sha, base_sha, patch_id, diff, changed_paths, stages, repo_files)
		VALUES ($1, $2, 'head7', 'base7', 'patch7', $3, '{a.go}',
			'[{"stage":"definitions","path":"b.go","start_line":1,"end_line":2,"text":"func F() {}"}]',
			'{".kritik.yaml":"mode: agentic\n"}')`, s.runID, s.tenantID, "diff of "+slug)
	e.exec(`INSERT INTO agent_runs (runner_run_id, tenant_id, stop_reason, result, steps, tool_calls, timeline, model, sources)
		VALUES ($1, $2, 'submitted', '{"findings":[]}', 2, '{"grep":1}',
			'[{"index":0,"tools":["grep"],"duration_ms":5,"output_bytes":7,"input_tokens":10,"output_tokens":2}]', 'acme/large',
			'["https://docs.example"]')`, s.runID, s.tenantID)
	e.exec(`INSERT INTO usage (tenant_id, repository_id, review_id, role, model, input_tokens, output_tokens, cost_usd)
		VALUES ($1, $2, $3, 'review', 'acme/large', 100, 10, 0.5)`, s.tenantID, s.repoID, s.reviewID)
	ix := e.scalar(`INSERT INTO index_runs (tenant_id, repository_id, commit_sha, embed_model, embed_dims, mode, status, finished_at)
		VALUES ($1, $2, 'commit7', 'embed', 8, 'full', 'completed', now()) RETURNING id::text`, s.tenantID, s.repoID)
	e.exec(`UPDATE repositories SET active_index_run_id = $1 WHERE id = $2`, ix, s.repoID)
	e.exec(`INSERT INTO followups (tenant_id, pull_request_id, comment_id, author, status, model)
		VALUES ($1, $2, $3, 'bob', 'answered', 'acme/large')`, s.tenantID, s.prID, followupComment)
	args, _ := json.Marshal(map[string]any{
		"tenant_id": s.tenantID, "repository_id": s.repoID, "number": 7, "head_sha": "head7", "trigger": "push",
	})
	e.exec(`INSERT INTO river_job (kind, args, max_attempts, state) VALUES ('review', $1, 5, 'available')`, args)
	e.seedModelCalls(s, slug)
	return s
}

// seedModelCalls records two agent steps of the review and one follow-up
// answered against it, which the review's transcript must leave out.
func (e *apiEnv) seedModelCalls(s seeded, slug string) {
	e.t.Helper()
	ctx := context.Background()
	tools := []model.ToolDef{{Name: "grep", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	msgs := []model.Message{{Role: model.RoleUser, Text: "review"}}
	for step := range 2 {
		if step == 1 {
			msgs = append(msgs, model.Message{Role: model.RoleAssistant, Text: "looking"}, model.Message{Role: model.RoleUser, Text: "more"})
		}
		req := model.StepRequest{System: "sys of " + slug, Messages: msgs, Tools: tools}
		err := e.st.WithTenant(ctx, s.tenantID, func(tx pgx.Tx) error {
			prev, n, err := store.AgentState(ctx, tx, s.runID)
			if err != nil {
				return err
			}
			row := transcript.Delta(prev, req, nil)
			row.Response = transcript.Response{Text: "ok", Stop: model.StopToolUse}
			return store.InsertModelCall(ctx, tx, store.ModelCall{
				TenantID: s.tenantID, ReviewID: s.reviewID, RunnerRunID: s.runID, Kind: store.ModelCallAgentStep, Step: n,
				Model: "acme/large", Row: row.Encode(), Usage: model.Usage{Input: 10, CacheRead: 4, Output: 2}, CostUSD: 0.25,
				Duration: time.Second,
			})
		})
		if err != nil {
			e.t.Fatal(err)
		}
	}
	err := e.st.WithTenant(ctx, s.tenantID, func(tx pgx.Tx) error {
		row := transcript.Delta(transcript.State{}, model.StepRequest{System: "follow of " + slug, Messages: msgs[:1]}, nil)
		row.Response = transcript.Response{Text: "reply", Stop: model.StopEndTurn}
		return store.InsertModelCall(ctx, tx, store.ModelCall{
			TenantID: s.tenantID, ReviewID: s.reviewID, FollowupCommentID: followupComment, Kind: store.ModelCallFollowUp,
			Model: "acme/large", Row: row.Encode(),
		})
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *apiEnv) signIn(name, subject string, grants []store.Grant) {
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
	e.cookie[name] = &http.Cookie{Name: auth.SessionCookieName(&url.URL{Scheme: "https", Host: "kritik.example"}), Value: token}
}

func (e *apiEnv) get(ctx context.Context, who, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", e.http.URL+path, nil)
	if err != nil {
		return nil, err
	}
	if c := e.cookie[who]; c != nil {
		req.AddCookie(c)
	}
	return e.http.Client().Do(req)
}

func (e *apiEnv) getBody(who, path string) (int, []byte) {
	e.t.Helper()
	resp, err := e.get(context.Background(), who, path)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	return resp.StatusCode, body
}

// TestWebAPI seeds one database for every case: the seed's pull requests
// and follow-ups are unique per repository, so it cannot run twice.
func TestWebAPI(t *testing.T) {
	e := newAPIEnv(t)
	t.Run("read endpoints scope to the tenant", func(t *testing.T) { testReadEndpointsScopeToTenant(t, e) })
	t.Run("tenant B's ids under tenant A", func(t *testing.T) { testCrossTenantIDs(t, e) })
	t.Run("me and tenant lists", func(t *testing.T) { testMeAndTenantLists(t, e) })
	t.Run("transcripts equal Rebuild", func(t *testing.T) { testTranscriptsEqualRebuild(t, e) })
	t.Run("repository pagination", func(t *testing.T) { testRepoPagination(t, e) })
	t.Run("event stream scopes to the tenant", func(t *testing.T) { testEventStreamScopesToTenant(t, e) })
}

func testReadEndpointsScopeToTenant(t *testing.T, e *apiEnv) {
	a := "/api/v1/tenants/webapi-a"
	// Each path of tenant A and a string only tenant A's answer contains.
	endpoints := []struct{ path, marker string }{
		{a, `"slug":"webapi-a"`},
		{a + "/repos", `"fullName":"wa/one"`},
		{a + "/repos/wa/one", `"activeCommit":"commit7"`},
		{a + "/pulls", `"title":"PR of webapi-a"`},
		{a + "/pulls?state=all&repo=wa/one&outcome=completed&q=webapi-a", `"title":"PR of webapi-a"`},
		{a + "/pulls/wa/one/7", `"title":"PR of webapi-a"`},
		{a + "/reviews/" + e.a.reviewID, `"logTail":"tail of webapi-a"`},
		{a + "/reviews/" + e.a.reviewID + "/diff", `"diff":"diff of webapi-a"`},
		{a + "/reviews/" + e.a.reviewID + "/transcript", `"system":"sys of webapi-a"`},
		{a + "/reviews/" + e.a.reviewID + "/raw", `"logTail":"tail of webapi-a"`},
		{a + "/index-runs?repo=wa/one", `"commitSha":"commit7"`},
		{a + "/followups?repo=wa/one", `"commentId":4242`},
		{a + "/followups/4242/transcript", `"system":"follow of webapi-a"`},
		{a + "/usage?group=repo", `"key":"wa/one"`},
		{a + "/queue", `"repository":"wa/one"`},
	}
	for _, ep := range endpoints {
		t.Run(ep.path, func(t *testing.T) {
			for _, tc := range []struct {
				who    string
				status int
			}{{"member-a", 200}, {"operator", 200}, {"member-b", 404}, {"nobody", 401}} {
				status, body := e.getBody(tc.who, ep.path)
				if status != tc.status {
					t.Fatalf("%s: status = %d, want %d: %s", tc.who, status, tc.status, body)
				}
				if status == 200 && !bytes.Contains(body, []byte(ep.marker)) {
					t.Errorf("%s: body lacks %s: %s", tc.who, ep.marker, body)
				}
				if status != 200 {
					continue
				}
				for _, leak := range e.bMarkers() {
					if bytes.Contains(body, []byte(leak)) {
						t.Errorf("%s: body leaks tenant B's %q: %s", tc.who, leak, body)
					}
				}
			}
		})
	}
}

// bMarkers are strings only tenant B's rows contain.
func (e *apiEnv) bMarkers() []string {
	return []string{
		"webapi-b", "wb/one", "PR of webapi-b", "tail of webapi-b", "diff of webapi-b", "sys of webapi-b", "follow of webapi-b",
		e.b.reviewID, e.b.prID, e.b.runID, e.b.repoID,
	}
}

// testCrossTenantIDs asks for tenant B's rows by id under tenant A's slug:
// even an operator, who may read B, finds nothing, since the query runs
// scoped to A.
func testCrossTenantIDs(t *testing.T, e *apiEnv) {
	a := "/api/v1/tenants/webapi-a"
	paths := []string{
		a + "/reviews/" + e.b.reviewID, a + "/reviews/" + e.b.reviewID + "/diff",
		a + "/reviews/" + e.b.reviewID + "/transcript", a + "/reviews/" + e.b.reviewID + "/raw",
		a + fmt.Sprintf("/followups/%d/transcript", onlyBComment), a + "/pulls/wb/one/7", a + "/repos/wb/one",
	}
	for _, path := range paths {
		for _, who := range []string{"member-a", "operator"} {
			t.Run(who+" "+path, func(t *testing.T) {
				if status, body := e.getBody(who, path); status != 404 {
					t.Errorf("status = %d, want 404: %s", status, body)
				}
			})
		}
	}
}

func testMeAndTenantLists(t *testing.T, e *apiEnv) {
	tests := []struct {
		who, path string
		status    int
		want      []string
		not       []string
	}{
		{"member-a", "/api/v1/me", 200, []string{`"slug":"webapi-a","role":"member"`}, []string{"webapi-b"}},
		{"operator", "/api/v1/me", 200, []string{`"operator":true`, `"slug":"webapi-a","role":"admin"`, `"slug":"webapi-b"`}, nil},
		{"member-a", "/api/v1/tenants", 200, []string{`"slug":"webapi-a"`, `"repositories":2`, `"reviews7d":1`}, []string{"webapi-b"}},
		{"member-b", "/api/v1/tenants", 200, []string{`"slug":"webapi-b"`}, []string{"webapi-a"}},
		{"operator", "/api/v1/operator/tenants", 200, []string{`"slug":"webapi-a"`, `"slug":"webapi-b"`, `"live":true`}, nil},
		{"member-a", "/api/v1/operator/tenants", 404, nil, nil},
		{"nobody", "/api/v1/tenants", 401, nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.who+" "+tt.path, func(t *testing.T) {
			status, body := e.getBody(tt.who, tt.path)
			if status != tt.status {
				t.Fatalf("status = %d, want %d: %s", status, tt.status, body)
			}
			for _, w := range tt.want {
				if !bytes.Contains(body, []byte(w)) {
					t.Errorf("body lacks %s: %s", w, body)
				}
			}
			for _, n := range tt.not {
				if bytes.Contains(body, []byte(n)) {
					t.Errorf("body contains %s: %s", n, body)
				}
			}
		})
	}
}

func testTranscriptsEqualRebuild(t *testing.T, e *apiEnv) {
	ctx := context.Background()
	var steps, followups []transcript.StoredRow
	if err := e.st.WithTenant(ctx, e.a.tenantID, func(tx pgx.Tx) error {
		var err error
		if steps, err = store.ModelCalls(ctx, tx, store.ModelCallFilter{RunnerRunID: e.a.runID}); err != nil {
			return err
		}
		followups, err = store.ModelCalls(ctx, tx, store.ModelCallFilter{FollowupCommentID: followupComment})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(steps) != 2 || len(followups) != 1 {
		t.Fatalf("seeded %d steps and %d follow-up calls, want 2 and 1", len(steps), len(followups))
	}
	for _, tc := range []struct {
		path string
		rows []transcript.StoredRow
	}{
		{"/api/v1/tenants/webapi-a/reviews/" + e.a.reviewID + "/transcript", steps},
		{"/api/v1/tenants/webapi-a/followups/4242/transcript", followups},
	} {
		t.Run(tc.path, func(t *testing.T) {
			status, body := e.getBody("member-a", tc.path)
			if status != 200 {
				t.Fatalf("status = %d: %s", status, body)
			}
			want, _ := json.Marshal(transcriptOf(transcript.Rebuild(tc.rows)))
			if strings.TrimSpace(string(body)) != string(want) {
				t.Errorf("transcript =\n%s\nwant\n%s", body, want)
			}
		})
	}
}

func testRepoPagination(t *testing.T, e *apiEnv) {
	var names []string
	path := "/api/v1/tenants/webapi-a/repos?limit=1"
	for range 5 {
		status, body := e.getBody("member-a", path)
		if status != 200 {
			t.Fatalf("status = %d: %s", status, body)
		}
		var page Page[Repository]
		if err := json.Unmarshal(body, &page); err != nil {
			t.Fatal(err)
		}
		for _, r := range page.Items {
			names = append(names, r.FullName)
		}
		if page.NextCursor == nil {
			break
		}
		path = "/api/v1/tenants/webapi-a/repos?limit=1&cursor=" + *page.NextCursor
	}
	if strings.Join(names, ",") != "wa/one,wa/two" {
		t.Errorf("paged repositories = %v, want wa/one, wa/two", names)
	}
}

// stream opens /api/events as who and sends every data line it reads.
func (e *apiEnv) stream(ctx context.Context, who string) <-chan Event {
	e.t.Helper()
	resp, err := e.get(ctx, who, "/api/events")
	if err != nil {
		e.t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		e.t.Fatalf("%s: /api/events = %d", who, resp.StatusCode)
	}
	br := bufio.NewReader(resp.Body)
	if line, err := br.ReadString('\n'); err != nil || line != "event: resync\n" {
		e.t.Fatalf("%s: first line %q, %v", who, line, err)
	}
	out := make(chan Event, 64)
	go func() {
		defer close(out)
		defer func() { _ = resp.Body.Close() }()
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			if data, ok := strings.CutPrefix(strings.TrimSpace(line), "data: "); ok {
				var ev Event
				if json.Unmarshal([]byte(data), &ev) == nil && ev.Tenant != "" {
					out <- ev
				}
			}
		}
	}()
	return out
}

func testEventStreamScopesToTenant(t *testing.T, e *apiEnv) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runCtx, stopRun := context.WithCancel(ctx)
	defer stopRun()
	ran := make(chan struct{})
	go func() {
		defer close(ran)
		_ = e.srv.Run(runCtx)
	}()
	aEvents, bEvents := e.stream(ctx, "member-a"), e.stream(ctx, "member-b")

	// The listener connects on its own schedule, and a notification sent
	// before it has is lost, so keep writing until one arrives.
	var got Event
	deadline := time.After(15 * time.Second)
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
wait:
	for {
		select {
		case got = <-aEvents:
			break wait
		case <-tick.C:
			e.exec(`INSERT INTO runner_runs (tenant_id, kind) VALUES ($1, 'index')`, e.a.tenantID)
		case <-deadline:
			t.Fatal("member of A received no event")
		}
	}
	if got.Tenant != "webapi-a" || got.Kind != store.EventRunnerRun {
		t.Errorf("event = %+v, want a runner_run of webapi-a", got)
	}
	e.exec(`INSERT INTO runner_runs (tenant_id, kind) VALUES ($1, 'index')`, e.a.tenantID)
	select {
	case ev := <-bEvents:
		t.Errorf("member of B received %+v", ev)
	case <-time.After(time.Second):
	}

	// Once Run returns, as on shutdown, the open streams end so the
	// browsers reconnect elsewhere.
	stopRun()
	<-ran
	end := time.After(5 * time.Second)
	for _, ch := range []<-chan Event{aEvents, bEvents} {
	drain:
		for {
			select {
			case _, ok := <-ch:
				if !ok {
					break drain
				}
			case <-end:
				t.Fatal("a stream stayed open after Run returned")
			}
		}
	}
}
