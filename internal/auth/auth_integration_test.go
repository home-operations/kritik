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
web:
  signIn:
    - name: corp
      type: oidc
      issuer: %[1]s
      clientId: kritik-client
      clientSecret: { env: KRITIK_TEST_TOKEN }
    - name: gh
      type: github
      host: %[2]s
      clientId: kritik-client
      clientSecret: { env: KRITIK_TEST_TOKEN }
    - name: fj
      type: forgejo
      host: %[3]s
      clientId: kritik-client
      clientSecret: { env: KRITIK_TEST_TOKEN }
  operators: ["corp:op-oidc", "gh:OpGH", "email:ops@ops.example"]
tenants:
  - slug: auth-personal
    installations:
      - {name: auth-personal-bot, forge: github, host: "%[2]s", account: alice-gh, app: &app {clientId: Iv1.x, privateKey: {env: KRITIK_TEST_TOKEN}, webhookSecret: {env: KRITIK_TEST_TOKEN}}}
  - slug: auth-acme
    installations:
      - {name: auth-acme-bot, forge: github, host: "%[2]s", account: acme, app: *app}
  - slug: auth-widgets
    installations:
      - {name: auth-widgets-bot, forge: github, host: "%[2]s", account: Widgets, app: *app}
  - slug: auth-pending
    installations:
      - {name: auth-pending-bot, forge: github, host: "%[2]s", account: pendco, app: *app}
  - slug: auth-fj
    installations:
      - {name: auth-fj-bot, forge: forgejo, host: "%[3]s", account: fjorg, token: {env: KRITIK_TEST_TOKEN}, webhookSecret: {env: KRITIK_TEST_TOKEN}}
  - slug: auth-invite
    installations:
      - {name: auth-invite-bot, forge: forgejo, host: "%[3]s", account: nobody, token: {env: KRITIK_TEST_TOKEN}, webhookSecret: {env: KRITIK_TEST_TOKEN}}
