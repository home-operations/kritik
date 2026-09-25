package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/jobs"
	"github.com/home-operations/kritik/internal/store"
	"github.com/home-operations/kritik/internal/webhook"
)

// Service is the store-backed Dispatcher: every write happens in one
// tenant-scoped transaction together with the River insert, so a row and
// its job either both exist or neither does.
type Service struct {
	store *store.Store
	queue *river.Client[pgx.Tx]
}

// NewService builds the dispatcher over the application pool and an
// insert-only River client.
func NewService(st *store.Store, queue *river.Client[pgx.Tx]) *Service {
	return &Service{store: st, queue: queue}
}

// Reasons an event was skipped or ignored, as reported in Outcome.Reason.
const (
	reasonNoRepository = "no repository"
	reasonAction       = "action"
	reasonDisabled     = "disabled"
	reasonDuplicate    = "duplicate"
)

// reviewActions are the pull request actions that produce a review.
// reviewActions are the pull request actions that start a review; "poll"
// is the poller's synthetic action.
var reviewActions = map[string]bool{"opened": true, "synchronize": true, "reopened": true, "ready_for_review": true, "poll": true}

// reviewInsertOpts returns the River insert options for a review job, or nil
// for the default (immediate). Only a new head (synchronize, or the
// poller's synthetic poll) settles: an initial open, reopen, or draft
// transition has no prior head to supersede, so there is nothing to wait
// out.
func reviewInsertOpts(trigger string, settle time.Duration, now time.Time) *river.InsertOpts {
	if (trigger == "synchronize" || trigger == "poll") && settle > 0 {
		return &river.InsertOpts{ScheduledAt: now.Add(settle)}
	}
	return nil
}

// Dispatch implements Dispatcher.
func (s *Service) Dispatch(ctx context.Context, req Request) (Outcome, error) {
	switch req.Event.Kind {
	case webhook.KindPullRequest:
		return s.pullRequest(ctx, req)
	case webhook.KindComment:
		return s.comment(ctx, req)
	case webhook.KindPush:
		return s.push(ctx, req)
	case webhook.KindInstallation:
		return s.installation(ctx, req)
	default:
		return Outcome{Status: Ignored, Reason: string(req.Event.Kind)}, nil
	}
}

