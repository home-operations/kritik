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
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/home-operations/kritika/internal/auth"
	"github.com/home-operations/kritika/internal/configfile"
	"github.com/home-operations/kritika/internal/configfile/configfiletest"
	"github.com/home-operations/kritika/internal/model"
	"github.com/home-operations/kritika/internal/store"
	"github.com/home-operations/kritika/internal/store/storetest"
	"github.com/home-operations/kritika/internal/transcript"
)

const integrationConfig = `
auth:
  oidc:
    issuer: https://idp.example
    clientId: kritika
    clientSecret: { env: KRITIKA_TEST_TOKEN }
    roleMappingExpr: '"kritika-admin" in roles ? "admin" : ""'
apps:
  webapi-a-bot:
    accounts: [wa]
    clientId: Iv1.test
    privateKey: { env: KRITIKA_TEST_TOKEN }
    webhookSecret: { env: KRITIKA_TEST_TOKEN }
  webapi-b-bot:
    accounts: [wb]
    clientId: Iv1.test
    privateKey: { env: KRITIKA_TEST_TOKEN }
    webhookSecret: { env: KRITIKA_TEST_TOKEN }
repositories:
  wa/one: {}
  wa/two: {}
  wb/one: {}
`

// followupComment is answered in both accounts, as comment ids from two
// forges may collide; onlyBComment is answered only in account B. Both
// exceed int4, as GitHub's comment ids do, so the queries must bind them
// as bigint.
const (
	followupComment = 4168513971
	onlyBComment    = 4168513972
)

