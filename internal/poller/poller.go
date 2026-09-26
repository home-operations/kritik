// Package poller backstops missed webhooks: on the leader, every interval
// and per installation, it lists the open pull requests updated since the
// last poll and hands them to the ingest dispatcher as if a webhook had
// delivered them. Review jobs are unique on the head SHA, so a head the
// webhook already enqueued is skipped as a duplicate, never reviewed twice.
// An installation's first poll records the pull requests last updated
// before kritik knew the installation as a baseline instead of reviewing
// them: no webhook for them was missed, and on a large install reviewing
// them all would be one burst of model calls nobody asked for.
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

// Forges builds a forge client per installation, as the worker does.
type Forges interface {
	For(ctx context.Context, in *configfile.Installation, externalID int64, repo string) (forge.Client, error)
}

// Poller lists open pull requests on a schedule.
type Poller struct {
	Store      *store.Store
	Current    *configfile.Current
	Forges     Forges
	Dispatcher ingest.Dispatcher
	Interval   time.Duration
	// Lookback bounds the first poll of an installation, or one whose
	// state is older than this, so a long outage does not list every
	// open pull request's history at once.
	Lookback time.Duration
	Logger   *slog.Logger
	// Metrics may be nil.
	Metrics *metrics.Metrics
}

// Run polls until ctx ends. The first poll happens after one interval, so
// a freshly elected leader does not hammer the forge while ingest is
// already serving webhooks.
func (p *Poller) Run(ctx context.Context) error {
	if p.Interval <= 0 {
		return nil
	}
	t := time.NewTicker(p.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			p.PollAll(ctx)
		}
	}
}

// PollAll polls every installation in the current configuration whose forge
// the worker can build a client for.
func (p *Poller) PollAll(ctx context.Context) {
	file := p.Current.Get()
	for ti := range file.Tenants {
		tenant := &file.Tenants[ti]
		for ii := range tenant.Installations {
			in := &tenant.Installations[ii]
			if in.Forge == configfile.ForgeGitLab {
				p.Logger.Debug("poll skipped: forge not supported", "installation", in.Name, "forge", in.Forge)
				continue
			}
			if ctx.Err() != nil {
				return
			}
			n, err := p.Poll(ctx, file, tenant, in)
			switch {
			case err != nil:
				p.Logger.Warn("poll failed", "installation", in.Name, "error", err)
				p.Metrics.Poll(in.Name, "error", 0)
			default:
				p.Metrics.Poll(in.Name, "ok", n)
			}
		}
	}
}

type repoRow struct {
	name       string
	externalID int64
}

// Poll lists one installation's repositories and returns how many pull
// requests were handed to the dispatcher.
func (p *Poller) Poll(ctx context.Context, file *configfile.File, tenant *configfile.Tenant, in *configfile.Installation) (int, error) {
	var repos []repoRow
	var known time.Time
	var polled *time.Time
	err := p.Store.WithTenant(ctx, tenant.ID(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT r.name, coalesce(i.external_id, 0) FROM repositories r JOIN installations i ON i.id = r.installation_id
			WHERE r.installation_id = $1 AND r.enabled ORDER BY r.name`, in.ID())
		if err != nil {
			return err
		}
		if repos, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (repoRow, error) {
			var r repoRow
			err := row.Scan(&r.name, &r.externalID)
			return r, err
		}); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT i.created_at, s.last_polled_at FROM installations i
			LEFT JOIN poll_state s ON s.installation_id = i.id WHERE i.id = $1`, in.ID()).Scan(&known, &polled)
	})
	if err != nil {
		return 0, fmt.Errorf("poller: read state: %w", err)
	}
	since := time.Now().Add(-p.Lookback)
	if polled != nil && polled.After(since) {
		since = *polled
	}
	started := time.Now()
	handled := 0
	for _, r := range repos {
		if ctx.Err() != nil {
			return handled, ctx.Err()
		}
		client, err := p.Forges.For(ctx, in, r.externalID, r.name)
		if err != nil {
			return handled, err
		}
		owner, name, _ := strings.Cut(r.name, "/")
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
				Repository: &webhook.Repository{FullName: r.name, DefaultBranch: pr.DefaultBranch}, Account: owner, PullRequest: &pr.PullRequest,
			}
			out, err := p.Dispatcher.Dispatch(ctx, ingest.Request{File: file, Tenant: tenant, Installation: in, Event: ev})
			if err != nil {
				return handled, err
			}
			handled++
			p.Logger.Info("polled pull request "+out.Status, "installation", in.Name, "repository", r.name, "pr", pr.Number, "reason", out.Reason)
		}
	}
	err = p.Store.WithTenant(ctx, tenant.ID(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO poll_state (installation_id, tenant_id, last_polled_at) VALUES ($1, $2, $3)
			ON CONFLICT (installation_id) DO UPDATE SET last_polled_at = excluded.last_polled_at, updated_at = now()`, in.ID(), tenant.ID(), started)
		return err
	})
	if err != nil {
		return handled, fmt.Errorf("poller: write state: %w", err)
	}
	return handled, nil
}
