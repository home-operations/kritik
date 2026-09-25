//go:build integration

package webapi

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
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
	"github.com/home-operations/kritik/internal/ingest"
	"github.com/home-operations/kritik/internal/sealbox"
	"github.com/home-operations/kritik/internal/store"
)

const manageConfig = `
web:
  signIn:
    - name: corp
      type: oidc
      issuer: https://idp.example
      clientId: kritik
      clientSecret: { env: KRITIK_TEST_TOKEN }
  operators: ["corp:mgr-op"]
  dashboardForgeHosts: [git.example, git2.example]
tenants:
  - slug: mgr-file
    installations:
      - name: mgr-file-bot
        forge: forgejo
        host: git.example
        account: mf
        token: { env: KRITIK_TEST_TOKEN }
        webhookSecret: { env: KRITIK_TEST_TOKEN }
    repositories:
      - name: mf/one
`

// fakeActions records each action with the tenant its transaction was
// scoped to.
type fakeActions struct {
	mu            sync.Mutex
	calls         []string
	notCancelable string
}

func (f *fakeActions) note(ctx context.Context, tx pgx.Tx, format string, args ...any) error {
	var scoped string
	if err := tx.QueryRow(ctx, `SELECT current_setting('app.tenant_id', true)`).Scan(&scoped); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf(format, args...)+" in "+scoped)
	return nil
}

func (f *fakeActions) Rerun(ctx context.Context, tx pgx.Tx, tenantID, repositoryID string, number int) (int64, error) {
	return 101, f.note(ctx, tx, "rerun %s %s %d", tenantID, repositoryID, number)
}

func (f *fakeActions) Cancel(ctx context.Context, tx pgx.Tx, reviewID, by string) error {
	if reviewID == f.notCancelable {
		return ErrNotCancelable
	}
	return f.note(ctx, tx, "cancel %s by %s", reviewID, by)
}

func (f *fakeActions) Reindex(ctx context.Context, tx pgx.Tx, tenantID, repositoryID string) (int64, error) {
	return 202, f.note(ctx, tx, "reindex %s %s", tenantID, repositoryID)
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
	account map[string]string
}

