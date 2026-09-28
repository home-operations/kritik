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
	"github.com/home-operations/kritik/internal/jobs"
	"github.com/home-operations/kritik/internal/sealbox"
	"github.com/home-operations/kritik/internal/store"
)

const manageConfig = `
providers:
  shared: { type: openrouter, apiKey: { env: KRITIK_TEST_TOKEN } }
auth:
  oidc:
    issuer: https://idp.example
    clientId: kritik
    clientSecret: { env: KRITIK_TEST_TOKEN }
    roleMapping: '"kritik-admin" in roles ? "admin" : ""'
tenants:
  - slug: mgr-file
    installations:
      - name: mgr-file-bot
        forge: github
        accounts: [mf]
        app: { clientId: Iv1.test, privateKey: { env: KRITIK_TEST_TOKEN }, webhookSecret: { env: KRITIK_TEST_TOKEN } }
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
		return jobs.ErrNotCancelable
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
	user    map[string]string
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
	e := &manageEnv{t: t, st: st, owner: owner, actions: &fakeActions{}, cookie: map[string]*http.Cookie{}, user: map[string]string{}}
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
	e.signIn("operator", "mgr-op", store.SessionGrant{Role: store.RoleAdmin})
	e.signIn("outsider", "mgr-outsider", memberOfAccounts("github/nobody"))
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

func (e *manageEnv) signIn(name, subject string, g store.SessionGrant) {
	e.t.Helper()
	ctx, now, origin := context.Background(), time.Now(), "oidc:https://idp.example"
	user, err := e.st.UpsertIdentity(ctx, store.SignInIdentity{
		Provider: "oidc", Origin: origin, Subject: subject, DisplayName: name, Email: name + "@example.com", EmailVerified: true,
	}, now)
	if err != nil {
		e.t.Fatal(err)
	}
	g.Key, _ = auth.GrantKey(e.src.Current.Get().Auth, "oidc")
	token, err := e.st.CreateSession(ctx, user.ID, "oidc", origin, g, now, now.Add(time.Hour))
	if err != nil {
		e.t.Fatal(err)
	}
	e.cookie[name] = &http.Cookie{Name: auth.SessionCookieName(&url.URL{Scheme: "https", Host: "kritik.example"}), Value: token}
	e.user[name] = user.ID
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

func dashSpec(keyRef, hookRef map[string]any, extra map[string]any) map[string]any {
	spec := map[string]any{
		"slug": "mgr-dash",
		"installations": []any{map[string]any{
			"name": "mgr-dash-bot", "forge": "github", "accounts": []string{"md"},
			"app": map[string]any{"clientId": "Iv1.test", "privateKey": keyRef, "webhookSecret": hookRef},
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
	e.signIn("member", "mgr-member", memberOfAccounts("github/md"))
	t.Run("config reads are redacted", func(t *testing.T) { testConfigRedacted(t, e) })
	t.Run("updates", func(t *testing.T) { testUpdate(t, e) })
	t.Run("collisions", func(t *testing.T) { testCollisions(t, e) })
	t.Run("a tenant's own provider key", func(t *testing.T) { testTenantProviderKey(t, e) })
	t.Run("actions", func(t *testing.T) { testActions(t, e, dashID) })
	t.Run("audit log", func(t *testing.T) { testAuditLog(t, e) })
	t.Run("operator deletes the tenant", func(t *testing.T) { testDelete(t, e) })
	t.Run("a file tenant a dashboard row crowds out", func(t *testing.T) { testFileTenantLeftOut(t, e) })
}

func testCreate(t *testing.T, e *manageEnv) string {
	before := e.totalAudits()
	spec := dashSpec(map[string]any{"value": "plain-key-xyz"}, map[string]any{"generate": true}, nil)
	status, body := e.do("anonymous", "POST", "/api/v1/tenants", CreateTenantRequest{Slug: "mgr-dash", Spec: mustJSON(t, spec)})
	e.expect(status, body, http.StatusUnauthorized, "")
	status, body = e.do("outsider", "POST", "/api/v1/tenants", CreateTenantRequest{Slug: "mgr-dash", Spec: mustJSON(t, spec)})
	e.expect(status, body, http.StatusForbidden, CodeForbidden)
	status, body = e.do("operator", "POST", "/api/v1/tenants", CreateTenantRequest{Slug: "mgr-dash", Spec: mustJSON(t, spec)})
	e.expect(status, body, http.StatusCreated, "")
	var res TenantWriteResult
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatal(err)
	}
	secret := res.Generated["installations[mgr-dash-bot].app.webhookSecret"]
	if res.Revision != 1 || len(secret) != 64 {
		t.Fatalf("result = %s", body)
	}
	stored := e.scalar(`SELECT spec::text FROM dashboard_tenants WHERE slug = 'mgr-dash'`)
	if strings.Contains(stored, "plain-key-xyz") || strings.Contains(stored, secret) || !strings.Contains(stored, `"sealed"`) {
		t.Errorf("stored spec is not sealed: %s", stored)
	}
	if n := e.audits(AuditTenantCreate, "mgr-dash"); n != 1 || e.totalAudits() != before+1 {
		t.Errorf("tenant.create audit rows = %d (total +%d), want exactly 1", n, e.totalAudits()-before)
	}
	detail := e.scalar(`SELECT detail::text FROM audit_events WHERE action = 'tenant.create' AND target = 'mgr-dash'`)
	if strings.Contains(detail, secret) || strings.Contains(detail, "plain-key-xyz") || !strings.Contains(detail, "webhookSecret") {
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
	if !ok || in.WebhookSecretValue().Value() != secret || in.App.PrivateKeyValue().Value() != "plain-key-xyz" {
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
			req.Header.Set("X-GitHub-Event", "repository")
			req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			if w.Code != tt.status {
				t.Errorf("status = %d, want %d: %s", w.Code, tt.status, w.Body)
			}
		})
	}
}

func testConfigRedacted(t *testing.T, e *manageEnv) {
	for _, who := range []string{"operator", "member"} {
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
			if s := string(c.Spec); strings.Contains(s, "sealed") || strings.Contains(s, "plain-key") ||
				!strings.Contains(s, `"privateKey":{"set":true}`) || !strings.Contains(s, `"webhookSecret":{"set":true}`) {
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

func testUpdate(t *testing.T, e *manageEnv) {
	before := e.totalAudits()
	stale := UpdateTenantRequest{Revision: 7, Spec: mustJSON(t, dashSpec(keep, keep, nil))}
	status, body := e.do("operator", "PUT", "/api/v1/tenants/mgr-dash/config", stale)
	e.expect(status, body, http.StatusConflict, CodeRevisionConflict)
	env := UpdateTenantRequest{Revision: 1, Spec: mustJSON(t, dashSpec(map[string]any{"env": "HOME"}, keep, nil))}
	status, body = e.do("operator", "PUT", "/api/v1/tenants/mgr-dash/config", env)
	e.expect(status, body, http.StatusUnprocessableEntity, CodeInvalidSpec)
	badForge := dashSpec(map[string]any{"value": "k"}, keep, nil)
	badForge["installations"].([]any)[0].(map[string]any)["forge"] = "gitlab"
	status, body = e.do("operator", "PUT", "/api/v1/tenants/mgr-dash/config", UpdateTenantRequest{Revision: 1, Spec: mustJSON(t, badForge)})
	e.expect(status, body, http.StatusUnprocessableEntity, CodeInvalidSpec)
	if !strings.Contains(string(body), `"path":"installations[0].forge"`) || strings.Contains(string(body), "dashboard[") {
		t.Errorf("merge error = %s", body)
	}
	moved := dashSpec(keep, keep, nil)
	moved["installations"].([]any)[0].(map[string]any)["accounts"] = []string{"md", "other"}
	status, body = e.do("operator", "PUT", "/api/v1/tenants/mgr-dash/config", UpdateTenantRequest{Revision: 1, Spec: mustJSON(t, moved)})
	e.expect(status, body, http.StatusUnprocessableEntity, CodeReenterSecret)
	if !strings.Contains(string(body), `"path":"installations[0].app.privateKey"`) {
		t.Errorf("reenter_secret details = %s", body)
	}
	good := UpdateTenantRequest{Revision: 1, Spec: mustJSON(t, dashSpec(keep, keep, map[string]any{"filter": "true"}))}
	status, body = e.do("member", "PUT", "/api/v1/tenants/mgr-dash/config", good)
	e.expect(status, body, http.StatusForbidden, CodeForbidden)
	status, body = e.do("outsider", "PUT", "/api/v1/tenants/mgr-dash/config", good)
	e.expect(status, body, http.StatusNotFound, CodeNotFound)
	status, body = e.do("operator", "PUT", "/api/v1/tenants/mgr-dash/config", good, false)
	e.expect(status, body, http.StatusForbidden, "")
	if e.totalAudits() != before {
		t.Fatalf("refused writes left %d audit rows", e.totalAudits()-before)
	}
	sealedKey := e.scalar(`SELECT spec->'installations'->0->'app'->'privateKey'->>'sealed' FROM dashboard_tenants WHERE slug = 'mgr-dash'`)
	status, body = e.do("operator", "PUT", "/api/v1/tenants/mgr-dash/config", good)
	e.expect(status, body, http.StatusOK, "")
	if !strings.Contains(string(body), `"revision":2`) {
		t.Errorf("update = %s", body)
	}
	if got := e.scalar(`SELECT spec->'installations'->0->'app'->'privateKey'->>'sealed' FROM dashboard_tenants WHERE slug = 'mgr-dash'`); got != sealedKey {
		t.Errorf("keep replaced the sealed private key")
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
	if strings.Contains(string(body), "adoptable") {
		t.Errorf("a slug the file still manages was offered for adoption: %s", body)
	}
	if e.totalAudits() != before {
		t.Errorf("refused creates left %d audit rows", e.totalAudits()-before)
	}

	// mgr-zed sorts after mgr-dash, so the merge reports mgr-dash taking
	// its installation name against mgr-zed; the blame is still mgr-dash's.
	zed := map[string]any{"slug": "mgr-zed", "installations": []any{map[string]any{
		"name": "mgr-zed-bot", "forge": "github", "accounts": []string{"mz"},
		"app": map[string]any{"clientId": "Iv1.test", "privateKey": map[string]any{"value": "z"}, "webhookSecret": map[string]any{"value": "z"}},
	}}}
	status, body = e.do("operator", "POST", "/api/v1/tenants", CreateTenantRequest{Slug: "mgr-zed", Spec: mustJSON(t, zed)})
	e.expect(status, body, http.StatusCreated, "")
	e.waitFor("mgr-zed to merge", func(f *configfile.File) bool { _, ok := f.Tenant("mgr-zed"); return ok })
	taken := dashSpec(keep, keep, map[string]any{"filter": "true"})
	taken["installations"] = append(taken["installations"].([]any), map[string]any{
		"name": "mgr-zed-bot", "forge": "github", "accounts": []string{"mz2"},
		"app": map[string]any{"clientId": "Iv1.test", "privateKey": map[string]any{"value": "k"}, "webhookSecret": map[string]any{"value": "w"}},
	})
	status, body = e.do("operator", "PUT", "/api/v1/tenants/mgr-dash/config", UpdateTenantRequest{Revision: 2, Spec: mustJSON(t, taken)})
	e.expect(status, body, http.StatusUnprocessableEntity, CodeInvalidSpec)
	if !strings.Contains(string(body), `"path":"installations[1].name"`) || strings.Contains(string(body), "dashboard[mgr-zed]") || strings.Contains(string(body), `tenant \"mgr-zed\"`) {
		t.Errorf("clash against a later tenant = %s", body)
	}
}