// seeded is what seedAccount wrote for one account.
type seeded struct {
	accountID, repoID, prID, reviewID, runID string
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
	logger := slog.New(slog.DiscardHandler)
	st := storetest.Open(t)
	owner, err := pgxpool.New(ctx, storetest.Env(t, "KRITIKA_TEST_OWNER_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	t.Setenv("KRITIKA_TEST_TOKEN", "tok")
	file := configfiletest.Load(t, integrationConfig)
	if err := st.ApplyConfig(ctx, file); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	cur := configfile.NewCurrent(file)
	webURL, _ := url.Parse("https://kritika.example")
	h, err := auth.New(auth.Config{Store: st, Current: cur, WebURL: webURL, Logger: logger})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	e := &apiEnv{t: t, st: st, owner: owner, file: file, cookie: map[string]*http.Cookie{}}
	e.srv = New(Config{Store: st, Current: cur, Auth: h, WebURL: webURL, Logger: logger})
	e.http = httptest.NewServer(e.srv.Handler())
	t.Cleanup(e.http.Close)
	e.a, e.b = e.seedAccount("webapi-a", "wa/one"), e.seedAccount("webapi-b", "wb/one")
	e.exec(`INSERT INTO followups (account_id, pull_request_id, comment_id, author, status)
		VALUES ($1, $2, $3, 'carol', 'answered')`, e.b.accountID, e.b.prID, onlyBComment)
	e.signIn("member-a", "alice", memberOfAccounts("github/wa"))
	e.signIn("member-b", "bob", memberOfAccounts("github/wb"))
	e.signIn("admin", "op-sub", store.SessionGrant{Role: store.RoleAdmin})
	return e
}

// memberOfAccounts is a member's grant on the forge accounts named.
func memberOfAccounts(accounts ...string) store.SessionGrant {
	return store.SessionGrant{Role: store.RoleMember, Accounts: accounts}
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

// seedAccount writes a pull request with a finished agentic review, its
// runner and agent runs, context pack, findings, usage, model calls, an
// index run, a follow-up and a queued job, all as the owner so no policy
// stands in the way.
func (e *apiEnv) seedAccount(slug, repo string) seeded {
	e.t.Helper()
	owner, _, _ := strings.Cut(repo, "/")
	a, _ := e.file.Account(configfile.ForgeGitHub, owner)
	s := seeded{accountID: a.ID()}
	s.repoID = configfile.RepositoryID(a.ID(), repo)
	e.exec(`UPDATE repositories SET default_branch = 'main' WHERE id = $1`, s.repoID)
	s.prID = e.scalar(`INSERT INTO pull_requests (account_id, repository_id, number, title, author, head_sha, labels)
		VALUES ($1, $2, 7, $3, 'ada', 'head7', '[{"name":"bug","color":"f00"}]') RETURNING id::text`, s.accountID, s.repoID, "PR of "+slug)
	s.reviewID = e.scalar(`INSERT INTO reviews (account_id, pull_request_id, head_sha, status, trigger, model, finished_at, summary)
		VALUES ($1, $2, 'head7', 'completed', 'push', 'acme/large', now(), '{"take":"ok","praise":["tests"]}')
		RETURNING id::text`, s.accountID, s.prID)
	e.exec(`INSERT INTO findings (account_id, review_id, path, line, severity, title, explanation)
		VALUES ($1, $2, 'a.go', 3, 'blocking', 'nil deref', 'x'), ($1, $2, 'b.go', 9, 'nit', 'naming', 'y')`, s.accountID, s.reviewID)
	s.runID = e.scalar(`INSERT INTO runner_runs (account_id, review_id, kind, phase, log_tail)
		VALUES ($1, $2, 'review', 'done', $3) RETURNING id::text`, s.accountID, s.reviewID, "tail of "+slug)
	e.exec(`INSERT INTO context_packs (runner_run_id, account_id, head_sha, base_sha, patch_id, diff, changed_paths, stages, repo_files)
		VALUES ($1, $2, 'head7', 'base7', 'patch7', $3, '{a.go}',
			'[{"stage":"definitions","path":"b.go","start_line":1,"end_line":2,"text":"func F() {}"}]',
			jsonb_build_object('.kritika.yaml', $4::text))`, s.runID, s.accountID, "diff of "+slug,
		"rules: [{ id: house-style, file: docs/rules-of-"+slug+".md }]\n")
	e.exec(`INSERT INTO agent_runs (runner_run_id, account_id, stop_reason, result, steps, tool_calls, timeline, model, sources)
		VALUES ($1, $2, 'submitted', '{"findings":[]}', 2, '{"grep":1}',
			'[{"index":0,"tools":["grep"],"duration_ms":5,"output_bytes":7,"input_tokens":10,"output_tokens":2}]', 'acme/large',
			'["https://docs.example"]')`, s.runID, s.accountID)
	e.exec(`INSERT INTO usage (account_id, repository_id, review_id, role, model, input_tokens, output_tokens, cost_usd)
		VALUES ($1, $2, $3, 'review', 'acme/large', 100, 10, 0.5)`, s.accountID, s.repoID, s.reviewID)
	ix := e.scalar(`INSERT INTO index_runs (account_id, repository_id, commit_sha, embed_model, embed_dims, mode, status, finished_at)
		VALUES ($1, $2, 'commit7', 'embed', 8, 'full', 'completed', now()) RETURNING id::text`, s.accountID, s.repoID)
	e.exec(`UPDATE repositories SET active_index_run_id = $1 WHERE id = $2`, ix, s.repoID)
	e.exec(`INSERT INTO followups (account_id, pull_request_id, comment_id, author, status, model)
		VALUES ($1, $2, $3, 'bob', 'answered', 'acme/large')`, s.accountID, s.prID, followupComment)
	args, _ := json.Marshal(map[string]any{
		"account_id": s.accountID, "repository_id": s.repoID, "number": 7, "head_sha": "head7", "trigger": "push",
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
		err := e.st.WithAccount(ctx, s.accountID, func(tx pgx.Tx) error {
			prev, n, err := store.AgentState(ctx, tx, s.runID)
			if err != nil {
				return err
			}
			row := transcript.Delta(prev, req, nil)
			row.Response = transcript.Response{Text: "ok", Stop: model.StopToolUse}
			return store.InsertModelCall(ctx, tx, store.ModelCall{
				AccountID: s.accountID, ReviewID: s.reviewID, RunnerRunID: s.runID, Kind: store.ModelCallAgentStep, Step: n,
				Model: "acme/large", Row: row.Encode(), Usage: model.Usage{Input: 10, CacheRead: 4, Output: 2}, CostUSD: 0.25,
				Duration: time.Second,
			})
		})
		if err != nil {
			e.t.Fatal(err)
		}
	}
	err := e.st.WithAccount(ctx, s.accountID, func(tx pgx.Tx) error {
		row := transcript.Delta(transcript.State{}, model.StepRequest{System: "follow of " + slug, Messages: msgs[:1]}, nil)
		row.Response = transcript.Response{Text: "reply", Stop: model.StopEndTurn}
		return store.InsertModelCall(ctx, tx, store.ModelCall{
			AccountID: s.accountID, ReviewID: s.reviewID, FollowupCommentID: followupComment, Kind: store.ModelCallFollowUp,
			Model: "acme/large", Row: row.Encode(),
		})
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *apiEnv) signIn(name, subject string, g store.SessionGrant) {
	e.t.Helper()
	ctx, now, origin := context.Background(), time.Now(), "oidc:https://idp.example"
	user, err := e.st.UpsertIdentity(ctx, store.SignInIdentity{
		Provider: "oidc", Origin: origin, Subject: subject, DisplayName: name,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	g.Key, _ = auth.GrantKey(e.file.Auth, "oidc")
	token, err := e.st.CreateSession(ctx, user.ID, "oidc", origin, g, now, now.Add(time.Hour))
	if err != nil {
		e.t.Fatal(err)
	}
	e.cookie[name] = &http.Cookie{Name: auth.SessionCookieName(&url.URL{Scheme: "https", Host: "kritika.example"}), Value: token}
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
	t.Run("read endpoints scope to the account", func(t *testing.T) { testReadEndpointsScopeToAccount(t, e) })
	t.Run("account B's ids under account A", func(t *testing.T) { testCrossAccountIDs(t, e) })
	t.Run("me and account lists", func(t *testing.T) { testMeAndAccountLists(t, e) })
	t.Run("account detail shows each connection's last webhook", func(t *testing.T) { testLastWebhook(t, e) })
	t.Run("transcripts equal Rebuild", func(t *testing.T) { testTranscriptsEqualRebuild(t, e) })
	t.Run("repository pagination", func(t *testing.T) { testRepoPagination(t, e) })
	t.Run("repository kinds", func(t *testing.T) { testRepoKinds(t, e) })
	t.Run("pull requests by author", func(t *testing.T) { testPullsByAuthor(t, e) })
	t.Run("pull numbers outside int4", func(t *testing.T) { testPullNumbersOutsideInt4(t, e) })
	t.Run("event stream scopes to the account", func(t *testing.T) { testEventStreamScopesToAccount(t, e) })
}

func testReadEndpointsScopeToAccount(t *testing.T, e *apiEnv) {
	a := "/api/v1/accounts/github/wa"
	// Each path of account A and a string only account A's answer contains.
	endpoints := []struct{ path, marker string }{
		{a, `"slug":"github/wa"`},
		{a + "/repos", `"fullName":"wa/one"`},
		{a + "/repos/wa/one", `"activeCommit":"commit7"`},
		{a + "/repos/wa/one", `"commit":"base7","found":true`},
		{a + "/pulls", `"title":"PR of webapi-a"`},
		{a + "/pulls?state=all&repo=wa/one&outcome=completed&q=webapi-a", `"title":"PR of webapi-a"`},
		{a + "/pulls?state=all&author=ADA", `"title":"PR of webapi-a"`},
		{a + "/pulls/wa/one/7", `"title":"PR of webapi-a"`},
		{a + "/pulls/wa/one/7", `"job":{"id":`},
		{a + "/pulls/wa/one/7", `"reviewCount":1,"costUsd":0.5`},
		{a + "/pulls?repo=wa/one", `"reviewCount":1,"costUsd":0.5`},
		{a + "/reviews/" + e.a.reviewID, `"logTail":"tail of webapi-a"`},
		{a + "/reviews/" + e.a.reviewID + "/diff", `"diff":"diff of webapi-a"`},
		{a + "/reviews/" + e.a.reviewID + "/transcript", `"system":"sys of webapi-a"`},
		{a + "/reviews/" + e.a.reviewID + "/raw", `"logTail":"tail of webapi-a"`},
		{a + "/index-runs?repo=wa/one", `"commitSha":"commit7"`},
		{a + "/findings?repo=wa/one&severity=blocking&status=open", `"title":"nil deref"`},
		{a + "/rules", `"path":"docs/rules-of-webapi-a.md","description":"","paths":[],"whenExpr":"","source":"repository","repositories":["wa/one"]`},
		{a + "/followups?repo=wa/one", fmt.Sprintf(`"commentId":%d`, followupComment)},
		{a + fmt.Sprintf("/followups/%d/transcript", followupComment), `"system":"follow of webapi-a"`},
		{a + "/usage?group=repo", `"key":"wa/one"`},
		{a + "/analytics?group=week", `"repository":"wa/one"`},
		{a + "/queue", `"repository":"wa/one"`},
	}
	for _, ep := range endpoints {
		t.Run(ep.path, func(t *testing.T) {
			for _, tc := range []struct {
				who    string
				status int
			}{{"member-a", 200}, {"admin", 200}, {"member-b", 404}, {"nobody", 401}} {
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
						t.Errorf("%s: body leaks account B's %q: %s", tc.who, leak, body)
					}
				}
			}
		})
	}
}

// bMarkers are strings only account B's rows contain.
func (e *apiEnv) bMarkers() []string {
	return []string{
		"webapi-b", "wb/one", "PR of webapi-b", "tail of webapi-b", "diff of webapi-b", "sys of webapi-b", "follow of webapi-b",
		e.b.reviewID, e.b.prID, e.b.runID, e.b.repoID,
	}
}

// testCrossAccountIDs asks for account B's rows by id under account A's slug:
// even an admin, who may read B, finds nothing, since the query runs
// scoped to A.
func testCrossAccountIDs(t *testing.T, e *apiEnv) {
	a := "/api/v1/accounts/github/wa"
	paths := []string{
		a + "/reviews/" + e.b.reviewID, a + "/reviews/" + e.b.reviewID + "/diff",
		a + "/reviews/" + e.b.reviewID + "/transcript", a + "/reviews/" + e.b.reviewID + "/raw",
		a + fmt.Sprintf("/followups/%d/transcript", onlyBComment), a + "/pulls/wb/one/7", a + "/repos/wb/one",
	}
	for _, path := range paths {
		for _, who := range []string{"member-a", "admin"} {
			t.Run(who+" "+path, func(t *testing.T) {
				if status, body := e.getBody(who, path); status != 404 {
					t.Errorf("status = %d, want 404: %s", status, body)
				}
			})
		}
	}
}

func testMeAndAccountLists(t *testing.T, e *apiEnv) {
	tests := []struct {
		who, path string
		status    int
		want      []string
		not       []string
	}{
		{"member-a", "/api/v1/me", 200, []string{`"admin":false`, `"accounts":["github/wa"]`}, []string{"webapi-b"}},
		{"admin", "/api/v1/me", 200, []string{`"admin":true`, `"accounts":["github/wa","github/wb"]`}, nil},
		{"member-a", "/api/v1/accounts", 200, []string{`"slug":"github/wa"`, `"repositories":2`, `"reviews7d":1`}, []string{"webapi-b"}},
		{"member-b", "/api/v1/accounts", 200, []string{`"slug":"github/wb"`}, []string{"webapi-a"}},
		{"admin", "/api/v1/admin/accounts", 200, []string{`"slug":"github/wa"`, `"slug":"github/wb"`, `"live":true`}, nil},
		{"member-a", "/api/v1/admin/accounts", 404, nil, nil},
		{"nobody", "/api/v1/accounts", 401, nil, nil},
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

func testLastWebhook(t *testing.T, e *apiEnv) {
	ctx := context.Background()
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	in, _ := e.file.Connection("webapi-a-bot")
	if err := e.st.WithAccount(ctx, e.a.accountID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE connections SET last_webhook_at = $2 WHERE id = $1`, in.ID(), at)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		who, slug string
		want      *time.Time
	}{
		{"member-a", "github/wa", &at},
		{"member-b", "github/wb", nil},
	} {
		status, body := e.getBody(tt.who, "/api/v1/accounts/"+tt.slug)
		var d AccountDetail
		if status != 200 || json.Unmarshal(body, &d) != nil || d.Connection.Name == "" {
			t.Fatalf("%s: status %d: %s", tt.who, status, body)
		}
		got := d.Connection.LastWebhookAt
		if (got == nil) != (tt.want == nil) || got != nil && !got.Equal(*tt.want) {
			t.Errorf("%s: lastWebhookAt = %v, want %v", tt.who, got, tt.want)
		}
	}
}

func testTranscriptsEqualRebuild(t *testing.T, e *apiEnv) {
	ctx := context.Background()
	var steps, followups []transcript.StoredRow
	if err := e.st.WithAccount(ctx, e.a.accountID, func(tx pgx.Tx) error {
		var err error
		if steps, err = store.ReviewModelCalls(ctx, tx, e.a.reviewID); err != nil {
			return err
		}
		followups, err = store.FollowupModelCalls(ctx, tx, e.a.prID, followupComment)
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
		{"/api/v1/accounts/github/wa/reviews/" + e.a.reviewID + "/transcript", steps},
		{fmt.Sprintf("/api/v1/accounts/github/wa/followups/%d/transcript", followupComment), followups},
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
	path := "/api/v1/accounts/github/wa/repos?limit=1"
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
		path = "/api/v1/accounts/github/wa/repos?limit=1&cursor=" + *page.NextCursor
	}
	if strings.Join(names, ",") != "wa/one,wa/two" {
		t.Errorf("paged repositories = %v, want wa/one, wa/two", names)
	}
}

// testRepoKinds: the repository list leaves out archived repositories and
// forks, but for a fork turned on by its own entry, as wa/one's is; each
// has a list of its own, and neither counts as the account's.
func testPullsByAuthor(t *testing.T, e *apiEnv) {
	for author, want := range map[string]string{"ada": `"title":"PR of webapi-a"`, "Ada": `"title":"PR of webapi-a"`, "ad": `"items":[]`} {
		status, body := e.getBody("member-a", "/api/v1/accounts/github/wa/pulls?state=all&author="+author)
		if status != 200 || !bytes.Contains(body, []byte(want)) {
			t.Errorf("author=%s: status %d, body %s, want %s", author, status, body, want)
		}
	}
}

// testPullNumbersOutsideInt4 asks for pull numbers no int4 column can
// hold: no such pull exists, so a search matches nothing and a lookup is
// not found, rather than a failure to bind the number.
func testPullNumbersOutsideInt4(t *testing.T, e *apiEnv) {
	a := "/api/v1/accounts/github/wa"
	for path, want := range map[string]struct {
		status int
		marker string
	}{
		a + "/pulls?state=all&q=%233000000000": {200, `"items":[]`},
		a + "/findings?q=%233000000000":        {200, `"items":[]`},
		a + "/pulls/wa/one/3000000000":         {404, `"code":"not_found"`},
	} {
		status, body := e.getBody("member-a", path)
		if status != want.status || !bytes.Contains(body, []byte(want.marker)) {
			t.Errorf("%s: status %d, body %s, want %d with %s", path, status, body, want.status, want.marker)
		}
	}
}

func testRepoKinds(t *testing.T, e *apiEnv) {
	t.Cleanup(func() {
		e.exec(`UPDATE repositories SET fork = false, archived = false, turned_on = NULL WHERE name IN ('wa/one', 'wa/two')`)
	})
	list := func(query string) string {
		t.Helper()
		status, body := e.getBody("member-a", "/api/v1/accounts/github/wa/repos"+query)
		if status != 200 {
			t.Fatalf("%s: status = %d: %s", query, status, body)
		}
		var page Page[Repository]
		if err := json.Unmarshal(body, &page); err != nil {
			t.Fatal(err)
		}
		out := make([]string, 0, len(page.Items))
		for _, r := range page.Items {
			out = append(out, fmt.Sprintf("%s:%v", r.FullName, r.Enabled))
		}
		return strings.Join(out, ",")
	}
	count := func() int {
		t.Helper()
		_, body := e.getBody("member-a", "/api/v1/accounts")
		var accounts []AccountSummary
		if err := json.Unmarshal(body, &accounts); err != nil || len(accounts) != 1 {
			t.Fatalf("accounts = %s, %v", body, err)
		}
		return accounts[0].Repositories
	}
	// An admin turned the fork wa/one on.
	e.exec(`UPDATE repositories SET fork = true, turned_on = name = 'wa/one' WHERE name IN ('wa/one', 'wa/two')`)
	for _, tt := range []struct{ query, want string }{{"", "wa/one:true"}, {"?type=forks", "wa/one:true,wa/two:false"}, {"?type=archived", ""}} {
		if got := list(tt.query); got != tt.want {
			t.Errorf("forks: repos%s = %q, want %q", tt.query, got, tt.want)
		}
	}
	if got := count(); got != 1 {
		t.Errorf("forks: the account counts %d repositories, want 1", got)
	}
	e.exec(`UPDATE repositories SET archived = true WHERE name = 'wa/one'`)
	for _, tt := range []struct{ query, want string }{{"", ""}, {"?type=forks", "wa/two:false"}, {"?type=archived", "wa/one:false"}} {
		if got := list(tt.query); got != tt.want {
			t.Errorf("archived: repos%s = %q, want %q", tt.query, got, tt.want)
		}
	}
	if got := count(); got != 0 {
		t.Errorf("archived: the account counts %d repositories, want 0", got)
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
				if json.Unmarshal([]byte(data), &ev) == nil && ev.Account != "" {
					out <- ev
				}
			}
		}
	}()
	return out
}

func testEventStreamScopesToAccount(t *testing.T, e *apiEnv) {
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
			e.exec(`INSERT INTO runner_runs (account_id, kind) VALUES ($1, 'index')`, e.a.accountID)
		case <-deadline:
			t.Fatal("member of A received no event")
		}
	}
	if got.Account != "github/wa" || got.Kind != store.EventRunnerRun {
		t.Errorf("event = %+v, want a runner_run of webapi-a", got)
	}
	e.exec(`INSERT INTO runner_runs (account_id, kind) VALUES ($1, 'index')`, e.a.accountID)
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
