//go:build integration

package poller

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/forge"
	"github.com/home-operations/kritik/internal/ingest"
	"github.com/home-operations/kritik/internal/store"
	"github.com/home-operations/kritik/internal/webhook"
)

const configYAML = `
tenants:
  - slug: onedr0p
    installations:
      - name: bot-ross
        forge: github
        account: onedr0p
        app:
          clientId: Iv1.x
          privateKey: { env: TEST_PEM }
          webhookSecret: { env: TEST_SECRET }
    repositories:
      - name: onedr0p/home-ops
`

// listForge answers only the listing call; the poller needs nothing else.
type listForge struct {
	forge.Client

	mu     sync.Mutex
	prs    []forge.OpenPullRequest
	sinces []time.Time
}

func (f *listForge) ListOpenPullRequests(_ context.Context, _, _ string, since time.Time) ([]forge.OpenPullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sinces = append(f.sinces, since)
	return f.prs, nil
}

type forges struct{ f forge.Client }

func (f *forges) For(context.Context, *configfile.Installation, int64, string) (forge.Client, error) {
	return f.f, nil
}

func env(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Skipf("%s not set", key)
	}
	return v
}

func TestPollerEnqueuesOnceAndAdvancesState(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(ctx, store.Options{
		AppURL: env(t, "KRITIK_TEST_APP_URL"), OwnerURL: env(t, "KRITIK_TEST_OWNER_URL"),
		AppRole: "kritik_app", RunnerRole: "kritik_runner", Logger: logger,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx, "kritik_app", "kritik_runner"); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	t.Setenv("TEST_PEM", "pem")
	t.Setenv("TEST_SECRET", "s")
	file, err := configfile.Parse([]byte(configYAML))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyConfig(ctx, file, "test"); err != nil {
		t.Fatal(err)
	}
	queue, err := river.NewClient(riverpgxv5.New(st.App()), &river.Config{})
	if err != nil {
		t.Fatal(err)
	}
	in, tenant, _ := file.Installation("bot-ross")
	lf := &listForge{prs: []forge.OpenPullRequest{{
		PullRequest: webhook.PullRequest{Number: 7, Title: "poll me", Author: "onedr0p", State: "open", HeadRef: "f", HeadSHA: "abc123", BaseRef: "main"},
		UpdatedAt:   time.Now(), DefaultBranch: "main",
	}}}
	p := &Poller{
		Store: st, Current: configfile.NewCurrent(file), Forges: &forges{f: lf}, Dispatcher: ingest.NewService(st, queue),
		Interval: time.Hour, Lookback: 24 * time.Hour, Logger: logger,
	}

	// Start from no poll state and no pull request 7, whatever earlier
	// suites left behind.
	err = st.WithTenant(ctx, tenant.ID(), func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM poll_state WHERE installation_id = $1`, in.ID()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM pull_requests WHERE number = 7`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.App().Exec(ctx, `DELETE FROM river_job WHERE kind = 'review' AND args->>'number' = '7'`); err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	n, err := p.Poll(ctx, file, tenant, in)
	if err != nil || n != 1 {
		t.Fatalf("first poll: n=%d err=%v", n, err)
	}
	countJobs := func() (jobs int, trigger string) {
		t.Helper()
		// Only this test's pull request: the ingest suite leaves jobs of its
		// own on the shared database.
		if err := st.App().QueryRow(ctx, `SELECT count(*), coalesce(max(args->>'trigger'), '') FROM river_job
			WHERE kind = 'review' AND args->>'number' = '7'`).Scan(&jobs, &trigger); err != nil {
			t.Fatal(err)
		}
		return jobs, trigger
	}
	if jobs, trigger := countJobs(); jobs != 1 || trigger != "poll" {
		t.Fatalf("after first poll: jobs=%d trigger=%q", jobs, trigger)
	}
	var head string
	var polledAt time.Time
	err = st.WithTenant(ctx, tenant.ID(), func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT head_sha FROM pull_requests WHERE number = 7`).Scan(&head); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT last_polled_at FROM poll_state WHERE installation_id = $1`, in.ID()).Scan(&polledAt)
	})
	if err != nil || head != "abc123" || polledAt.Before(before) {
		t.Fatalf("rows: err=%v head=%s polled=%v", err, head, polledAt)
	}

	// Same head again: ingest sees a duplicate, no second job.
	if n, err := p.Poll(ctx, file, tenant, in); err != nil || n != 1 {
		t.Fatalf("second poll: n=%d err=%v", n, err)
	}
	if jobs, _ := countJobs(); jobs != 1 {
		t.Fatalf("after second poll: jobs=%d, want the head deduplicated", jobs)
	}
	lf.mu.Lock()
	sinces := lf.sinces
	lf.mu.Unlock()
	if len(sinces) != 2 || sinces[0].After(before.Add(-23*time.Hour)) || sinces[1].Before(before) {
		t.Fatalf("since values: first should be the lookback floor, second the first poll's start; got %v (before=%v)", sinces, before)
	}

	// A closed pull request from the forge is ignored by ingest, not an error.
	lf.mu.Lock()
	lf.prs[0].State = "closed"
	lf.prs[0].HeadSHA = "def456"
	lf.mu.Unlock()
	if n, err := p.Poll(ctx, file, tenant, in); err != nil || n != 1 {
		t.Fatalf("third poll: n=%d err=%v", n, err)
	}
	if jobs, _ := countJobs(); jobs != 2 {
		// The poll action reviews whatever the forge lists as open; state is
		// the forge's word, so a new head is a new job.
		t.Fatalf("after third poll: jobs=%d", jobs)
	}
}
