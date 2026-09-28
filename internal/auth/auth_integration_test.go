//go:build integration

package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/store"
)

func testEnv(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Skipf("%s not set", key)
	}
	return v
}

func randomHex(t *testing.T) string {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func mustParseURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return u
}

const authConfigYAML = `
auth:
  admin:
    password: { env: KRITIK_TEST_ADMIN_PASSWORD }
  oidc:
    issuer: %[1]s
    clientId: kritik-client
    clientSecret: { env: KRITIK_TEST_TOKEN }
    rolesClaim: groups
    roleMapping: '"ops" in roles ? "admin" : ("staff" in roles ? "member" : "")'
  github:
    clientId: kritik-client
    clientSecret: { env: KRITIK_TEST_TOKEN }
    roleMapping: 'login == "opgh" ? "admin" : ("mapped-org" in orgs ? dyn({"github/acme": "member"}) : "")'
tenants:
  - slug: auth-personal
    installations:
      - {name: auth-personal-bot, forge: github, accounts: [alice-gh], app: &app {clientId: Iv1.x, privateKey: {env: KRITIK_TEST_TOKEN}, webhookSecret: {env: KRITIK_TEST_TOKEN}}}
  - slug: auth-acme
    installations:
      - {name: auth-acme-bot, forge: github, accounts: [acme], app: *app}
  - slug: auth-widgets
    installations:
      - {name: auth-widgets-bot, forge: github, accounts: [Widgets], app: *app}
  - slug: auth-pending
    installations:
      - {name: auth-pending-bot, forge: github, accounts: [pendco], app: *app}
  - slug: auth-invite
    installations:
      - {name: auth-invite-bot, forge: github, accounts: [nobody], app: *app}
`

type authEnv struct {
	t        *testing.T
	st       *store.Store
	h        *Handler
	mux      *http.ServeMux
	current  *configfile.Current
	file     *configfile.File
	oidc     *fakeOAuth
	oidc2    *fakeOAuth
	gh       *fakeOAuth
	now      time.Time
	tenantID map[string]string
}

