package webapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Actions queues the work an admin can ask for from the dashboard, each in
// the caller's tenant transaction so the job and its audit row commit
// together.
type Actions interface {
	// Rerun queues a manual review of the pull request's current head,
	// ErrNoHead when none is known, ErrRerunQueued when one is already
	// queued or running.
	Rerun(ctx context.Context, tx pgx.Tx, tenantID, repositoryID string, number int) (int64, error)
	// Cancel asks a running review to stop, recording by as the account
	// that asked; ErrNotCancelable when it is not running.
	Cancel(ctx context.Context, tx pgx.Tx, reviewID, by string) error
	// Reindex queues a full reindex of the repository.
	Reindex(ctx context.Context, tx pgx.Tx, tenantID, repositoryID string) (int64, error)
}

// Errors an Actions implementation returns for a request that cannot be
// carried out as asked.
var (
	ErrNoHead        = errors.New("webapi: the pull request has no known head to review")
	ErrNotCancelable = errors.New("webapi: the review is not running")
	// ErrRerunQueued is returned by Rerun when a review of the pull
	// request's current head is already queued or running.
	ErrRerunQueued = errors.New("webapi: a review of this head is already queued or running")
	// ErrRepositoryNotFound is returned by Reindex when repositoryID no
	// longer exists; findRepo already resolves it in the same
	// transaction, so this is defense in depth rather than an expected path.
	ErrRepositoryNotFound = errors.New("webapi: the repository was not found")
	// ErrReindexQueued is returned by Reindex when a forced reindex of the
	// repository is already queued or running.
	ErrReindexQueued = errors.New("webapi: a reindex is already queued for this repository")
)

var errActionsDisabled = errStatus(http.StatusServiceUnavailable, CodeActionsDisabled, "this process does not queue dashboard actions", nil)

// registerActions mounts re-run, cancel and reindex.
func (s *Server) registerActions(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/tenants/{slug}/pulls/{owner}/{repo}/{number}/rerun", s.admin(s.rerun))
	mux.HandleFunc("POST /api/v1/tenants/{slug}/reviews/{id}/cancel", s.admin(s.cancel))
	mux.HandleFunc("POST /api/v1/tenants/{slug}/repos/{owner}/{repo}/reindex", s.admin(s.reindex))
}

// jobAudit is a queued action's audit detail.
type jobAudit struct {
	JobID int64 `json:"jobId,omitempty"`
}

func (s *Server) rerun(w http.ResponseWriter, r *http.Request, t *tenantScope) error {
	if s.actions == nil {
		return errActionsDisabled
	}
	ctx, tid := r.Context(), t.tenant.ID()
	var job int64
	err := s.read(ctx, t, func(tx pgx.Tx) error {
		p, err := findPull(r, tx)
		if err != nil {
			return err
		}
		job, err = s.actions.Rerun(ctx, tx, tid, p.RepositoryID, p.Number)
		switch {
		case errors.Is(err, ErrNoHead):
			return errStatus(http.StatusConflict, CodeNoHead, "the pull request has no known head to review", nil)
		case errors.Is(err, ErrRerunQueued):
			return errStatus(http.StatusConflict, CodeAlreadyQueued, "a review of this head is already queued or running", nil)
		case err != nil:
			return err
		}
		target := p.Repository + "#" + strconv.Itoa(p.Number)
		return record(ctx, tx, t.principal, &tid, AuditReviewRerun, target, jobAudit{JobID: job})
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusAccepted, Accepted{JobID: job})
	return nil
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request, t *tenantScope) error {
	if s.actions == nil {
		return errActionsDisabled
	}
	ctx, tid, id := r.Context(), t.tenant.ID(), r.PathValue("id")
	if uuid.Validate(id) != nil {
		return errNotFound("review")
	}
	err := s.read(ctx, t, func(tx pgx.Tx) error {
		err := s.actions.Cancel(ctx, tx, id, t.principal.Account.ID)
		if errors.Is(err, ErrNotCancelable) {
			return errStatus(http.StatusConflict, CodeNotCancelable, "the review is not running", nil)
		}
		if err != nil {
			return err
		}
		return record(ctx, tx, t.principal, &tid, AuditReviewCancel, id, jobAudit{})
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusAccepted, Accepted{})
	return nil
}

func (s *Server) reindex(w http.ResponseWriter, r *http.Request, t *tenantScope) error {
	if s.actions == nil {
		return errActionsDisabled
	}
	ctx, tid := r.Context(), t.tenant.ID()
	var job int64
	err := s.read(ctx, t, func(tx pgx.Tx) error {
		repo, err := findRepo(ctx, tx, r)
		if err != nil {
			return err
		}
		job, err = s.actions.Reindex(ctx, tx, tid, repo.ID)
		if errors.Is(err, ErrRepositoryNotFound) {
			return errNotFound("repository")
		}
		if errors.Is(err, ErrReindexQueued) {
			return errStatus(http.StatusConflict, CodeAlreadyQueued, "a reindex is already queued for this repository", nil)
		}
		if err != nil {
			return err
		}
		return record(ctx, tx, t.principal, &tid, AuditRepoReindex, repo.FullName, jobAudit{JobID: job})
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusAccepted, Accepted{JobID: job})
	return nil
}