func (s *Service) pullRequest(ctx context.Context, req Request) (Outcome, error) {
	ev := req.Event
	pr := ev.PullRequest
	if ev.Repository == nil || pr == nil {
		return Outcome{Status: Ignored, Reason: reasonNoRepository}, nil
	}
	if !reviewActions[ev.Action] {
		if ev.Action == "closed" {
			err := s.store.WithTenant(ctx, req.Tenant.ID(), func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `UPDATE pull_requests SET state = 'closed', updated_at = now()
					WHERE repository_id = $1 AND number = $2`, repoID(req, ev.Repository.FullName), pr.Number)
				return err
			})
			return Outcome{Status: Ignored, Reason: "closed"}, err
		}
		return Outcome{Status: Ignored, Reason: reasonAction}, nil
	}
	settings := req.File.Settings(req.Tenant, ev.Repository.FullName)
	switch {
	case !settings.Enabled:
		return Outcome{Status: Skipped, Reason: reasonDisabled}, nil
	case pr.Fork && !settings.Forks:
		return Outcome{Status: Skipped, Reason: "fork"}, nil
	case settings.Filter != nil:
		ok, err := settings.Filter.Eval(pr.FilterVars())
		if err != nil {
			return Outcome{}, fmt.Errorf("ingest: filter: %w", err)
		}
		if !ok {
			return Outcome{Status: Skipped, Reason: "filter"}, nil
		}
	}

	labels, err := json.Marshal(pr.FilterVars()["labels"])
	if err != nil {
		return Outcome{}, fmt.Errorf("ingest: encode labels: %w", err)
	}
	out := Outcome{Status: Enqueued, Job: "review"}
	err = s.store.WithTenant(ctx, req.Tenant.ID(), func(tx pgx.Tx) error {
		rid, err := ensureRepository(ctx, tx, req, ev.Repository)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO pull_requests (tenant_id, repository_id, number, title, author, author_is_bot, draft, fork, state,
				head_ref, head_sha, base_ref, base_sha, url, body, opened_at, labels, merged)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'open', $9, $10, $11, $12, $13, $14, $15, $16, $17)
			ON CONFLICT (repository_id, number) DO UPDATE SET
				title = EXCLUDED.title, author = EXCLUDED.author, author_is_bot = EXCLUDED.author_is_bot, draft = EXCLUDED.draft,
				fork = EXCLUDED.fork, state = 'open', head_ref = EXCLUDED.head_ref, head_sha = EXCLUDED.head_sha,
				base_ref = EXCLUDED.base_ref, base_sha = EXCLUDED.base_sha, url = EXCLUDED.url, body = EXCLUDED.body,
				labels = EXCLUDED.labels, merged = EXCLUDED.merged, updated_at = now()`,
			req.Tenant.ID(), rid, pr.Number, pr.Title, pr.Author, pr.AuthorIsBot, pr.Draft, pr.Fork,
			pr.HeadRef, pr.HeadSHA, pr.BaseRef, pr.BaseSHA, pr.URL, pr.Body, nullTime(pr), labels, pr.Merged); err != nil {
			return fmt.Errorf("ingest: upsert pull request: %w", err)
		}
		res, err := s.queue.InsertTx(ctx, tx, jobs.ReviewArgs{
			TenantID: req.Tenant.ID(), RepositoryID: rid, Number: pr.Number, HeadSHA: pr.HeadSHA, Trigger: ev.Action,
		}, reviewInsertOpts(ev.Action, settings.Settle, time.Now()))
		if err != nil {
			return fmt.Errorf("ingest: enqueue review: %w", err)
		}
		if res.UniqueSkippedAsDuplicate {
			out = Outcome{Status: Skipped, Reason: reasonDuplicate, Job: "review"}
		}
		return nil
	})
	return out, err
}

func (s *Service) comment(ctx context.Context, req Request) (Outcome, error) {
	ev := req.Event
	c := ev.Comment
	if ev.Repository == nil || c == nil {
		return Outcome{Status: Ignored, Reason: reasonNoRepository}, nil
	}
	if ev.Action != "created" {
		return Outcome{Status: Ignored, Reason: reasonAction}, nil
	}
	// The cheap gate: a bot never triggers a follow-up, and a comment with
	// no mention at all is not one. The worker checks the mention against
	// the installation's resolved bot identity and the author's access.
	if c.AuthorIsBot || !strings.Contains(c.Body, "@") {
		return Outcome{Status: Skipped, Reason: "no-mention"}, nil
	}
	settings := req.File.Settings(req.Tenant, ev.Repository.FullName)
	if !settings.Enabled {
		return Outcome{Status: Skipped, Reason: reasonDisabled}, nil
	}
	out := Outcome{Status: Enqueued, Job: "followup"}
	err := s.store.WithTenant(ctx, req.Tenant.ID(), func(tx pgx.Tx) error {
		rid, err := ensureRepository(ctx, tx, req, ev.Repository)
		if err != nil {
			return err
		}
		res, err := s.queue.InsertTx(ctx, tx, jobs.FollowUpArgs{
			TenantID: req.Tenant.ID(), RepositoryID: rid, Number: c.Number, CommentID: c.ID,
			Inline: c.Inline, Path: c.Path, Line: c.Line,
		}, nil)
		if err != nil {
			return fmt.Errorf("ingest: enqueue follow-up: %w", err)
		}
		if res.UniqueSkippedAsDuplicate {
			out = Outcome{Status: Skipped, Reason: reasonDuplicate, Job: "followup"}
		}
		return nil
	})
	return out, err
}

func (s *Service) push(ctx context.Context, req Request) (Outcome, error) {
	ev := req.Event
	if ev.Repository == nil || ev.Push == nil {
		return Outcome{Status: Ignored, Reason: reasonNoRepository}, nil
	}
	if ev.Repository.DefaultBranch == "" || ev.Push.Ref != "refs/heads/"+ev.Repository.DefaultBranch {
		return Outcome{Status: Skipped, Reason: "not-default-branch"}, nil
	}
	if ev.Push.After == "" || strings.Trim(ev.Push.After, "0") == "" {
		return Outcome{Status: Skipped, Reason: "branch-deleted"}, nil
	}
	settings := req.File.Settings(req.Tenant, ev.Repository.FullName)
	if !settings.Enabled {
		return Outcome{Status: Skipped, Reason: reasonDisabled}, nil
	}
	out := Outcome{Status: Enqueued, Job: "index"}
	err := s.store.WithTenant(ctx, req.Tenant.ID(), func(tx pgx.Tx) error {
		rid, err := ensureRepository(ctx, tx, req, ev.Repository)
		if err != nil {
			return err
		}
		res, err := s.queue.InsertTx(ctx, tx, jobs.IndexArgs{
			TenantID: req.Tenant.ID(), RepositoryID: rid, CommitSHA: ev.Push.After, Trigger: "push",
		}, nil)
		if err != nil {
			return fmt.Errorf("ingest: enqueue index: %w", err)
		}
		if res.UniqueSkippedAsDuplicate {
			out = Outcome{Status: Skipped, Reason: reasonDuplicate, Job: "index"}
		}
		return nil
	})
	return out, err
}

// installation records the forge's installation id and the repositories
// the App now sees. Repositories the App loses are disabled, not deleted.
func (s *Service) installation(ctx context.Context, req Request) (Outcome, error) {
	ev := req.Event
	inst := ev.Installation
	if inst == nil {
		return Outcome{Status: Ignored, Reason: "no installation"}, nil
	}
	enable := ev.Action == "created" || ev.Action == "added" || ev.Action == "unsuspend"
	disable := ev.Action == "removed" || ev.Action == "deleted" || ev.Action == "suspend"
	if !enable && !disable {
		return Outcome{Status: Ignored, Reason: reasonAction}, nil
	}
	err := s.store.WithTenant(ctx, req.Tenant.ID(), func(tx pgx.Tx) error {
		if inst.ID != 0 {
			if _, err := tx.Exec(ctx, `UPDATE installations SET external_id = $1, updated_at = now() WHERE id = $2`,
				inst.ID, req.Installation.ID()); err != nil {
				return fmt.Errorf("ingest: record installation id: %w", err)
			}
		}
		if disable && len(inst.Repositories) == 0 {
			// The whole installation went away.
			_, err := tx.Exec(ctx, `UPDATE repositories SET enabled = false, disabled_at = coalesce(disabled_at, now()), updated_at = now()
				WHERE installation_id = $1 AND managed_by = 'forge'`, req.Installation.ID())
			return err
		}
		for _, name := range inst.Repositories {
			if enable {
				if _, err := ensureRepository(ctx, tx, req, &webhook.Repository{FullName: name}); err != nil {
					return err
				}
				continue
			}
			if _, err := tx.Exec(ctx, `UPDATE repositories SET enabled = false, disabled_at = coalesce(disabled_at, now()), updated_at = now()
				WHERE id = $1 AND managed_by = 'forge'`, repoID(req, name)); err != nil {
				return fmt.Errorf("ingest: disable repository %s: %w", name, err)
			}
		}
		return nil
	})
	return Outcome{Status: Enqueued, Job: "installation", Reason: ev.Action}, err
}

// ensureRepository makes sure the repository row exists and, for a
// forge-managed row, that it is enabled: the forge just told us about it. A
// file-managed row keeps its settings and enabled flag, only learning the
// default branch.
func ensureRepository(ctx context.Context, tx pgx.Tx, req Request, repo *webhook.Repository) (string, error) {
	id := repoID(req, repo.FullName)
	_, err := tx.Exec(ctx, `
		INSERT INTO repositories (id, tenant_id, installation_id, name, default_branch, managed_by, enabled)
		VALUES ($1, $2, $3, $4, $5, 'forge', true)
		ON CONFLICT (installation_id, name) DO UPDATE SET
			default_branch = CASE WHEN EXCLUDED.default_branch <> '' THEN EXCLUDED.default_branch ELSE repositories.default_branch END,
			enabled = CASE WHEN repositories.managed_by = 'forge' THEN true ELSE repositories.enabled END,
			disabled_at = CASE WHEN repositories.managed_by = 'forge' THEN NULL ELSE repositories.disabled_at END,
			updated_at = now()`,
		id, req.Tenant.ID(), req.Installation.ID(), repo.FullName, repo.DefaultBranch)
	if err != nil {
		return "", fmt.Errorf("ingest: ensure repository %s: %w", repo.FullName, err)
	}
	return id, nil
}

func repoID(req Request, fullName string) string {
	return configfile.RepositoryID(req.Installation.ID(), fullName)
}

func nullTime(pr *webhook.PullRequest) any {
	if pr.CreatedAt.IsZero() {
		return nil
	}
	return pr.CreatedAt
}