func newAuthEnv(t *testing.T) *authEnv {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, store.Options{
		AppURL: testEnv(t, "KRITIK_TEST_APP_URL"), OwnerURL: testEnv(t, "KRITIK_TEST_OWNER_URL"),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx, "kritik_app", "kritik_runner"); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	e := &authEnv{
		t: t, st: st, oidc: newFakeOIDC(t), oidc2: newFakeOIDC(t), gh: newFakeGitHub(t),
		now: time.Now(), tenantID: map[string]string{},
	}
	t.Setenv("KRITIK_TEST_TOKEN", fakeClientSecret)
	t.Setenv("KRITIK_TEST_ADMIN_PASSWORD", adminTestPassword)
	e.file, err = configfile.Parse(fmt.Appendf(nil, authConfigYAML, e.oidc.srv.URL))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := st.ApplyConfig(ctx, e.file, "auth-test"); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	for i := range e.file.Tenants {
		e.tenantID[e.file.Tenants[i].Slug] = e.file.Tenants[i].ID()
	}
	e.current = configfile.NewCurrent(e.file)
	e.h, err = New(Config{
		Store: st, Current: e.current, WebURL: mustParseURL(t, "https://kritik.example.com/dash/"),
		HTTPClient: trustingClient(e.gh, e.oidc, e.oidc2), Now: func() time.Time { return e.now },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	e.mux = http.NewServeMux()
	e.h.Register(e.mux)
	return e
}

func (e *authEnv) do(r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	e.mux.ServeHTTP(w, r)
	return w
}

// login is a sign-in the browser has started: the provider authorize URL
// it was sent to and the login cookie that binds the sign-in to it.
type login struct {
	loc    *url.URL
	cookie *http.Cookie
}

func (l login) state() string { return l.loc.Query().Get("state") }

// startLogin begins a sign-in.
func (e *authEnv) startLogin(provider, returnTo string) login {
	e.t.Helper()
	w := e.do(httptest.NewRequest(http.MethodGet, "/auth/login/"+provider+"?return_to="+url.QueryEscape(returnTo), nil))
	if w.Code != http.StatusFound {
		e.t.Fatalf("login %s: status %d body %s", provider, w.Code, w.Body.String())
	}
	l := login{loc: mustParseURL(e.t, w.Header().Get("Location"))}
	for _, c := range w.Result().Cookies() {
		if c.Name == loginCookieName(e.h.webURL) {
			l.cookie = c
		}
	}
	if l.cookie == nil || l.cookie.Path != "/dash/auth/callback" || !l.cookie.HttpOnly || !l.cookie.Secure || l.cookie.MaxAge != 600 {
		e.t.Fatalf("login cookie = %+v", l.cookie)
	}
	return l
}

// callback calls the callback route as a browser holding cookies.
func (e *authEnv) callback(provider string, q url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	e.t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/auth/callback/"+provider+"?"+q.Encode(), nil)
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := e.do(r)
	cleared := false
	for _, c := range w.Result().Cookies() {
		cleared = cleared || (c.Name == loginCookieName(e.h.webURL) && c.MaxAge < 0)
	}
	if !cleared {
		e.t.Fatalf("callback (status %d) did not clear the login cookie", w.Code)
	}
	return w
}

// finish completes l as user in the browser that started it.
func (e *authEnv) finish(provider string, fake *fakeOAuth, l login, user *fakeUser, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	e.t.Helper()
	code := fake.authorize(l.loc.String(), user, "")
	return e.callback(provider, url.Values{"code": {code}, "state": {l.state()}}, append(cookies, l.cookie)...)
}

// signIn runs a whole sign-in as user and returns the callback's response.
func (e *authEnv) signIn(provider string, fake *fakeOAuth, user *fakeUser, returnTo string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.finish(provider, fake, e.startLogin(provider, returnTo), user, cookies...)
}

// mustSignIn signs in and returns the session cookie.
func (e *authEnv) mustSignIn(provider string, fake *fakeOAuth, user *fakeUser, cookies ...*http.Cookie) *http.Cookie {
	e.t.Helper()
	w := e.signIn(provider, fake, user, "", cookies...)
	if w.Code != http.StatusFound {
		e.t.Fatalf("sign in %s as %s: status %d body %s", provider, user.Login, w.Code, w.Body.String())
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == SessionCookieName(e.h.webURL) && c.Value != "" {
			return c
		}
	}
	e.t.Fatalf("sign in %s as %s set no session cookie", provider, user.Login)
	return nil
}

// principal is what Authenticate makes of a request carrying c.
func (e *authEnv) principal(c *http.Cookie) *Principal {
	e.t.Helper()
	var got *Principal
	r := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	if c != nil {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	e.h.Authenticate(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = PrincipalFrom(r.Context()) })).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		e.t.Fatalf("Authenticate: status %d body %s", w.Code, w.Body.String())
	}
	return got
}

// tenants lists the slugs of the file's tenants p reads.
func (e *authEnv) tenants(p *Principal) []string {
	e.t.Helper()
	if p == nil {
		e.t.Fatal("no principal")
	}
	var out []string
	for slug, id := range e.tenantID {
		if p.CanRead(id) {
			out = append(out, slug)
		}
	}
	slices.Sort(out)
	return out
}

func assertTenants(t *testing.T, got []string, want ...string) {
	t.Helper()
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("tenants = %v, want %v", got, want)
	}
}

func assertFailed(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if w.Code != status || !strings.Contains(w.Body.String(), "<code>"+code+"</code>") {
		t.Fatalf("status %d body %s; want %d with code %s", w.Code, w.Body.String(), status, code)
	}
	for _, c := range w.Result().Cookies() {
		if strings.HasSuffix(c.Name, sessionCookieBase) {
			t.Fatalf("failed sign-in set a session cookie: %+v", c)
		}
	}
}

