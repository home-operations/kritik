package webapi

import (
	"cmp"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/store"
)

// analyticsRepos is how many repositories the analytics list.
const analyticsRepos = 5

func (s *Server) getAnalytics(w http.ResponseWriter, r *http.Request, t *accountScope) error {
	group := store.AnalyticsGroup(cmp.Or(r.URL.Query().Get("group"), string(store.AnalyticsByDay)))
	if !group.Valid() {
		return errBadRequest(CodeBadRequest, "group must be day, week or month")
	}
	from, to, err := s.window(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	out := Analytics{Group: group, From: from, To: to}
	if err := s.read(ctx, t, func(tx pgx.Tx) error {
		cur, err := store.ReadAnalyticsTotals(ctx, tx, from, to)
		if err != nil {
			return err
		}
		prev, err := store.ReadAnalyticsTotals(ctx, tx, from.Add(-to.Sub(from)), from)
		if err != nil {
			return err
		}
		series, err := store.ReadAnalyticsSeries(ctx, tx, group, from, to)
		if err != nil {
			return err
		}
		repos, err := store.ReadRepoActivity(ctx, tx, from, to, analyticsRepos)
		if err != nil {
			return err
		}
		out.Current, out.Previous = analyticsTotals(cur), analyticsTotals(prev)
		out.Series = make([]AnalyticsPoint, len(series))
		for i, p := range series {
			out.Series[i] = AnalyticsPoint{Key: p.Key, Reviews: p.Reviews, Findings: severityCounts(p.Findings), CostUSD: p.CostUSD}
		}
		out.Repositories = make([]RepoActivity, len(repos))
		for i, a := range repos {
			out.Repositories[i] = RepoActivity{
				Repository: a.Repository, Reviews: a.Reviews, Findings: severityCounts(a.Findings), Addressed: a.Addressed,
			}
		}
		return nil
	}); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func analyticsTotals(t store.AnalyticsTotals) AnalyticsTotals {
	return AnalyticsTotals{
		PullRequests: t.PullRequests, Reviews: t.Reviews, Failed: t.Failed, Findings: severityCounts(t.Findings),
		Addressed: t.Addressed, ReactionsUp: t.ReactionsUp, ReactionsDown: t.ReactionsDown, CostUSD: t.CostUSD,
		MedianReviewMs: t.MedianReviewMs, MedianMergeMs: t.MedianMergeMs,
	}
}

func severityCounts(c store.SeverityCounts) SeverityCounts {
	return SeverityCounts{Blocking: c.Blocking, Important: c.Important, Nit: c.Nit}
}

// window reads ?from= and ?to=: the last 30 days unless given.
func (s *Server) window(r *http.Request) (time.Time, time.Time, error) {
	q := r.URL.Query()
	to := s.now().UTC()
	if v := q.Get("to"); v != "" {
		t, ok := parseTime(v)
		if !ok {
			return time.Time{}, time.Time{}, errBadRequest(CodeBadRequest, "to must be RFC 3339 or YYYY-MM-DD")
		}
		to = t
	}
	from := to.Add(-defaultUsageWindow)
	if v := q.Get("from"); v != "" {
		t, ok := parseTime(v)
		if !ok {
			return time.Time{}, time.Time{}, errBadRequest(CodeBadRequest, "from must be RFC 3339 or YYYY-MM-DD")
		}
		from = t
	}
	if !from.Before(to) {
		return time.Time{}, time.Time{}, errBadRequest(CodeBadRequest, "from must be before to")
	}
	return from, to, nil
}
