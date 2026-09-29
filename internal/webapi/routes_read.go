package webapi

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/auth"
	"github.com/home-operations/kritik/internal/config"
	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/repoconfig"
	"github.com/home-operations/kritik/internal/store"
)

// recentIndexRuns is how many index runs a repository's detail lists.
const recentIndexRuns = 20

// registerReads mounts the read-only API.
func (s *Server) registerReads(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/me", s.handler(s.getMe))
	mux.HandleFunc("GET /api/v1/accounts", s.handler(s.listAccounts))
	mux.HandleFunc("GET /api/v1/operator/accounts", s.operator(s.listOperatorAccounts))
	mux.HandleFunc("GET /api/v1/operator/instance", s.operator(s.listInstanceSettings))
	mux.HandleFunc("GET /api/v1/accounts/{forge}/{name}", s.account(s.getAccount))
	mux.HandleFunc("GET /api/v1/accounts/{forge}/{name}/repos", s.account(s.listRepos))
	mux.HandleFunc("GET /api/v1/accounts/{forge}/{name}/repos/{owner}/{repo}", s.account(s.getRepo))
	mux.HandleFunc("GET /api/v1/accounts/{forge}/{name}/index-runs", s.account(s.listIndexRuns))
	mux.HandleFunc("GET /api/v1/accounts/{forge}/{name}/pulls", s.account(s.listPulls))
	mux.HandleFunc("GET /api/v1/accounts/{forge}/{name}/pulls/{owner}/{repo}/{number}", s.account(s.getPull))
	mux.HandleFunc("GET /api/v1/accounts/{forge}/{name}/followups", s.account(s.listFollowups))
	mux.HandleFunc("GET /api/v1/accounts/{forge}/{name}/followups/{commentId}/transcript", s.account(s.getFollowupTranscript))
	mux.HandleFunc("GET /api/v1/accounts/{forge}/{name}/reviews/{id}", s.account(s.getReview))
	mux.HandleFunc("GET /api/v1/accounts/{forge}/{name}/reviews/{id}/diff", s.account(s.getReviewDiff))
	mux.HandleFunc("GET /api/v1/accounts/{forge}/{name}/reviews/{id}/transcript", s.account(s.getReviewTranscript))
	mux.HandleFunc("GET /api/v1/accounts/{forge}/{name}/reviews/{id}/raw", s.account(s.getReviewRaw))
	mux.HandleFunc("GET /api/v1/accounts/{forge}/{name}/usage", s.account(s.getUsage))
	mux.HandleFunc("GET /api/v1/accounts/{forge}/{name}/queue", s.account(s.listQueue))
}

func toUser(a store.User) User {
	return User{ID: a.ID, DisplayName: a.DisplayName, Email: a.Email, AvatarURL: a.AvatarURL}
}

func (s *Server) getMe(w http.ResponseWriter, r *http.Request) error {
	p := auth.PrincipalFrom(r.Context())
	me := Me{
		User:     toUser(p.User),
		Operator: p.Operator, Accounts: []AccountMembership{},
	}
	for _, t := range readable(s.current.Get(), p) {
		me.Accounts = append(me.Accounts, AccountMembership{Slug: t.Slug(), Role: roleOn(p)})
	}
	writeJSON(w, http.StatusOK, me)
	return nil
}

// readable lists the running accounts p may read, in order.
func readable(file *configfile.File, p *auth.Principal) []*configfile.Account {
	var out []*configfile.Account
	for i := range file.Accounts {
		if p.CanRead(file.Accounts[i].ID()) {
			out = append(out, &file.Accounts[i])
		}
	}
	return out
}