// testTenantProviderKey: a tenant's own model key, which its review model
// points at, is sealed at rest and opened in the merged configuration.
func testTenantProviderKey(t *testing.T, e *manageEnv) {
	mine := map[string]any{"mine": map[string]any{"type": "openai", "apiKey": map[string]any{"value": "sk-mine"}}}
	own := dashSpec(keep, keep, map[string]any{"filter": "true", "providers": mine, "models": map[string]any{"review": "mine/gpt"}})
	status, body := e.do("operator", "PUT", "/api/v1/tenants/mgr-dash/config", UpdateTenantRequest{Revision: 2, Spec: mustJSON(t, own)})
	e.expect(status, body, http.StatusOK, "")
	if sealed := e.scalar(`SELECT spec->'providers'->'mine'->'apiKey'->>'sealed' FROM dashboard_tenants WHERE slug = 'mgr-dash'`); sealed == "" ||
		strings.Contains(sealed, "sk-mine") {
		t.Errorf("stored provider key = %q, want it sealed", sealed)
	}
	e.waitFor("the provider key to merge", func(f *configfile.File) bool {
		tn, ok := f.Tenant("mgr-dash")
		if !ok {
			return false
		}
		p, ok := f.Provider(tn, "mine")
		return ok && p.APIKeyValue().Value() == "sk-mine"
	})
	status, body = e.do("operator", "GET", "/api/v1/tenants/mgr-dash/config", nil)
	if status != http.StatusOK || strings.Contains(string(body), "sk-mine") || !strings.Contains(string(body), `"apiKey":{"set":true}`) {
		t.Errorf("config read = %d %s", status, body)
	}
}

