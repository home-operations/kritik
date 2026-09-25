package webapi

import (
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/store"
)

// defaultUsageWindow is the span a usage series covers without ?from=.
const defaultUsageWindow = 30 * 24 * time.Hour

// parseTime reads an RFC 3339 time or a YYYY-MM-DD date (midnight UTC).
func parseTime(s string) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	t, err := time.Parse(time.DateOnly, s)
	return t, err == nil
}

// usageQuery reads ?group=, ?from= and ?to=: by day over the last 30 days
// unless given.
func (s *Server) usageQuery(r *http.Request) (store.UsageGroup, time.Time, time.Time, error) {
	q := r.URL.Query()
	group := store.UsageGroup(q.Get("group"))
	if group == "" {
		group = store.UsageByDay
	}
	if !group.Valid() {
		return "", time.Time{}, time.Time{}, errBadRequest(CodeBadRequest, "group must be day, model, repo or role")
	}
	to := s.now().UTC()
	if v := q.Get("to"); v != "" {
		t, ok := parseTime(v)
		if !ok {
			return "", time.Time{}, time.Time{}, errBadRequest(CodeBadRequest, "to must be RFC 3339 or YYYY-MM-DD")
		}
		to = t
	}
	from := to.Add(-defaultUsageWindow)
	if v := q.Get("from"); v != "" {
		t, ok := parseTime(v)
		if !ok {
			return "", time.Time{}, time.Time{}, errBadRequest(CodeBadRequest, "from must be RFC 3339 or YYYY-MM-DD")
		}
		from = t
	}
	if !from.Before(to) {
		return "", time.Time{}, time.Time{}, errBadRequest(CodeBadRequest, "from must be before to")
	}
	return group, from, to, nil
}

func (s *Server) getUsage(w http.ResponseWriter, r *http.Request, t *tenantScope) error {
	group, from, to, err := s.usageQuery(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	var rows []store.UsageSeriesRow
	if err := s.read(ctx, t, func(tx pgx.Tx) error {
		rows, err = store.UsageSeries(ctx, tx, group, from, to)
		return err
	}); err != nil {
		return err
	}
	out := UsageSeries{Group: group, From: from, To: to, Rows: make([]UsagePoint, len(rows))}
	for i, u := range rows {
		out.Rows[i] = UsagePoint{
			Key: u.Key, InputTokens: u.InputTokens, CacheReadTokens: u.CacheReadTokens, CacheWriteTokens: u.CacheWriteTokens,
			OutputTokens: u.OutputTokens, CostUSD: u.CostUSD, Calls: u.Calls,
		}
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) listQueue(w http.ResponseWriter, r *http.Request, t *tenantScope) error {
	ctx := r.Context()
	var rows []store.JobRow
	if err := s.read(ctx, t, func(tx pgx.Tx) error {
		var err error
		rows, err = store.ListQueue(ctx, tx)
		return err
	}); err != nil {
		return err
	}
	out := make([]Job, len(rows))
	for i, j := range rows {
		out[i] = Job{
			ID: j.ID, Kind: j.Kind, State: j.State, Attempt: j.Attempt, MaxAttempts: j.MaxAttempts, CreatedAt: j.CreatedAt,
			ScheduledAt: j.ScheduledAt, AttemptedAt: j.AttemptedAt, FinalizedAt: j.FinalizedAt, LastError: j.LastError,
			Args: JobArgs{Repository: j.Repository, Number: j.Number, Head: j.Head, Trigger: j.Trigger, CommentID: j.CommentID},
		}
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}
