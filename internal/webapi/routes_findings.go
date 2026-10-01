package webapi

import (
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/review"
	"github.com/home-operations/kritik/internal/store"
)

// listFindings serves the account's findings, one per pull request and
// fingerprint, most recently reported first.
func (s *Server) listFindings(w http.ResponseWriter, r *http.Request, t *accountScope) error {
	page, err := parsePage(r)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	f := store.FindingFilter{
		Severity: review.Severity(q.Get("severity")), Status: store.FindingStatus(q.Get("status")), Rule: q.Get("rule"), Query: q.Get("q"),
	}
	if f.Severity != "" && !f.Severity.Valid() {
		return errBadRequest(CodeBadRequest, "severity must be blocking, important or nit")
	}
	if f.Status != "" && !f.Status.Valid() {
		return errBadRequest(CodeBadRequest, "status must be open or addressed")
	}
	ctx := r.Context()
	var rows []store.AccountFinding
	var next *store.Cursor
	if err := s.read(ctx, t, func(tx pgx.Tx) error {
		if f.RepositoryID, err = repoFilter(ctx, tx, r); err != nil {
			return err
		}
		rows, next, err = store.ListAccountFindings(ctx, tx, f, page)
		return err
	}); err != nil {
		return err
	}
	items := make([]AccountFinding, len(rows))
	for i, a := range rows {
		items[i] = AccountFinding{
			Finding: finding(a.FindingRow), ReviewID: a.ReviewID, Status: a.Status, FirstSeenAt: a.FirstSeenAt, LastSeenAt: a.LastSeenAt,
			Pull: pullRef(a.PullRequest),
		}
	}
	writeJSON(w, http.StatusOK, newPage(items, next))
	return nil
}
