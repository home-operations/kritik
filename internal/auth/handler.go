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

// CookieName is the dashboard session cookie.
const CookieName = "kritik_session"

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
}

// New builds a Handler from c.
func New(c Config) *Handler {
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
		origin:    strings.ToLower(c.WebURL.Scheme + "://" + c.WebURL.Host),
		now:       c.Now,
		logger:    c.Logger,
		providers: newProviders(c.WebURL, c.HTTPClient, c.Now),
	}
}

// Register mounts the sign-in routes on mux, relative to the dashboard's
// root; the caller strips any base path.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/providers", h.listProviders)
	mux.HandleFunc("GET /auth/login/{name}", h.login)
	mux.HandleFunc("GET /auth/callback/{name}", h.callback)
	mux.Handle("POST /auth/logout", h.SameOrigin(http.HandlerFunc(h.logout)))
}

// ProviderInfo is one sign-in as the sign-in page lists it.
type ProviderInfo struct {
	Name        string                `json:"name"`
	Type        configfile.SignInType `json:"type"`
	DisplayName string                `json:"displayName"`
}

func (h *Handler) listProviders(w http.ResponseWriter, _ *http.Request) {
	signIns := h.current.Get().Web.SignIn
	out := make([]ProviderInfo, 0, len(signIns))
	for _, s := range signIns {
		out = append(out, ProviderInfo{Name: s.Name, Type: s.Type, DisplayName: displayName(s)})
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	p, _, err := h.providers.get(r.Context(), h.current.Get().Web, name)
	if errors.Is(err, ErrUnknownProvider) {
		h.fail(w, r, http.StatusNotFound, codeUnknownSignIn, nil)
		return
	}
	if err != nil {
		h.fail(w, r, http.StatusBadGateway, codeProviderUnavailable, err)
		return
	}
	nonce, err := randomString()
	if err != nil {
		h.fail(w, r, http.StatusInternalServerError, codeInternal, err)
		return
	}
	ls := store.LoginState{
		Provider: name, Nonce: nonce, PKCEVerifier: oauth2.GenerateVerifier(),
		ReturnTo: returnTo(r.URL.Query().Get("return_to")),
	}
	state, err := h.store.CreateLoginState(r.Context(), ls, h.now())
	if err != nil {
		h.fail(w, r, http.StatusInternalServerError, codeInternal, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, p.AuthCodeURL(state, ls.Nonce, ls.PKCEVerifier), http.StatusFound)
}

// callback completes a sign-in: it consumes the state the login left,
// trades the code for an identity, refreshes the account and its forge
// memberships, accepts pending invites for a verified email, and starts a
// session.
func (h *Handler) callback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name, q := r.PathValue("name"), r.URL.Query()
	ls, err := h.store.ConsumeLoginState(ctx, q.Get("state"), h.now())
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
	p, signIn, err := h.providers.get(ctx, file.Web, name)
	if errors.Is(err, ErrUnknownProvider) {
		h.fail(w, r, http.StatusNotFound, codeUnknownSignIn, nil)
		return
	}
	if err != nil {
		h.fail(w, r, http.StatusBadGateway, codeProviderUnavailable, err)
		return
	}
	id, m, err := p.Exchange(ctx, q.Get("code"), ls.PKCEVerifier, ls.Nonce)
	if err != nil {
		h.fail(w, r, http.StatusBadGateway, codeExchangeFailed, err)
		return
	}
	grants, err := Resolve(ctx, file, signIn, id, m)
	if err != nil {
		h.fail(w, r, http.StatusBadGateway, codeMembershipFailed, err)
		return
	}
	token, expires, err := h.startSession(r, id, grants, file.Web.SessionTTLOrDefault())
	if err != nil {
		h.fail(w, r, http.StatusInternalServerError, codeInternal, err)
		return
	}
	http.SetCookie(w, sessionCookie(h.webURL, token, expires))
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, h.home()+returnTo(ls.ReturnTo), http.StatusFound)
}

// startSession records the signed-in identity and its memberships and
// returns a new session's cookie value and expiry.
func (h *Handler) startSession(r *http.Request, id Identity, grants []Grant, ttl time.Duration) (string, time.Time, error) {
	ctx, now := r.Context(), h.now()
	acct, err := h.store.UpsertIdentity(ctx, store.SignInIdentity(id), now)
	if err != nil {
		return "", time.Time{}, err
	}
	if err := h.store.ReplaceForgeMemberships(ctx, acct.ID, grants, now); err != nil {
		return "", time.Time{}, err
	}
	if id.EmailVerified {
		n, err := h.store.AcceptInvites(ctx, acct.ID, id.Email, now)
		if err != nil {
			return "", time.Time{}, err
		}
		if n > 0 {
			h.logger.InfoContext(ctx, "auth: accepted invites", "account", acct.ID, "count", n)
		}
	}
	expires := now.Add(ttl)
	token, err := h.store.CreateSession(ctx, acct.ID, id.Provider, now, expires)
	if err != nil {
		return "", time.Time{}, err
	}
	h.logger.InfoContext(ctx, "auth: signed in", "account", acct.ID, "sign_in", id.Provider, "login", id.Login, "tenants", len(grants))
	return token, expires, nil
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(CookieName); err == nil && c.Value != "" {
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

// cookiePath scopes the session cookie to the dashboard.
func cookiePath(webURL *url.URL) string {
	if webURL.Path == "" {
		return "/"
	}
	return webURL.Path
}

func sessionCookie(webURL *url.URL, token string, expires time.Time) *http.Cookie {
	return &http.Cookie{
		Name: CookieName, Value: token, Path: cookiePath(webURL), Expires: expires,
		HttpOnly: true, Secure: webURL.Scheme != "http", SameSite: http.SameSiteLaxMode,
	}
}

func clearedCookie(webURL *url.URL) *http.Cookie {
	return &http.Cookie{
		Name: CookieName, Path: cookiePath(webURL), MaxAge: -1,
		HttpOnly: true, Secure: webURL.Scheme != "http", SameSite: http.SameSiteLaxMode,
	}
}

func randomString() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("auth: random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
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
