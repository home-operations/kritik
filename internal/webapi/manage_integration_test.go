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
auth:
  oidc:
    issuer: https://idp.example
    clientId: kritik
    clientSecret: { env: KRITIK_TEST_TOKEN }
    roleMapping: '"kritik-admin" in roles ? "admin" : ""'
connections:
  - name: mgr-file-bot
    forge: github
    accounts: [mf]
    app: { clientId: Iv1.test, privateKey: { env: KRITIK_TEST_TOKEN }, webhookSecret: { env: KRITIK_TEST_TOKEN } }
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
	github  *fakeGitHubServer
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
	e := &manageEnv{
		t: t, st: st, owner: owner, actions: &fakeActions{}, github: fakeGitHub(t),
		cookie: map[string]*http.Cookie{}, user: map[string]string{},
	}
	// Other suites leave a spec sealed under other keys.
	e.exec(`DELETE FROM instance_config`)
	t.Cleanup(func() { _, _ = owner.Exec(context.Background(), `DELETE FROM instance_config`) })

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
	if err := st.ApplyConfig(ctx, file); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	go e.src.Run(ctx, path, time.Hour)

	webURL, _ := url.Parse("https://kritik.example")
	h, err := auth.New(auth.Config{Store: st, Current: e.src.Current, WebURL: webURL, Logger: logger})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	e.srv = New(Config{
		Store: st, Current: e.src.Current, Auth: h, Keyring: kr, Actions: e.actions, WebURL: webURL, Logger: logger,
		GitHubAPI: e.github.url + "/api/v3",
	})
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

// instanceSpec is a spec serving md through mgr-dash-bot, with keyRef and
// hookRef its App's secrets, and the shared provider's key.
func instanceSpec(keyRef, hookRef map[string]any, extra map[string]any) map[string]any {
	spec := map[string]any{
		"providers": map[string]any{"shared": map[string]any{"type": "openrouter", "apiKey": map[string]any{"value": "sk-shared"}}},
		"connections": []any{map[string]any{
			"name": "mgr-dash-bot", "forge": "github", "accounts": []string{"md"},
			"app": map[string]any{"clientId": "Iv1.test", "privateKey": keyRef, "webhookSecret": hookRef},
		}},
		"accounts": []any{map[string]any{"forge": "github", "name": "md", "repositories": []any{map[string]any{"name": "one"}}}},
	}
	maps.Copy(spec, extra)
	return spec
}

// mdEntry is md's entry with extra keys.
func mdEntry(extra map[string]any) map[string]any {
	entry := map[string]any{"forge": "github", "name": "md", "repositories": []any{map[string]any{"name": "one"}}}
	maps.Copy(entry, extra)
	return entry
}

var keep = map[string]any{"keep": true}

const (
	instancePath = "/api/v1/config"
	mdPath       = "/api/v1/accounts/github/md"
)

// TestManage walks the instance spec and one account through their life;
// each step depends on the ones before it.
func TestManage(t *testing.T) {
	e := newManageEnv(t)
	var generated string
	t.Run("an admin writes the instance spec", func(t *testing.T) { generated = testWriteSpec(t, e) })
	t.Run("the generated webhook secret verifies a hook", func(t *testing.T) { testHookVerifies(t, e, generated) })
	md := configfile.AccountID(configfile.ForgeGitHub, "md")
	e.signIn("member", "mgr-member", memberOfAccounts("github/md"))
	t.Run("config reads are redacted", func(t *testing.T) { testConfigRedacted(t, e) })
	t.Run("account updates", func(t *testing.T) { testAccountUpdate(t, e) })
	t.Run("claims on the file's connections", func(t *testing.T) { testFileClaims(t, e) })
	t.Run("actions", func(t *testing.T) { testActions(t, e, md) })
	t.Run("audit log", func(t *testing.T) { testAuditLog(t, e, md) })
	t.Run("a file connection the spec crowds out", func(t *testing.T) { testFileConnectionLeftOut(t, e) })
}

