package webapi

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/auth"
	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/store"
)

// recentIndexRuns is how many index runs a repository's detail lists.
const recentIndexRuns = 20

// registerReads mounts the read-only API.
func (s *Server) registerReads(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/me", s.handler(s.getMe))
	mux.HandleFunc("GET /api/v1/tenants", s.handler(s.listTenants))
	mux.HandleFunc("GET /api/v1/operator/tenants", s.handler(s.listOperatorTenants))
	mux.HandleFunc("GET /api/v1/tenants/{slug}", s.tenant(s.getTenant))
	mux.HandleFunc("GET /api/v1/tenants/{slug}/repos", s.tenant(s.listRepos))
	mux.HandleFunc("GET /api/v1/tenants/{slug}/repos/{owner}/{repo}", s.tenant(s.getRepo))
	mux.HandleFunc("GET /api/v1/tenants/{slug}/index-runs", s.tenant(s.listIndexRuns))
	mux.HandleFunc("GET /api/v1/tenants/{slug}/pulls", s.tenant(s.listPulls))
	mux.HandleFunc("GET /api/v1/tenants/{slug}/pulls/{owner}/{repo}/{number}", s.tenant(s.getPull))
	mux.HandleFunc("GET /api/v1/tenants/{slug}/followups", s.tenant(s.listFollowups))
	mux.HandleFunc("GET /api/v1/tenants/{slug}/followups/{commentId}/transcript", s.tenant(s.getFollowupTranscript))
	mux.HandleFunc("GET /api/v1/tenants/{slug}/reviews/{id}", s.tenant(s.getReview))
	mux.HandleFunc("GET /api/v1/tenants/{slug}/reviews/{id}/diff", s.tenant(s.getReviewDiff))
	mux.HandleFunc("GET /api/v1/tenants/{slug}/reviews/{id}/transcript", s.tenant(s.getReviewTranscript))
	mux.HandleFunc("GET /api/v1/tenants/{slug}/reviews/{id}/raw", s.tenant(s.getReviewRaw))
	mux.HandleFunc("GET /api/v1/tenants/{slug}/usage", s.tenant(s.getUsage))
	mux.HandleFunc("GET /api/v1/tenants/{slug}/queue", s.tenant(s.listQueue))
}

func (s *Server) getMe(w http.ResponseWriter, r *http.Request) error {
	p := auth.PrincipalFrom(r.Context())
	me := Me{
		Account:  account(p.Account),
		Operator: p.Operator, Tenants: []TenantMembership{},
	}
	for _, t := range readable(s.current.Get(), p) {
		me.Tenants = append(me.Tenants, TenantMembership{Slug: t.Slug, Role: roleOn(p, t.ID()), ManagedBy: t.Origin()})
	}
	writeJSON(w, http.StatusOK, me)
	return nil
}

// readable lists the file's tenants p may read, in file order.
func readable(file *configfile.File, p *auth.Principal) []*configfile.Tenant {
	var out []*configfile.Tenant
	for i := range file.Tenants {
		if p.CanRead(file.Tenants[i].ID()) {
			out = append(out, &file.Tenants[i])
		}
	}
	return out
}

