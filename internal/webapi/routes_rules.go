package webapi

import (
	"cmp"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/repoconfig"
	"github.com/home-operations/kritik/internal/store"
)

// rulesRepoPage is how many repositories one read of the list takes.
const rulesRepoPage = 500

// listRules serves the rules, review instructions and context files the
// account's running repositories read: the configuration's, and each
// repository's own from its .kritik.yaml as its last review read it.
func (s *Server) listRules(w http.ResponseWriter, r *http.Request, t *accountScope) error {
	ctx := r.Context()
	var repos []store.RepoRow
	var files map[string]store.RepoFileRow
	if err := s.read(ctx, t, func(tx pgx.Tx) error {
		f := store.RepoFilter{}
		page := store.Page{Limit: rulesRepoPage}
		for {
			rows, next, err := store.ListRepos(ctx, tx, f, page)
			if err != nil {
				return err
			}
			repos = append(repos, rows...)
			if next == nil {
				break
			}
			page.After = *next
		}
		var err error
		files, err = store.LastRepoFiles(ctx, tx)
		return err
	}); err != nil {
		return err
	}
	in := make([]repoRules, 0, len(repos))
	for _, row := range repos {
		if !row.Enabled || !t.file.Runs(t.account, row.FullName, row.RepoTraits) {
			continue
		}
		rr := repoRules{
			name: row.FullName, settings: t.file.Settings(t.account, row.FullName), sources: t.file.Sources(t.account, row.FullName),
			ruleScopes: t.file.RuleScopes(t.account, row.FullName),
		}
		if f, ok := files[row.ID]; ok && f.Doc != nil {
			rr.doc = []byte(*f.Doc)
		}
		in = append(in, rr)
	}
	writeJSON(w, http.StatusOK, collectRules(in))
	return nil
}

// repoRules is what one repository's rules come from.
type repoRules struct {
	name       string
	settings   configfile.Settings
	sources    map[string]configfile.Source
	ruleScopes map[string]configfile.Scope
	// doc is the .kritik.yaml its last review read, nil for none.
	doc []byte
}

// collectRules lists each rule once, with every repository that reads it,
// ordered by kind, path or id, and source; a rule a repository's entry
// sets is listed for that repository alone. A .kritik.yaml that does not
// parse adds nothing, as a review ignores it.
func collectRules(repos []repoRules) []Rule {
	byKey := map[string]*Rule{}
	add := func(r Rule, repo string) {
		parts := []string{string(r.Kind), r.ID, r.Text, r.Path, r.Description, strings.Join(r.Paths, "\x00"), string(r.Source)}
		key := strings.Join(parts, "\x01")
		if r.Source == RuleFromEntry {
			key += "\x01" + repo
		}
		if got, ok := byKey[key]; ok {
			got.Repositories = append(got.Repositories, repo)
			return
		}
		r.Repositories = []string{repo}
		byKey[key] = &r
	}
	for _, rr := range repos {
		own := rr.settings.Review
		instructionsFrom, contextFrom := RuleSource(rr.sources["review.instructions"]), RuleSource(rr.sources["review.context"])
		for _, p := range own.Instructions {
			add(Rule{Kind: RuleInstructions, Path: p, Paths: []string{}, Source: instructionsFrom}, rr.name)
		}
		for _, c := range own.Context {
			add(Rule{Kind: RuleContext, Path: c.Path, Description: c.Description, Paths: nonNil(c.Paths), Source: contextFrom}, rr.name)
		}
		for _, w := range own.Rules {
			add(Rule{Kind: RuleWritten, ID: w.ID, Text: w.Rule, Paths: nonNil(w.Paths), Source: ruleFrom[rr.ruleScopes[w.ID]]}, rr.name)
		}
		if rr.doc == nil {
			continue
		}
		m, err := repoconfig.Merge(rr.doc, rr.settings)
		if err != nil {
			continue
		}
		for _, p := range m.Review.Instructions {
			if !slices.Contains(own.Instructions, p) {
				add(Rule{Kind: RuleInstructions, Path: p, Paths: nonNil(m.Scoped[p]), Source: RuleFromRepository}, rr.name)
			}
		}
		for _, c := range m.Review.Context {
			if !slices.ContainsFunc(own.Context, func(o configfile.ContextFile) bool { return o.Path == c.Path }) {
				add(Rule{Kind: RuleContext, Path: c.Path, Description: c.Description, Paths: nonNil(c.Paths), Source: RuleFromRepository}, rr.name)
			}
		}
		for _, w := range m.Review.Rules[len(own.Rules):] {
			add(Rule{Kind: RuleWritten, ID: w.ID, Text: w.Rule, Paths: nonNil(w.Paths), Source: RuleFromRepository}, rr.name)
		}
	}
	out := make([]Rule, 0, len(byKey))
	for _, r := range byKey {
		slices.Sort(r.Repositories)
		out = append(out, *r)
	}
	slices.SortFunc(out, func(a, b Rule) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Path, b.Path), cmp.Compare(a.ID, b.ID), cmp.Compare(a.Source, b.Source),
			cmp.Compare(a.Description, b.Description), cmp.Compare(a.Text, b.Text))
	})
	return out
}

// ruleFrom is the source of a rule the configuration writes, by the scope
// that writes it.
var ruleFrom = map[configfile.Scope]RuleSource{
	configfile.ScopeDefaults:   RuleSource(configfile.SourceDefaults),
	configfile.ScopeAccount:    RuleSource(configfile.SourceAccount),
	configfile.ScopeRepository: RuleFromEntry,
}