func testWriteSpec(t *testing.T, e *manageEnv) string {
	before := e.totalAudits()
	req := UpdateConfigRequest{Spec: mustJSON(t, instanceSpec(map[string]any{"value": "plain-key-xyz"}, map[string]any{"generate": true}, nil))}
	status, body := e.do("anonymous", "PUT", instancePath, req)
	e.expect(status, body, http.StatusUnauthorized, "")
	status, body = e.do("outsider", "PUT", instancePath, req)
	e.expect(status, body, http.StatusForbidden, CodeForbidden)
	status, body = e.do("admin", "PUT", instancePath, UpdateConfigRequest{Revision: 3, Spec: req.Spec})
	e.expect(status, body, http.StatusConflict, CodeRevisionConflict)
	if e.totalAudits() != before {
		t.Fatal("a refused write left an audit row")
	}
	status, body = e.do("admin", "PUT", instancePath, req)
	e.expect(status, body, http.StatusOK, "")
	var res ConfigWriteResult
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatal(err)
	}
	secret := res.Generated["connections[mgr-dash-bot].app.webhookSecret"]
	if res.Revision != 1 || len(secret) != 64 {
		t.Fatalf("result = %s", body)
	}
	stored := e.scalar(`SELECT spec::text FROM instance_config`)
	for _, plain := range []string{"plain-key-xyz", secret, "sk-shared"} {
		if strings.Contains(stored, plain) {
			t.Errorf("stored spec holds %q: %s", plain, stored)
		}
	}
	if n := e.audits(AuditConfigUpdate, "instance"); n != 1 {
		t.Errorf("config.update audit rows = %d, want 1", n)
	}
	e.waitFor("md to merge", func(f *configfile.File) bool { _, ok := f.Account(configfile.ForgeGitHub, "md"); return ok })
	return secret
}

func testHookVerifies(t *testing.T, e *manageEnv, secret string) {
	in, ok := e.src.Current.Get().Connection("mgr-dash-bot")
	if !ok || in.WebhookSecretValue().Value() != secret || in.App.PrivateKeyValue().Value() != "plain-key-xyz" {
		t.Fatalf("merged connection does not open to the written secrets")
	}
	mux := http.NewServeMux()
	mux.Handle("POST /hooks/{connection}", ingest.NewHandler(e.src.Current, nil, slog.New(slog.NewTextHandler(io.Discard, nil))))
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
	status, body := e.do("admin", "GET", instancePath, nil)
	e.expect(status, body, http.StatusOK, "")
	var ic InstanceConfig
	if err := json.Unmarshal(body, &ic); err != nil || ic.Revision != 1 || !ic.Editable {
		t.Fatalf("instance config = %s", body)
	}
	if s := string(ic.Spec); strings.Contains(s, "sealed") || !strings.Contains(s, `"privateKey":{"set":true}`) || !strings.Contains(s, `"apiKey":{"set":true}`) {
		t.Errorf("instance spec is not redacted: %s", s)
	}
	status, body = e.do("member", "GET", instancePath, nil)
	e.expect(status, body, http.StatusForbidden, CodeForbidden)
	for _, who := range []string{"admin", "member"} {
		t.Run(who, func(t *testing.T) {
			status, body := e.do(who, "GET", mdPath+"/config", nil)
			e.expect(status, body, http.StatusOK, "")
			var c AccountConfig
			if err := json.Unmarshal(body, &c); err != nil {
				t.Fatal(err)
			}
			if c.Revision != 1 || c.Editable != (who == "admin") || !strings.Contains(string(c.Spec), `"name":"md"`) {
				t.Errorf("config = %s", body)
			}
		})
	}
	status, body = e.do("outsider", "GET", mdPath+"/config", nil)
	e.expect(status, body, http.StatusNotFound, CodeNotFound)
	status, body = e.do("admin", "GET", "/api/v1/accounts/github/mf/config", nil)
	e.expect(status, body, http.StatusOK, "")
	if !strings.Contains(string(body), `"spec":{"forge":"github","name":"mf"}`) {
		t.Errorf("an account without an entry = %s", body)
	}
}

