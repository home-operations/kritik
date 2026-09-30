//go:build integration

package webapi

import (
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
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/home-operations/kritik/internal/auth"
	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/configsource"
	"github.com/home-operations/kritik/internal/jobs"
	"github.com/home-operations/kritik/internal/store"
)

const manageConfig = `
auth:
  oidc:
    issuer: https://idp.example
    clientId: kritik
    clientSecret: { env: KRITIK_TEST_TOKEN }
    roleMappingExpr: '"kritik-admin" in roles ? "admin" : ""'
apps:
  - name: mgr-file-bot
    accounts: [mf, md]
    clientId: Iv1.test
    privateKey: { env: KRITIK_TEST_TOKEN }
    webhookSecret: { env: KRITIK_TEST_TOKEN }
repositories:
  md/one: {}
`

// fakeActions records each action with the account its transaction was
// scoped to.
type fakeActions struct {
	mu            sync.Mutex
	calls         []string
	notCancelable string
}

func (f *fakeActions) note(ctx context.Context, tx pgx.Tx, format string, args ...any) error {
	var scoped string
	if err := tx.QueryRow(ctx, `SELECT current_setting('app.account_id', true)`).Scan(&scoped); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf(format, args...)+" in "+scoped)
	return nil
}

func (f *fakeActions) Rerun(ctx context.Context, tx pgx.Tx, accountID, repositoryID string, number int) (int64, error) {
	return 101, f.note(ctx, tx, "rerun %s %s %d", accountID, repositoryID, number)
}

func (f *fakeActions) Cancel(ctx context.Context, tx pgx.Tx, reviewID string) error {
	if reviewID == f.notCancelable {
		return jobs.ErrNotCancelable
	}
	return f.note(ctx, tx, "cancel %s", reviewID)
}

func (f *fakeActions) Reindex(ctx context.Context, tx pgx.Tx, accountID, repositoryID string) (int64, error) {
	return 202, f.note(ctx, tx, "reindex %s %s", accountID, repositoryID)
}

func (f *fakeActions) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return ""
	}
	return f.calls[len(f.calls)-1]
}

type manageEnv struct {
	t       *testing.T
	st      *store.Store
	owner   *pgxpool.Pool
	src     *configsource.Source
	actions *fakeActions
	srv     *Server
	http    *httptest.Server
	cookie  map[string]*http.Cookie
}