// adminTestPassword is the local admin's password, KRITIK_TEST_ADMIN_PASSWORD.
const adminTestPassword = "correct horse battery staple"

func TestOIDCSignIn(t *testing.T) {
	e := newAuthEnv(t)
	alice := &fakeUser{Login: "alice-oidc-" + randomHex(t), Email: "alice@oidc.example", EmailVerified: true, Groups: []string{"staff"}}

	w := e.signIn("oidc", e.oidc, alice, "#/reviews/42")
	if w.Code != http.StatusFound || w.Header().Get("Location") != "https://kritik.example.com/dash/#/reviews/42" {
		t.Fatalf("callback: status %d location %q body %s", w.Code, w.Header().Get("Location"), w.Body.String())
	}
	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == SessionCookieName(e.h.webURL) {
			cookie = c
		}
	}
	if cookie == nil || cookie.Path != "/dash" || !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie = %+v", cookie)
	}
	p := e.principal(cookie)
	if p == nil || p.Identity.Provider != "oidc" || p.Identity.Subject != alice.Login || p.Identity.Login != alice.Login ||
		!p.Account.EmailVerified || p.Account.Email != alice.Email || p.Operator || !p.AllTenants {
		t.Fatalf("principal = %+v", p)
	}

	t.Run("return_to outside the dashboard falls back to its root", func(t *testing.T) {
		w := e.signIn("oidc", e.oidc, alice, "https://evil.example/")
		if w.Header().Get("Location") != "https://kritik.example.com/dash/#/" {
			t.Fatalf("location = %q", w.Header().Get("Location"))
		}
	})
	t.Run("state cannot be replayed", func(t *testing.T) {
		l := e.startLogin("oidc", "")
		if w := e.finish("oidc", e.oidc, l, alice); w.Code != http.StatusFound {
			t.Fatalf("first callback: %d %s", w.Code, w.Body.String())
		}
		assertFailed(t, e.finish("oidc", e.oidc, l, alice), http.StatusBadRequest, "invalid_state")
	})
	t.Run("login CSRF: a callback in a browser that did not start the sign-in", func(t *testing.T) {
		l := e.startLogin("oidc", "")
		code := e.oidc.authorize(l.loc.String(), alice, "")
		q := url.Values{"code": {code}, "state": {l.state()}}
		assertFailed(t, e.callback("oidc", q), http.StatusBadRequest, "invalid_state")
		assertFailed(t, e.callback("oidc", q, &http.Cookie{Name: loginCookieName(e.h.webURL), Value: "attacker-browser"}), http.StatusBadRequest, "invalid_state")
		// Neither attempt burned the state: the browser that started the
		// sign-in can still finish it.
		if w := e.callback("oidc", q, l.cookie); w.Code != http.StatusFound {
			t.Fatalf("callback in the starting browser: %d %s", w.Code, w.Body.String())
		}
	})
	t.Run("state is bound to its sign-in", func(t *testing.T) {
		l := e.startLogin("oidc", "")
		assertFailed(t, e.callback("github", url.Values{"code": {"x"}, "state": {l.state()}}, l.cookie), http.StatusBadRequest, "invalid_state")
	})
	t.Run("expired state", func(t *testing.T) {
		l := e.startLogin("oidc", "")
		e.now = e.now.Add(store.LoginStateTTL + time.Second)
		defer func() { e.now = e.now.Add(-store.LoginStateTTL - time.Second) }()
		assertFailed(t, e.callback("oidc", url.Values{"code": {"x"}, "state": {l.state()}}, l.cookie), http.StatusBadRequest, "invalid_state")
	})
	t.Run("nonce mismatch", func(t *testing.T) {
		l := e.startLogin("oidc", "")
		code := e.oidc.authorize(l.loc.String(), alice, "another-nonce")
		assertFailed(t, e.callback("oidc", url.Values{"code": {code}, "state": {l.state()}}, l.cookie), http.StatusBadGateway, "exchange_failed")
	})
	t.Run("PKCE verifier must match the challenge", func(t *testing.T) {
		l := e.startLogin("oidc", "")
		q := l.loc.Query()
		q.Set("code_challenge", "not-the-challenge")
		l.loc.RawQuery = q.Encode()
		assertFailed(t, e.finish("oidc", e.oidc, l, alice), http.StatusBadGateway, "exchange_failed")
	})
	t.Run("signing in again ends the browser's previous session", func(t *testing.T) {
		old := e.mustSignIn("oidc", e.oidc, alice)
		fresh := e.mustSignIn("oidc", e.oidc, alice, old)
		if e.principal(old) != nil || e.principal(fresh) == nil {
			t.Fatal("the previous session survived a new sign-in in the same browser")
		}
	})
	t.Run("provider error is not echoed", func(t *testing.T) {
		l := e.startLogin("oidc", "")
		w := e.callback("oidc", url.Values{"error": {"<script>x</script>"}, "state": {l.state()}}, l.cookie)
		assertFailed(t, w, http.StatusBadRequest, "sign_in_denied")
		if strings.Contains(w.Body.String(), "script") {
			t.Fatalf("provider error leaked: %s", w.Body.String())
		}
	})
	t.Run("an admin by role", func(t *testing.T) {
		op := &fakeUser{Login: "op-oidc-" + randomHex(t), Email: "op@oidc.example", Groups: []string{"ops"}}
		if p := e.principal(e.mustSignIn("oidc", e.oidc, op)); p == nil || !p.Operator {
			t.Fatalf("principal = %+v, want an admin", p)
		}
	})
	t.Run("nobody the mapping places is refused", func(t *testing.T) {
		stranger := &fakeUser{Login: "stranger-" + randomHex(t), Email: "s@oidc.example", EmailVerified: true, Groups: []string{"other"}}
		assertFailed(t, e.signIn("oidc", e.oidc, stranger, ""), http.StatusForbidden, "not_allowed")
	})
}