func testActions(t *testing.T, e *manageEnv, dashID string) {
	in, _, _ := e.src.Current.Get().Installation("mgr-dash-bot")
	repoID := configfile.RepositoryID(in.ID(), "md/one")
	e.exec(`INSERT INTO pull_requests (tenant_id, repository_id, number, title, author, head_sha)
		VALUES ($1, $2, 3, 'x', 'ada', 'h3') ON CONFLICT DO NOTHING`, dashID, repoID)

	status, body := e.do("member", "POST", "/api/v1/tenants/mgr-dash/pulls/md/one/3/rerun", nil)
	e.expect(status, body, http.StatusForbidden, CodeForbidden)
	status, body = e.do("operator", "POST", "/api/v1/tenants/mgr-dash/pulls/md/one/3/rerun", nil, false)
	e.expect(status, body, http.StatusForbidden, "")
	status, body = e.do("operator", "POST", "/api/v1/tenants/mgr-dash/pulls/md/one/99/rerun", nil)
	e.expect(status, body, http.StatusNotFound, CodeNotFound)

	status, body = e.do("operator", "POST", "/api/v1/tenants/mgr-dash/pulls/md/one/3/rerun", nil)
	e.expect(status, body, http.StatusAccepted, "")
	if string(bytes.TrimSpace(body)) != `{"jobId":101}` || e.actions.last() != fmt.Sprintf("rerun %s %s 3 in %s", dashID, repoID, dashID) {
		t.Errorf("rerun = %s, call %q", body, e.actions.last())
	}
	if n := e.audits(AuditReviewRerun, "md/one#3"); n != 1 {
		t.Errorf("review.rerun audit rows = %d", n)
	}

	review := "00000000-0000-4000-8000-000000000001"
	e.actions.notCancelable = "00000000-0000-4000-8000-000000000002"
	status, body = e.do("operator", "POST", "/api/v1/tenants/mgr-dash/reviews/"+e.actions.notCancelable+"/cancel", nil)
	e.expect(status, body, http.StatusConflict, CodeNotCancelable)
	status, body = e.do("operator", "POST", "/api/v1/tenants/mgr-dash/reviews/"+review+"/cancel", nil)
	e.expect(status, body, http.StatusAccepted, "")
	if e.actions.last() != fmt.Sprintf("cancel %s by %s in %s", review, e.user["operator"], dashID) {
		t.Errorf("cancel call %q", e.actions.last())
	}
	if e.audits(AuditReviewCancel, review) != 1 || e.audits(AuditReviewCancel, e.actions.notCancelable) != 0 {
		t.Errorf("review.cancel audit rows are wrong")
	}

	status, body = e.do("operator", "POST", "/api/v1/tenants/mgr-dash/repos/md/one/reindex", nil)
	e.expect(status, body, http.StatusAccepted, "")
	if e.actions.last() != fmt.Sprintf("reindex %s %s in %s", dashID, repoID, dashID) || e.audits(AuditRepoReindex, "md/one") != 1 {
		t.Errorf("reindex = %s, call %q", body, e.actions.last())
	}
}

