package webapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/model"
	"github.com/home-operations/kritik/internal/store"
)

// What the first-run wizard needs beyond the settings routes (ADR-0014
// §2.6): how far the instance is from reviewing, a test of a model key or
// an embedder before it is saved, and the repositories a connection's App
// reaches.

// probeTimeout bounds one test call to a provider.
const probeTimeout = 20 * time.Second

func (s *Server) registerSetup(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/admin/setup", s.admin(s.getSetup))
	mux.HandleFunc("POST /api/v1/admin/providers/test", s.admin(s.testProvider))
	mux.HandleFunc("POST /api/v1/admin/embedding/test", s.admin(s.testEmbedding))
	mux.HandleFunc("GET /api/v1/admin/connections/{name}/repositories", s.admin(s.listReached))
	mux.HandleFunc("POST /api/v1/admin/connections/{name}/repositories", s.admin(s.registerReached))
}

func (s *Server) getSetup(w http.ResponseWriter, r *http.Request) error {
	writeJSON(w, http.StatusOK, setupStatus(s.current.Get(), s.webURL.String()))
	return nil
}

// setupStatus is f's distance from reviewing, for the dashboard at webURL.
func setupStatus(f *configfile.File, webURL string) SetupStatus {
	base := strings.TrimSuffix(webURL, "/")
	out := SetupStatus{WebURL: base, HooksURL: base + "/hooks/", FileConnections: []string{}, Connections: []string{}}
	for _, in := range f.FileConnections() {
		out.FileConnections = append(out.FileConnections, in.Name)
	}
	for _, in := range f.Connections {
		out.Connections = append(out.Connections, in.Name)
	}
	if ref := f.Defaults.Models.Review; ref != nil {
		out.ReviewModel = string(*ref)
	}
	out.Embedding = f.Embedding != nil
	return out
}

// testKey is the key a test request names: a value typed, or, for
// {"keep": true}, the running one held, which is kept only while the
// request's endpoint is the one held's.
func testKey(raw json.RawMessage, held string, sameEndpoint bool) (string, error) {
	var in struct {
		Value string `json:"value"`
		Keep  bool   `json:"keep"`
	}
	refuse := func(msg string) error {
		return errStatus(http.StatusUnprocessableEntity, CodeInvalidSpec, msg, pathDetails{Path: apiKeyKey})
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return "", refuse(`apiKey must be {"value": "..."} or {"keep": true}`)
	}
	switch {
	case in.Value != "":
		return in.Value, nil
	case !in.Keep:
		return "", refuse("enter the key to test")
	case held == "":
		return "", refuse("there is no stored key to test")
	case !sameEndpoint:
		return "", errStatus(http.StatusUnprocessableEntity, CodeReenterSecret, "the endpoint changed; enter the key again",
			pathDetails{Path: apiKeyKey})
	}
	return held, nil
}

// endpointOf normalises a base URL as keeping a key compares it.
func endpointOf(baseURL string) string {
	return strings.TrimRight(strings.ToLower(strings.TrimSpace(baseURL)), "/")
}

func (s *Server) testProvider(w http.ResponseWriter, r *http.Request) error {
	var req ProviderTestRequest
	if err := readBody(r, &req); err != nil {
		return err
	}
	if !req.Type.Valid() {
		return errStatus(http.StatusUnprocessableEntity, CodeInvalidSpec, "type must be openrouter, openai or anthropic",
			pathDetails{Path: "type"})
	}
	var held configfile.Provider
	if req.Name != "" {
		f := s.current.Get()
		var acct *configfile.Account
		if forge, name, ok := strings.Cut(req.Account, "/"); ok {
			acct, _ = f.Account(configfile.Forge(forge), name)
		}
		held, _ = f.Provider(acct, req.Name)
	}
	key, err := testKey(req.APIKey, held.APIKeyValue().Value(),
		held.Type == req.Type && endpointOf(held.BaseURL) == endpointOf(req.BaseURL))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()
	models, err := model.Probe(ctx, req.Type, req.BaseURL, key, nil)
	res := TestResult{OK: err == nil, Models: models}
	if err != nil {
		res.Error = err.Error()
	}
	writeJSON(w, http.StatusOK, res)
	return nil
}

func (s *Server) testEmbedding(w http.ResponseWriter, r *http.Request) error {
	var req EmbeddingTestRequest
	if err := readBody(r, &req); err != nil {
		return err
	}
	switch {
	case strings.TrimSpace(req.BaseURL) == "" || strings.TrimSpace(req.Model) == "":
		return errStatus(http.StatusUnprocessableEntity, CodeInvalidSpec, "an endpoint and a model are required", nil)
	case req.Dims <= 0 || req.Dims > configfile.MaxEmbedDims:
		return errStatus(http.StatusUnprocessableEntity, CodeInvalidSpec, fmt.Sprintf("dims must be between 1 and %d", configfile.MaxEmbedDims),
			pathDetails{Path: "dims"})
	}
	var held string
	var sameEndpoint bool
	if e := s.current.Get().Embedding; e != nil {
		held, sameEndpoint = e.APIKeyValue().Value(), endpointOf(e.BaseURL) == endpointOf(req.BaseURL)
	}
	key, err := testKey(req.APIKey, held, sameEndpoint)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()
	_, _, err = model.NewOpenAIEmbedder(req.BaseURL, key, req.Model, req.Dims).Embed(ctx, []string{"kritik"})
	res := TestResult{OK: err == nil}
	if err != nil {
		res.Error = err.Error()
	}
	writeJSON(w, http.StatusOK, res)
	return nil
}

// reached lists, for each account connection r names serves, whether its
// App is installed there and the repositories it reaches.
func (s *Server) reached(r *http.Request) (*configfile.Connection, []AccountRepositories, error) {
	in, app, err := s.connectionApp(r)
	if err != nil {
		return nil, nil, err
	}
	insts, err := app.Installations(r.Context())
	if err != nil {
		return nil, nil, errForge(err)
	}
	out := make([]AccountRepositories, 0, len(in.Accounts))
	for _, account := range in.Accounts {
		entry := AccountRepositories{Account: account, Repositories: []AppRepository{}}
		for _, inst := range insts {
			if !strings.EqualFold(inst.Account, account) {
				continue
			}
			repos, err := app.Repositories(r.Context(), inst.ID)
			if err != nil {
				return nil, nil, errForge(err)
			}
			entry.Installed = true
			for _, x := range repos {
				entry.Repositories = append(entry.Repositories, AppRepository{
					Name: x.Name, FullName: x.FullName, DefaultBranch: x.DefaultBranch, Archived: x.Archived, Fork: x.Fork,
				})
			}
		}
		out = append(out, entry)
	}
	return in, out, nil
}

func (s *Server) listReached(w http.ResponseWriter, r *http.Request) error {
	_, out, err := s.reached(r)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// registerReached records every repository connection r names reaches on
// an account it serves, so the leader polls them and, with an embedder,
// indexes them before any webhook names them.
func (s *Server) registerReached(w http.ResponseWriter, r *http.Request) error {
	in, accounts, err := s.reached(r)
	if err != nil {
		return err
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
