package webapi

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/store"
	"github.com/home-operations/kritik/internal/transcript"
)

// pullFollowups bounds the follow-ups a pull request's detail lists.
const pullFollowups = 200

func (s *Server) listPulls(w http.ResponseWriter, r *http.Request, t *tenantScope) error {
	page, err := parsePage(r)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	f := store.PullFilter{State: store.PullState(q.Get("state")), Outcome: store.ReviewStatus(q.Get("outcome")), Query: q.Get("q")}
	if f.State == "" {
		f.State = store.PullOpen
	}
	if !f.State.Valid() {
		return errBadRequest(CodeBadRequest, "state must be open, closed or all")
	}
	if f.Outcome != "" && !f.Outcome.Valid() {
		return errBadRequest(CodeBadRequest, "outcome is not a review status")
	}
	ctx := r.Context()
	var rows []store.PullRow
	var next *store.Cursor
	if err := s.read(ctx, t, func(tx pgx.Tx) error {
		if f.RepositoryID, err = repoFilter(ctx, tx, r); err != nil {
			return err
		}
		rows, next, err = store.ListPulls(ctx, tx, f, page)
		return err
	}); err != nil {
		return err
	}
	items := make([]Pull, len(rows))
	for i, row := range rows {
		items[i] = pull(row)
	}
	writeJSON(w, http.StatusOK, newPage(items, next))
	return nil
}

func pull(p store.PullRow) Pull {
	out := Pull{
		Repository: p.Repository, Number: p.Number, Title: p.Title, Author: p.Author, State: p.State, Draft: p.Draft,
		Merged: p.Merged, HeadSHA: p.HeadSHA, HeadRef: p.HeadRef, BaseRef: p.BaseRef, URL: p.URL, OpenedAt: p.OpenedAt,
		UpdatedAt: p.UpdatedAt, Labels: make([]Label, len(p.Labels)),
	}
	for i, l := range p.Labels {
		out.Labels[i] = Label{Name: l.Name, Color: l.Color}
	}
	if v := p.LastReview; v != nil {
		out.LastReview = &ReviewBrief{
			ID: v.ID, Status: v.Status, Mode: v.Mode, Scope: v.Scope, CreatedAt: v.CreatedAt,
			Findings: SeverityCounts{Blocking: v.Findings.Blocking, Important: v.Findings.Important, Nit: v.Findings.Nit},
		}
	}
	return out
}

// findPull resolves {owner}/{repo}/{number}.
func findPull(r *http.Request, tx pgx.Tx) (store.PullRow, error) {
	ctx := r.Context()
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil || number <= 0 {
		return store.PullRow{}, errNotFound("pull request")
	}
	repo, err := findRepo(ctx, tx, r)
	if err != nil {
		return store.PullRow{}, err
	}
	p, err := store.FindPull(ctx, tx, repo.ID, number)
	if errors.Is(err, store.ErrNotFound) {
		return p, errNotFound("pull request")
	}
	return p, err
}

func (s *Server) getPull(w http.ResponseWriter, r *http.Request, t *tenantScope) error {
	ctx := r.Context()
	var d PullDetail
	if err := s.read(ctx, t, func(tx pgx.Tx) error {
		p, err := findPull(r, tx)
		if err != nil {
			return err
		}
		reviews, err := store.ListPullReviews(ctx, tx, p.ID)
		if err != nil {
			return err
		}
		followups, _, err := store.ListFollowups(ctx, tx, store.FollowupFilter{PullRequestID: p.ID}, store.Page{Limit: pullFollowups})
		if err != nil {
			return err
		}
		d = PullDetail{Pull: pull(p), Reviews: make([]Review, len(reviews)), Followups: followupList(followups)}
		for i, v := range reviews {
			d.Reviews[i] = reviewItem(v)
		}
		return nil
	}); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, d)
	return nil
}

func reviewItem(v store.ReviewRow) Review {
	out := Review{
		ID: v.ID, Status: v.Status, Trigger: v.Trigger, Mode: v.Mode, Scope: v.Scope, Model: v.Model, HeadSHA: v.HeadSHA,
		CostUSD: v.CostUSD, Tokens: TokenCounts{Input: v.InputTokens, Output: v.OutputTokens}, CreatedAt: v.CreatedAt,
		FinishedAt: v.FinishedAt, SkipReason: v.SkipReason, Error: v.Error,
	}
	if v.FinishedAt != nil {
		ms := v.FinishedAt.Sub(v.CreatedAt).Milliseconds()
		out.DurationMs = &ms
	}
	return out
}

func followupList(rows []store.FollowupRow) []Followup {
	out := make([]Followup, len(rows))
	for i, f := range rows {
		out[i] = Followup{
			ID: f.ID, CommentID: f.CommentID, Repository: f.Repository, Number: f.Number, Author: f.Author, Inline: f.Inline,
			Path: f.Path, Line: f.Line, Status: f.Status, Reason: f.Reason, ReplyCommentID: f.ReplyCommentID, Model: f.Model,
			CreatedAt: f.CreatedAt,
		}
	}
	return out
}

func (s *Server) listFollowups(w http.ResponseWriter, r *http.Request, t *tenantScope) error {
	page, err := parsePage(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	var rows []store.FollowupRow
	var next *store.Cursor
	if err := s.read(ctx, t, func(tx pgx.Tx) error {
		var f store.FollowupFilter
		if f.RepositoryID, err = repoFilter(ctx, tx, r); err != nil {
			return err
		}
		rows, next, err = store.ListFollowups(ctx, tx, f, page)
		return err
	}); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, newPage(followupList(rows), next))
	return nil
}

// getFollowupTranscript serves the model calls that answered a follow-up
// comment. A comment id is unique only per forge, so when two of the
// tenant's pull requests have a follow-up with it, ?repo= (and
// ?installation=) must say which is meant.
func (s *Server) getFollowupTranscript(w http.ResponseWriter, r *http.Request, t *tenantScope) error {
	commentID, err := strconv.ParseInt(r.PathValue("commentId"), 10, 64)
	if err != nil || commentID <= 0 {
		return errNotFound("follow-up")
	}
	ctx := r.Context()
	var rows []transcript.StoredRow
	if err := s.read(ctx, t, func(tx pgx.Tx) error {
		f := store.FollowupFilter{CommentID: commentID}
		if f.RepositoryID, err = repoFilter(ctx, tx, r); err != nil {
			return err
		}
		matches, _, err := store.ListFollowups(ctx, tx, f, store.Page{Limit: 2})
		switch {
		case err != nil:
			return err
		case len(matches) == 0:
			return errNotFound("follow-up")
		case len(matches) > 1:
			return &apiError{status: http.StatusConflict, code: CodeAmbiguous, message: "comment id matches several follow-ups; pass ?repo="}
		}
		rows, err = store.FollowupModelCalls(ctx, tx, matches[0].PullRequestID, commentID)
		return err
	}); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, transcriptOf(transcript.Rebuild(rows)))
	return nil
}