func (s *Server) listAccounts(w http.ResponseWriter, r *http.Request) error {
	p := auth.PrincipalFrom(r.Context())
	file := s.current.Get()
	out := []AccountSummary{}
	for _, t := range readable(file, p) {
		sum, err := s.accountSummary(r.Context(), file, t, roleOn(p))
		if err != nil {
			return err
		}
		out = append(out, sum)
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) accountSummary(ctx context.Context, file *configfile.File, t *configfile.Account, role auth.Role) (AccountSummary, error) {
	var stats store.AccountStats
	err := s.store.WithAccount(ctx, t.ID(), func(tx pgx.Tx) error {
		var err error
		stats, err = store.ReadAccountStats(ctx, tx)
		return err
	})
	if err != nil {
		return AccountSummary{}, err
	}
	sum := AccountSummary{
		Slug: t.Slug(), Role: role, Repositories: stats.Repositories,
		Reviews7d: stats.Reviews7d, Usage: monthUsage(stats.Month, file.Settings(t, "").Limits),
	}
	if in := file.ConnectionFor(t); in != nil {
		sum.Connection = in.Name
	}
	return sum, nil
}

func monthUsage(m store.MonthUsage, l configfile.Limits) MonthUsage {
	return MonthUsage{
		Tokens: m.Tokens, CostUSD: m.CostUSD, TokensPerMonth: l.TokensPerMonth,
		ReviewsToday: m.ReviewsToday, ReviewsPerDay: l.ReviewsPerDay,
	}
}

// listOperatorAccounts lists every running account, and every entry of the
// instance spec no connection serves. It is reported as a missing route to
// anyone but an admin.
func (s *Server) listOperatorAccounts(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	file := s.current.Get()
	out := []OperatorAccount{}
	for i := range file.Accounts {
		sum, err := s.accountSummary(ctx, file, &file.Accounts[i], auth.RoleAdmin)
		if err != nil {
			return err
		}
		out = append(out, OperatorAccount{AccountSummary: sum, Live: true})
	}
	for _, a := range file.Unserved() {
		out = append(out, OperatorAccount{
			Slug: a.Slug(), Role: auth.RoleAdmin,
			Conflict: "no connection serves this account",
		})
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) getAccount(w http.ResponseWriter, r *http.Request, t *accountScope) error {
	ctx := r.Context()
	var month store.MonthUsage
	var webhooks map[string]time.Time
	if err := s.read(ctx, t, func(tx pgx.Tx) error {
		var err error
		if month, err = store.ReadMonthUsage(ctx, tx); err != nil {
			return err
		}
		webhooks, err = store.ReadWebhookDeliveries(ctx, tx)
		return err
	}); err != nil {
		return err
	}
	settings := t.file.Settings(t.account, "")
	d := AccountDetail{
		Slug: t.account.Slug(), Role: t.role(),
		Models: models(settings.Models), Limits: limits(settings.Limits), Filter: filterSource(settings),
		Usage: monthUsage(month, settings.Limits),
	}
	if in := t.file.ConnectionFor(t.account); in != nil {
		d.Connection = connection(in)
		if at, ok := webhooks[in.ID()]; ok {
			d.Connection.LastWebhookAt = &at
		}
	}
	writeJSON(w, http.StatusOK, d)
	return nil
}

func connection(in *configfile.Connection) Connection {
	return Connection{
		Name: in.Name, Forge: in.Forge, ManagedBy: in.Origin(), Accounts: in.Accounts, HookPath: "/hooks/" + in.Name,
		Credentials: CredentialsSet{
			ClientID: in.App.ClientIDValue() != "", PrivateKey: in.App.PrivateKeyValue().Value() != "",
			WebhookSecret: in.WebhookSecretValue().Value() != "",
		},
	}
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

func (s *Server) listRepos(w http.ResponseWriter, r *http.Request, t *accountScope) error {
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
		ID: r.ID, FullName: r.FullName, Enabled: r.Enabled, ManagedBy: r.ManagedBy,
		DefaultBranch: r.DefaultBranch,
		Index:         IndexState{ActiveCommit: r.ActiveCommit, ActiveAt: r.ActiveAt, LastRunStatus: r.LastIndexStatus, LastRunAt: r.LastIndexAt},
	}
	if r.LastReview != nil {
		out.LastReview = &ReviewRef{ID: r.LastReview.ID, Status: r.LastReview.Status, CreatedAt: r.LastReview.CreatedAt}
	}
	return out
}

// findRepo resolves {owner}/{repo}.
func findRepo(ctx context.Context, tx pgx.Tx, r *http.Request) (store.RepoRow, error) {
	return lookupRepo(ctx, tx, r.PathValue("owner")+"/"+r.PathValue("repo"))
}

// lookupRepo resolves a repository of the account by its full name.
func lookupRepo(ctx context.Context, tx pgx.Tx, name string) (store.RepoRow, error) {
	row, err := store.FindRepo(ctx, tx, name)
	if errors.Is(err, store.ErrNotFound) {
		return row, errNotFound("repository")
	}
	return row, err
}

func (s *Server) getRepo(w http.ResponseWriter, r *http.Request, t *accountScope) error {
	ctx := r.Context()
	var row store.RepoRow
	var runs []store.IndexRunRow
	var file *store.RepoFileRow
	if err := s.read(ctx, t, func(tx pgx.Tx) error {
		var err error
		if row, err = findRepo(ctx, tx, r); err != nil {
			return err
		}
		if runs, _, err = store.ListIndexRuns(ctx, tx, row.ID, store.Page{Limit: recentIndexRuns}); err != nil {
			return err
		}
		f, err := store.LastRepoFile(ctx, tx, row.ID)
		switch {
		case err == nil:
			file = &f
		case !errors.Is(err, store.ErrNotFound):
			return err
		}
		return nil
	}); err != nil {
		return err
	}
	settings := t.file.Settings(t.account, row.FullName)
	d := RepoDetail{
		Repository: repository(row), Settings: repoSettings(settings), Sources: t.file.Sources(t.account, row.FullName),
		RepoConfig: repoConfig(settings, file), IndexRuns: indexRuns(runs),
	}
	writeJSON(w, http.StatusOK, d)
	return nil
}

// repoConfig applies the .kritik.yaml a review read to the operator's
// settings as they are now; nil when no review has read one.
func repoConfig(settings configfile.Settings, row *store.RepoFileRow) *RepoConfig {
	if row == nil {
		return nil
	}
	var doc []byte
	if row.Doc != nil {
		doc = []byte(*row.Doc)
	}
	m, err := repoconfig.Merge(doc, settings)
	out := &RepoConfig{
		ReviewID: row.ReviewID, Commit: row.Commit, Found: row.Doc != nil, Settings: repoSettings(m.Settings),
		SkipPaths: nonNil(m.Skip.OnlyPaths), Dropped: nonNil(m.Dropped),
	}
	if m.InRepoFilter != nil {
		out.Filter = m.InRepoFilter.Source()
	}
	if err != nil {
		out.Ignored = err.Error()
	}
	return out
}

func repoSettings(s configfile.Settings) RepoSettings {
	return RepoSettings{
		Enabled: s.Enabled, Mode: s.Mode, Models: models(s.Models), Filter: filterSource(s), Forks: s.Forks,
		Ignore: nonNil(slices.Clone(s.Ignore)), SettleSeconds: int64(s.Settle.Seconds()), MaxDeltaFiles: s.Incremental.MaxDeltaFiles,
		Review: ReviewBlock{
			Instructions: nonNil(s.Review.Instructions), RequireSuggestedFix: s.Review.RequireSuggestedFix, Templates: s.Review.Templates,
			MinSeverity: s.Review.MinSeverity, InlineComments: s.Review.InlineComments, Context: nonNil(s.Review.Context),
		},
		Agent: AgentLimits{
			MaxSteps: s.Agent.MaxSteps, MaxToolOutputBytes: s.Agent.MaxToolOutputBytes, MaxTokens: s.Agent.MaxTokens,
			TimeoutSeconds: int64(s.Agent.Timeout.Seconds()), Commands: nonNil(s.Agent.Commands),
			CommandTimeoutSeconds: int64(s.Agent.CommandTimeout.Seconds()),
		},
		Limits: limits(s.Limits),
		Allow:  allowBounds(s.Allow),
	}
}

func allowBounds(a configfile.Allow) AllowBounds {
	seconds := func(d *time.Duration) *int64 {
		if d == nil {
			return nil
		}
		return new(int64(d.Seconds()))
	}
	return AllowBounds{
		Modes: a.Modes, Models: a.Models, Commands: a.Commands, SettleSeconds: seconds(a.Settle),
		Agent: AllowAgentBounds{
			MaxSteps: a.Agent.MaxSteps, MaxToolOutputBytes: a.Agent.MaxToolOutputBytes, MaxTokens: a.Agent.MaxTokens,
			TimeoutSeconds: seconds(a.Agent.Timeout),
		},
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
	row, err := lookupRepo(ctx, tx, name)
	return row.ID, err
}

func (s *Server) listIndexRuns(w http.ResponseWriter, r *http.Request, t *accountScope) error {
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

func (s *Server) listInstanceSettings(w http.ResponseWriter, r *http.Request) error {
	writeJSON(w, http.StatusOK, instanceSettings(s.current.Get(), s.env))
	return nil
}

// instanceSettings are the settings no account owns: this process's
// environment, then the file's instance blocks.
func instanceSettings(f *configfile.File, env []config.EnvVar) []InstanceSetting {
	out := []InstanceSetting{}
	add := func(section, key, value string, source configfile.Source) {
		out = append(out, InstanceSetting{Section: section, Key: key, Value: value, Source: source})
	}
	from := func(set bool) configfile.Source {
		if set {
			return configfile.SourceFile
		}
		return configfile.SourceDefault
	}
	for _, e := range env {
		source := configfile.SourceDefault
		if e.Set {
			source = configfile.SourceEnv
		}
		add("environment", e.Name, withoutCredentials(e.Value), source)
	}
	for _, in := range f.Connections {
		source := configfile.SourceDashboard
		switch {
		case f.ConnectionFromEnv(in.Name):
			source = configfile.SourceEnv
		case in.Origin() == configfile.OriginFile:
			source = configfile.SourceFile
		}
		add("connections", in.Name, strings.Join(in.Accounts, ", ")+", webhook /hooks/"+in.Name, source)
	}
	for _, sk := range f.Skipped() {
		source := configfile.SourceFile
		if f.ConnectionFromEnv(sk.Name) {
			source = configfile.SourceEnv
		}
		add("connections", sk.Name, "left out: "+sk.Reason, source)
	}
	a := f.Auth
	// An auth key an environment variable set shows as coming from it.
	authFrom := func(path string, set bool) configfile.Source {
		if a.FromEnv(path) {
			return configfile.SourceEnv
		}
		return from(set)
	}
	add("auth", "sessionTTL", a.SessionTTLOrDefault().String(), authFrom("sessionTTL", a.SessionTTL > 0))
	var admin []string
	if user, _, ok := a.AdminUser(); ok {
		admin = []string{user}
	}
	add("auth", "admin", listOrNone(admin), authFrom("admin.password", admin != nil))
	for _, s := range a.SignIns() {
		value := s.Label()
		if s.Issuer != "" {
			value += " at " + s.Issuer
		}
		if s.RoleMapping == "" {
			value += ", no role mapping"
		}
		add("auth", string(s.Type()), value, authFrom(string(s.Type())+".clientId", true))
	}
	return out
}

// withoutCredentials is s with the credentials of a URL it is removed: an
// endpoint may carry its password.
func withoutCredentials(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.User == nil {
		return s
	}
	u.User = nil
	return u.String() + " (credentials hidden)"
}

func listOrNone(xs []string) string {
	if len(xs) == 0 {
		return "none"
	}
	return strings.Join(xs, ", ")
}
