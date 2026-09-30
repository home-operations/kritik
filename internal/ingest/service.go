package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/jobs"
	"github.com/home-operations/kritik/internal/store"
	"github.com/home-operations/kritik/internal/webhook"
)

// Service is the store-backed Dispatcher: every write happens in one
// account-scoped transaction together with the River insert, so a row and
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
	reasonFork         = "fork"
	reasonReviewed     = "reviewed"
)

// The poller's synthetic actions: ActionPoll for an open pull request it
// lists, and ActionBaseline for one that predates kritik's knowing its
// connection, which is recorded, not reviewed.
const (
	ActionPoll     = "poll"
	ActionBaseline = "baseline"
)

// pullRequestActions are the pull request actions that record the pull
// request, each saying whether it also starts a review; ActionPoll and
// ActionBaseline are the poller's synthetic ones. The review job starts at
// once: the worker waits out the repository's settle time (jobs.Settles),
// since .kritik.yaml may set it.
var pullRequestActions = map[string]bool{
	"opened":           true,
	"reopened":         true,
	"ready_for_review": true,
	"synchronize":      true,
	ActionPoll:         true,
	ActionBaseline:     false,
}

// RecordDelivery implements DeliveryRecorder.
func (s *Service) RecordDelivery(ctx context.Context, connectionID string) error {
	if err := s.store.RecordWebhookDelivery(ctx, connectionID); err != nil {
		return fmt.Errorf("ingest: record delivery: %w", err)
	}
	return nil
}

