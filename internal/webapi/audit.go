package webapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/auth"
	"github.com/home-operations/kritik/internal/store"
)

// registerAudit mounts the audit log reads.
func (s *Server) registerAudit(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/tenants/{slug}/audit", s.admin(s.listTenantAudit))
	mux.HandleFunc("GET /api/v1/operator/audit", s.handler(s.listOperatorAudit))
}

// record writes an audit event for a write p made, in the write's own
// transaction, so the two commit or fail together. detail must never hold
// a secret.
func record(ctx context.Context, tx pgx.Tx, p *auth.Principal, tenantID *string, action AuditAction, target string, detail any) error {
	if !action.Valid() {
		return fmt.Errorf("webapi: audit action %q is not valid", action)
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("webapi: audit detail: %w", err)
	}
	e := store.AuditEntry{AccountID: p.Account.ID, Action: action.String(), Target: target, Detail: raw}
	if tenantID != nil {
		e.TenantID = *tenantID
	}
	return store.InsertAudit(ctx, tx, e)
}

// admin adapts h like tenant, and also requires p to administer the
// tenant.
func (s *Server) admin(h tenantHandler) http.HandlerFunc {
	return s.tenant(func(w http.ResponseWriter, r *http.Request, t *tenantScope) error {
		if !t.principal.CanAdmin(t.tenant.ID()) {
			return errForbidden
		}
		return h(w, r, t)
	})
}

func (s *Server) listTenantAudit(w http.ResponseWriter, r *http.Request, t *tenantScope) error {
	return s.writeAudit(w, r, t.tenant.ID())
}

func (s *Server) listOperatorAudit(w http.ResponseWriter, r *http.Request) error {
	if !auth.PrincipalFrom(r.Context()).Operator {
		return errOperatorOnly
	}
	return s.writeAudit(w, r, "")
}

func (s *Server) writeAudit(w http.ResponseWriter, r *http.Request, tenantID string) error {
	page, err := parsePage(r)
	if err != nil {
		return err
	}
	events, next, err := s.store.ListAudit(r.Context(), tenantID, page)
	if err != nil {
		return err
	}
	slugs := map[string]string{}
	file := s.current.Get()
	for i := range file.Tenants {
		slugs[file.Tenants[i].ID()] = file.Tenants[i].Slug
	}
	items := make([]AuditEvent, len(events))
	for i, e := range events {
		items[i] = AuditEvent{
			ID: fmt.Sprint(e.ID), At: e.At, Tenant: slugs[e.TenantID], Action: AuditAction(e.Action), Target: e.Target, Detail: e.Detail,
		}
		if a := e.Actor; a != nil {
			items[i].Actor = &Account{ID: a.ID, DisplayName: a.DisplayName, Email: a.Email, AvatarURL: a.AvatarURL}
		}
	}
	writeJSON(w, http.StatusOK, newPage(items, next))
	return nil
}