func testAccountUpdate(t *testing.T, e *manageEnv) {
	before := e.totalAudits()
	sealedKey := e.scalar(`SELECT spec->'connections'->0->'app'->'privateKey'->>'sealed' FROM instance_config`)
	stale := UpdateConfigRequest{Revision: 7, Spec: mustJSON(t, mdEntry(nil))}
	status, body := e.do("admin", "PUT", mdPath+"/config", stale)
	e.expect(status, body, http.StatusConflict, CodeRevisionConflict)
	badModel := UpdateConfigRequest{Revision: 1, Spec: mustJSON(t, mdEntry(map[string]any{"models": map[string]any{"review": "nope/x"}}))}
	status, body = e.do("admin", "PUT", mdPath+"/config", badModel)
	e.expect(status, body, http.StatusUnprocessableEntity, CodeInvalidSpec)
	if !strings.Contains(string(body), `"path":"models.review"`) {
		t.Errorf("bad model = %s", body)
	}
	envKey := UpdateConfigRequest{Revision: 1, Spec: mustJSON(t, mdEntry(map[string]any{
		"providers": map[string]any{"mine": map[string]any{"type": "openai", "apiKey": map[string]any{"env": "HOME"}}}}))}
	status, body = e.do("admin", "PUT", mdPath+"/config", envKey)
	e.expect(status, body, http.StatusUnprocessableEntity, CodeInvalidSpec)
	other := UpdateConfigRequest{Revision: 1, Spec: mustJSON(t, map[string]any{"forge": "github", "name": "mf"})}
	status, body = e.do("admin", "PUT", mdPath+"/config", other)
	e.expect(status, body, http.StatusUnprocessableEntity, CodeInvalidSpec)
	good := UpdateConfigRequest{Revision: 1, Spec: mustJSON(t, mdEntry(map[string]any{
		"filter":    "true",
		"providers": map[string]any{"mine": map[string]any{"type": "openai", "apiKey": map[string]any{"value": "sk-mine"}}},
		"models":    map[string]any{"review": "mine/gpt"},
	}))}
	status, body = e.do("member", "PUT", mdPath+"/config", good)
	e.expect(status, body, http.StatusForbidden, CodeForbidden)
	status, body = e.do("outsider", "PUT", mdPath+"/config", good)
	e.expect(status, body, http.StatusNotFound, CodeNotFound)
	status, body = e.do("admin", "PUT", mdPath+"/config", good, false)
	e.expect(status, body, http.StatusForbidden, "")
	if e.totalAudits() != before {
		t.Fatalf("refused writes left %d audit rows", e.totalAudits()-before)
	}
	status, body = e.do("admin", "PUT", mdPath+"/config", good)
	e.expect(status, body, http.StatusOK, "")
	if !strings.Contains(string(body), `"revision":2`) {
		t.Errorf("update = %s", body)
	}
	if got := e.scalar(`SELECT spec->'connections'->0->'app'->'privateKey'->>'sealed' FROM instance_config`); got != sealedKey {
		t.Error("an account write replaced the connection's sealed private key")
	}
	if sealed := e.scalar(`SELECT spec->'accounts'->0->'providers'->'mine'->'apiKey'->>'sealed' FROM instance_config`); sealed == "" ||
		strings.Contains(sealed, "sk-mine") {
		t.Errorf("stored provider key = %q, want it sealed", sealed)
	}
	if n := e.audits(AuditAccountUpdate, "github/md"); n != 1 || e.totalAudits() != before+1 {
		t.Errorf("account.update audit rows = %d, want exactly 1", n)
	}
	e.waitFor("the provider key to merge", func(f *configfile.File) bool {
		a, ok := f.Account(configfile.ForgeGitHub, "md")
		if !ok {
			return false
		}
		p, ok := f.Provider(a, "mine")
		return ok && p.APIKeyValue().Value() == "sk-mine" && f.Settings(a, "").Models.Review == "mine/gpt"
	})
	status, body = e.do("admin", "GET", mdPath+"/config", nil)
	if status != http.StatusOK || strings.Contains(string(body), "sk-mine") || !strings.Contains(string(body), `"apiKey":{"set":true}`) {
		t.Errorf("config read = %d %s", status, body)
	}

	moved := instanceSpec(keep, keep, map[string]any{"providers": map[string]any{"shared": map[string]any{"type": "openrouter", "apiKey": keep}}})
	moved["connections"].([]any)[0].(map[string]any)["accounts"] = []string{"md", "other"}
	moved["accounts"] = []any{mdEntry(map[string]any{"filter": "true", "models": map[string]any{"review": "mine/gpt"},
		"providers": map[string]any{"mine": map[string]any{"type": "openai", "apiKey": keep}}})}
	status, body = e.do("admin", "PUT", instancePath, UpdateConfigRequest{Revision: 2, Spec: mustJSON(t, moved)})
	e.expect(status, body, http.StatusUnprocessableEntity, CodeReenterSecret)
	if !strings.Contains(string(body), `"path":"connections[0].app.privateKey"`) {
		t.Errorf("reenter_secret details = %s", body)
	}
}

