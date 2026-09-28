package auth

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/store"
)

// URL schemes the dashboard may be served on.
const (
	schemeHTTP  = "http"
	schemeHTTPS = "https"
)

// sessionCookieBase is the dashboard session cookie's name before its
// prefix; SessionCookieName is the name a browser sees.
const sessionCookieBase = "kritik_session"

// loginCookieBase names the cookie binding an in-flight sign-in to the
// browser that started it, so a callback URL carried into another browser
// cannot sign that browser in as someone else (login CSRF).
const loginCookieBase = "kritik_login"

// defaultHTTPTimeout bounds each call to a sign-in provider.
const defaultHTTPTimeout = 15 * time.Second

// returnToRe is the only shape a post-sign-in destination may take: a route
// of the dashboard's hash router, so a return_to can never leave WebURL.
var returnToRe = regexp.MustCompile(`^#/[A-Za-z0-9/_.~%-]*$`)

// Config wires a Handler.
type Config struct {
	Store   *store.Store
	Current *configfile.Current
	// WebURL is the dashboard's external URL, KRITIK_WEB_URL: callbacks,
	// the cookie's path and Secure flag, and the allowed Origin derive from
	// it.
	WebURL *url.URL
	// HTTPClient calls sign-in providers; nil gets one with a timeout.
	HTTPClient *http.Client
	// Now defaults to time.Now.
	Now func() time.Time
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// Handler serves sign-in and sign-out and authenticates dashboard requests.
type Handler struct {
	store     *store.Store
	current   *configfile.Current
	webURL    *url.URL
	origin    string
	now       func() time.Time
	logger    *slog.Logger
	providers *providers
	attempts  *attempts
}

// New builds a Handler from c, or fails with ErrWebURL.
func New(c Config) (*Handler, error) {
	if c.WebURL == nil {
		return nil, ErrWebURL
	}
	origin := normalizeOrigin(c.WebURL.Scheme + "://" + c.WebURL.Host)
	if origin == "" || c.WebURL.Hostname() == "" {
		return nil, fmt.Errorf("%w: got %q", ErrWebURL, c.WebURL.Redacted())
	}
	if c.HTTPClient == nil {
		c.HTTPClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return &Handler{
		store:     c.Store,
		current:   c.Current,
		webURL:    c.WebURL,
		origin:    origin,
		now:       c.Now,
		logger:    c.Logger,
		providers: newProviders(c.WebURL, c.HTTPClient, c.Now),
		attempts:  newAttempts(c.Now),
	}, nil
}

// ErrWebURL is a dashboard URL that is not an absolute http or https URL
// with a host: the allowed Origin, the callbacks and the cookies all derive
// from it.
var ErrWebURL = errors.New("auth: the web URL must be an absolute http or https URL with a host")

// Register mounts the sign-in routes on mux, relative to the dashboard's
// root; the caller strips any base path.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/providers", h.listProviders)
	mux.HandleFunc("GET /auth/login/{name}", h.login)
	mux.HandleFunc("GET /auth/callback/{name}", h.callback)
	mux.Handle("POST /auth/local", h.SameOrigin(http.HandlerFunc(h.localSignIn)))
	mux.Handle("POST /auth/logout", h.SameOrigin(http.HandlerFunc(h.logout)))
}

// ProviderInfo is one sign-in as the sign-in page lists it.
type ProviderInfo struct {
	Name        string                `json:"name"`
	Type        configfile.SignInType `json:"type"`
	DisplayName string                `json:"displayName"`
}

// Providers lists the ways to sign in the current file configures: the
// local admin's password form first, then OIDC and GitHub.
func (h *Handler) Providers() []ProviderInfo {
	auth := h.current.Get().Auth
	out := []ProviderInfo{}
	if _, _, ok := auth.AdminUser(); ok {
		out = append(out, ProviderInfo{Name: string(configfile.SignInLocal), Type: configfile.SignInLocal, DisplayName: "Admin"})
	}
	for _, s := range auth.SignIns() {
		out = append(out, ProviderInfo{Name: string(s.Type()), Type: s.Type(), DisplayName: s.Label()})
	}
	return out
}

func (h *Handler) listProviders(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, h.Providers())
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	p, _, err := h.providers.get(r.Context(), h.current.Get().Auth, name)
	if errors.Is(err, ErrUnknownProvider) {
		h.fail(w, r, http.StatusNotFound, codeUnknownSignIn, nil)
		return
	}
	if err != nil {
		h.fail(w, r, http.StatusBadGateway, codeProviderUnavailable, err)
		return
	}
	nonce, browser := randomString(), randomString()
	ls := store.LoginState{
		Provider: name, Nonce: nonce, PKCEVerifier: oauth2.GenerateVerifier(),
		ReturnTo: returnTo(r.URL.Query().Get("return_to")),
	}
	state, err := h.store.CreateLoginState(r.Context(), ls, browser, h.now())
	if errors.Is(err, store.ErrLoginStatesFull) {
		h.fail(w, r, http.StatusServiceUnavailable, codeTooManySignIns, err)
		return
	}
	if err != nil {
		h.fail(w, r, http.StatusInternalServerError, codeInternal, err)
		return
	}
	http.SetCookie(w, loginCookie(h.webURL, browser))
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, p.AuthCodeURL(state, ls.Nonce, ls.PKCEVerifier), http.StatusFound)
}