`

type authEnv struct {
	t        *testing.T
	st       *store.Store
	h        *Handler
	mux      *http.ServeMux
	current  *configfile.Current
	file     *configfile.File
	oidc     *fakeOAuth
	gh, fj   *fakeOAuth
	gh2      *fakeOAuth
	now      time.Time
	tenantID map[string]string
}

func newAuthEnv(t *testing.T) *authEnv {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, store.Options{
		AppURL: testEnv(t, "KRITIK_TEST_APP_URL"), OwnerURL: testEnv(t, "KRITIK_TEST_OWNER_URL"),
		AppRole: "kritik_app", RunnerRole: "kritik_runner",
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
		t: t, st: st, oidc: newFakeOIDC(t), gh: newFakeGitHub(t), gh2: newFakeGitHub(t), fj: newFakeForgejo(t),
		now: time.Now(), tenantID: map[string]string{},
	}
	t.Setenv("KRITIK_TEST_TOKEN", fakeClientSecret)
	e.file, err = configfile.Parse(fmt.Appendf(nil, authConfigYAML, e.oidc.srv.URL, e.gh.srv.URL, e.fj.srv.URL))
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
	e.h = New(Config{
		Store: st, Current: e.current, WebURL: mustParseURL(t, "https://kritik.example.com/dash/"),
		HTTPClient: trustingClient(e.oidc, e.gh, e.gh2, e.fj), Now: func() time.Time { return e.now },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
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
		if c.Name == loginCookieName {
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
		cleared = cleared || (c.Name == loginCookieName && c.MaxAge < 0)
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
		if c.Name == CookieName && c.Value != "" {
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

func (e *authEnv) roles(p *Principal) map[string]Role {
	e.t.Helper()
	if p == nil {
		e.t.Fatal("no principal")
	}
	bySlug := map[string]Role{}
	for slug, id := range e.tenantID {
		if r, ok := p.Memberships[id]; ok {
			bySlug[slug] = r
		}
	}
	return bySlug
}

func assertRoles(t *testing.T, got, want map[string]Role) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("roles = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("roles = %v, want %v", got, want)
		}
	}
}

func assertFailed(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if w.Code != status || !strings.Contains(w.Body.String(), "<code>"+code+"</code>") {
		t.Fatalf("status %d body %s; want %d with code %s", w.Code, w.Body.String(), status, code)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == CookieName {
			t.Fatalf("failed sign-in set a session cookie: %+v", c)
		}
	}
}

func TestOIDCSignIn(t *testing.T) {
	e := newAuthEnv(t)
	alice := &fakeUser{Login: "alice-oidc-" + randomHex(t), Email: "alice@oidc.example", EmailVerified: true}

	w := e.signIn("corp", e.oidc, alice, "#/reviews/42")
	if w.Code != http.StatusFound || w.Header().Get("Location") != "https://kritik.example.com/dash/#/reviews/42" {
		t.Fatalf("callback: status %d location %q body %s", w.Code, w.Header().Get("Location"), w.Body.String())
	}
	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == CookieName {
			cookie = c
		}
	}
	if cookie == nil || cookie.Path != "/dash" || !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie = %+v", cookie)
	}
	p := e.principal(cookie)
	if p == nil || p.Identity.Provider != "corp" || p.Identity.Subject != alice.Login || p.Identity.Login != alice.Login ||
		!p.Account.EmailVerified || p.Account.Email != alice.Email || p.Operator || len(p.Memberships) != 0 {
		t.Fatalf("principal = %+v", p)
	}

	t.Run("return_to outside the dashboard falls back to its root", func(t *testing.T) {
		w := e.signIn("corp", e.oidc, alice, "https://evil.example/")
		if w.Header().Get("Location") != "https://kritik.example.com/dash/#/" {
			t.Fatalf("location = %q", w.Header().Get("Location"))
		}
	})
	t.Run("state cannot be replayed", func(t *testing.T) {
		l := e.startLogin("corp", "")
		if w := e.finish("corp", e.oidc, l, alice); w.Code != http.StatusFound {
			t.Fatalf("first callback: %d %s", w.Code, w.Body.String())
		}
		assertFailed(t, e.finish("corp", e.oidc, l, alice), http.StatusBadRequest, "invalid_state")
	})
	t.Run("login CSRF: a callback in a browser that did not start the sign-in", func(t *testing.T) {
		l := e.startLogin("corp", "")
		code := e.oidc.authorize(l.loc.String(), alice, "")
		q := url.Values{"code": {code}, "state": {l.state()}}
		assertFailed(t, e.callback("corp", q), http.StatusBadRequest, "invalid_state")
		assertFailed(t, e.callback("corp", q, &http.Cookie{Name: loginCookieName, Value: "attacker-browser"}), http.StatusBadRequest, "invalid_state")
		// Neither attempt burned the state: the browser that started the
		// sign-in can still finish it.
		if w := e.callback("corp", q, l.cookie); w.Code != http.StatusFound {
			t.Fatalf("callback in the starting browser: %d %s", w.Code, w.Body.String())
		}
	})
	t.Run("state is bound to its sign-in", func(t *testing.T) {
		l := e.startLogin("corp", "")
		assertFailed(t, e.callback("gh", url.Values{"code": {"x"}, "state": {l.state()}}, l.cookie), http.StatusBadRequest, "invalid_state")
	})
	t.Run("expired state", func(t *testing.T) {
		l := e.startLogin("corp", "")
		e.now = e.now.Add(store.LoginStateTTL + time.Second)
		defer func() { e.now = e.now.Add(-store.LoginStateTTL - time.Second) }()
		assertFailed(t, e.callback("corp", url.Values{"code": {"x"}, "state": {l.state()}}, l.cookie), http.StatusBadRequest, "invalid_state")
	})
	t.Run("nonce mismatch", func(t *testing.T) {
		l := e.startLogin("corp", "")
		code := e.oidc.authorize(l.loc.String(), alice, "another-nonce")
		assertFailed(t, e.callback("corp", url.Values{"code": {code}, "state": {l.state()}}, l.cookie), http.StatusBadGateway, "exchange_failed")
	})
	t.Run("PKCE verifier must match the challenge", func(t *testing.T) {
		l := e.startLogin("corp", "")
		q := l.loc.Query()
		q.Set("code_challenge", "not-the-challenge")
		l.loc.RawQuery = q.Encode()
		assertFailed(t, e.finish("corp", e.oidc, l, alice), http.StatusBadGateway, "exchange_failed")
	})
	t.Run("signing in again ends the browser's previous session", func(t *testing.T) {
		old := e.mustSignIn("corp", e.oidc, alice)
		fresh := e.mustSignIn("corp", e.oidc, alice, old)
		if e.principal(old) != nil || e.principal(fresh) == nil {
			t.Fatal("the previous session survived a new sign-in in the same browser")
		}
	})
	t.Run("provider error is not echoed", func(t *testing.T) {
		l := e.startLogin("corp", "")
		w := e.callback("corp", url.Values{"error": {"<script>x</script>"}, "state": {l.state()}}, l.cookie)
		assertFailed(t, w, http.StatusBadRequest, "sign_in_denied")
		if strings.Contains(w.Body.String(), "script") {
			t.Fatalf("provider error leaked: %s", w.Body.String())
		}
	})
	t.Run("operator by subject", func(t *testing.T) {
		op := &fakeUser{Login: "op-oidc", Email: "op@oidc.example"}
		if p := e.principal(e.mustSignIn("corp", e.oidc, op)); p == nil || !p.Operator {
			t.Fatalf("principal = %+v, want an operator", p)
		}
	})
	t.Run("operator by verified email only", func(t *testing.T) {
		unverified := &fakeUser{Login: "ops-unverified-" + randomHex(t), Email: "ops@ops.example"}
		if p := e.principal(e.mustSignIn("corp", e.oidc, unverified)); p == nil || p.Operator {
			t.Fatalf("principal = %+v, want no operator for an unverified email", p)
		}
		verified := &fakeUser{Login: "ops-verified-" + randomHex(t), Email: "OPS@ops.example", EmailVerified: true}
		if p := e.principal(e.mustSignIn("corp", e.oidc, verified)); p == nil || !p.Operator {
			t.Fatalf("principal = %+v, want an operator for a verified email", p)
		}
	})
}

func TestGitHubSignInMemberships(t *testing.T) {
	e := newAuthEnv(t)
	alice := &fakeUser{ID: 1001, Login: "Alice-GH", Email: "alice@gh.example", EmailVerified: true,
		Orgs: map[string]string{"acme": "member", "widgets": "admin", "pendco": "pending"}}
	firstCookie := e.mustSignIn("gh", e.gh, alice)
	p := e.principal(firstCookie)
	assertRoles(t, e.roles(p), map[string]Role{"auth-personal": RoleAdmin, "auth-acme": RoleMember, "auth-widgets": RoleAdmin})
	if p.Identity.Subject != "1001" || p.Identity.Login != "Alice-GH" || p.Account.Email != "alice@gh.example" || !p.Account.EmailVerified {
		t.Fatalf("principal = %+v", p)
	}
	if !p.CanAdmin(e.tenantID["auth-widgets"]) || p.CanAdmin(e.tenantID["auth-acme"]) || !p.CanRead(e.tenantID["auth-acme"]) || p.CanRead(e.tenantID["auth-fj"]) {
		t.Fatalf("role checks wrong for %+v", p.Memberships)
	}

	delete(alice.Orgs, "acme")
	again := e.principal(e.mustSignIn("gh", e.gh, alice))
	assertRoles(t, e.roles(again), map[string]Role{"auth-personal": RoleAdmin, "auth-widgets": RoleAdmin})
	if again.Account.ID != p.Account.ID {
		t.Fatalf("second sign-in made account %s, want %s", again.Account.ID, p.Account.ID)
	}
	// The first session sees the refreshed memberships too: they are read
	// per request.
	assertRoles(t, e.roles(e.principal(firstCookie)), e.roles(again))

	op := &fakeUser{ID: 1002, Login: "opgh", Email: "op@gh.example"}
	if p := e.principal(e.mustSignIn("gh", e.gh, op)); !p.Operator || !p.CanAdmin(e.tenantID["auth-acme"]) {
		t.Fatalf("principal = %+v, want an operator", p)
	}
}

func TestForgejoSignInMemberships(t *testing.T) {
	e := newAuthEnv(t)
	tests := []struct {
		user *fakeUser
		want map[string]Role
	}{
		{&fakeUser{ID: 2001, Login: "bob-fj", Email: "bob@fj.example", EmailVerified: true, Orgs: map[string]string{"fjorg": "admin"}},
			map[string]Role{"auth-fj": RoleAdmin}},
		{&fakeUser{ID: 2002, Login: "carol-fj", Orgs: map[string]string{"fjorg": "member", "acme": "admin"}},
			map[string]Role{"auth-fj": RoleMember}},
		{&fakeUser{ID: 2003, Login: "nobody"}, map[string]Role{"auth-invite": RoleAdmin}},
		{&fakeUser{ID: 2004, Login: "dave-fj"}, map[string]Role{}},
	}
	for _, tt := range tests {
		t.Run(tt.user.Login, func(t *testing.T) {
			p := e.principal(e.mustSignIn("fj", e.fj, tt.user))
			assertRoles(t, e.roles(p), tt.want)
			if p.Identity.Subject != fmt.Sprint(tt.user.ID) || p.Account.EmailVerified != tt.user.EmailVerified {
				t.Fatalf("principal = %+v", p)
			}
		})
	}
}

func insertInvite(t *testing.T, st *store.Store, tenantID, email string, role Role, expires time.Time) string {
	t.Helper()
	var id string
	if err := st.App().QueryRow(context.Background(), `INSERT INTO invites (id, tenant_id, email, role, expires_at)
		VALUES (gen_random_uuid(), $1, $2, $3, $4) RETURNING id`, tenantID, email, role, expires).Scan(&id); err != nil {
		t.Fatalf("insert invite: %v", err)
	}
	return id
}

func TestInviteAcceptance(t *testing.T) {
	e := newAuthEnv(t)
	ctx := context.Background()
	suffix := randomHex(t)
	email := "carol-" + suffix + "@invite.example"
	pending := insertInvite(t, e.st, e.tenantID["auth-invite"], strings.ToUpper(email), RoleMember, e.now.Add(time.Hour))
	insertInvite(t, e.st, e.tenantID["auth-acme"], email, RoleAdmin, e.now.Add(-time.Minute))

	unverified := &fakeUser{Login: "carol-unverified-" + suffix, Email: email}
	assertRoles(t, e.roles(e.principal(e.mustSignIn("corp", e.oidc, unverified))), map[string]Role{})

	carol := &fakeUser{Login: "carol-" + suffix, Email: email, EmailVerified: true}
	p := e.principal(e.mustSignIn("corp", e.oidc, carol))
	assertRoles(t, e.roles(p), map[string]Role{"auth-invite": RoleMember})
	var acceptedBy *string
	if err := e.st.App().QueryRow(ctx, `SELECT accepted_by FROM invites WHERE id = $1 AND accepted_at IS NOT NULL`, pending).Scan(&acceptedBy); err != nil ||
		acceptedBy == nil || *acceptedBy != p.Account.ID {
		t.Fatalf("invite accepted_by = %v, %v; want %s", acceptedBy, err, p.Account.ID)
	}
	// An accepted invite survives the next sign-in's forge refresh.
	assertRoles(t, e.roles(e.principal(e.mustSignIn("corp", e.oidc, carol))), map[string]Role{"auth-invite": RoleMember})

	t.Run("forge and invite memberships combine to the higher role", func(t *testing.T) {
		dana := &fakeUser{ID: 3001, Login: "dana-" + suffix, Email: "dana-" + suffix + "@gh.example", EmailVerified: true,
			Orgs: map[string]string{"widgets": "admin"}}
		insertInvite(t, e.st, e.tenantID["auth-widgets"], dana.Email, RoleMember, e.now.Add(time.Hour))
		insertInvite(t, e.st, e.tenantID["auth-acme"], dana.Email, RoleAdmin, e.now.Add(time.Hour))
		// Forge admin plus a member invite is admin while the forge says so.
		assertRoles(t, e.roles(e.principal(e.mustSignIn("gh", e.gh, dana))), map[string]Role{"auth-widgets": RoleAdmin, "auth-acme": RoleAdmin})
		// Once the forge admin lapses, the invite's member role is what is left.
		delete(dana.Orgs, "widgets")
		assertRoles(t, e.roles(e.principal(e.mustSignIn("gh", e.gh, dana))), map[string]Role{"auth-widgets": RoleMember, "auth-acme": RoleAdmin})
	})
}

func TestSessionLifecycle(t *testing.T) {
	e := newAuthEnv(t)
	ctx := context.Background()
	user := &fakeUser{ID: 4001, Login: "erin-" + randomHex(t), Email: "erin@fj.example"}
	cookie := e.mustSignIn("fj", e.fj, user)
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
		trimmed.Web.SignIn = e.file.Web.SignIn[:2]
		e.current.Set(&trimmed)
		defer e.current.Set(e.file)
		if p := e.principal(cookie); p != nil {
			t.Fatalf("principal = %+v, want none", p)
		}
	})
	t.Run("unknown cookie", func(t *testing.T) {
		if p := e.principal(&http.Cookie{Name: CookieName, Value: "bogus"}); p != nil {
			t.Fatalf("principal = %+v", p)
		}
	})
	t.Run("logout", func(t *testing.T) {
		other := e.mustSignIn("fj", e.fj, user)
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
			cleared = cleared || (c.Name == CookieName && c.MaxAge < 0)
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
	viaOIDC := e.principal(e.mustSignIn("corp", e.oidc, &fakeUser{Login: "shared-oidc-" + randomHex(t), Email: email, EmailVerified: true}))
	viaGitHub := e.principal(e.mustSignIn("gh", e.gh, &fakeUser{ID: 5001, Login: "shared-gh", Email: email, EmailVerified: true}))
	viaForgejo := e.principal(e.mustSignIn("fj", e.fj, &fakeUser{ID: 5001, Login: "shared-fj", Email: email, EmailVerified: true}))
	if viaOIDC.Account.ID == viaGitHub.Account.ID || viaGitHub.Account.ID == viaForgejo.Account.ID || viaOIDC.Account.ID == viaForgejo.Account.ID {
		t.Fatalf("accounts linked by email: oidc %s github %s forgejo %s", viaOIDC.Account.ID, viaGitHub.Account.ID, viaForgejo.Account.ID)
	}
}

func TestSignInMovedToAnotherOrigin(t *testing.T) {
	e := newAuthEnv(t)
	user := &fakeUser{ID: 6001, Login: "frank-" + randomHex(t), Email: "frank@gh.example", EmailVerified: true}
	before := e.mustSignIn("gh", e.gh, user)
	was := e.principal(before)

	// The same sign-in name, now pointing at another GitHub host whose
	// user 6001 is someone else entirely.
	moved, err := configfile.Parse(fmt.Appendf(nil, authConfigYAML, e.oidc.srv.URL, e.gh2.srv.URL, e.fj.srv.URL))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	e.current.Set(moved)
	if p := e.principal(before); p != nil {
		t.Fatalf("session from the old origin still authenticates: %+v", p)
	}
	now := e.principal(e.mustSignIn("gh", e.gh2, user))
	if now == nil || now.Account.ID == was.Account.ID {
		t.Fatalf("same subject on a new origin linked to account %s", was.Account.ID)
	}
}