// testFileClaims: the spec may not take a file connection's name or
// account.
func testFileClaims(t *testing.T, e *manageEnv) {
	for _, tt := range []struct{ name, conn, account, want string }{
		{"a name", "mgr-file-bot", "other", `"path":"connections[1].name"`},
		{"an account", "mgr-claim-bot", "MF", `"path":"connections[1].accounts[0]"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			spec := instanceSpec(keep, keep, map[string]any{"providers": map[string]any{"shared": map[string]any{"type": "openrouter", "apiKey": keep}}})
			spec["accounts"] = []any{mdEntry(map[string]any{"filter": "true", "models": map[string]any{"review": "mine/gpt"},
				"providers": map[string]any{"mine": map[string]any{"type": "openai", "apiKey": keep}}})}
			spec["connections"] = append(spec["connections"].([]any), map[string]any{
				"name": tt.conn, "forge": "github", "accounts": []string{tt.account},
				"app": map[string]any{"clientId": "Iv1.test", "privateKey": map[string]any{"value": "k"}, "webhookSecret": map[string]any{"value": "w"}},
			})
			status, body := e.do("admin", "PUT", instancePath, UpdateConfigRequest{Revision: 2, Spec: mustJSON(t, spec)})
			e.expect(status, body, http.StatusUnprocessableEntity, CodeInvalidSpec)
			if !strings.Contains(string(body), tt.want) {
				t.Errorf("claim = %s, want %s", body, tt.want)
			}
		})
	}
}

func testActions(t *testing.T, e *manageEnv, md string) {
	// The leader applies what the dashboard wrote; this test is the leader.
	if err := e.st.ApplyConfig(context.Background(), e.src.Current.Get()); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	repoID := configfile.RepositoryID(md, "md/one")
	e.exec(`INSERT INTO pull_requests (account_id, repository_id, number, title, author, head_sha)
		VALUES ($1, $2, 3, 'x', 'ada', 'h3') ON CONFLICT DO NOTHING`, md, repoID)

	status, body := e.do("member", "POST", mdPath+"/pulls/md/one/3/rerun", nil)
	e.expect(status, body, http.StatusForbidden, CodeForbidden)
	status, body = e.do("admin", "POST", mdPath+"/pulls/md/one/3/rerun", nil, false)
	e.expect(status, body, http.StatusForbidden, "")
	status, body = e.do("admin", "POST", mdPath+"/pulls/md/one/99/rerun", nil)
	e.expect(status, body, http.StatusNotFound, CodeNotFound)

	status, body = e.do("admin", "POST", mdPath+"/pulls/md/one/3/rerun", nil)
	e.expect(status, body, http.StatusAccepted, "")
	if string(bytes.TrimSpace(body)) != `{"jobId":101}` || e.actions.last() != fmt.Sprintf("rerun %s %s 3 in %s", md, repoID, md) {
		t.Errorf("rerun = %s, call %q", body, e.actions.last())
	}
	if n := e.audits(AuditReviewRerun, "md/one#3"); n != 1 {
		t.Errorf("review.rerun audit rows = %d", n)
	}

	review := "00000000-0000-4000-8000-000000000001"
	e.actions.notCancelable = "00000000-0000-4000-8000-000000000002"
	status, body = e.do("admin", "POST", mdPath+"/reviews/"+e.actions.notCancelable+"/cancel", nil)
	e.expect(status, body, http.StatusConflict, CodeNotCancelable)
	status, body = e.do("admin", "POST", mdPath+"/reviews/"+review+"/cancel", nil)
	e.expect(status, body, http.StatusAccepted, "")
	if e.actions.last() != fmt.Sprintf("cancel %s in %s", review, md) {
		t.Errorf("cancel call %q", e.actions.last())
	}
	if e.audits(AuditReviewCancel, review) != 1 || e.audits(AuditReviewCancel, e.actions.notCancelable) != 0 {
		t.Errorf("review.cancel audit rows are wrong")
	}

	status, body = e.do("admin", "POST", mdPath+"/repos/md/one/reindex", nil)
	e.expect(status, body, http.StatusAccepted, "")
	if e.actions.last() != fmt.Sprintf("reindex %s %s in %s", md, repoID, md) || e.audits(AuditRepoReindex, "md/one") != 1 {
		t.Errorf("reindex = %s, call %q", body, e.actions.last())
	}
}

func testAuditLog(t *testing.T, e *manageEnv, md string) {
	status, body := e.do("member", "GET", mdPath+"/audit", nil)
	e.expect(status, body, http.StatusForbidden, CodeForbidden)
	status, body = e.do("member", "GET", "/api/v1/admin/audit", nil)
	e.expect(status, body, http.StatusForbidden, CodeForbidden)

	var seen []AuditEvent
	path := mdPath + "/audit?limit=2"
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
	status, body = e.do("admin", "GET", "/api/v1/admin/audit?limit=200", nil)
	e.expect(status, body, http.StatusOK, "")
	if !strings.Contains(string(body), `"account":"","action":"config.update","target":"instance"`) {
		t.Errorf("admin audit lacks the instance write: %s", body)
	}
}

// testFileConnectionLeftOut stores a spec connection on the file
// connection's name directly, as no API write may: the file's leaves the
// running configuration, the admin console says why, and it returns once
// the spec lets it go.
func testFileConnectionLeftOut(t *testing.T, e *manageEnv) {
	ctx := context.Background()
	seal := func(v string) string {
		s, err := e.srv.keyring.Seal([]byte(v))
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	spec := mustJSON(t, map[string]any{"connections": []any{map[string]any{
		"name": "mgr-file-bot", "forge": "github", "accounts": []string{"mh"},
		"app": map[string]any{"clientId": "Iv1.test", "privateKey": map[string]any{"sealed": seal("k")}, "webhookSecret": map[string]any{"sealed": seal("w")}},
	}}})
	write := func(spec json.RawMessage) {
		err := e.st.WithAccount(ctx, "", func(tx pgx.Tx) error {
			stored, err := store.InstanceSpecIn(ctx, tx)
			if err != nil {
				return err
			}
			_, err = e.st.PutInstanceSpec(ctx, tx, spec, stored.Revision)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	write(spec)
	e.waitFor("mgr-file-bot to be left out", func(f *configfile.File) bool { return len(f.Skipped()) == 1 })
	status, body := e.do("admin", "GET", "/api/v1/admin/instance", nil)
	e.expect(status, body, http.StatusOK, "")
	if !strings.Contains(string(body), `"key":"mgr-file-bot","value":"left out: the dashboard's connection \"mgr-file-bot\" already holds the name"`) {
		t.Fatalf("instance settings = %s", body)
	}
	write(json.RawMessage(`{}`))
	e.waitFor("mgr-file-bot to return", func(f *configfile.File) bool {
		in, ok := f.Connection("mgr-file-bot")
		return ok && in.Origin() == configfile.OriginFile && len(f.Skipped()) == 0
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

// TestConcurrentSpecWrites: writes at one revision race, and exactly one
// lands; the rest are told to reload.
func TestConcurrentSpecWrites(t *testing.T) {
	e := newManageEnv(t)
	spec := mustJSON(t, map[string]any{"defaults": map[string]any{"forks": true}})
	const writers = 6
	statuses := make([]int, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Go(func() { statuses[i], _ = e.do("admin", "PUT", instancePath, UpdateConfigRequest{Spec: spec}) })
	}
	wg.Wait()
	counts := map[int]int{}
	for _, s := range statuses {
		counts[s]++
	}
	if counts[http.StatusOK] != 1 || counts[http.StatusConflict] != writers-1 {
		t.Fatalf("statuses = %v, want one 200 and the rest 409", statuses)
	}
	if rev := e.scalar(`SELECT revision::text FROM instance_config`); rev != "1" {
		t.Fatalf("revision = %s, want 1", rev)
	}
}

// TestReindexFileEmbedder: with an embedder the environment sets, a spec
// that sets another, or drops its own for the environment's while the
// index is its own, rebuilds the index and needs confirming.
func TestReindexFileEmbedder(t *testing.T) {
	t.Setenv("KRITIK_EMBEDDING_BASE_URL", "https://embed.example/v1")
	t.Setenv("KRITIK_EMBEDDING_API_KEY", "ek")
	t.Setenv("KRITIK_EMBEDDING_MODEL", "f")
	t.Setenv("KRITIK_EMBEDDING_DIMS", "8")
	e := newManageEnv(t)
	t.Cleanup(func() { e.exec(`DELETE FROM index_schema`) })
	revision := int64(0)
	put := func(spec map[string]any, confirm bool, wantStatus int, wantCode ErrorCode) {
		t.Helper()
		status, body := e.do("admin", "PUT", instancePath, UpdateConfigRequest{Revision: revision, Spec: mustJSON(t, spec), ConfirmReindex: confirm})
		e.expect(status, body, wantStatus, wantCode)
		if status == http.StatusOK {
			revision++
		}
	}
	own := map[string]any{"embedding": map[string]any{"baseUrl": "https://embed.example/v1", "apiKey": map[string]any{"value": "ek"}, "model": "g", "dims": 8}}
	// The leader has built the index for the environment's embedder.
	e.exec(`INSERT INTO index_schema (id, embed_model, embed_dims) VALUES (1, 'f', 8)`)
	first := instanceSpec(map[string]any{"value": "pem"}, map[string]any{"generate": true}, nil)
	put(first, false, http.StatusOK, "")
	put(instanceSpec(keep, keep, own), false, http.StatusConflict, CodeReindexRequired)
	put(instanceSpec(keep, keep, own), true, http.StatusOK, "")
	// Rebuilt for the spec's; dropping it for the environment's rebuilds again.
	e.exec(`UPDATE index_schema SET embed_model = 'g'`)
	put(instanceSpec(keep, keep, nil), false, http.StatusConflict, CodeReindexRequired)
	put(instanceSpec(keep, keep, nil), true, http.StatusOK, "")
}

// TestReindexConfirmation: a write whose embedder would rebuild the index
// needs the admin's confirmation, once; one that keeps the index's model
// and dimension, or adds the first embedder, does not.
func TestReindexConfirmation(t *testing.T) {
	e := newManageEnv(t)
	t.Cleanup(func() { e.exec(`DELETE FROM index_schema`) })
	revision := int64(0)
	put := func(path string, spec map[string]any, confirm bool, wantStatus int, wantCode ErrorCode) {
		t.Helper()
		status, body := e.do("admin", "PUT", path, UpdateConfigRequest{Revision: revision, Spec: mustJSON(t, spec), ConfirmReindex: confirm})
		e.expect(status, body, wantStatus, wantCode)
		if status == http.StatusOK {
			revision++
		}
	}
	embedding := func(model string, dims int, key map[string]any) map[string]any {
		return map[string]any{"embedding": map[string]any{"baseUrl": "https://embed.example/v1", "apiKey": key, "model": model, "dims": dims}}
	}
	withKeys := func(extra map[string]any) map[string]any {
		spec := instanceSpec(keep, keep, extra)
		spec["providers"] = map[string]any{"shared": map[string]any{"type": "openrouter", "apiKey": keep}}
		return spec
	}

	put(instancePath, instanceSpec(map[string]any{"value": "pem"}, map[string]any{"generate": true},
		embedding("m", 8, map[string]any{"value": "ek"})), false, http.StatusOK, "")
	// The leader has built the index for m/8.
	e.exec(`INSERT INTO index_schema (id, embed_model, embed_dims) VALUES (1, 'm', 8)`)

	put(instancePath, withKeys(embedding("m", 16, keep)), false, http.StatusConflict, CodeReindexRequired)
	put(instancePath, withKeys(embedding("n", 8, keep)), false, http.StatusConflict, CodeReindexRequired)
	put(instancePath, withKeys(embedding("n", 8, keep)), true, http.StatusOK, "")
	if got := e.scalar(`SELECT detail->>'reindex' FROM audit_events WHERE action = 'config.update' ORDER BY id DESC LIMIT 1`); got != "true" {
		t.Fatalf("audit detail reindex = %s, want true", got)
	}
	// Confirmed once: until the leader rebuilds, later writes naming n pass.
	polled := withKeys(embedding("n", 8, keep))
	polled["polling"] = map[string]any{"interval": "5m"}
	put(instancePath, polled, false, http.StatusOK, "")
	put(mdPath+"/config", mdEntry(map[string]any{"limits": map[string]any{"concurrency": 2}}), false, http.StatusOK, "")
	// No embedder, then the index's own again, need nothing; another does.
	put(instancePath, withKeys(nil), false, http.StatusOK, "")
	put(instancePath, withKeys(embedding("m", 8, map[string]any{"value": "ek"})), false, http.StatusOK, "")
	put(instancePath, withKeys(embedding("x", 8, keep)), false, http.StatusConflict, CodeReindexRequired)
}
