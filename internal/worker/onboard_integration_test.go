//go:build integration

package worker

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/store"
)

// TestOnboarderKeepsToItsWindow checks the feeder queues onboarding jobs up
// to its window and no further, and fills a place a finished job leaves.
func TestOnboarderKeepsToItsWindow(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(ctx, store.Options{
		AppURL: env(t, "KRITIK_TEST_APP_URL"), OwnerURL: env(t, "KRITIK_TEST_OWNER_URL"),
		Logger: logger,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx, "kritik_app", "kritik_runner"); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	t.Setenv("TEST_PEM", "pem")
	t.Setenv("TEST_SECRET", "s3cret")
	file, err := configfile.Parse([]byte(configYAML + `      - name: onedr0p/a
      - name: onedr0p/b
      - name: onedr0p/c
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyConfig(ctx, file, "test"); err != nil {
		t.Fatal(err)
	}
	// The suites share one database: count from what is already there,
	// and leave none of this test's jobs behind.
	var lastJob int64
	if err := st.App().QueryRow(ctx, `SELECT coalesce(max(id), 0) FROM river_job`).Scan(&lastJob); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = st.App().Exec(context.Background(), `DELETE FROM river_job WHERE id > $1`, lastJob) })
	queue, err := river.NewClient(riverpgxv5.New(st.App()), &river.Config{})
	if err != nil {
		t.Fatal(err)
	}
	before, err := st.OnboardingInFlight(ctx)
	if err != nil {
		t.Fatal(err)
	}
	o := &Onboarder{Store: st, Queue: queue, Window: before + 2, Logger: logger}
	queued := func() (jobs, repos int) {
		t.Helper()
		if err := st.App().QueryRow(ctx, `SELECT count(*), count(DISTINCT args->>'repository_id') FROM river_job
			WHERE id > $1 AND kind = 'index' AND args->>'trigger' = 'onboard'`, lastJob).Scan(&jobs, &repos); err != nil {
			t.Fatal(err)
		}
		return jobs, repos
	}
	for range 2 {
		if err := o.Offer(ctx); err != nil {
			t.Fatalf("Offer: %v", err)
		}
		if n, err := st.OnboardingInFlight(ctx); err != nil || n != o.Window {
			t.Fatalf("in flight = %d, %v; want the window, %d", n, err, o.Window)
		}
	}
	if jobs, repos := queued(); jobs != 2 || repos != 2 {
		t.Fatalf("queued %d jobs for %d repositories, want 2 for 2", jobs, repos)
	}
	if _, err := st.App().Exec(ctx, `UPDATE river_job SET state = 'completed', finalized_at = now()
		WHERE id = (SELECT min(id) FROM river_job WHERE id > $1)`, lastJob); err != nil {
		t.Fatal(err)
	}
	if err := o.Offer(ctx); err != nil {
		t.Fatalf("Offer: %v", err)
	}
	if jobs, repos := queued(); jobs != 3 || repos != 3 {
		t.Fatalf("after one finished: queued %d jobs for %d repositories, want 3 for 3", jobs, repos)
	}
}
