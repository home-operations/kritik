package webapi

import (
	"net/http"
	"slices"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/auth"
	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/forge/github"
	"github.com/home-operations/kritik/internal/store"
)

// registerConnections mounts the admin's view of the running connections
// and of the accounts each one's GitHub App is installed on, where an
// installation no connection serves can be removed.
func (s *Server) registerConnections(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/admin/connections", s.admin(s.listConnections))
	mux.HandleFunc("GET /api/v1/admin/connections/{name}/installations", s.admin(s.listInstallations))
	mux.HandleFunc("DELETE /api/v1/admin/connections/{name}/installations/{id}", s.admin(s.uninstall))
}

func (s *Server) listConnections(w http.ResponseWriter, r *http.Request) error {
	var webhooks map[string]store.WebhookDeliveries
	if err := s.store.WithAccount(r.Context(), "", func(tx pgx.Tx) error {
		var err error
		webhooks, err = store.ReadWebhookDeliveries(r.Context(), tx)
		return err
	}); err != nil {
		return err
	}
	f := s.current.Get()
	out := make([]Connection, 0, len(f.Connections))
	for i := range f.Connections {
		in := &f.Connections[i]
		c := connection(in)
		c.delivered(webhooks[in.ID()])
		out = append(out, c)
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// connectionApp is the running connection r names and its App.
func (s *Server) connectionApp(r *http.Request) (*configfile.Connection, *github.App, error) {
	in, ok := s.current.Get().Connection(r.PathValue("name"))
	if !ok {
		return nil, nil, errNotFound("connection")
	}
	app, err := github.NewApp(in.App.ClientIDValue(), in.App.PrivateKeyValue().Value(), s.githubAPI)
	if err != nil {
		return nil, nil, errForge(err)
	}
	return in, app, nil
}

// errForge is GitHub failing or refusing a call the admin asked for.
func errForge(err error) error {
	return errStatus(http.StatusBadGateway, CodeForgeError, err.Error())
}

func (s *Server) listInstallations(w http.ResponseWriter, r *http.Request) error {
	in, app, err := s.connectionApp(r)
	if err != nil {
		return err
	}
	insts, err := app.Installations(r.Context())
	if err != nil {
		return errForge(err)
	}
	out := make([]AppInstallation, 0, len(insts))
	for _, x := range insts {
		out = append(out, AppInstallation{
			ID: x.ID, Account: x.Account, AccountType: x.AccountType, AllRepositories: x.AllRepositories,
			Suspended: x.Suspended, Served: in.Serves(x.Account), URL: x.HTMLURL,
		})
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// uninstall removes an installation of a connection's App from an account
// the connection does not serve, audited in the same transaction.
func (s *Server) uninstall(w http.ResponseWriter, r *http.Request) error {
	p := auth.PrincipalFrom(r.Context())
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return errNotFound("installation")
	}
	in, app, err := s.connectionApp(r)
	if err != nil {
		return err
	}
	insts, err := app.Installations(r.Context())
	if err != nil {
		return errForge(err)
	}
	i := slices.IndexFunc(insts, func(x github.Installation) bool { return x.ID == id })
	if i < 0 {
		return errNotFound("installation")
	}
	inst := &insts[i]
	if in.Serves(inst.Account) {
		return errStatus(http.StatusConflict, CodeInstallationServed,
			"connection "+in.Name+" serves "+inst.Account+": remove the account from its accounts first")
	}
	err = s.store.WithAccount(r.Context(), "", func(tx pgx.Tx) error {
		detail := map[string]any{"installation": id}
		if err := record(r.Context(), tx, p, "", AuditAppUninstall, in.Name+"/"+inst.Account, detail); err != nil {
			return err
		}
		if err := app.Uninstall(r.Context(), id); err != nil {
			return errForge(err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// delivered sets when c's webhook last delivered a verified request and an
// unsigned one.
func (c *Connection) delivered(d store.WebhookDeliveries) {
	c.LastWebhookAt, c.LastUnsignedWebhookAt = d.Verified, d.Unsigned
}