func TestGitHubSignInGrants(t *testing.T) {
	e := newAuthEnv(t)
	alice := &fakeUser{ID: 1001, Login: "Alice-GH", Email: "alice@gh.example", EmailVerified: true,
		Orgs: map[string]string{"acme": "member", "widgets": "admin", "pendco": "pending"}}
	firstCookie := e.mustSignIn("github", e.gh, alice)
	p := e.principal(firstCookie)
	assertTenants(t, e.tenants(p), "auth-personal", "auth-acme", "auth-widgets")
	if p.Identity.Subject != "1001" || p.Identity.Login != "Alice-GH" || p.Account.Email != "alice@gh.example" || !p.Account.EmailVerified ||
		p.Operator || p.AllTenants {
		t.Fatalf("principal = %+v", p)
	}

	delete(alice.Orgs, "acme")
	again := e.principal(e.mustSignIn("github", e.gh, alice))
	assertTenants(t, e.tenants(again), "auth-personal", "auth-widgets")
	if again.Account.ID != p.Account.ID {
		t.Fatalf("second sign-in made account %s, want %s", again.Account.ID, p.Account.ID)
	}
	// A session keeps the grant it signed in with.
	assertTenants(t, e.tenants(e.principal(firstCookie)), "auth-personal", "auth-acme", "auth-widgets")

	t.Run("an admin by login", func(t *testing.T) {
		op := &fakeUser{ID: 1002, Login: "opgh", Email: "op@gh.example"}
		if p := e.principal(e.mustSignIn("github", e.gh, op)); !p.Operator || !p.CanRead(e.tenantID["auth-acme"]) {
			t.Fatalf("principal = %+v, want an admin", p)
		}
	})
	t.Run("a mapped account", func(t *testing.T) {
		bob := &fakeUser{ID: 1003, Login: "bob", Orgs: map[string]string{"mapped-org": "member"}}
		assertTenants(t, e.tenants(e.principal(e.mustSignIn("github", e.gh, bob))), "auth-acme")
	})
	t.Run("a stranger is refused", func(t *testing.T) {
		assertFailed(t, e.signIn("github", e.gh, &fakeUser{ID: 1004, Login: "mallory"}, ""), http.StatusForbidden, "not_allowed")
	})
}

