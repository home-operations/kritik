package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/jobs"
	"github.com/home-operations/kritik/internal/repoconfig"
	"github.com/home-operations/kritik/internal/store"
	"github.com/home-operations/kritik/internal/taskrun"
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
	reasonNotIndexed   = "not-indexed"
)

// ActionBaseline is the poller's synthetic action for a pull request that
// predates kritik's knowing its installation: it is recorded, not reviewed.
const ActionBaseline = "baseline"

// pullRequestActions are the pull request actions that record the pull
// request, each saying whether it also starts a review; "poll" and
// ActionBaseline are the poller's synthetic ones. The review job starts at
// once: the worker waits out the repository's settle time (jobs.Settles),
// since .kritik.yaml may set it.
var pullRequestActions = map[string]bool{
	"opened":           true,
	"reopened":         true,
	"ready_for_review": true,
	"synchronize":      true,
	"poll":             true,
	ActionBaseline:     false,
}

// Dispatch implements Dispatcher. A delivery is offered to tasks too,
// whatever the review pipeline made of it; the outcome is the review
// pipeline's unless it did nothing and tasks did.
func (s *Service) Dispatch(ctx context.Context, req Request) (Outcome, error) {
	var out Outcome
	var err error
	switch req.Event.Kind {
	case webhook.KindPullRequest:
		out, err = s.pullRequest(ctx, req)
	case webhook.KindComment:
		out, err = s.comment(ctx, req)
	case webhook.KindPush:
		out, err = s.push(ctx, req)
	case webhook.KindInstallation:
		out, err = s.installation(ctx, req)
	default:
		out = Outcome{Status: Ignored, Reason: string(req.Event.Kind)}
	}
	if err != nil {
		return out, err
	}
	// Tasks persist in a transaction of their own, after the review
	// pipeline's has committed. When that one enqueued nothing, what it
	// wrote is idempotent, so a failure here is the forge's to redeliver
	// (the delivery id keeps a redelivery from running tasks twice); when
	// it enqueued a job, a 500 would redeliver a delivery already acted
	// on, so tasks miss this one instead.
	queued, err := s.tasks(ctx, req)
	if err != nil {
		if out.Status != Enqueued {
			return out, err
		}
		slog.Error("ingest: delivery not offered to tasks", "delivery", req.Event.Delivery, "error", err)
		return out, nil
	}
	if queued && out.Status != Enqueued {
		out = Outcome{Status: Enqueued, Job: jobTaskDispatch}
	}
	return out, nil
}

const jobTaskDispatch = "task_dispatch"

// tasks stores a delivery some task could run on and enqueues the job that
// resolves which do. Only a forge's own delivery is offered: the poller's
// synthetic events carry no event name. Resolving the repository's tasks
// reads its .kritik.yaml through the forge, so it is the job's to do, as
// is the loop guard, which needs the installation's bot login; here only
// the operator's settings are consulted (see taskrun.Candidate).
func (s *Service) tasks(ctx context.Context, req Request) (bool, error) {
	ev := req.Event
	if ev.Repository == nil || ev.RawEvent == "" {
		return false, nil
	}
	settings := req.File.Settings(req.Tenant, req.Installation.Name, ev.Repository.FullName)
	operator, err := repoconfig.Merge(nil, settings)
	if err != nil {
		// Without a file Merge has nothing to parse; the operator's tasks
		// are still in operator.
		slog.Warn("ingest: operator tasks not merged", "error", err)
	}
	in := taskrun.Input(ev)
	if !taskrun.Candidate(in, operator.Tasks, settings.TaskBounds) {
		return false, nil
	}
	var payload json.RawMessage
	if in.Raw != nil {
		payload = ev.Raw
	}
	var subjectKind string
	var subjectNumber int
	if in.Subject != nil {
		subjectKind, subjectNumber = in.Subject.Kind, in.Subject.Number
	}
	queued := false
	err = s.store.WithTenant(ctx, req.Tenant.ID(), func(tx pgx.Tx) error {
		rid, err := ensureRepository(ctx, tx, req, ev.Repository)
		if err != nil {
			return err
		}
		id, err := store.InsertTaskEvent(ctx, tx, store.TaskEvent{
			TenantID: req.Tenant.ID(), InstallationID: req.Installation.ID(), RepositoryID: rid, Forge: in.Forge, Event: in.Event,
			RawEvent: ev.RawEvent, Action: ev.Action, Sender: ev.Sender, Delivery: ev.Delivery, SubjectKind: subjectKind,
			SubjectNumber: subjectNumber, Payload: payload,
		})
		if err != nil || id == "" {
			return err
		}
		dispatch := jobs.TaskDispatchArgs{TenantID: req.Tenant.ID(), RepositoryID: rid, EventID: id}
		if _, err := s.queue.InsertTx(ctx, tx, dispatch, nil); err != nil {
			return fmt.Errorf("ingest: enqueue task dispatch: %w", err)
		}
		queued = true
		return nil
	})
	return queued, err
}

