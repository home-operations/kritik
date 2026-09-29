// Package poller backstops missed webhooks: on the leader, every interval
// and per account, it lists the open pull requests updated since the last
// poll and hands them to the ingest dispatcher as if a webhook had
// delivered them. Review jobs are unique on the head SHA, so a head the
// webhook already enqueued is skipped as a duplicate, never reviewed twice.
// An account's first poll records the pull requests last updated before
// kritik knew the account as a baseline instead of reviewing them: no
// webhook for them was missed, and on a large install reviewing them all
// would be one burst of model calls nobody asked for.
package poller

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/forge"
	"github.com/home-operations/kritik/internal/ingest"
	"github.com/home-operations/kritik/internal/metrics"
	"github.com/home-operations/kritik/internal/store"
	"github.com/home-operations/kritik/internal/webhook"
)

// Forges builds a forge client per connection and repository owner, as
// the worker does.
type Forges interface {
	For(ctx context.Context, in *configfile.Connection, repo string) (forge.Client, error)
}

// Poller lists open pull requests on a schedule.
type Poller struct {
	Store      *store.Store
	Current    *configfile.Current
	Forges     Forges
	Dispatcher ingest.Dispatcher
	Logger     *slog.Logger
	// Metrics may be nil.
	Metrics *metrics.Metrics
}

// pollOffRecheck is how often Run looks again at a file that turns
// polling off, so turning it back on takes effect without a restart.
const pollOffRecheck = time.Minute

// Run polls until ctx ends, every polling.interval of the file current at
// the time. The first poll happens after one interval, so a freshly
// elected leader does not hammer the forge while ingest is already serving
// webhooks.
func (p *Poller) Run(ctx context.Context) {
	for {
		interval := p.Current.Get().PollInterval()
		wait := interval
		if interval <= 0 {
			wait = pollOffRecheck
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
			if interval > 0 {
				p.PollAll(ctx)
			}
		}
	}
}

// PollAll polls every account in the current configuration through the
// connection serving it.
func (p *Poller) PollAll(ctx context.Context) {
	file := p.Current.Get()
	for i := range file.Accounts {
		account := &file.Accounts[i]
		in := file.ConnectionFor(account)
		if in == nil || ctx.Err() != nil {
			continue
		}
		n, err := p.Poll(ctx, file, account, in)
		switch {
		case err != nil:
			p.Logger.Warn("poll failed", "connection", in.Name, "account", account.Key(), "error", err)
			p.Metrics.Poll(in.Name, "error", 0)
		default:
			p.Metrics.Poll(in.Name, "ok", n)
		}
	}
}

// pollRepo is one enabled repository as a poll sees it. indexed is the
// commit its active index generation covers, "" when it has none.
type pollRepo struct {
	name, defaultBranch, indexed string
	configfile.RepoTraits
}