// localSignIn posts the password form.
func (e *authEnv) localSignIn(user, password string) *httptest.ResponseRecorder {
	e.t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/auth/local", strings.NewReader(fmt.Sprintf(`{"user":%q,"password":%q}`, user, password)))
	r.Header.Set("X-Kritik", "1")
	r.Header.Set("Origin", "https://kritik.example.com")
	r.RemoteAddr = "192.0.2.1:1234"
	return e.do(r)
}

func sessionFrom(t *testing.T, w *httptest.ResponseRecorder, webURL *url.URL) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == SessionCookieName(webURL) && c.Value != "" {
			return c
		}
	}
	t.Fatalf("no session cookie (status %d body %s)", w.Code, w.Body.String())
	return nil
}

func TestLocalAdminSignIn(t *testing.T) {
	e := newAuthEnv(t)
	if w := e.localSignIn("admin", "wrong"); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d %s", w.Code, w.Body.String())
	}
	w := e.localSignIn("admin", adminTestPassword)
	if w.Code != http.StatusNoContent {
		t.Fatalf("sign-in: %d %s", w.Code, w.Body.String())
	}
	cookie := sessionFrom(t, w, e.h.webURL)
	p := e.principal(cookie)
	if p == nil || !p.Operator || p.Identity.Provider != "local" || p.Identity.Login != "admin" || p.Account.DisplayName != "admin" {
		t.Fatalf("principal = %+v", p)
	}
	again := e.principal(sessionFrom(t, e.localSignIn("admin", adminTestPassword), e.h.webURL))
	if again == nil || again.Account.ID != p.Account.ID {
		t.Fatalf("second sign-in = %+v, want account %s", again, p.Account.ID)
	}

	t.Run("rotating the password ends the session", func(t *testing.T) {
		t.Setenv("KRITIK_TEST_ADMIN_PASSWORD", "rotated")
		rotated, err := configfile.Parse(fmt.Appendf(nil, authConfigYAML, e.oidc.srv.URL))
		if err != nil {
			t.Fatal(err)
		}
		e.current.Set(rotated)
		defer e.current.Set(e.file)
		if p := e.principal(cookie); p != nil {
			t.Fatalf("principal = %+v after the password changed", p)
		}
	})
}

