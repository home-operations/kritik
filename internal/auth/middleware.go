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

// Account is a human who has signed in to the dashboard.
type Account = store.Account

// Principal is who an authenticated request acts as.
type Principal struct {
	Account  Account
	Identity Identity
	// Operator is recomputed from the current file on every request, so
	// removing an operator takes effect at once.
	Operator bool
	// Memberships maps tenant id to role, limited to tenants the current
	// file declares.
	Memberships map[string]Role
}

// CanRead reports whether p may read the tenant's content.
func (p *Principal) CanRead(tenantID string) bool {
	if p == nil {
		return false
	}
	_, ok := p.Memberships[tenantID]
	return p.Operator || ok
}

// CanAdmin reports whether p may change the tenant.
func (p *Principal) CanAdmin(tenantID string) bool {
	if p == nil {
		return false
	}
	return p.Operator || p.Memberships[tenantID] == RoleAdmin
}

type principalKey struct{}

func withPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// WithPrincipal returns ctx acting as p, for a caller that authenticated
// the request some other way than Authenticate, such as a handler test.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return withPrincipal(ctx, p)
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
// whose sign-in the file no longer declares, or now points at another host
// or issuer.
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
		signIn, ok := file.Web.SignInByName(sess.Identity.Provider)
		if !ok || signInOrigin(signIn) != sess.Identity.Origin {
			next.ServeHTTP(w, r)
			return
		}
		roles, err := h.store.Memberships(ctx, sess.Account.ID)
		if err != nil {
			h.logger.ErrorContext(ctx, "auth: load memberships", "error", err)
			writeJSON(w, http.StatusInternalServerError, errorBody{Code: codeInternal})
			return
		}
		next.ServeHTTP(w, r.WithContext(withPrincipal(ctx, principalFor(file, sess, roles))))
	})
}

// principalFor builds the principal a session acts as under file.
func principalFor(file *configfile.File, sess store.Session, roles map[string]Role) *Principal {
	id := Identity(sess.Identity)
	p := &Principal{Account: sess.Account, Identity: id, Operator: IsOperator(file.Web, id), Memberships: map[string]Role{}}
	for i := range file.Tenants {
		tid := file.Tenants[i].ID()
		if r, ok := roles[tid]; ok && r.Valid() {
			p.Memberships[tid] = r
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
