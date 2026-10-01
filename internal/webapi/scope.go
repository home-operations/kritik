package webapi

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritika/internal/auth"
	"github.com/home-operations/kritika/internal/configfile"
)

// accountScope is a request resolved to one account its principal may read.
type accountScope struct {
	file      *configfile.File
	account   *configfile.Account
	principal *auth.Principal
}

// resolveAccount finds the running account name on forge for p. An account
// p may not read is reported exactly like one that does not exist, so the
// API never confirms an account to someone outside it.
func resolveAccount(file *configfile.File, p *auth.Principal, forge, name string) (*accountScope, error) {
	t, ok := file.Account(configfile.Forge(forge), name)
	if !ok || !p.CanRead(t.ID()) {
		return nil, errNotFound("account")
	}
	return &accountScope{file: file, account: t, principal: p}, nil
}

// accountHandler serves one /api/v1/accounts/{forge}/{name}/... route.
type accountHandler func(w http.ResponseWriter, r *http.Request, t *accountScope) error

// account adapts h: it resolves {forge}/{name} for the request's principal
// and writes any error h returns.
func (s *Server) account(h accountHandler) http.HandlerFunc {
	return s.handler(func(w http.ResponseWriter, r *http.Request) error {
		t, err := resolveAccount(s.current.Get(), auth.PrincipalFrom(r.Context()), r.PathValue("forge"), r.PathValue("name"))
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

// admin adapts a handler only an admin may call: to anyone else its route
// does not exist.
func (s *Server) admin(h func(w http.ResponseWriter, r *http.Request) error) http.HandlerFunc {
	return s.handler(func(w http.ResponseWriter, r *http.Request) error {
		if !auth.PrincipalFrom(r.Context()).Admin {
			return errNotFound("route")
		}
		return h(w, r)
	})
}

// read runs fn in a transaction scoped to t's account, so row-level
// security confines every query in it.
func (s *Server) read(ctx context.Context, t *accountScope, fn func(pgx.Tx) error) error {
	return s.store.WithAccount(ctx, t.account.ID(), fn)
}
