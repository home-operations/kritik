package auth

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/home-operations/kritika/internal/configfile"
	"github.com/home-operations/kritika/internal/store"
)

// User is a human who has signed in to the dashboard.
type User = store.User

// Principal is who an authenticated request acts as, and what the grant
// their sign-in decided lets them do.
type Principal struct {
	User     User
	Identity Identity
	// Admin administers the instance: it reads and changes every account
	// and the instance's configuration.
	Admin bool
	// AllAccounts is a member who reads every account.
	AllAccounts bool
	// Accounts are the accounts a member reads, by id: those among the
	// current file's whose connections serve an account the grant names.
	Accounts map[string]bool
}

// CanRead reports whether p may read the account's content.
func (p *Principal) CanRead(accountID string) bool {
	return p != nil && (p.Admin || p.AllAccounts || p.Accounts[accountID])
}

type principalKey struct{}

// WithPrincipal returns ctx acting as p, for a caller that authenticated
// the request some other way than Authenticate, such as a handler test.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the request's principal, nil when it is not signed
// in.
func PrincipalFrom(ctx context.Context) *Principal {
	p, _ := ctx.Value(principalKey{}).(*Principal)
	return p
}

// Authenticate resolves the session cookie, if any, to a Principal on the
// request's context. A request without a valid session passes through
// unauthenticated; RequirePrincipal is what rejects it. So does a session
// whose sign-in the file no longer configures, now points at another
// issuer, or has had its role configuration changed since the session
// began.
func (h *Handler) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		file := h.current.Get()
		sess, ok, err := h.session(r, file)
		if err != nil {
			h.logger.ErrorContext(ctx, "auth: look up session", "error", err)
			writeJSON(w, http.StatusInternalServerError, errorBody{Code: codeInternal})
			return
		}
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(ctx, principalFor(file, sess))))
	})
}

// session resolves r's session cookie to the session it names, false when
// there is none, it has expired or been signed out, or file no longer
// honours it.
func (h *Handler) session(r *http.Request, file *configfile.File) (store.Session, bool, error) {
	c, err := r.Cookie(SessionCookieName(h.webURL))
	if err != nil || c.Value == "" {
		return store.Session{}, false, nil
	}
	sess, err := h.store.LookupSession(r.Context(), c.Value, h.now())
	if errors.Is(err, store.ErrSession) {
		return store.Session{}, false, nil
	}
	if err != nil {
		return store.Session{}, false, err
	}
	return sess, honoured(file.Auth, sess), nil
}

// Stands reports whether the session r was authenticated with still
// stands, for a request that outlives its authentication, as an event
// stream does. A lookup the database fails counts as standing: a stream
// is not ended over a failover.
func (h *Handler) Stands(r *http.Request) bool {
	_, ok, err := h.session(r, h.current.Get())
	if err != nil {
		h.logger.WarnContext(r.Context(), "auth: look up session", "error", err)
		return true
	}
	return ok
}

// honoured reports whether a session still stands under auth: its sign-in
// is configured, points where it did, and would decide grants as it did.
func honoured(auth configfile.Auth, sess store.Session) bool {
	provider := sess.Identity.Provider
	origin := localOrigin
	if provider != string(configfile.SignInLocal) {
		s, ok := auth.SignInByType(configfile.SignInType(provider))
		if !ok {
			return false
		}
		origin = signInOrigin(s)
	}
	key, ok := GrantKey(auth, provider)
	return ok && origin == sess.Identity.Origin && key == sess.Grant.Key
}

// principalFor builds the principal a session acts as under file.
func principalFor(file *configfile.File, sess store.Session) *Principal {
	g := sess.Grant
	p := &Principal{
		User: sess.User, Identity: Identity(sess.Identity),
		Admin: g.Role == RoleAdmin, AllAccounts: g.AllAccounts, Accounts: map[string]bool{},
	}
	if p.Admin || p.AllAccounts {
		return p
	}
	granted := map[string]bool{}
	for _, key := range g.Accounts {
		granted[key] = true
	}
	for i := range file.Accounts {
		if a := &file.Accounts[i]; granted[a.Key()] {
			p.Accounts[a.ID()] = true
		}
	}
	return p
}

// RequirePrincipal rejects a request Authenticate found no principal for.
func (h *Handler) RequirePrincipal(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if PrincipalFrom(r.Context()) == nil {
			writeJSON(w, http.StatusUnauthorized, errorBody{Code: codeUnauthenticated})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// SameOrigin rejects a state-changing request that does not carry X-Kritika:
// 1 and come from the dashboard's own origin. A cross-site form cannot set
// a custom header, and a cross-site script that does must pass a CORS
// preflight kritika never grants.
func (h *Handler) SameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		// An empty origin never matches, even an absent Origin header.
		originOK := h.origin != "" && normalizeOrigin(r.Header.Get("Origin")) == h.origin
		sameOrigin := originOK || r.Header.Get("Sec-Fetch-Site") == "same-origin"
		if r.Header.Get("X-Kritika") != "1" || !sameOrigin {
			writeJSON(w, http.StatusForbidden, errorBody{Code: codeCSRF})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// normalizeOrigin lowercases an origin's scheme and host and drops a default
// port, so https://host:443 and https://host compare equal; anything that
// is not an http or https origin normalises to "".
func normalizeOrigin(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return ""
	}
	scheme, host, port := strings.ToLower(u.Scheme), strings.ToLower(u.Hostname()), u.Port()
	if (scheme == schemeHTTPS && port == "443") || (scheme == schemeHTTP && port == "80") {
		port = ""
	}
	if scheme != schemeHTTPS && scheme != schemeHTTP {
		return ""
	}
	if port != "" {
		return scheme + "://" + net.JoinHostPort(host, port)
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return scheme + "://" + host
}
