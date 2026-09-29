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
	// Reach lists, by lowercased account login, the repositories a
	// connection's App reaches; nil registers none.
	Reach func(ctx context.Context, in *configfile.Connection) (map[string][]store.ReachedRepository, error)
}

// pollOffRecheck is how often Run looks again at a file that turns
// polling off, so turning it back on takes effect without a restart.
const pollOffRecheck = time.Minute

// reachTimeout bounds one connection's repository listing: the leader
// lists right after applying the configuration and applies no change
// until it is done, so a hung forge must not hold that up.
const reachTimeout = time.Minute

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

// SyncRepositories registers the repositories each running connection's
// App reaches on the accounts it serves, as a webhook from each would, so
// they are known, polled and indexed without one: an App installed before
// kritik started sends no installation event. A connection whose listing
// fails, or takes longer than reachTimeout, is logged and left for the
// next poll.
func (p *Poller) SyncRepositories(ctx context.Context) {
	if p.Reach == nil {
		return
	}
	file := p.Current.Get()
	for i := range file.Connections {
		in := &file.Connections[i]
		if ctx.Err() != nil {
			return
		}
		reachCtx, cancel := context.WithTimeout(ctx, reachTimeout)
		reach, err := p.Reach(reachCtx, in)
		cancel()
		if err != nil {
			p.Logger.Warn("repositories not synced", "connection", in.Name, "error", err)
			continue
		}
		for j := range file.Accounts {
			account := &file.Accounts[j]
			if c := file.ConnectionFor(account); c == nil || c.Name != in.Name {
				continue
			}
			repos := reach[strings.ToLower(account.Name)]
			if len(repos) == 0 {
				continue
			}
			added, err := p.Store.RegisterRepositories(ctx, account.ID(), repos)
			if err != nil {
				p.Logger.Warn("repositories not synced", "connection", in.Name, "account", account.Key(), "error", err)
				continue
			}
			if added > 0 {
				p.Logger.Info("repositories registered", "connection", in.Name, "account", account.Key(), "added", added)
			}
		}
	}
}

// PollAll registers the repositories the Apps reach, then polls every
// account in the current configuration through the connection serving it.
func (p *Poller) PollAll(ctx context.Context) {
	p.SyncRepositories(ctx)
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
	runs := map[string]bool{}
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
		runs[repo] = true
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
	if err := p.pollReactions(ctx, account, in, runs); err != nil {
		p.Logger.Warn("reactions not read", "connection", in.Name, "account", account.Key(), "error", err)
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

// reactionWindow is how long after its latest review a pull request's
// reactions are read again: a finding is reacted to soon after it is
// posted, if at all.
const reactionWindow = 7 * 24 * time.Hour

// reactionPulls bounds the pull requests one poll reads the reactions of,
// per account, the most recently reviewed first, so an account with many
// open pull requests does not spend its API quota on them every poll.
const reactionPulls = 30

// pollReactions reads the 👍 and 👎 on the inline comments kritik posted
// on the account's recently reviewed pull requests, in the repositories
// that run, into their findings. GitHub sends no webhook for a reaction.
func (p *Poller) pollReactions(ctx context.Context, account *configfile.Account, in *configfile.Connection, runs map[string]bool) error {
	type pull struct {
		id, repo string
		number   int
	}
	var pulls []pull
	err := p.Store.WithAccount(ctx, account.ID(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT p.id, r.name, p.number FROM findings f
			JOIN reviews v ON v.id = f.review_id JOIN pull_requests p ON p.id = v.pull_request_id
			JOIN repositories r ON r.id = p.repository_id
			WHERE f.forge_comment_id IS NOT NULL AND v.created_at > $1
			GROUP BY p.id, r.name, p.number ORDER BY max(v.created_at) DESC LIMIT $2`, time.Now().Add(-reactionWindow), reactionPulls)
		if err != nil {
			return err
		}
		pulls, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (pull, error) {
			var x pull
			err := row.Scan(&x.id, &x.repo, &x.number)
			return x, err
		})
		return err
	})
	if err != nil {
		return fmt.Errorf("poller: read reviewed pull requests: %w", err)
	}
	for _, pr := range pulls {
		if !runs[pr.repo] || ctx.Err() != nil {
			continue
		}
		client, err := p.Forges.For(ctx, in, pr.repo)
		if err != nil {
			return err
		}
		owner, name, _ := strings.Cut(pr.repo, "/")
		comments, err := client.ListInline(ctx, owner, name, pr.number)
		if err != nil {
			return err
		}
		ids := make([]int64, len(comments))
		up := make([]int, len(comments))
		down := make([]int, len(comments))
		for i, c := range comments {
			ids[i], up[i], down[i] = c.ID, c.ReactionsUp, c.ReactionsDown
		}
		err = p.Store.WithAccount(ctx, account.ID(), func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE findings f SET reactions_up = x.up, reactions_down = x.down
				FROM reviews v, unnest($2::bigint[], $3::int[], $4::int[]) AS x(id, up, down)
				WHERE v.id = f.review_id AND v.pull_request_id = $1 AND f.forge_comment_id = x.id
					AND (f.reactions_up, f.reactions_down) IS DISTINCT FROM (x.up, x.down)`, pr.id, ids, up, down)
			return err
		})
		if err != nil {
			return fmt.Errorf("poller: write reactions of %s#%d: %w", pr.repo, pr.number, err)
		}
	}
	return nil
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
