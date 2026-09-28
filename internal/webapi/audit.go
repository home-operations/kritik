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
	mux.HandleFunc("GET /api/v1/accounts/{slug}/audit", s.admin(s.listAccountAudit))
	mux.HandleFunc("GET /api/v1/operator/audit", s.handler(s.listOperatorAudit))
}

// record writes an audit event for a write p made, in the write's own
// transaction, so the two commit or fail together. detail must never hold
// a secret.
func record(ctx context.Context, tx pgx.Tx, p *auth.Principal, accountID *string, action AuditAction, target string, detail any) error {
	if !action.Valid() {
		return fmt.Errorf("webapi: audit action %q is not valid", action)
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("webapi: audit detail: %w", err)
	}
	e := store.AuditEntry{UserID: p.User.ID, Action: action.String(), Target: target, Detail: raw}
	if accountID != nil {
		e.AccountID = *accountID
	}
	return store.InsertAudit(ctx, tx, e)
}

// admin adapts h like account, and also requires p to administer the
// account.
func (s *Server) admin(h accountHandler) http.HandlerFunc {
	return s.account(func(w http.ResponseWriter, r *http.Request, t *accountScope) error {
		if !t.principal.Operator {
			return errForbidden
		}
		return h(w, r, t)
	})
}

func (s *Server) listAccountAudit(w http.ResponseWriter, r *http.Request, t *accountScope) error {
	return s.writeAudit(w, r, t.account.ID())
}

func (s *Server) listOperatorAudit(w http.ResponseWriter, r *http.Request) error {
	if !auth.PrincipalFrom(r.Context()).Operator {
		return errForbidden
	}
	return s.writeAudit(w, r, "")
}

func (s *Server) writeAudit(w http.ResponseWriter, r *http.Request, accountID string) error {
	page, err := parsePage(r)
	if err != nil {
		return err
	}
	events, next, err := s.store.ListAudit(r.Context(), accountID, page)
	if err != nil {
		return err
	}
	slugs := map[string]string{}
	file := s.current.Get()
	for i := range file.Accounts {
		slugs[file.Accounts[i].ID()] = file.Accounts[i].Slug
	}
	items := make([]AuditEvent, len(events))
	for i, e := range events {
		items[i] = AuditEvent{
			ID: fmt.Sprint(e.ID), At: e.At, Account: slugs[e.AccountID], Action: AuditAction(e.Action), Target: e.Target, Detail: e.Detail,
		}
		if e.Actor != nil {
			a := toUser(*e.Actor)
			items[i].Actor = &a
		}
	}
	writeJSON(w, http.StatusOK, newPage(items, next))
	return nil
}