// RecordUnsigned implements DeliveryRecorder.
func (s *Service) RecordUnsigned(ctx context.Context, connectionID string) error {
	if err := s.store.RecordUnsignedWebhook(ctx, connectionID); err != nil {
		return fmt.Errorf("ingest: record unsigned delivery: %w", err)
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
	case webhook.KindRepository:
		return s.repository(ctx, req)
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
	review, ok := pullRequestActions[ev.Action]
	if !ok {
		if ev.Action == "closed" {
			err := s.store.WithAccount(ctx, req.Account.ID(), func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `UPDATE pull_requests SET state = 'closed', merged = $3, closed_at = coalesce($4, now()),
					updated_at = now() WHERE repository_id = $1 AND number = $2`,
					repoID(req, ev.Repository.FullName), pr.Number, pr.Merged, pr.ClosedAt); err != nil {
					return fmt.Errorf("ingest: close pull request: %w", err)
				}
				return nil
			})
			return Outcome{Status: Ignored, Reason: "closed"}, err
		}
		return Outcome{Status: Ignored, Reason: reasonAction}, nil
	}
	settings := req.File.Settings(req.Account, ev.Repository.FullName)
	// A fork's pull request is recorded but not reviewed unless the
	// settings review forks: a maintainer asks for its review with
	// "@<bot> review", which needs the pull request known.
	fork := pr.Fork && !settings.Forks
	runs, err := s.runs(ctx, req)
	if err != nil {
		return Outcome{}, err
	}
	switch {
	case !runs:
		return Outcome{Status: Skipped, Reason: reasonDisabled}, nil
	case !fork && settings.Filter != nil:
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
	err = s.store.WithAccount(ctx, req.Account.ID(), func(tx pgx.Tx) error {
		rid, err := ensureRepository(ctx, tx, req, ev.Repository)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO pull_requests (account_id, repository_id, number, title, author, author_is_bot, draft, fork, state,
				head_ref, head_sha, base_ref, url, body, opened_at, labels, merged)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'open', $9, $10, $11, $12, $13, $14, $15, $16)
			ON CONFLICT (repository_id, number) DO UPDATE SET
				title = EXCLUDED.title, author = EXCLUDED.author, author_is_bot = EXCLUDED.author_is_bot, draft = EXCLUDED.draft,
				fork = EXCLUDED.fork, state = 'open', head_ref = EXCLUDED.head_ref, head_sha = EXCLUDED.head_sha,
				base_ref = EXCLUDED.base_ref, url = EXCLUDED.url, body = EXCLUDED.body,
				labels = EXCLUDED.labels, merged = EXCLUDED.merged, closed_at = NULL, updated_at = now()`,
			req.Account.ID(), rid, pr.Number, pr.Title, pr.Author, pr.AuthorIsBot, pr.Draft, pr.Fork,
			pr.HeadRef, pr.HeadSHA, pr.BaseRef, pr.URL, pr.Body, nullTime(pr), labels, pr.Merged); err != nil {
			return fmt.Errorf("ingest: upsert pull request: %w", err)
		}
		switch {
		case fork:
			out = Outcome{Status: Skipped, Reason: reasonFork}
			return nil
		case !review:
			out = Outcome{Status: Skipped, Reason: ev.Action}
			return nil
		}
		// A poll lists a pull request whenever anything about it moved, a
		// comment or a label included; only a head no review has seen is
		// news. The queue's unique key alone would not say so once River
		// has cleaned the earlier job up.
		if ev.Action == ActionPoll {
			var reviewed bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM reviews r JOIN pull_requests p ON p.id = r.pull_request_id
				WHERE p.repository_id = $1 AND p.number = $2 AND r.head_sha = $3)`, rid, pr.Number, pr.HeadSHA).Scan(&reviewed); err != nil {
				return fmt.Errorf("ingest: read reviews of the head: %w", err)
			}
			if reviewed {
				out = Outcome{Status: Skipped, Reason: reasonReviewed}
				return nil
			}
		}
		res, err := s.queue.InsertTx(ctx, tx, jobs.ReviewArgs{
			AccountID: req.Account.ID(), RepositoryID: rid, Number: pr.Number, HeadSHA: pr.HeadSHA, Trigger: ev.Action,
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
	// The cheap gate: a bot never triggers a follow-up, and a comment with
	// no mention at all is not one. The worker checks the mention against
	// the connection's resolved bot identity and the author's access.
	if c.AuthorIsBot || !strings.Contains(c.Body, "@") {
		return Outcome{Status: Skipped, Reason: "no-mention"}, nil
	}
	if runs, err := s.runs(ctx, req); err != nil || !runs {
		return Outcome{Status: Skipped, Reason: reasonDisabled}, err
	}
	out := Outcome{Status: Enqueued, Job: "followup"}
	err := s.store.WithAccount(ctx, req.Account.ID(), func(tx pgx.Tx) error {
		rid, err := ensureRepository(ctx, tx, req, ev.Repository)
		if err != nil {
			return err
		}
		res, err := s.queue.InsertTx(ctx, tx, jobs.FollowUpArgs{
			AccountID: req.Account.ID(), RepositoryID: rid, Number: c.Number, CommentID: c.ID,
			Inline: c.Inline,
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
	if runs, err := s.runs(ctx, req); err != nil || !runs {
		return Outcome{Status: Skipped, Reason: reasonDisabled}, err
	}
	out := Outcome{Status: Enqueued, Job: "index"}
	err := s.store.WithAccount(ctx, req.Account.ID(), func(tx pgx.Tx) error {
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
			AccountID: req.Account.ID(), RepositoryID: rid, CommitSHA: ev.Push.After, Trigger: jobs.TriggerPush,
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

// installation records the repositories the App now sees. Repositories the
// App loses are disabled, not deleted.
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
	err := s.store.WithAccount(ctx, req.Account.ID(), func(tx pgx.Tx) error {
		if disable && len(inst.Repositories) == 0 {
			// The App left this account: its repositories go, not those of
			// the other accounts the connection serves.
			if _, err := tx.Exec(ctx, `UPDATE repositories SET enabled = false, disabled_at = coalesce(disabled_at, now()), updated_at = now()
				WHERE account_id = $1 AND managed_by = 'forge'`, req.Account.ID()); err != nil {
				return fmt.Errorf("ingest: disable the account's repositories: %w", err)
			}
			return nil
		}
		for _, name := range inst.Repositories {
			if enable {
				// An installation event names a repository without saying
				// whether it is archived or a fork.
				if _, _, err := store.EnsureRepository(ctx, tx, req.Account.ID(), store.ReachedRepository{FullName: name}); err != nil {
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
	return Outcome{Status: Recorded, Reason: ev.Action}, err
}

// repository records what the forge now says of a repository: that it was
// created, archived or unarchived.
func (s *Service) repository(ctx context.Context, req Request) (Outcome, error) {
	ev := req.Event
	if ev.Repository == nil {
		return Outcome{Status: Ignored, Reason: reasonNoRepository}, nil
	}
	err := s.store.WithAccount(ctx, req.Account.ID(), func(tx pgx.Tx) error {
		_, err := ensureRepository(ctx, tx, req, ev.Repository)
		return err
	})
	return Outcome{Status: Recorded, Reason: ev.Action}, err
}

func ensureRepository(ctx context.Context, tx pgx.Tx, req Request, repo *webhook.Repository) (string, error) {
	id, _, err := store.EnsureRepository(ctx, tx, req.Account.ID(), store.ReachedRepository{
		FullName: repo.FullName, DefaultBranch: repo.DefaultBranch, Traits: &repo.RepoTraits,
	})
	return id, err
}

// runs reports whether the event's repository is reviewed: what the event
// says of it, and the choice an admin made for it in the dashboard.
func (s *Service) runs(ctx context.Context, req Request) (bool, error) {
	repo := req.Event.Repository
	t := repo.RepoTraits
	err := s.store.WithAccount(ctx, req.Account.ID(), func(tx pgx.Tx) error {
		var err error
		t.TurnedOn, err = store.TurnedOn(ctx, tx, repoID(req, repo.FullName))
		return err
	})
	if err != nil {
		return false, err
	}
	return req.File.Runs(req.Account, repo.FullName, t), nil
}

func repoID(req Request, fullName string) string {
	return configfile.RepositoryID(req.Account.ID(), fullName)
}

func nullTime(pr *webhook.PullRequest) any {
	if pr.CreatedAt.IsZero() {
		return nil
	}
	return pr.CreatedAt
}