func newManageEnv(t *testing.T) *manageEnv {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
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
	e := &manageEnv{t: t, st: st, owner: owner, actions: &fakeActions{}, cookie: map[string]*http.Cookie{}, account: map[string]string{}}
	// Other suites leave dashboard tenants sealed under other keys.
	e.exec(`DELETE FROM dashboard_tenants`)
	t.Cleanup(func() { _, _ = owner.Exec(context.Background(), `DELETE FROM dashboard_tenants`) })

	t.Setenv("KRITIK_TEST_TOKEN", "tok")
	path := filepath.Join(t.TempDir(), "kritik.yaml")
	if err := os.WriteFile(path, []byte(manageConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	kr, err := sealbox.NewKeyring(key)
	if err != nil {
		t.Fatal(err)
	}
	e.src = &configsource.Source{Store: st, Keyring: kr, Logger: logger, Poll: time.Second}
	file, err := e.src.Load(ctx, path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := st.ApplyConfig(ctx, file, "manage-test"); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	go func() { _ = e.src.Run(ctx, path, time.Hour) }()

	webURL, _ := url.Parse("https://kritik.example")
	h, err := auth.New(auth.Config{Store: st, Current: e.src.Current, WebURL: webURL, Logger: logger})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	e.srv = New(Config{Store: st, Current: e.src.Current, Auth: h, Keyring: kr, Actions: e.actions, WebURL: webURL, Logger: logger})
	e.http = httptest.NewServer(e.srv.Handler())
	t.Cleanup(e.http.Close)
	e.signIn("operator", "mgr-op", nil)
	e.signIn("outsider", "mgr-outsider", nil)
	return e
}

func (e *manageEnv) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.owner.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}

func (e *manageEnv) scalar(sql string) string {
	e.t.Helper()
	var v string
	if err := e.owner.QueryRow(context.Background(), sql).Scan(&v); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
	return v
}

func (e *manageEnv) signIn(name, subject string, grants []store.Grant) {
	e.t.Helper()
	ctx, now, origin := context.Background(), time.Now(), "oidc:https://idp.example"
	acct, err := e.st.UpsertIdentity(ctx, store.SignInIdentity{
		Provider: "corp", Origin: origin, Subject: subject, DisplayName: name, Email: name + "@example.com", EmailVerified: true,
	}, now)
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
	e.cookie[name] = &http.Cookie{Name: auth.CookieName, Value: token}
	e.account[name] = acct.ID
}

// do sends a request as who; a mutation carries the same-origin headers
// unless csrf is false.
func (e *manageEnv) do(who, method, path string, body any, csrf ...bool) (int, []byte) {
	e.t.Helper()
	var r io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			e.t.Fatal(err)
		}
		r = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, e.http.URL+path, r)
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

func (e *manageEnv) totalAudits() int {
	e.t.Helper()
	var n int
	if err := e.owner.QueryRow(context.Background(), `SELECT count(*) FROM audit_events`).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// waitFor polls until cond holds: configsource picks a write up on its
// notification, asynchronously.
func (e *manageEnv) waitFor(what string, cond func(*configfile.File) bool) {
	e.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond(e.src.Current.Get()) {
		if time.Now().After(deadline) {
			e.t.Fatalf("timed out waiting for %s (last error %v)", what, e.src.LastError())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func dashSpec(tokenRef, hookRef map[string]any, extra map[string]any) map[string]any {
	spec := map[string]any{
		"slug": "mgr-dash",
		"installations": []any{map[string]any{
			"name": "mgr-dash-bot", "forge": "forgejo", "host": "git.example", "account": "md",
			"token": tokenRef, "webhookSecret": hookRef,
		}},
		"repositories": []any{map[string]any{"name": "md/one"}},
	}
	maps.Copy(spec, extra)
	return spec
}

var keep = map[string]any{"keep": true}

// TestManage walks one dashboard tenant through its life; each step
// depends on the ones before it.
func TestManage(t *testing.T) {
	e := newManageEnv(t)
	var generated string
	t.Run("operator creates a dashboard tenant", func(t *testing.T) { generated = testCreate(t, e) })
	t.Run("the generated webhook secret verifies a hook", func(t *testing.T) { testHookVerifies(t, e, generated) })
	dashID := (&configfile.Tenant{Slug: "mgr-dash"}).ID()
	e.signIn("admin", "mgr-admin", []store.Grant{{TenantID: dashID, Role: store.RoleAdmin}})
	e.signIn("member", "mgr-member", []store.Grant{{TenantID: dashID, Role: store.RoleMember}})
	t.Run("config reads are redacted", func(t *testing.T) { testConfigRedacted(t, e) })
	t.Run("tenant admin updates", func(t *testing.T) { testAdminUpdate(t, e) })
	t.Run("collisions", func(t *testing.T) { testCollisions(t, e) })
	t.Run("actions", func(t *testing.T) { testActions(t, e, dashID) })
	t.Run("invites and members", func(t *testing.T) { testMembers(t, e, dashID) })
	t.Run("mutual demotion", func(t *testing.T) { testMutualDemotion(t, e, dashID) })
	t.Run("audit log", func(t *testing.T) { testAuditLog(t, e) })
	t.Run("operator deletes the tenant", func(t *testing.T) { testDelete(t, e) })
}

func testCreate(t *testing.T, e *manageEnv) string {
	before := e.totalAudits()
	spec := dashSpec(map[string]any{"value": "plain-token-xyz"}, map[string]any{"generate": true}, nil)
	status, body := e.do("admin", "POST", "/api/v1/tenants", CreateTenantRequest{Slug: "mgr-dash", Spec: mustJSON(t, spec)})
	e.expect(status, body, http.StatusUnauthorized, "")
	status, body = e.do("outsider", "POST", "/api/v1/tenants", CreateTenantRequest{Slug: "mgr-dash", Spec: mustJSON(t, spec)})
	e.expect(status, body, http.StatusForbidden, CodeForbidden)
	status, body = e.do("operator", "POST", "/api/v1/tenants", CreateTenantRequest{Slug: "mgr-dash", Spec: mustJSON(t, spec)})
	e.expect(status, body, http.StatusCreated, "")
	var res TenantWriteResult
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatal(err)
	}
	secret := res.Generated["installations[mgr-dash-bot].webhookSecret"]
	if res.Revision != 1 || len(secret) != 64 {
		t.Fatalf("result = %s", body)
	}
	stored := e.scalar(`SELECT spec::text FROM dashboard_tenants WHERE slug = 'mgr-dash'`)
	if strings.Contains(stored, "plain-token-xyz") || strings.Contains(stored, secret) || !strings.Contains(stored, `"sealed"`) {
		t.Errorf("stored spec is not sealed: %s", stored)
	}
	if n := e.audits(AuditTenantCreate, "mgr-dash"); n != 1 || e.totalAudits() != before+1 {
		t.Errorf("tenant.create audit rows = %d (total +%d), want exactly 1", n, e.totalAudits()-before)
	}
	detail := e.scalar(`SELECT detail::text FROM audit_events WHERE action = 'tenant.create' AND target = 'mgr-dash'`)
	if strings.Contains(detail, secret) || strings.Contains(detail, "plain-token-xyz") || !strings.Contains(detail, "webhookSecret") {
		t.Errorf("audit detail = %s", detail)
	}
	status, body = e.do("operator", "POST", "/api/v1/tenants", CreateTenantRequest{Slug: "mgr-dash", Spec: mustJSON(t, spec)})
	e.expect(status, body, http.StatusConflict, CodeSlugTaken)

	e.waitFor("mgr-dash to merge", func(f *configfile.File) bool { _, ok := f.Tenant("mgr-dash"); return ok })
	if err := e.st.ApplyConfig(context.Background(), e.src.Current.Get(), "manage-test"); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	return secret
}

func testHookVerifies(t *testing.T, e *manageEnv, secret string) {
	in, _, ok := e.src.Current.Get().Installation("mgr-dash-bot")
	if !ok || in.WebhookSecretValue().Value() != secret || in.TokenValue().Value() != "plain-token-xyz" {
		t.Fatalf("merged installation does not open to the written secrets")
	}
	mux := http.NewServeMux()
	mux.Handle("POST /hooks/{installation}", ingest.NewHandler(e.src.Current, nil, slog.New(slog.NewTextHandler(io.Discard, nil))))
	body := []byte(`{}`)
	for _, tt := range []struct {
		name   string
		key    string
		status int
	}{{"the generated secret", secret, http.StatusAccepted}, {"another secret", "nope", http.StatusUnauthorized}} {
		t.Run(tt.name, func(t *testing.T) {
			mac := hmac.New(sha256.New, []byte(tt.key))
			mac.Write(body)
			req := httptest.NewRequest("POST", "/hooks/mgr-dash-bot", bytes.NewReader(body))
			req.Header.Set("X-Gitea-Event", "repository")
			req.Header.Set("X-Gitea-Signature", hex.EncodeToString(mac.Sum(nil)))
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			if w.Code != tt.status {
				t.Errorf("status = %d, want %d: %s", w.Code, tt.status, w.Body)
			}
		})
	}
}

func testConfigRedacted(t *testing.T, e *manageEnv) {
	for _, who := range []string{"operator", "admin", "member"} {
		t.Run(who, func(t *testing.T) {
			status, body := e.do(who, "GET", "/api/v1/tenants/mgr-dash/config", nil)
			e.expect(status, body, http.StatusOK, "")
			var c TenantConfig
			if err := json.Unmarshal(body, &c); err != nil {
				t.Fatal(err)
			}
			if c.ManagedBy != configfile.OriginDashboard || c.Revision == nil || *c.Revision != 1 || c.Editable != (who != "member") {
				t.Errorf("config = %s", body)
			}
			if s := string(c.Spec); strings.Contains(s, "sealed") || strings.Contains(s, "plain-token") ||
				!strings.Contains(s, `"token":{"set":true}`) || !strings.Contains(s, `"webhookSecret":{"set":true}`) {
				t.Errorf("spec is not redacted: %s", s)
			}
		})
	}
	status, body := e.do("outsider", "GET", "/api/v1/tenants/mgr-dash/config", nil)
	e.expect(status, body, http.StatusNotFound, CodeNotFound)
	status, body = e.do("operator", "GET", "/api/v1/tenants/mgr-file/config", nil)
	e.expect(status, body, http.StatusOK, "")
	if !strings.Contains(string(body), `"managedBy":"file"`) || strings.Contains(string(body), "KRITIK_TEST_TOKEN") {
		t.Errorf("file config = %s", body)
	}
}

func testAdminUpdate(t *testing.T, e *manageEnv) {
	before := e.totalAudits()
	limits := UpdateTenantRequest{Revision: 1, Spec: mustJSON(t, dashSpec(keep, keep, map[string]any{"limits": map[string]any{"concurrency": 9}}))}
	status, body := e.do("admin", "PUT", "/api/v1/tenants/mgr-dash/config", limits)
	e.expect(status, body, http.StatusUnprocessableEntity, CodeOperatorOnly)
	if !strings.Contains(string(body), `"path":"limits"`) {
		t.Errorf("operator_only details = %s", body)
	}
	stale := UpdateTenantRequest{Revision: 7, Spec: mustJSON(t, dashSpec(keep, keep, nil))}
	status, body = e.do("admin", "PUT", "/api/v1/tenants/mgr-dash/config", stale)
	e.expect(status, body, http.StatusConflict, CodeRevisionConflict)
	env := UpdateTenantRequest{Revision: 1, Spec: mustJSON(t, dashSpec(map[string]any{"env": "HOME"}, keep, nil))}
	status, body = e.do("admin", "PUT", "/api/v1/tenants/mgr-dash/config", env)
	e.expect(status, body, http.StatusUnprocessableEntity, CodeInvalidSpec)
	badHost := dashSpec(map[string]any{"value": "t"}, keep, nil)
	badHost["installations"].([]any)[0].(map[string]any)["host"] = "evil.example"
	status, body = e.do("admin", "PUT", "/api/v1/tenants/mgr-dash/config", UpdateTenantRequest{Revision: 1, Spec: mustJSON(t, badHost)})
	e.expect(status, body, http.StatusUnprocessableEntity, CodeInvalidSpec)
	if !strings.Contains(string(body), `"path":"installations[0].host"`) || strings.Contains(string(body), "dashboard[") {
		t.Errorf("merge error = %s", body)
	}
	moved := dashSpec(keep, keep, nil)
	moved["installations"].([]any)[0].(map[string]any)["host"] = "git2.example"
	status, body = e.do("admin", "PUT", "/api/v1/tenants/mgr-dash/config", UpdateTenantRequest{Revision: 1, Spec: mustJSON(t, moved)})
	e.expect(status, body, http.StatusUnprocessableEntity, CodeReenterSecret)
	if !strings.Contains(string(body), `"path":"installations[0].token"`) {
		t.Errorf("reenter_secret details = %s", body)
	}
	plain := dashSpec(map[string]any{"value": "t"}, keep, nil)
	plain["installations"].([]any)[0].(map[string]any)["host"] = "http://git.example"
	status, body = e.do("admin", "PUT", "/api/v1/tenants/mgr-dash/config", UpdateTenantRequest{Revision: 1, Spec: mustJSON(t, plain)})
	e.expect(status, body, http.StatusUnprocessableEntity, CodeInvalidSpec)
	if !strings.Contains(string(body), `"path":"installations[0].host"`) {
		t.Errorf("plain-http host = %s", body)
	}
	good := UpdateTenantRequest{Revision: 1, Spec: mustJSON(t, dashSpec(keep, keep, map[string]any{"filter": "true"}))}
	status, body = e.do("member", "PUT", "/api/v1/tenants/mgr-dash/config", good)
	e.expect(status, body, http.StatusForbidden, CodeForbidden)
	status, body = e.do("outsider", "PUT", "/api/v1/tenants/mgr-dash/config", good)
	e.expect(status, body, http.StatusNotFound, CodeNotFound)
	status, body = e.do("admin", "PUT", "/api/v1/tenants/mgr-dash/config", good, false)
	e.expect(status, body, http.StatusForbidden, "")
	if e.totalAudits() != before {
		t.Fatalf("refused writes left %d audit rows", e.totalAudits()-before)
	}
	sealedToken := e.scalar(`SELECT spec->'installations'->0->'token'->>'sealed' FROM dashboard_tenants WHERE slug = 'mgr-dash'`)
	status, body = e.do("admin", "PUT", "/api/v1/tenants/mgr-dash/config", good)
	e.expect(status, body, http.StatusOK, "")
	if !strings.Contains(string(body), `"revision":2`) {
		t.Errorf("update = %s", body)
	}
	if got := e.scalar(`SELECT spec->'installations'->0->'token'->>'sealed' FROM dashboard_tenants WHERE slug = 'mgr-dash'`); got != sealedToken {
		t.Errorf("keep replaced the sealed token")
	}
	if n := e.audits(AuditTenantUpdate, "mgr-dash"); n != 1 || e.totalAudits() != before+1 {
		t.Errorf("tenant.update audit rows = %d, want exactly 1", n)
	}
	e.waitFor("revision 2 to merge", func(f *configfile.File) bool {
		d := f.Dashboard()
		return len(d) == 1 && d[0].Revision == 2
	})
}

func testCollisions(t *testing.T, e *manageEnv) {
	before := e.totalAudits()
	fileSlug := dashSpec(map[string]any{"value": "x"}, map[string]any{"value": "y"}, map[string]any{"slug": "mgr-file"})
	status, body := e.do("operator", "POST", "/api/v1/tenants", CreateTenantRequest{Slug: "mgr-file", Spec: mustJSON(t, fileSlug)})
	e.expect(status, body, http.StatusConflict, CodeSlugTaken)
	status, body = e.do("operator", "PUT", "/api/v1/tenants/mgr-file/config", UpdateTenantRequest{Revision: 1, Spec: mustJSON(t, fileSlug)})
	e.expect(status, body, http.StatusForbidden, CodeFileManaged)

	dup := dashSpec(map[string]any{"value": "x"}, map[string]any{"value": "y"}, map[string]any{"slug": "mgr-dup"})
	dup["installations"].([]any)[0].(map[string]any)["name"] = "mgr-file-bot"
	status, body = e.do("operator", "POST", "/api/v1/tenants", CreateTenantRequest{Slug: "mgr-dup", Spec: mustJSON(t, dup)})
	e.expect(status, body, http.StatusUnprocessableEntity, CodeInvalidSpec)
	if !strings.Contains(string(body), `"path":"installations[0].name"`) {
		t.Errorf("duplicate installation = %s", body)
	}

	// A tenant the file dropped but the leader has not disabled yet.
	staleID := (&configfile.Tenant{Slug: "mgr-stale"}).ID()
	e.exec(`INSERT INTO tenants (id, slug, managed_by) VALUES ($1, 'mgr-stale', 'file') ON CONFLICT (id) DO NOTHING`, staleID)
	stale := dashSpec(map[string]any{"value": "x"}, map[string]any{"value": "y"}, map[string]any{"slug": "mgr-stale"})
	stale["installations"].([]any)[0].(map[string]any)["name"] = "mgr-stale-bot"
	status, body = e.do("operator", "POST", "/api/v1/tenants", CreateTenantRequest{Slug: "mgr-stale", Spec: mustJSON(t, stale)})
	e.expect(status, body, http.StatusConflict, CodeSlugTaken)
	if e.totalAudits() != before {
		t.Errorf("refused creates left %d audit rows", e.totalAudits()-before)
	}

	// mgr-zed sorts after mgr-dash, so the merge reports mgr-dash taking
	// its installation name against mgr-zed; the blame is still mgr-dash's.
	zed := map[string]any{"slug": "mgr-zed", "installations": []any{map[string]any{
		"name": "mgr-zed-bot", "forge": "forgejo", "host": "git.example", "account": "mz",
		"token": map[string]any{"value": "z"}, "webhookSecret": map[string]any{"value": "z"},
	}}}
	status, body = e.do("operator", "POST", "/api/v1/tenants", CreateTenantRequest{Slug: "mgr-zed", Spec: mustJSON(t, zed)})
	e.expect(status, body, http.StatusCreated, "")
	e.waitFor("mgr-zed to merge", func(f *configfile.File) bool { _, ok := f.Tenant("mgr-zed"); return ok })
	taken := dashSpec(keep, keep, map[string]any{"filter": "true"})
	taken["installations"] = append(taken["installations"].([]any), map[string]any{
		"name": "mgr-zed-bot", "forge": "forgejo", "host": "git.example", "account": "mz2",
		"token": map[string]any{"value": "t"}, "webhookSecret": map[string]any{"value": "w"},
	})
	status, body = e.do("admin", "PUT", "/api/v1/tenants/mgr-dash/config", UpdateTenantRequest{Revision: 2, Spec: mustJSON(t, taken)})
	e.expect(status, body, http.StatusUnprocessableEntity, CodeInvalidSpec)
	if !strings.Contains(string(body), `"path":"installations[1].name"`) || strings.Contains(string(body), "dashboard[mgr-zed]") || strings.Contains(string(body), `tenant \"mgr-zed\"`) {
		t.Errorf("clash against a later tenant = %s", body)
	}
}

func testActions(t *testing.T, e *manageEnv, dashID string) {
	in, _, _ := e.src.Current.Get().Installation("mgr-dash-bot")
	repoID := configfile.RepositoryID(in.ID(), "md/one")
	e.exec(`INSERT INTO pull_requests (tenant_id, repository_id, number, title, author, head_sha)
		VALUES ($1, $2, 3, 'x', 'ada', 'h3') ON CONFLICT DO NOTHING`, dashID, repoID)

	status, body := e.do("member", "POST", "/api/v1/tenants/mgr-dash/pulls/md/one/3/rerun", nil)
	e.expect(status, body, http.StatusForbidden, CodeForbidden)
	status, body = e.do("admin", "POST", "/api/v1/tenants/mgr-dash/pulls/md/one/3/rerun", nil, false)
	e.expect(status, body, http.StatusForbidden, "")
	status, body = e.do("admin", "POST", "/api/v1/tenants/mgr-dash/pulls/md/one/99/rerun", nil)
	e.expect(status, body, http.StatusNotFound, CodeNotFound)

	status, body = e.do("admin", "POST", "/api/v1/tenants/mgr-dash/pulls/md/one/3/rerun", nil)
	e.expect(status, body, http.StatusAccepted, "")
	if string(bytes.TrimSpace(body)) != `{"jobId":101}` || e.actions.last() != fmt.Sprintf("rerun %s %s 3 in %s", dashID, repoID, dashID) {
		t.Errorf("rerun = %s, call %q", body, e.actions.last())
	}
	if n := e.audits(AuditReviewRerun, "md/one#3"); n != 1 {
		t.Errorf("review.rerun audit rows = %d", n)
	}

	review := "00000000-0000-4000-8000-000000000001"
	e.actions.notCancelable = "00000000-0000-4000-8000-000000000002"
	status, body = e.do("admin", "POST", "/api/v1/tenants/mgr-dash/reviews/"+e.actions.notCancelable+"/cancel", nil)
	e.expect(status, body, http.StatusConflict, CodeNotCancelable)
	status, body = e.do("admin", "POST", "/api/v1/tenants/mgr-dash/reviews/"+review+"/cancel", nil)
	e.expect(status, body, http.StatusAccepted, "")
	if e.actions.last() != fmt.Sprintf("cancel %s by %s in %s", review, e.account["admin"], dashID) {
		t.Errorf("cancel call %q", e.actions.last())
	}
	if e.audits(AuditReviewCancel, review) != 1 || e.audits(AuditReviewCancel, e.actions.notCancelable) != 0 {
		t.Errorf("review.cancel audit rows are wrong")
	}

	status, body = e.do("admin", "POST", "/api/v1/tenants/mgr-dash/repos/md/one/reindex", nil)
	e.expect(status, body, http.StatusAccepted, "")
	if e.actions.last() != fmt.Sprintf("reindex %s %s in %s", dashID, repoID, dashID) || e.audits(AuditRepoReindex, "md/one") != 1 {
		t.Errorf("reindex = %s, call %q", body, e.actions.last())
	}
}

func testMembers(t *testing.T, e *manageEnv, dashID string) {
	ctx := context.Background()
	status, body := e.do("member", "POST", "/api/v1/tenants/mgr-dash/invites", CreateInviteRequest{Email: "carol@example.com", Role: "admin"})
	e.expect(status, body, http.StatusForbidden, CodeForbidden)
	status, body = e.do("admin", "POST", "/api/v1/tenants/mgr-dash/invites", CreateInviteRequest{Email: "carol@example.com", Role: "admin"})
	e.expect(status, body, http.StatusCreated, "")
	var inv Invite
	if err := json.Unmarshal(body, &inv); err != nil || inv.CreatedBy == nil || inv.CreatedBy.ID != e.account["admin"] {
		t.Fatalf("invite = %s", body)
	}
	if e.audits(AuditInviteCreate, inv.ID) != 1 {
		t.Errorf("invite.create audit rows are wrong")
	}
	status, body = e.do("admin", "POST", "/api/v1/tenants/mgr-dash/invites", CreateInviteRequest{Email: "Carol@example.com", Role: "member"})
	e.expect(status, body, http.StatusConflict, CodeInviteExists)
	status, body = e.do("admin", "POST", "/api/v1/tenants/mgr-dash/invites", CreateInviteRequest{Email: "Member@example.com", Role: "admin"})
	e.expect(status, body, http.StatusConflict, CodeAlreadyMember)

	var members Members
	status, body = e.do("member", "GET", "/api/v1/tenants/mgr-dash/members", nil)
	e.expect(status, body, http.StatusOK, "")
	if err := json.Unmarshal(body, &members); err != nil || members.Invites != nil || len(members.Members) != 2 {
		t.Errorf("members as a member = %s", body)
	}
	status, body = e.do("admin", "GET", "/api/v1/tenants/mgr-dash/members", nil)
	e.expect(status, body, http.StatusOK, "")
	if err := json.Unmarshal(body, &members); err != nil || len(members.Invites) != 1 || members.Invites[0].Email != "carol@example.com" {
		t.Errorf("members as an admin = %s", body)
	}

	// Carol signs in and accepts; the invite grant is hers to lose.
	e.signIn("carol", "mgr-carol", nil)
	if n, err := e.st.AcceptInvites(ctx, e.account["carol"], "carol@example.com", time.Now()); err != nil || n != 1 {
		t.Fatalf("AcceptInvites = %d, %v", n, err)
	}
	status, body = e.do("carol", "POST", "/api/v1/tenants/mgr-dash/invites", CreateInviteRequest{Email: "dan@example.com", Role: "member"})
	e.expect(status, body, http.StatusCreated, "")
	var dan Invite
	_ = json.Unmarshal(body, &dan)
	status, body = e.do("carol", "DELETE", "/api/v1/tenants/mgr-dash/invites/"+dan.ID, nil)
	e.expect(status, body, http.StatusNoContent, "")
	if e.audits(AuditInviteDelete, dan.ID) != 1 {
		t.Errorf("invite.delete audit rows are wrong")
	}

	// The forge admin drops out: carol is the last admin but operators.
	if err := e.st.ReplaceForgeMemberships(ctx, e.account["admin"], nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	status, body = e.do("carol", "DELETE", "/api/v1/tenants/mgr-dash/members/"+e.account["carol"], nil)
	e.expect(status, body, http.StatusConflict, CodeLastAdmin)
	status, body = e.do("carol", "PATCH", "/api/v1/tenants/mgr-dash/members/"+e.account["carol"], UpdateMemberRequest{Role: "member"})
	e.expect(status, body, http.StatusConflict, CodeLastAdmin)
	if err := e.st.ReplaceForgeMemberships(ctx, e.account["admin"], []store.Grant{{TenantID: dashID, Role: store.RoleAdmin}}, time.Now()); err != nil {
		t.Fatal(err)
	}

	status, body = e.do("admin", "PATCH", "/api/v1/tenants/mgr-dash/members/"+e.account["member"], UpdateMemberRequest{Role: "admin"})
	e.expect(status, body, http.StatusConflict, CodeNotInviteMember)
	status, body = e.do("admin", "PATCH", "/api/v1/tenants/mgr-dash/members/"+e.account["carol"], UpdateMemberRequest{Role: "member"})
	e.expect(status, body, http.StatusNoContent, "")
	if e.audits(AuditMemberUpdate, e.account["carol"]) != 1 {
		t.Errorf("member.update audit rows are wrong")
	}
	status, body = e.do("admin", "DELETE", "/api/v1/tenants/mgr-dash/members/"+e.account["carol"], nil)
	e.expect(status, body, http.StatusOK, "")
	if !strings.Contains(string(body), "sign-in") || e.audits(AuditMemberRemove, e.account["carol"]) != 1 {
		t.Errorf("remove = %s", body)
	}
	status, body = e.do("carol", "GET", "/api/v1/tenants/mgr-dash/members", nil)
	e.expect(status, body, http.StatusNotFound, CodeNotFound)
	status, body = e.do("admin", "POST", "/api/v1/tenants/mgr-dash/invites", CreateInviteRequest{Email: "erin@example.com", Role: "member"})
	e.expect(status, body, http.StatusCreated, "")
}

// testMutualDemotion has two invite admins demote each other, the second
// acting on a principal authenticated before the first change landed, as
// two concurrent requests would.
func testMutualDemotion(t *testing.T, e *manageEnv, dashID string) {
	ctx := context.Background()
	for _, who := range []string{"ann", "ben"} {
		status, body := e.do("admin", "POST", "/api/v1/tenants/mgr-dash/invites", CreateInviteRequest{Email: who + "@example.com", Role: "admin"})
		e.expect(status, body, http.StatusCreated, "")
		e.signIn(who, "mgr-"+who, nil)
		if n, err := e.st.AcceptInvites(ctx, e.account[who], who+"@example.com", time.Now()); err != nil || n != 1 {
			t.Fatalf("AcceptInvites(%s) = %d, %v", who, n, err)
		}
	}
	if err := e.st.ReplaceForgeMemberships(ctx, e.account["admin"], nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	stale := func(who string) *auth.Principal {
		return &auth.Principal{Account: auth.Account{ID: e.account[who]}, Memberships: map[string]auth.Role{dashID: auth.RoleAdmin}}
	}
	demote := func(by, target string) *httptest.ResponseRecorder {
		body := strings.NewReader(`{"role":"member"}`)
		req := httptest.NewRequest("PATCH", "/api/v1/tenants/mgr-dash/members/"+e.account[target], body)
		req.Header.Set("Origin", "https://kritik.example")
		req.Header.Set("X-Kritik", "1")
		req = req.WithContext(auth.WithPrincipal(req.Context(), stale(by)))
		w := httptest.NewRecorder()
		e.srv.Handler().ServeHTTP(w, req)
		return w
	}
	if w := demote("ann", "ben"); w.Code != http.StatusNoContent {
		t.Fatalf("ann demotes ben = %d %s", w.Code, w.Body)
	}
	if w := demote("ben", "ann"); w.Code != http.StatusForbidden {
		t.Fatalf("ben, no longer an admin, demotes ann = %d %s", w.Code, w.Body)
	}
	if w := demote("ben", "ben"); w.Code != http.StatusForbidden {
		t.Fatalf("ben, no longer an admin, changes his own role = %d %s", w.Code, w.Body)
	}
	admins := e.scalar(`SELECT count(DISTINCT account_id)::text FROM memberships WHERE tenant_id = '` + dashID + `' AND role = 'admin'`)
	if admins != "1" {
		t.Errorf("admins left = %s, want 1", admins)
	}
	if err := e.st.ReplaceForgeMemberships(ctx, e.account["admin"], []store.Grant{{TenantID: dashID, Role: store.RoleAdmin}}, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func testAuditLog(t *testing.T, e *manageEnv) {
	status, body := e.do("member", "GET", "/api/v1/tenants/mgr-dash/audit", nil)
	e.expect(status, body, http.StatusForbidden, CodeForbidden)
	status, body = e.do("admin", "GET", "/api/v1/operator/audit", nil)
	e.expect(status, body, http.StatusForbidden, CodeForbidden)

	var seen []AuditEvent
	path := "/api/v1/tenants/mgr-dash/audit?limit=3"
	for range 20 {
		status, body = e.do("admin", "GET", path, nil)
		e.expect(status, body, http.StatusOK, "")
		var page Page[AuditEvent]
		if err := json.Unmarshal(body, &page); err != nil {
			t.Fatal(err)
		}
		seen = append(seen, page.Items...)
		if page.NextCursor == nil {
			break
		}
		path = "/api/v1/tenants/mgr-dash/audit?limit=3&cursor=" + *page.NextCursor
	}
	var want int
	if err := e.owner.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE tenant_id = $1`,
		(&configfile.Tenant{Slug: "mgr-dash"}).ID()).Scan(&want); err != nil {
		t.Fatal(err)
	}
	if len(seen) != want || want < 8 {
		t.Fatalf("paged %d events, want %d", len(seen), want)
	}
	var prev int64
	for i, ev := range seen {
		id, err := strconv.ParseInt(ev.ID, 10, 64)
		if err != nil || ev.Tenant != "mgr-dash" || ev.Actor == nil || (i > 0 && id >= prev) {
			t.Errorf("event %d = %+v, want newest first", i, ev)
		}
		prev = id
	}
	status, body = e.do("operator", "GET", "/api/v1/operator/audit?limit=200", nil)
	e.expect(status, body, http.StatusOK, "")
	if !strings.Contains(string(body), `"action":"tenant.create","target":"mgr-dash"`) {
		t.Errorf("operator audit lacks the create: %s", body)
	}
}

func testDelete(t *testing.T, e *manageEnv) {
	status, body := e.do("admin", "DELETE", "/api/v1/tenants/mgr-dash?revision=2", nil)
	e.expect(status, body, http.StatusForbidden, CodeForbidden)
	status, body = e.do("operator", "DELETE", "/api/v1/tenants/mgr-dash?revision=1", nil)
	e.expect(status, body, http.StatusConflict, CodeRevisionConflict)
	status, body = e.do("operator", "DELETE", "/api/v1/tenants/mgr-file?revision=1", nil)
	e.expect(status, body, http.StatusForbidden, CodeFileManaged)
	status, body = e.do("operator", "DELETE", "/api/v1/tenants/mgr-dash?revision=2", nil)
	e.expect(status, body, http.StatusNoContent, "")
	if e.audits(AuditTenantDelete, "mgr-dash") != 1 {
		t.Errorf("tenant.delete audit rows are wrong")
	}
	dashID := (&configfile.Tenant{Slug: "mgr-dash"}).ID()
	if n := e.scalar(`SELECT ((SELECT count(*) FROM memberships WHERE tenant_id = '` + dashID + `') +
		(SELECT count(*) FROM invites WHERE tenant_id = '` + dashID + `' AND accepted_at IS NULL))::text`); n != "0" {
		t.Errorf("%s memberships and invites outlived the tenant", n)
	}
	e.waitFor("mgr-dash to leave", func(f *configfile.File) bool { _, ok := f.Tenant("mgr-dash"); return !ok })
	status, body = e.do("operator", "GET", "/api/v1/tenants/mgr-dash/config", nil)
	e.expect(status, body, http.StatusNotFound, CodeNotFound)
	status, body = e.do("outsider", "GET", "/api/v1/meta", nil)
	e.expect(status, body, http.StatusOK, "")
	if !strings.Contains(string(body), `"management":true`) {
		t.Errorf("meta = %s", body)
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
