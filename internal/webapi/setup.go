package webapi

import (
	"net/http"
	"strings"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/store"
)

// How far the instance is from reviewing, for the Configuration page's
// checklist (ADR-0019 §2.2), and re-reading the repositories a
// connection's App reaches.

func (s *Server) registerSetup(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/admin/setup", s.admin(s.getSetup))
	mux.HandleFunc("POST /api/v1/admin/connections/{name}/repositories", s.admin(s.registerReached))
}

func (s *Server) getSetup(w http.ResponseWriter, r *http.Request) error {
	out := setupStatus(s.current.Get(), s.webURL.String())
	if err := s.configError(); err != nil {
		out.ConfigError = err.Error()
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// setupStatus is f's distance from reviewing, for the dashboard at webURL.
func setupStatus(f *configfile.File, webURL string) SetupStatus {
	base := strings.TrimSuffix(webURL, "/")
	out := SetupStatus{WebURL: base, HooksURL: base + "/hooks/", Connections: []string{}}
	for _, in := range f.Connections {
		out.Connections = append(out.Connections, in.Name)
	}
	if ref := f.Defaults.Models.Review; ref != nil {
		out.ReviewModel = string(*ref)
	}
	out.Embedding = f.Embedding != nil
	return out
}

// registerReached records every repository connection r names reaches on
// an account it serves, so the leader polls them and, with an embedder,
// indexes them before any webhook names them.
func (s *Server) registerReached(w http.ResponseWriter, r *http.Request) error {
	in, app, err := s.connectionApp(r)
	if err != nil {
		return err
	}
	accounts, err := app.Reach(r.Context(), in.Accounts)
	if err != nil {
		return errForge(err)
	}
	f := s.current.Get()
	var res RegisterResult
	for _, a := range accounts {
		acct, ok := f.Account(in.Forge, a.Account)
		if !ok || len(a.Repositories) == 0 {
			continue
		}
		repos := make([]store.ReachedRepository, 0, len(a.Repositories))
		for _, x := range a.Repositories {
			repos = append(repos, store.ReachedRepository{
				FullName: x.FullName, DefaultBranch: x.DefaultBranch, Traits: &configfile.RepoTraits{Archived: x.Archived, Fork: x.Fork},
			})
		}
		added, err := s.store.RegisterRepositories(r.Context(), acct.ID(), repos)
		if err != nil {
			return err
		}
		res.Added += added
	}
	writeJSON(w, http.StatusOK, res)
	return nil
}