func (s *Server) listTenants(w http.ResponseWriter, r *http.Request) error {
	p := auth.PrincipalFrom(r.Context())
	file := s.current.Get()
	out := []TenantSummary{}
	for _, t := range readable(file, p) {
		sum, err := s.tenantSummary(r.Context(), file, t, roleOn(p, t.ID()))
		if err != nil {
			return err
		}
		out = append(out, sum)
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) tenantSummary(ctx context.Context, file *configfile.File, t *configfile.Tenant, role auth.Role) (TenantSummary, error) {
	var stats store.TenantStats
	err := s.store.WithTenant(ctx, t.ID(), func(tx pgx.Tx) error {
		var err error
		stats, err = store.ReadTenantStats(ctx, tx)
		return err
	})
	if err != nil {
		return TenantSummary{}, err
	}
	return TenantSummary{
		Slug: t.Slug, ManagedBy: t.Origin(), Role: role, Installations: stats.Installations, Repositories: stats.Repositories,
		Reviews7d: stats.Reviews7d, Usage: monthUsage(stats.Month, file.Settings(t, "", "").Limits),
	}, nil
}

func monthUsage(m store.MonthUsage, l configfile.Limits) MonthUsage {
	return MonthUsage{
		Tokens: m.Tokens, CostUSD: m.CostUSD, TokensPerMonth: l.TokensPerMonth,
		ReviewsToday: m.ReviewsToday, ReviewsPerDay: l.ReviewsPerDay,
	}
}

// listOperatorTenants lists every tenant of the running configuration and
// every dashboard tenant stored but not part of it. It is reported as a
// missing route to anyone but an operator.
func (s *Server) listOperatorTenants(w http.ResponseWriter, r *http.Request) error {
	p := auth.PrincipalFrom(r.Context())
	if !p.Operator {
		return errNotFound("route")
	}
	ctx := r.Context()
	file := s.current.Get()
	stored, err := s.store.DashboardTenants(ctx)
	if err != nil {
		return err
	}
	revisions := map[string]int64{}
	for _, d := range stored {
		revisions[d.Slug] = d.Revision
	}
	out := []OperatorTenant{}
	for i := range file.Tenants {
		t := &file.Tenants[i]
		sum, err := s.tenantSummary(ctx, file, t, auth.RoleAdmin)
		if err != nil {
			return err
		}
		out = append(out, OperatorTenant{TenantSummary: sum, Live: true, Revision: revisions[t.Slug]})
		delete(revisions, t.Slug)
	}
	for _, d := range stored {
		if rev, ok := revisions[d.Slug]; ok {
			out = append(out, OperatorTenant{
				Slug: d.Slug, ManagedBy: configfile.OriginDashboard, Role: auth.RoleAdmin,
				Revision: rev,
			})
		}
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) getTenant(w http.ResponseWriter, r *http.Request, t *tenantScope) error {
	ctx := r.Context()
	var month store.MonthUsage
	if err := s.read(ctx, t, func(tx pgx.Tx) error {
		var err error
		month, err = store.ReadMonthUsage(ctx, tx)
		return err
	}); err != nil {
		return err
	}
	settings := t.file.Settings(t.tenant, "", "")
	d := TenantDetail{
		Slug: t.tenant.Slug, ManagedBy: t.tenant.Origin(), Role: t.role(), Installations: []Installation{},
		Models: models(settings.Models), Limits: limits(settings.Limits), Filter: filterSource(settings),
		Usage: monthUsage(month, settings.Limits),
	}
	for i := range t.tenant.Installations {
		d.Installations = append(d.Installations, installation(&t.tenant.Installations[i]))
	}
	writeJSON(w, http.StatusOK, d)
	return nil
}

func installation(in *configfile.Installation) Installation {
	out := Installation{
		Name: in.Name, Forge: in.Forge, Host: in.Host, Account: in.Account, CredentialKind: CredentialToken,
		HookPath: "/hooks/" + in.Name,
		Credentials: CredentialsSet{
			Token: in.TokenValue().Value() != "", GitToken: in.GitTokenValue().Value() != "",
			WebhookSecret: in.WebhookSecretValue().Value() != "",
		},
	}
	if in.App != nil {
		out.CredentialKind = CredentialApp
		out.Credentials.ClientID = in.App.ClientIDValue() != ""
		out.Credentials.PrivateKey = in.App.PrivateKeyValue().Value() != ""
	}
	return out
}

func models(m configfile.Models) Models { return Models{Review: m.Review, Fallback: m.Fallback} }

func limits(l configfile.Limits) Limits {
	return Limits{Concurrency: l.Concurrency, ReviewsPerDay: l.ReviewsPerDay, TokensPerMonth: l.TokensPerMonth}
}

func filterSource(s configfile.Settings) string {
	if s.Filter == nil {
		return ""
	}
	return s.Filter.Source()
}

func (s *Server) listRepos(w http.ResponseWriter, r *http.Request, t *tenantScope) error {
	page, err := parsePage(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	var rows []store.RepoRow
	var next *store.Cursor
	if err := s.read(ctx, t, func(tx pgx.Tx) error {
		rows, next, err = store.ListRepos(ctx, tx, page)
		return err
	}); err != nil {
		return err
	}
	items := make([]Repository, len(rows))
	for i, row := range rows {
		items[i] = repository(row)
	}
	writeJSON(w, http.StatusOK, newPage(items, next))
	return nil
}

func repository(r store.RepoRow) Repository {
	out := Repository{
		ID: r.ID, FullName: r.FullName, Installation: r.Installation, Enabled: r.Enabled, ManagedBy: r.ManagedBy,
		DefaultBranch: r.DefaultBranch,
		Index:         IndexState{ActiveCommit: r.ActiveCommit, ActiveAt: r.ActiveAt, LastRunStatus: r.LastIndexStatus, LastRunAt: r.LastIndexAt},
	}
	if r.LastReview != nil {
		out.LastReview = &ReviewRef{ID: r.LastReview.ID, Status: r.LastReview.Status, CreatedAt: r.LastReview.CreatedAt}
	}
	return out
}

// findRepo resolves {owner}/{repo}, and ?installation= when a tenant has
// the same repository under two installations.
func findRepo(ctx context.Context, tx pgx.Tx, r *http.Request) (store.RepoRow, error) {
	row, err := store.FindRepo(ctx, tx, r.PathValue("owner")+"/"+r.PathValue("repo"), r.URL.Query().Get("installation"))
	if errors.Is(err, store.ErrNotFound) {
		return row, errNotFound("repository")
	}
	return row, err
}

func (s *Server) getRepo(w http.ResponseWriter, r *http.Request, t *tenantScope) error {
	ctx := r.Context()
	var row store.RepoRow
	var runs []store.IndexRunRow
	if err := s.read(ctx, t, func(tx pgx.Tx) error {
		var err error
		if row, err = findRepo(ctx, tx, r); err != nil {
			return err
		}
		runs, _, err = store.ListIndexRuns(ctx, tx, row.ID, store.Page{Limit: recentIndexRuns})
		return err
	}); err != nil {
		return err
	}
	settings := t.file.Settings(t.tenant, row.Installation, row.FullName)
	d := RepoDetail{Repository: repository(row), Settings: repoSettings(settings), IndexRuns: indexRuns(runs)}
	writeJSON(w, http.StatusOK, d)
	return nil
}

func repoSettings(s configfile.Settings) RepoSettings {
	return RepoSettings{
		Enabled: s.Enabled, Mode: s.Mode, Models: models(s.Models), Filter: filterSource(s), Forks: s.Forks,
		Ignore: nonNil(slices.Clone(s.Ignore)), SettleSeconds: int64(s.Settle.Seconds()), MaxDeltaFiles: s.Incremental.MaxDeltaFiles,
		Review: ReviewBlock{Instructions: nonNil(s.Review.Instructions), RequireSuggestedFix: s.Review.RequireSuggestedFix},
		Agent: AgentLimits{
			MaxSteps: s.Agent.MaxSteps, MaxToolOutputBytes: s.Agent.MaxToolOutputBytes, MaxTokens: s.Agent.MaxTokens,
			TimeoutSeconds: int64(s.Agent.Timeout.Seconds()), Commands: nonNil(s.Agent.Commands),
			CommandTimeoutSeconds: int64(s.Agent.CommandTimeout.Seconds()),
		},
		Limits: limits(s.Limits),
	}
}

func indexRuns(rows []store.IndexRunRow) []IndexRun {
	out := make([]IndexRun, len(rows))
	for i, x := range rows {
		out[i] = IndexRun{
			ID: x.ID, Repository: x.Repository, CommitSHA: x.CommitSHA, BaseSHA: x.BaseSHA, EmbedModel: x.EmbedModel, Mode: x.Mode,
			Status: x.Status, Trigger: x.Trigger, ChunkCount: x.ChunkCount, Error: x.Error, CreatedAt: x.CreatedAt, FinishedAt: x.FinishedAt,
		}
	}
	return out
}

// repoFilter resolves ?repo=owner/name to a repository id, "" without one.
func repoFilter(ctx context.Context, tx pgx.Tx, r *http.Request) (string, error) {
	name := r.URL.Query().Get("repo")
	if name == "" {
		return "", nil
	}
	row, err := store.FindRepo(ctx, tx, name, r.URL.Query().Get("installation"))
	if errors.Is(err, store.ErrNotFound) {
		return "", errNotFound("repository")
	}
	return row.ID, err
}

func (s *Server) listIndexRuns(w http.ResponseWriter, r *http.Request, t *tenantScope) error {
	page, err := parsePage(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	var rows []store.IndexRunRow
	var next *store.Cursor
	if err := s.read(ctx, t, func(tx pgx.Tx) error {
		repoID, err := repoFilter(ctx, tx, r)
		if err != nil {
			return err
		}
		rows, next, err = store.ListIndexRuns(ctx, tx, repoID, page)
		return err
	}); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, newPage(indexRuns(rows), next))
	return nil
}