// callback completes a sign-in: it consumes the state the login left,
// provided this browser holds the login cookie it was bound to, trades the
// code for an identity, decides what the person may do, and starts a
// session with that grant in place of any this browser already had. A
// sign-in that grants nothing is refused.
func (h *Handler) callback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name, q := r.PathValue("name"), r.URL.Query()
	// Whatever happens, this sign-in attempt is over for the browser.
	http.SetCookie(w, clearedLoginCookie(h.webURL))
	var browser string
	if c, err := r.Cookie(loginCookieName(h.webURL)); err == nil {
		browser = c.Value
	}
	if browser == "" {
		h.fail(w, r, http.StatusBadRequest, codeInvalidState, nil)
		return
	}
	ls, err := h.store.ConsumeLoginState(ctx, q.Get("state"), browser, h.now())
	if errors.Is(err, store.ErrLoginState) || (err == nil && ls.Provider != name) {
		h.fail(w, r, http.StatusBadRequest, codeInvalidState, nil)
		return
	}
	if err != nil {
		h.fail(w, r, http.StatusInternalServerError, codeInternal, err)
		return
	}
	if q.Get("error") != "" || q.Get("code") == "" {
		// The provider's own error text is not shown: it is attacker-steerable.
		h.fail(w, r, http.StatusBadRequest, codeSignInDenied, fmt.Errorf("auth: %s: provider error %q", name, q.Get("error")))
		return
	}
	file := h.current.Get()
	p, signIn, err := h.providers.get(ctx, file.Auth, name)
	if errors.Is(err, ErrUnknownProvider) {
		h.fail(w, r, http.StatusNotFound, codeUnknownSignIn, nil)
		return
	}
	if err != nil {
		h.fail(w, r, http.StatusBadGateway, codeProviderUnavailable, err)
		return
	}
	id, facts, err := p.Exchange(ctx, q.Get("code"), ls.PKCEVerifier, ls.Nonce)
	if err != nil {
		h.fail(w, r, http.StatusBadGateway, codeExchangeFailed, err)
		return
	}
	g, err := grant(ctx, file, signIn, id, facts)
	switch {
	case errors.Is(err, ErrNoGrant):
		h.fail(w, r, http.StatusForbidden, codeNotAllowed, fmt.Errorf("auth: %s: %s: %w", name, id.Login, err))
		return
	case errors.Is(err, ErrRoleMapping):
		h.fail(w, r, http.StatusForbidden, codeRoleMappingFailed, err)
		return
	case err != nil:
		h.fail(w, r, http.StatusBadGateway, codeMembershipFailed, err)
		return
	}
	token, expires, err := h.replaceSession(r, id, g, file.Auth.SessionTTLOrDefault())
	if err != nil {
		h.fail(w, r, http.StatusInternalServerError, codeInternal, err)
		return
	}
	http.SetCookie(w, sessionCookie(h.webURL, token, expires))
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, h.home()+returnTo(ls.ReturnTo), http.StatusFound)
}

// replaceSession ends the session r's browser already has, if any, records
// the signed-in identity, and returns a new session's cookie value and
// expiry, holding grant g.
func (h *Handler) replaceSession(r *http.Request, id Identity, g store.SessionGrant, ttl time.Duration) (string, time.Time, error) {
	ctx, now := r.Context(), h.now()
	if c, err := r.Cookie(SessionCookieName(h.webURL)); err == nil && c.Value != "" {
		if err := h.store.DeleteSession(ctx, c.Value); err != nil {
			return "", time.Time{}, err
		}
	}
	user, err := h.store.UpsertIdentity(ctx, store.SignInIdentity(id))
	if err != nil {
		return "", time.Time{}, err
	}
	expires := now.Add(ttl)
	token, err := h.store.CreateSession(ctx, user.ID, id.Provider, id.Origin, g, now, expires)
	if err != nil {
		return "", time.Time{}, err
	}
	h.logger.InfoContext(ctx, "auth: signed in", "user", user.ID, "sign_in", id.Provider, "login", id.Login,
		"role", g.Role, "all_accounts", g.AllAccounts, "accounts", len(g.Accounts))
	return token, expires, nil
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(SessionCookieName(h.webURL)); err == nil && c.Value != "" {
		if err := h.store.DeleteSession(r.Context(), c.Value); err != nil {
			h.logger.ErrorContext(r.Context(), "auth: sign out", "error", err)
			writeJSON(w, http.StatusInternalServerError, errorBody{Code: codeInternal})
			return
		}
	}
	http.SetCookie(w, clearedCookie(h.webURL))
	w.WriteHeader(http.StatusNoContent)
}

// home is WebURL with a trailing slash, where the dashboard's index lives.
func (h *Handler) home() string {
	return strings.TrimSuffix(h.webURL.String(), "/") + "/"
}