// Poll lists one account's repositories through the connection serving it
// and returns how many pull requests were handed to the dispatcher. For a
// connection no webhook has reached lately, it also checks each indexed
// repository's default branch, since no push webhook will say it moved.
func (p *Poller) Poll(ctx context.Context, file *configfile.File, account *configfile.Account, in *configfile.Connection) (int, error) {
	var repos []pollRepo
	var known time.Time
	var polled, delivered *time.Time
	err := p.Store.WithAccount(ctx, account.ID(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT r.name, r.default_branch, coalesce(g.commit_sha, ''), r.archived, r.fork
			FROM repositories r LEFT JOIN index_runs g ON g.id = r.active_index_run_id
			WHERE r.enabled ORDER BY r.name`)
		if err != nil {
			return err
		}
		if repos, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (pollRepo, error) {
			var r pollRepo
			err := row.Scan(&r.name, &r.defaultBranch, &r.indexed, &r.Archived, &r.Fork)
			return r, err
		}); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT a.created_at, s.last_polled_at, c.last_webhook_at FROM accounts a
			CROSS JOIN connections c LEFT JOIN poll_state s ON s.account_id = a.id
			WHERE a.id = $1 AND c.id = $2`, account.ID(), in.ID()).Scan(&known, &polled, &delivered)
	})
	if err != nil {
		return 0, fmt.Errorf("poller: read state: %w", err)
	}
	since := time.Now().Add(-file.PollLookback())
	if polled != nil && polled.After(since) {
		since = *polled
	}
	checkTips := delivered == nil || delivered.Before(time.Now().Add(-file.PollLookback()))
	started := time.Now()
	handled := 0
	for _, r := range repos {
		if ctx.Err() != nil {
			return handled, ctx.Err()
		}
		repo := r.name
		// The App can reach it, but it is archived, a fork not turned on,
		// or off in the settings.
		if !file.Runs(account, repo, r.RepoTraits) {
			continue
		}
		client, err := p.Forges.For(ctx, in, repo)
		if err != nil {
			return handled, err
		}
		owner, name, _ := strings.Cut(repo, "/")
		if checkTips && r.indexed != "" {
			if err := p.pollTip(ctx, file, account, in, client, r, started); err != nil {
				return handled, err
			}
		}
		prs, err := client.ListOpenPullRequests(ctx, owner, name, since)
		if err != nil {
			return handled, err
		}
		for _, pr := range prs {
			action := "poll"
			if polled == nil && !pr.UpdatedAt.After(known) {
				action = ingest.ActionBaseline
			}
			ev := webhook.Event{
				Kind: webhook.KindPullRequest, Action: action, Delivery: fmt.Sprintf("poll-%s-%d", started.UTC().Format("20060102T150405"), pr.Number),
				Repository: &webhook.Repository{FullName: repo, DefaultBranch: pr.DefaultBranch, RepoTraits: r.RepoTraits},
				Account:    owner, PullRequest: &pr.PullRequest,
			}
			out, err := p.Dispatcher.Dispatch(ctx, ingest.Request{File: file, Account: account, Event: ev})
			if err != nil {
				return handled, err
			}
			handled++
			p.Logger.Info("polled pull request "+out.Status, "connection", in.Name, "repository", repo, "pr", pr.Number, "reason", out.Reason)
		}
	}
	err = p.Store.WithAccount(ctx, account.ID(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO poll_state (account_id, last_polled_at) VALUES ($1, $2)
			ON CONFLICT (account_id) DO UPDATE SET last_polled_at = excluded.last_polled_at, updated_at = now()`, account.ID(), started)
		return err
	})
	if err != nil {
		return handled, fmt.Errorf("poller: write state: %w", err)
	}
	return handled, nil
}

// pollTip hands the dispatcher a push to r's default branch when its tip is
// not the commit r's index covers, so the index follows the branch as a
// push webhook would have made it.
func (p *Poller) pollTip(
	ctx context.Context, file *configfile.File, account *configfile.Account, in *configfile.Connection, client forge.Client, r pollRepo,
	started time.Time,
) error {
	owner, name, _ := strings.Cut(r.name, "/")
	tip, branch, err := client.BranchTip(ctx, owner, name, r.defaultBranch)
	if err != nil {
		return err
	}
	if tip == r.indexed {
		return nil
	}
	ev := webhook.Event{
		Kind: webhook.KindPush, Delivery: fmt.Sprintf("poll-%s-push", started.UTC().Format("20060102T150405")),
		Repository: &webhook.Repository{FullName: r.name, DefaultBranch: branch, RepoTraits: r.RepoTraits}, Account: owner,
		Push: &webhook.Push{Ref: "refs/heads/" + branch, After: tip},
	}
	out, err := p.Dispatcher.Dispatch(ctx, ingest.Request{File: file, Account: account, Event: ev})
	if err != nil {
		return err
	}
	p.Logger.Info("polled default branch "+out.Status, "connection", in.Name, "repository", r.name, "tip", tip, "reason", out.Reason)
	return nil
}