func newManageEnv(t *testing.T) *manageEnv {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(ctx, store.Options{
		AppURL: testEnv(t, "KRITIK_TEST_APP_URL"), OwnerURL: testEnv(t, "KRITIK_TEST_OWNER_URL"),
		Logger: logger,
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
	e := &manageEnv{t: t, st: st, owner: owner, actions: &fakeActions{}, cookie: map[string]*http.Cookie{}}

	t.Setenv("KRITIK_TEST_TOKEN", "tok")
	path := filepath.Join(t.TempDir(), "kritik.yaml")
	if err := os.WriteFile(path, []byte(manageConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	e.src = &configsource.Source{Logger: logger}
	file, err := e.src.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := st.ApplyConfig(ctx, file); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}

	webURL, _ := url.Parse("https://kritik.example")
	h, err := auth.New(auth.Config{Store: st, Current: e.src.Current, WebURL: webURL, Logger: logger})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	e.srv = New(Config{Store: st, Current: e.src.Current, Auth: h, Actions: e.actions, WebURL: webURL, Logger: logger})
	e.http = httptest.NewServer(e.srv.Handler())
	t.Cleanup(e.http.Close)
	e.signIn("admin", "mgr-op", store.SessionGrant{Role: store.RoleAdmin})
	e.signIn("outsider", "mgr-outsider", memberOfAccounts("github/nobody"))
	return e
}

func (e *manageEnv) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.owner.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}

func (e *manageEnv) signIn(name, subject string, g store.SessionGrant) {
	e.t.Helper()
	ctx, now, origin := context.Background(), time.Now(), "oidc:https://idp.example"
	user, err := e.st.UpsertIdentity(ctx, store.SignInIdentity{
		Provider: "oidc", Origin: origin, Subject: subject, DisplayName: name, Email: name + "@example.com", EmailVerified: true,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	g.Key, _ = auth.GrantKey(e.src.Current.Get().Auth, "oidc")
	token, err := e.st.CreateSession(ctx, user.ID, "oidc", origin, g, now, now.Add(time.Hour))
	if err != nil {
		e.t.Fatal(err)
	}
	e.cookie[name] = &http.Cookie{Name: auth.SessionCookieName(&url.URL{Scheme: "https", Host: "kritik.example"}), Value: token}
}

// do sends a request as who; a mutation carries the same-origin headers
// unless csrf is false.
func (e *manageEnv) do(who, method, path string, csrf ...bool) (int, []byte) {
	e.t.Helper()
	req, err := http.NewRequest(method, e.http.URL+path, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	if c := e.cookie[who]; c != nil {
		req.AddCookie(c)
	}
	if len(csrf) == 0 || csrf[0] {
		req.Header.Set("Origin", "https://kritik.example")
		req.Header.Set("X-Kritik", "1")
	}
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

// expect checks a response's status and, for an error, its code.
func (e *manageEnv) expect(status int, body []byte, wantStatus int, wantCode ErrorCode) {
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

func (e *manageEnv) audits(action AuditAction, target string) int {
	e.t.Helper()
	var n int
	if err := e.owner.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE action = $1 AND target = $2`,
		string(action), target).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

const mdPath = "/api/v1/accounts/github/md"

// TestManage walks the actions an admin takes and the audit log they
// leave; each step depends on the ones before it.
func TestManage(t *testing.T) {
	e := newManageEnv(t)
	md := configfile.AccountID(configfile.ForgeGitHub, "md")
	e.signIn("member", "mgr-member", memberOfAccounts("github/md"))
	t.Run("actions", func(t *testing.T) { testActions(t, e, md) })
	t.Run("audit log", func(t *testing.T) { testAuditLog(t, e, md) })
}

func testActions(t *testing.T, e *manageEnv, md string) {
	repoID := configfile.RepositoryID(md, "md/one")
	e.exec(`INSERT INTO pull_requests (account_id, repository_id, number, title, author, head_sha)
		VALUES ($1, $2, 3, 'x', 'ada', 'h3') ON CONFLICT DO NOTHING`, md, repoID)

	status, body := e.do("member", "POST", mdPath+"/pulls/md/one/3/rerun")
	e.expect(status, body, http.StatusForbidden, CodeForbidden)
	status, body = e.do("admin", "POST", mdPath+"/pulls/md/one/3/rerun", false)
	e.expect(status, body, http.StatusForbidden, "")
	status, body = e.do("admin", "POST", mdPath+"/pulls/md/one/99/rerun")
	e.expect(status, body, http.StatusNotFound, CodeNotFound)

	status, body = e.do("admin", "POST", mdPath+"/pulls/md/one/3/rerun")
	e.expect(status, body, http.StatusAccepted, "")
	if string(bytes.TrimSpace(body)) != `{"jobId":101}` || e.actions.last() != fmt.Sprintf("rerun %s %s 3 in %s", md, repoID, md) {
		t.Errorf("rerun = %s, call %q", body, e.actions.last())
	}
	if n := e.audits(AuditReviewRerun, "md/one#3"); n != 1 {
		t.Errorf("review.rerun audit rows = %d", n)
	}

	review := "00000000-0000-4000-8000-000000000001"
	e.actions.notCancelable = "00000000-0000-4000-8000-000000000002"
	status, body = e.do("admin", "POST", mdPath+"/reviews/"+e.actions.notCancelable+"/cancel")
	e.expect(status, body, http.StatusConflict, CodeNotCancelable)
	status, body = e.do("admin", "POST", mdPath+"/reviews/"+review+"/cancel")
	e.expect(status, body, http.StatusAccepted, "")
	if e.actions.last() != fmt.Sprintf("cancel %s in %s", review, md) {
		t.Errorf("cancel call %q", e.actions.last())
	}
	if e.audits(AuditReviewCancel, review) != 1 || e.audits(AuditReviewCancel, e.actions.notCancelable) != 0 {
		t.Errorf("review.cancel audit rows are wrong")
	}

	status, body = e.do("admin", "POST", mdPath+"/repos/md/one/reindex")
	e.expect(status, body, http.StatusAccepted, "")
	if e.actions.last() != fmt.Sprintf("reindex %s %s in %s", md, repoID, md) || e.audits(AuditRepoReindex, "md/one") != 1 {
		t.Errorf("reindex = %s, call %q", body, e.actions.last())
	}
}

func testAuditLog(t *testing.T, e *manageEnv, md string) {
	status, body := e.do("member", "GET", mdPath+"/audit")
	e.expect(status, body, http.StatusForbidden, CodeForbidden)
	status, body = e.do("member", "GET", "/api/v1/admin/audit")
	e.expect(status, body, http.StatusForbidden, CodeForbidden)

	var seen []AuditEvent
	path := mdPath + "/audit?limit=2"
	for range 20 {
		status, body = e.do("admin", "GET", path)
		e.expect(status, body, http.StatusOK, "")
		var page Page[AuditEvent]
		if err := json.Unmarshal(body, &page); err != nil {
			t.Fatal(err)
		}
		seen = append(seen, page.Items...)
		if page.NextCursor == nil {
			break
		}
		path = mdPath + "/audit?limit=2&cursor=" + *page.NextCursor
	}
	var want int
	if err := e.owner.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE account_id = $1`, md).Scan(&want); err != nil {
		t.Fatal(err)
	}
	if len(seen) != want || want < 3 {
		t.Fatalf("paged %d events, want %d", len(seen), want)
	}
	var prev int64
	for i, ev := range seen {
		id, err := strconv.ParseInt(ev.ID, 10, 64)
		if err != nil || ev.Account != "github/md" || ev.Actor == nil || (i > 0 && id >= prev) {
			t.Errorf("event %d = %+v, want newest first", i, ev)
		}
		prev = id
	}
	status, body = e.do("admin", "GET", "/api/v1/admin/audit?limit=200")
	e.expect(status, body, http.StatusOK, "")
	if !strings.Contains(string(body), `"account":"github/md","action":"repo.reindex","target":"md/one"`) {
		t.Errorf("admin audit lacks the account's reindex: %s", body)
	}
}