const errorPage = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Sign-in failed</title></head>
<body><h1>Sign-in failed</h1><p>Error code: <code>%s</code></p><p><a href="%s">Back to kritik</a></p></body></html>
`

// fail renders the sign-in error page with only a fixed error code; err,
// which may carry provider detail, goes to the log.
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, status int, code errorCode, err error) {
	if err != nil {
		h.logger.WarnContext(r.Context(), "auth: sign-in failed", "code", code, "sign_in", r.PathValue("name"), "error", err)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, errorPage, html.EscapeString(string(code)), html.EscapeString(h.home())) // a failed write has no one to report to
}

// returnTo is s when it is a dashboard route, else the dashboard's root.
func returnTo(s string) string {
	if returnToRe.MatchString(s) {
		return s
	}
	return "#/"
}

// cookiePath scopes the session cookie to the dashboard: WebURL's path
// without a trailing slash, or "/" at the root.
func cookiePath(webURL *url.URL) string {
	if p := strings.TrimRight(webURL.Path, "/"); p != "" {
		return p
	}
	return "/"
}

// loginCookiePath limits the login cookie to the callback routes, except
// under __Host-, which requires Path=/.
func loginCookiePath(webURL *url.URL) string {
	if cookiePrefix(webURL) == hostPrefix {
		return "/"
	}
	return strings.TrimSuffix(cookiePath(webURL), "/") + "/auth/callback"
}

const hostPrefix = "__Host-"

// cookiePrefix is the prefix on every auth cookie's name. Over https at the
// root it is __Host-: the browser then accepts the cookie only from this
// exact host, Secure, with no Domain and Path=/, so a sibling subdomain
// cannot plant a session or login cookie of its own. Under a path only
// __Secure- is possible; over plain http, none.
func cookiePrefix(webURL *url.URL) string {
	switch {
	case webURL.Scheme == schemeHTTP:
		return ""
	case cookiePath(webURL) == "/":
		return hostPrefix
	default:
		return "__Secure-"
	}
}

// SessionCookieName is the name of the dashboard session cookie served for
// webURL.
func SessionCookieName(webURL *url.URL) string { return cookiePrefix(webURL) + sessionCookieBase }

func loginCookieName(webURL *url.URL) string { return cookiePrefix(webURL) + loginCookieBase }

// newCookie is a cookie with the attributes every auth cookie shares:
// HttpOnly, SameSite=Lax, and Secure unless the dashboard is served over
// plain http.
func newCookie(webURL *url.URL, name, value, path string) *http.Cookie {
	return &http.Cookie{
		Name: name, Value: value, Path: path,
		HttpOnly: true, Secure: webURL.Scheme != schemeHTTP, SameSite: http.SameSiteLaxMode,
	}
}

func sessionCookie(webURL *url.URL, token string, expires time.Time) *http.Cookie {
	c := newCookie(webURL, SessionCookieName(webURL), token, cookiePath(webURL))
	c.Expires = expires
	return c
}

func clearedCookie(webURL *url.URL) *http.Cookie {
	c := newCookie(webURL, SessionCookieName(webURL), "", cookiePath(webURL))
	c.MaxAge = -1
	return c
}

func loginCookie(webURL *url.URL, value string) *http.Cookie {
	c := newCookie(webURL, loginCookieName(webURL), value, loginCookiePath(webURL))
	c.MaxAge = int(store.LoginStateTTL / time.Second)
	return c
}

func clearedLoginCookie(webURL *url.URL) *http.Cookie {
	c := newCookie(webURL, loginCookieName(webURL), "", loginCookiePath(webURL))
	c.MaxAge = -1
	return c
}

func randomString() string {
	raw := make([]byte, 32)
	_, _ = rand.Read(raw) // never fails
	return base64.RawURLEncoding.EncodeToString(raw)
}

// errorCode is the only failure detail a client is shown.
type errorCode string

const (
	codeInternal            errorCode = "internal"
	codeUnauthenticated     errorCode = "unauthenticated"
	codeCSRF                errorCode = "csrf"
	codeUnknownSignIn       errorCode = "unknown_sign_in"
	codeProviderUnavailable errorCode = "provider_unavailable"
	codeInvalidState        errorCode = "invalid_state"
	codeSignInDenied        errorCode = "sign_in_denied"
	codeExchangeFailed      errorCode = "exchange_failed"
	codeMembershipFailed    errorCode = "membership_failed"
	codeTooManySignIns      errorCode = "too_many_sign_ins"
	codeNotAllowed          errorCode = "not_allowed"
	codeRoleMappingFailed   errorCode = "role_mapping_failed"
	codeInvalidCredentials  errorCode = "invalid_credentials"
	codeTooManyAttempts     errorCode = "too_many_attempts"
	codeInvalidRequest      errorCode = "invalid_request"
)

// errorBody is every JSON error the auth middleware returns.
type errorBody struct {
	Code errorCode `json:"code"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // a failed write has no one to report to
}
