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
	mux.HandleFunc("GET /api/v1/accounts/{forge}/{name}/audit", s.accountAdmin(s.listAccountAudit))
	mux.HandleFunc("GET /api/v1/admin/audit", s.admin(s.listAdminAudit))
}

// record writes an audit event for a write p made, in the write's own
// transaction, so the two commit or fail together. accountID is "" for an
// event that names no account. detail must never hold a secret.
func record(ctx context.Context, tx pgx.Tx, p *auth.Principal, accountID string, action AuditAction, target string, detail any) error {
	raw, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("webapi: audit detail: %w", err)
	}
	return store.InsertAudit(ctx, tx, store.AuditEntry{
		UserID: p.User.ID, AccountID: accountID, Action: string(action), Target: target, Detail: raw,
	})
}

// admin adapts h like account, and also requires p to administer the
// account.
func (s *Server) accountAdmin(h accountHandler) http.HandlerFunc {
	return s.account(func(w http.ResponseWriter, r *http.Request, t *accountScope) error {
		if !t.principal.Admin {
			return errForbidden
		}
		return h(w, r, t)
	})
}

func (s *Server) listAccountAudit(w http.ResponseWriter, r *http.Request, t *accountScope) error {
	return s.writeAudit(w, r, t.account.ID())
}

func (s *Server) listAdminAudit(w http.ResponseWriter, r *http.Request) error {
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
		slugs[file.Accounts[i].ID()] = file.Accounts[i].Slug()
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