func TestSessionLifecycle(t *testing.T) {
	e := newAuthEnv(t)
	ctx := context.Background()
	user := &fakeUser{ID: 4001, Login: "erin-" + randomHex(t), Email: "erin@gh.example", Orgs: map[string]string{"acme": "member"}}
	cookie := e.mustSignIn("github", e.gh, user)
	p := e.principal(cookie)
	if p == nil {
		t.Fatal("no principal for a fresh session")
	}
	lastSeen := func() time.Time {
		t.Helper()
		var ts time.Time
		if err := e.st.App().QueryRow(ctx, `SELECT last_seen_at FROM sessions WHERE account_id = $1`, p.Account.ID).Scan(&ts); err != nil {
			t.Fatalf("last_seen_at: %v", err)
		}
		return ts
	}
	first := lastSeen()
	e.now = e.now.Add(30 * time.Second)
	e.principal(cookie)
	if !lastSeen().Equal(first) {
		t.Fatal("last_seen_at written within a minute of the last write")
	}
	e.now = e.now.Add(time.Minute)
	e.principal(cookie)
	if !lastSeen().After(first) {
		t.Fatal("last_seen_at not refreshed after a minute")
	}

	t.Run("a sign-in removed from the file ends its sessions", func(t *testing.T) {
		trimmed := *e.file
		trimmed.Auth.GitHub = nil
		e.current.Set(&trimmed)
		defer e.current.Set(e.file)
		if p := e.principal(cookie); p != nil {
			t.Fatalf("principal = %+v, want none", p)
		}
	})
	t.Run("a changed role mapping ends its sessions", func(t *testing.T) {
		changed, err := configfile.Parse([]byte(strings.Replace(fmt.Sprintf(authConfigYAML, e.oidc.srv.URL), `login == "opgh"`, `login == "someone"`, 1)))
		if err != nil {
			t.Fatal(err)
		}
		e.current.Set(changed)
		defer e.current.Set(e.file)
		if p := e.principal(cookie); p != nil {
			t.Fatalf("principal = %+v, want none", p)
		}
	})
	t.Run("unknown cookie", func(t *testing.T) {
		if p := e.principal(&http.Cookie{Name: SessionCookieName(e.h.webURL), Value: "bogus"}); p != nil {
			t.Fatalf("principal = %+v", p)
		}
	})
	t.Run("logout", func(t *testing.T) {
		other := e.mustSignIn("github", e.gh, user)
		r := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
		r.Header.Set("X-Kritik", "1")
		r.Header.Set("Origin", "https://kritik.example.com")
		r.AddCookie(other)
		w := e.do(r)
		if w.Code != http.StatusNoContent {
			t.Fatalf("logout: %d %s", w.Code, w.Body.String())
		}
		cleared := false
		for _, c := range w.Result().Cookies() {
			cleared = cleared || (c.Name == SessionCookieName(e.h.webURL) && c.MaxAge < 0)
		}
		if !cleared || e.principal(other) != nil {
			t.Fatalf("logout left the session usable (cleared cookie %v)", cleared)
		}
		if e.principal(cookie) == nil {
			t.Fatal("logout ended another session")
		}
	})
	t.Run("expiry", func(t *testing.T) {
		e.now = e.now.Add(configfile.DefaultSessionTTL)
		if p := e.principal(cookie); p != nil {
			t.Fatalf("principal = %+v after the session TTL", p)
		}
	})
}

func TestIdentitiesNotLinkedAcrossProviders(t *testing.T) {
	e := newAuthEnv(t)
	email := "shared-" + randomHex(t) + "@example.com"
	viaOIDC := e.principal(e.mustSignIn("oidc", e.oidc, &fakeUser{Login: "shared-oidc-" + randomHex(t), Email: email, EmailVerified: true, Groups: []string{"staff"}}))
	viaGitHub := e.principal(e.mustSignIn("github", e.gh, &fakeUser{ID: 5001, Login: "shared-gh", Email: email, EmailVerified: true,
		Orgs: map[string]string{"acme": "member"}}))
	if viaOIDC.Account.ID == viaGitHub.Account.ID {
		t.Fatalf("accounts linked by email: oidc %s github %s", viaOIDC.Account.ID, viaGitHub.Account.ID)
	}
}

func TestSignInMovedToAnotherOrigin(t *testing.T) {
	e := newAuthEnv(t)
	user := &fakeUser{Login: "frank-" + randomHex(t), Email: "frank@example.com", EmailVerified: true, Groups: []string{"staff"}}
	before := e.mustSignIn("oidc", e.oidc, user)
	was := e.principal(before)

	// The same sign-in, now pointing at another issuer whose subject of the
	// same name is someone else entirely.
	moved, err := configfile.Parse(fmt.Appendf(nil, authConfigYAML, e.oidc2.srv.URL))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	e.current.Set(moved)
	if p := e.principal(before); p != nil {
		t.Fatalf("session from the old origin still authenticates: %+v", p)
	}
	now := e.principal(e.mustSignIn("oidc", e.oidc2, user))
	if now == nil || now.Account.ID == was.Account.ID {
		t.Fatalf("same subject on a new origin linked to account %s", was.Account.ID)
	}
}
