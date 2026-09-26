package webapi

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/auth"
	"github.com/home-operations/kritik/internal/configfile"
)

// tenantScope is a request resolved to one tenant its principal may read.
type tenantScope struct {
	file      *configfile.File
	tenant    *configfile.Tenant
	principal *auth.Principal
}

// role is the principal's effective role on the tenant; an operator is an
// admin of every tenant.
func (t *tenantScope) role() auth.Role { return roleOn(t.principal, t.tenant.ID()) }

func roleOn(p *auth.Principal, tenantID string) auth.Role {
	if p.Operator {
		return auth.RoleAdmin
	}
	return p.Memberships[tenantID]
}

// resolveTenant finds the tenant named slug in the current file for p. A
// tenant p may not read is reported exactly like one that does not exist,
// so the API never confirms a slug to someone outside it.
func resolveTenant(file *configfile.File, p *auth.Principal, slug string) (*tenantScope, error) {
	t, ok := file.Tenant(slug)
	if !ok || !p.CanRead(t.ID()) {
		return nil, errNotFound("tenant")
	}
	return &tenantScope{file: file, tenant: t, principal: p}, nil
}

// tenantHandler serves one /api/v1/tenants/{slug}/... route.
type tenantHandler func(w http.ResponseWriter, r *http.Request, t *tenantScope) error

// tenant adapts h: it resolves {slug} for the request's principal and
// writes any error h returns.
func (s *Server) tenant(h tenantHandler) http.HandlerFunc {
	return s.handler(func(w http.ResponseWriter, r *http.Request) error {
		t, err := resolveTenant(s.current.Get(), auth.PrincipalFrom(r.Context()), r.PathValue("slug"))
		if err != nil {
			return err
		}
		return h(w, r, t)
	})
}

// handler adapts a handler that returns its error.
func (s *Server) handler(h func(w http.ResponseWriter, r *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := h(w, r); err != nil {
			writeError(w, r, s.logger, err)
		}
	}
}

// read runs fn in a transaction scoped to t's tenant, so row-level
// security confines every query in it.
func (s *Server) read(ctx context.Context, t *tenantScope, fn func(pgx.Tx) error) error {
	return s.store.WithTenant(ctx, t.tenant.ID(), fn)
}
