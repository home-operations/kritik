package auth

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/store"
)

// User is a human who has signed in to the dashboard.
type User = store.User

// Principal is who an authenticated request acts as, and what the grant
// their sign-in decided lets them do.
type Principal struct {
	User     User
	Identity Identity
	// Operator administers the instance: the admin role, which reads and
	// changes everything.
	Operator bool
	// AllTenants is a member who reads every tenant.
	AllTenants bool
	// Tenants are the tenants a member reads, by id: those among the
	// current file's whose connections serve an account the grant names.
	Tenants map[string]bool
}

// CanRead reports whether p may read the tenant's content.
func (p *Principal) CanRead(tenantID string) bool {
	return p != nil && (p.Operator || p.AllTenants || p.Tenants[tenantID])
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
		c, err := r.Cookie(SessionCookieName(h.webURL))
		if err != nil || c.Value == "" {
			next.ServeHTTP(w, r)
			return
		}
		ctx := r.Context()
		sess, err := h.store.LookupSession(ctx, c.Value, h.now())
		if errors.Is(err, store.ErrSession) {
			next.ServeHTTP(w, r)
			return
		}
		if err != nil {
			h.logger.ErrorContext(ctx, "auth: look up session", "error", err)
			writeJSON(w, http.StatusInternalServerError, errorBody{Code: codeInternal})
			return
		}
		file := h.current.Get()
		if !honoured(file.Auth, sess) {
			next.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(ctx, principalFor(file, sess))))
	})
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
		Operator: g.Role == RoleAdmin, AllTenants: g.AllAccounts, Tenants: map[string]bool{},
	}
	if p.Operator || p.AllTenants {
		return p
	}
	accounts := map[string]bool{}
	for _, a := range g.Accounts {
		accounts[a] = true
	}
	for i := range file.Tenants {
		t := &file.Tenants[i]
		for _, in := range t.Connections {
			for _, a := range in.Accounts {
				if accounts[AccountKey(in.Forge, a)] {
					p.Tenants[t.ID()] = true
				}
			}
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

// SameOrigin rejects a state-changing request that does not carry
// X-Kritik: 1 and come from the dashboard's own origin (ADR-0009 §2.7). A
// cross-site form cannot set a custom header, and a cross-site script that
// does must pass a CORS preflight kritik never grants.
func (h *Handler) SameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		// An empty origin never matches, even an absent Origin header.
		originOK := h.origin != "" && normalizeOrigin(r.Header.Get("Origin")) == h.origin
		sameOrigin := originOK || r.Header.Get("Sec-Fetch-Site") == "same-origin"
		if r.Header.Get("X-Kritik") != "1" || !sameOrigin {
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