func (s *Service) pullRequest(ctx context.Context, req Request) (Outcome, error) {
	ev := req.Event
	pr := ev.PullRequest
	if ev.Repository == nil || pr == nil {
		return Outcome{Status: Ignored, Reason: reasonNoRepository}, nil
	}
	review, ok := pullRequestActions[ev.Action]
	if !ok {
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
	settings := req.File.Settings(req.Tenant, req.Installation.Name, ev.Repository.FullName)
	switch {
	case !settings.Enabled:
		return Outcome{Status: Skipped, Reason: reasonDisabled}, nil
	case pr.Fork && !settings.Forks:
		return Outcome{Status: Skipped, Reason: "fork"}, nil
	case settings.Filter != nil:
		ok, err := settings.Filter.Eval(pr.FilterVars(ev.Action))
		if err != nil {
			return Outcome{}, fmt.Errorf("ingest: filter: %w", err)
		}
		if !ok {
			return Outcome{Status: Skipped, Reason: "filter"}, nil
		}
	}

	labels, err := json.Marshal(pr.LabelVars())
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
		if !review {
			out = Outcome{Status: Skipped, Reason: ev.Action}
			return nil
		}
		res, err := s.queue.InsertTx(ctx, tx, jobs.ReviewArgs{
			TenantID: req.Tenant.ID(), RepositoryID: rid, Number: pr.Number, HeadSHA: pr.HeadSHA, Trigger: ev.Action,
		}, nil)
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
	// Only pull request comments are review follow-ups. An event without a
	// subject (the poller's, a test's) is a pull request comment.
	if ev.Subject != nil && ev.Subject.Kind != webhook.SubjectPull {
		return Outcome{Status: Ignored, Reason: ev.Subject.Kind}, nil
	}
	// The cheap gate: a bot never triggers a follow-up, and a comment with
	// no mention at all is not one. The worker checks the mention against
	// the installation's resolved bot identity and the author's access.
	if c.AuthorIsBot || !strings.Contains(c.Body, "@") {
		return Outcome{Status: Skipped, Reason: "no-mention"}, nil
	}
	settings := req.File.Settings(req.Tenant, req.Installation.Name, ev.Repository.FullName)
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
	settings := req.File.Settings(req.Tenant, req.Installation.Name, ev.Repository.FullName)
	if !settings.Enabled {
		return Outcome{Status: Skipped, Reason: reasonDisabled}, nil
	}
	out := Outcome{Status: Enqueued, Job: "index"}
	err := s.store.WithTenant(ctx, req.Tenant.ID(), func(tx pgx.Tx) error {
		rid, err := ensureRepository(ctx, tx, req, ev.Repository)
		if err != nil {
			return err
		}
		// A repository without an index waits for the leader's onboarding
		// feeder, which paces full builds; a push would queue its full build
		// ahead of every other repository's.
		var indexed bool
		if err := tx.QueryRow(ctx, `SELECT active_index_run_id IS NOT NULL FROM repositories WHERE id = $1`, rid).Scan(&indexed); err != nil {
			return fmt.Errorf("ingest: read index state: %w", err)
		}
		if !indexed {
			out = Outcome{Status: Skipped, Reason: reasonNotIndexed}
			return nil
		}
		res, err := s.queue.InsertTx(ctx, tx, jobs.IndexArgs{
			TenantID: req.Tenant.ID(), RepositoryID: rid, CommitSHA: ev.Push.After, Trigger: jobs.TriggerPush,
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