func testAuditLog(t *testing.T, e *manageEnv) {
	status, body := e.do("member", "GET", "/api/v1/tenants/mgr-dash/audit", nil)
	e.expect(status, body, http.StatusForbidden, CodeForbidden)
	status, body = e.do("member", "GET", "/api/v1/operator/audit", nil)
	e.expect(status, body, http.StatusForbidden, CodeForbidden)

	var seen []AuditEvent
	path := "/api/v1/tenants/mgr-dash/audit?limit=3"
	for range 20 {
		status, body = e.do("operator", "GET", path, nil)
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
	if len(seen) != want || want < 4 {
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
	status, body := e.do("member", "DELETE", "/api/v1/tenants/mgr-dash?revision=3", nil)
	e.expect(status, body, http.StatusForbidden, CodeForbidden)
	status, body = e.do("operator", "DELETE", "/api/v1/tenants/mgr-dash?revision=1", nil)
	e.expect(status, body, http.StatusConflict, CodeRevisionConflict)
	status, body = e.do("operator", "DELETE", "/api/v1/tenants/mgr-file?revision=1", nil)
	e.expect(status, body, http.StatusForbidden, CodeFileManaged)
	status, body = e.do("operator", "DELETE", "/api/v1/tenants/mgr-dash?revision=3", nil)
	e.expect(status, body, http.StatusNoContent, "")
	if e.audits(AuditTenantDelete, "mgr-dash") != 1 {
		t.Errorf("tenant.delete audit rows are wrong")
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

// testFileTenantLeftOut stores a dashboard tenant on the file tenant's slug
// directly, as no API write may: the file tenant leaves the running
// configuration, the operator console says why, and it returns once the
// row is gone.
func testFileTenantLeftOut(t *testing.T, e *manageEnv) {
	ctx := context.Background()
	seal := func(v string) string {
		s, err := e.srv.keyring.Seal([]byte(v))
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	spec := mustJSON(t, map[string]any{"slug": "mgr-file", "installations": []any{map[string]any{
		"name": "mgr-held-bot", "forge": "github", "accounts": []string{"mh"},
		"app": map[string]any{"clientId": "Iv1.test", "privateKey": map[string]any{"sealed": seal("k")}, "webhookSecret": map[string]any{"sealed": seal("w")}},
	}}})
	write := func(fn func(pgx.Tx) error) {
		if err := e.st.WithTenant(ctx, (&configfile.Tenant{Slug: "mgr-file"}).ID(), fn); err != nil {
			t.Fatal(err)
		}
	}
	write(func(tx pgx.Tx) error {
		_, err := e.st.PutDashboardTenant(ctx, tx, "mgr-file", spec, 0, "")
		return err
	})
	e.waitFor("mgr-file to be left out", func(f *configfile.File) bool { return len(f.Skipped()) == 1 })
	status, body := e.do("operator", "GET", "/api/v1/operator/tenants", nil)
	e.expect(status, body, http.StatusOK, "")
	var list []OperatorTenant
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatal(err)
	}
	live := map[configfile.Origin]bool{}
	for _, o := range list {
		if o.Slug == "mgr-file" {
			live[o.ManagedBy] = o.Live
			if o.ManagedBy == configfile.OriginFile && o.Conflict != `dashboard tenant "mgr-file" already holds the slug` {
				t.Errorf("conflict = %q", o.Conflict)
			}
		}
	}
	if len(live) != 2 || !live[configfile.OriginDashboard] || live[configfile.OriginFile] {
		t.Fatalf("operator tenants = %s", body)
	}
	write(func(tx pgx.Tx) error { return e.st.DeleteDashboardTenant(ctx, tx, "mgr-file", 1) })
	e.waitFor("mgr-file to return", func(f *configfile.File) bool {
		ft, ok := f.Tenant("mgr-file")
		return ok && ft.Origin() == configfile.OriginFile && len(f.Skipped()) == 0
	})
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
