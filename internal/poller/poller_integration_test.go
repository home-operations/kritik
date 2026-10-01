//go:build integration

package poller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/configfile/configfiletest"
	"github.com/home-operations/kritik/internal/forge"
	"github.com/home-operations/kritik/internal/ingest"
	"github.com/home-operations/kritik/internal/store"
	"github.com/home-operations/kritik/internal/store/storetest"
)

const configYAML = `
apps:
  - name: bot-ross
    accounts: [onedr0p]
    clientId: Iv1.x
    privateKey: { env: TEST_PEM }
    webhookSecret: { env: TEST_SECRET }
repositories:
  onedr0p/home-ops: {}
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

func (f *forges) For(context.Context, *configfile.Connection, string) (forge.Client, error) {
	return f.f, nil
}

func TestPollerEnqueuesOnceAndAdvancesState(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.DiscardHandler)
	st := storetest.Open(t)
	t.Setenv("TEST_PEM", "pem")
	t.Setenv("TEST_SECRET", "s")
	file := configfiletest.Load(t, configYAML)
	if err := st.ApplyConfig(ctx, file); err != nil {
		t.Fatal(err)
	}
	queue, err := river.NewClient(riverpgxv5.New(st.App()), &river.Config{})
	if err != nil {
		t.Fatal(err)
	}
	in, _ := file.Connection("bot-ross")
	account, _ := file.Account(configfile.ForgeGitHub, "onedr0p")
	var installed time.Time
	if err := st.WithAccount(ctx, account.ID(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT created_at FROM accounts WHERE id = $1`, account.ID()).Scan(&installed)
	}); err != nil {
		t.Fatal(err)
	}
	lf := &listForge{prs: []forge.OpenPullRequest{{
		Number: 7, Title: "poll me", Author: "onedr0p", State: "open", HeadRef: "f", HeadSHA: "abc123", BaseRef: "main",
		UpdatedAt: time.Now(), DefaultBranch: "main",
	}, {
		// Last touched before kritik knew the account.
		Number: 8, Title: "leave me", Author: "onedr0p", State: "open", HeadRef: "g", HeadSHA: "old888", BaseRef: "main",
		UpdatedAt: installed.Add(-time.Hour), DefaultBranch: "main",
	}}}
	p := &Poller{
		Store: st, Current: configfile.NewCurrent(file), Forges: &forges{f: lf}, Dispatcher: ingest.NewService(st, queue),
		Logger: logger,
	}

	// Start from no poll state, no pull requests 7 or 8 and home-ops as
	// the one enabled repository, whatever earlier suites left behind:
	// the repositories they listed are the forge's again, and enabled.
	err = st.WithAccount(ctx, account.ID(), func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM poll_state WHERE account_id = $1`, account.ID()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE repositories SET enabled = false WHERE name <> 'onedr0p/home-ops'`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM pull_requests WHERE number IN (7, 8)`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.App().Exec(ctx, `DELETE FROM river_job WHERE kind = 'review' AND args->>'number' IN ('7', '8')`); err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	n, err := p.Poll(ctx, file, account, in)
	if err != nil || n != 2 {
		t.Fatalf("first poll: n=%d err=%v", n, err)
	}
	countJobs := func(number int) (jobs int, trigger string) {
		t.Helper()
		// Only this test's pull requests: the ingest suite leaves jobs of
		// its own on the shared database.
		if err := st.App().QueryRow(ctx, `SELECT count(*), coalesce(max(args->>'trigger'), '') FROM river_job
			WHERE kind = 'review' AND (args->>'number')::int = $1`, number).Scan(&jobs, &trigger); err != nil {
			t.Fatal(err)
		}
		return jobs, trigger
	}
	if jobs, trigger := countJobs(7); jobs != 1 || trigger != "poll" {
		t.Fatalf("after first poll: jobs=%d trigger=%q", jobs, trigger)
	}
	checkBaseline(ctx, t, st, account.ID(), 8, "old888")
	var head string
	var polledAt time.Time
	err = st.WithAccount(ctx, account.ID(), func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT head_sha FROM pull_requests WHERE number = 7`).Scan(&head); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT last_polled_at FROM poll_state WHERE account_id = $1`, account.ID()).Scan(&polledAt)
	})
	if err != nil || head != "abc123" || polledAt.Before(before) {
		t.Fatalf("rows: err=%v head=%s polled=%v", err, head, polledAt)
	}

	// The forge lists only what changed since the first poll. Same head
	// again: ingest sees a duplicate, no second job.
	lf.mu.Lock()
	lf.prs = lf.prs[:1]
	lf.mu.Unlock()
	if n, err := p.Poll(ctx, file, account, in); err != nil || n != 1 {
		t.Fatalf("second poll: n=%d err=%v", n, err)
	}
	if jobs, _ := countJobs(7); jobs != 1 {
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
	if n, err := p.Poll(ctx, file, account, in); err != nil || n != 1 {
		t.Fatalf("third poll: n=%d err=%v", n, err)
	}
	if jobs, _ := countJobs(7); jobs != 2 {
		// The poll action reviews whatever the forge lists as open; state is
		// the forge's word, so a new head is a new job.
		t.Fatalf("after third poll: jobs=%d", jobs)
	}
	checkAccountOff(ctx, t, p, file, account, in, lf)
}

// checkAccountOff asserts a poll lists nothing for an account that leaves
// its repositories off, though the App still reaches them.
func checkAccountOff(
	ctx context.Context, t *testing.T, p *Poller, file *configfile.File, account *configfile.Account, in *configfile.Connection, lf *listForge,
) {
	t.Helper()
	off := *account
	off.Enabled = new(false)
	lf.mu.Lock()
	listed := len(lf.sinces)
	lf.mu.Unlock()
	if n, err := p.Poll(ctx, file, &off, in); err != nil || n != 0 {
		t.Fatalf("poll with the account off: n=%d err=%v", n, err)
	}
	lf.mu.Lock()
	defer lf.mu.Unlock()
	if len(lf.sinces) != listed {
		t.Fatalf("the forge was listed %d more times with the account off", len(lf.sinces)-listed)
	}
}

// checkBaseline asserts the pull request numbered number was recorded at
// head with no review job: the first poll's baseline.
func checkBaseline(ctx context.Context, t *testing.T, st *store.Store, accountID string, number int, head string) {
	t.Helper()
	var jobs int
	var got string
	if err := st.App().QueryRow(ctx, `SELECT count(*) FROM river_job WHERE kind = 'review' AND (args->>'number')::int = $1`, number).
		Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	err := st.WithAccount(ctx, accountID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT head_sha FROM pull_requests WHERE number = $1`, number).Scan(&got)
	})
	if err != nil || got != head || jobs != 0 {
		t.Fatalf("baseline pull request %d: head=%q jobs=%d err=%v; want %s recorded with no review job", number, got, jobs, err, head)
	}
}

// tipForge answers the listing with nothing and the branch tip with tip,
// counting the tip calls.
type tipForge struct {
	forge.Client
	tip   string
	calls int
}

func (f *tipForge) ListOpenPullRequests(context.Context, string, string, time.Time) ([]forge.OpenPullRequest, error) {
	return nil, nil
}

func (f *tipForge) BranchTip(context.Context, string, string, string) (string, string, error) {
	f.calls++
	return f.tip, "main", nil
}

// TestPollerIndexesAMovedDefaultBranch: a connection no webhook reaches
// has its indexed repositories' default branches checked, and an index job
// queued when one moved; one that webhooks reach does not.
func TestPollerIndexesAMovedDefaultBranch(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.DiscardHandler)
	st := storetest.Open(t)
	t.Setenv("TEST_PEM", "pem")
	t.Setenv("TEST_SECRET", "s")
	file := configfiletest.Load(t, configYAML)
	if err := st.ApplyConfig(ctx, file); err != nil {
		t.Fatal(err)
	}
	queue, err := river.NewClient(riverpgxv5.New(st.App()), &river.Config{})
	if err != nil {
		t.Fatal(err)
	}
	in, _ := file.Connection("bot-ross")
	account, _ := file.Account(configfile.ForgeGitHub, "onedr0p")
	repoID := configfile.RepositoryID(account.ID(), "onedr0p/home-ops")
	exec := func(sql string, args ...any) {
		t.Helper()
		if err := st.WithAccount(ctx, account.ID(), func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, sql, args...)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	var runID string
	if err := st.WithAccount(ctx, account.ID(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO index_runs (account_id, repository_id, commit_sha, embed_model, embed_dims, mode, status)
			VALUES ($1, $2, 'indexed', 'fake-embed', 8, 'full', 'completed') RETURNING id`, account.ID(), repoID).Scan(&runID)
	}); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE repositories SET active_index_run_id = $1 WHERE id = $2`, runID, repoID)
	// The worker's tests share the database and index this repository from
	// nothing.
	t.Cleanup(func() {
		exec(`UPDATE repositories SET active_index_run_id = NULL WHERE id = $1`, repoID)
		exec(`DELETE FROM index_runs WHERE id = $1`, runID)
		_, _ = st.App().Exec(ctx, `DELETE FROM river_job WHERE kind = 'index'`)
	})
	indexJobs := func() []string {
		t.Helper()
		rows, err := st.App().Query(ctx, `SELECT args->>'commit_sha' FROM river_job WHERE kind = 'index' AND args->>'repository_id' = $1`, repoID)
		if err != nil {
			t.Fatal(err)
		}
		commits, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return commits
	}
	tf := &tipForge{tip: "moved"}
	p := &Poller{Store: st, Current: configfile.NewCurrent(file), Forges: &forges{f: tf}, Dispatcher: ingest.NewService(st, queue), Logger: logger}
	poll := func() {
		t.Helper()
		if _, err := p.Poll(ctx, file, account, in); err != nil {
			t.Fatal(err)
		}
	}

	// Earlier suites leave index jobs of this repository behind.
	if _, err := st.App().Exec(ctx, `DELETE FROM river_job WHERE kind = 'index'`); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE connections SET last_webhook_at = NULL WHERE id = $1`, in.ID())
	poll()
	if got := indexJobs(); len(got) != 1 || got[0] != "moved" {
		t.Fatalf("index jobs = %v, want one at the moved tip", got)
	}

	if _, err := st.App().Exec(ctx, `DELETE FROM river_job WHERE kind = 'index'`); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE connections SET last_webhook_at = now() WHERE id = $1`, in.ID())
	calls := tf.calls
	poll()
	if tf.calls != calls || len(indexJobs()) != 0 {
		t.Fatalf("with webhooks arriving: %d tip checks and index jobs %v, want none", tf.calls-calls, indexJobs())
	}

	exec(`UPDATE connections SET last_webhook_at = NULL WHERE id = $1`, in.ID())
	tf.tip = "indexed"
	poll()
	if got := indexJobs(); len(got) != 0 {
		t.Fatalf("index jobs = %v, want none for a tip the index covers", got)
	}
}

// TestSyncRepositories: the repositories a running connection's App reaches
// are registered on the accounts it serves, with what the forge says of
// them, and a second sync adds none; an account it does not serve gets
// nothing, and a listing that fails is logged.
func TestSyncRepositories(t *testing.T) {
	ctx := context.Background()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	st := storetest.Open(t)
	t.Setenv("TEST_PEM", "pem")
	t.Setenv("TEST_SECRET", "s")
	file := configfiletest.Load(t, configYAML)
	if err := st.ApplyConfig(ctx, file); err != nil {
		t.Fatal(err)
	}
	account, _ := file.Account(configfile.ForgeGitHub, "onedr0p")
	// The other suites poll every enabled repository of the account.
	t.Cleanup(func() {
		_ = st.WithAccount(ctx, account.ID(), func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE repositories SET enabled = false WHERE name LIKE 'onedr0p/synced%'`)
			return err
		})
	})
	var asked []string
	p := &Poller{Store: st, Current: configfile.NewCurrent(file), Logger: logger,
		Reach: func(_ context.Context, in *configfile.Connection) (map[string][]store.ReachedRepository, error) {
			asked = append(asked, in.Name)
			return map[string][]store.ReachedRepository{
				"onedr0p": {
					{FullName: "onedr0p/synced", DefaultBranch: "main", Traits: &configfile.RepoTraits{}},
					{FullName: "onedr0p/synced-fork", DefaultBranch: "main", Traits: &configfile.RepoTraits{Fork: true}},
				},
				"stranger": {{FullName: "stranger/elsewhere", Traits: &configfile.RepoTraits{}}},
			}, nil
		},
	}
	rows := func() string {
		t.Helper()
		var out []string
		if err := st.WithAccount(ctx, account.ID(), func(tx pgx.Tx) error {
			r, err := tx.Query(ctx, `SELECT name, enabled, default_branch, fork FROM repositories
				WHERE name LIKE 'onedr0p/synced%' OR name LIKE 'stranger/%' ORDER BY name COLLATE "C"`)
			if err != nil {
				return err
			}
			var name, branch string
			var enabled, fork bool
			_, err = pgx.ForEachRow(r, []any{&name, &enabled, &branch, &fork}, func() error {
				out = append(out, fmt.Sprintf("%s:%v:%s:%v", name, enabled, branch, fork))
				return nil
			})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return strings.Join(out, ",")
	}

	p.SyncRepositories(ctx)
	want := "onedr0p/synced:true:main:false,onedr0p/synced-fork:true:main:true"
	if got := rows(); got != want || len(asked) != 1 || asked[0] != "bot-ross" {
		t.Fatalf("after a sync: repositories %q, asked %v; want %q from bot-ross alone", got, asked, want)
	}
	if !strings.Contains(logs.String(), "repositories registered") || !strings.Contains(logs.String(), "added=2") {
		t.Fatalf("logs = %s", logs.String())
	}
	logs.Reset()
	p.SyncRepositories(ctx)
	if got := rows(); got != want || strings.Contains(logs.String(), "repositories registered") {
		t.Fatalf("a second sync: repositories %q, logs %s; want nothing new", got, logs.String())
	}

	p.Reach = func(context.Context, *configfile.Connection) (map[string][]store.ReachedRepository, error) {
		return nil, errors.New("github: list App installations: 502")
	}
	p.SyncRepositories(ctx)
	if !strings.Contains(logs.String(), "repositories not synced") || !strings.Contains(logs.String(), "502") {
		t.Fatalf("a failed listing is not logged: %s", logs.String())
	}
}

// reactionForge lists no open pull requests, and the inline comments of
// the pull requests it has them for.
type reactionForge struct {
	forge.Client

	mu     sync.Mutex
	inline map[int][]forge.Comment
	listed []int
}

func (f *reactionForge) ListOpenPullRequests(context.Context, string, string, time.Time) ([]forge.OpenPullRequest, error) {
	return nil, nil
}

func (f *reactionForge) ListInline(_ context.Context, _, _ string, number int) ([]forge.Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listed = append(f.listed, number)
	return f.inline[number], nil
}

// TestPollerReadsReactions: a poll reads the reactions on kritik's inline
// comments into every finding that carries the comment's thread, for the
// pull requests reviewed lately only.
func TestPollerReadsReactions(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.DiscardHandler)
	st := storetest.Open(t)
	t.Setenv("TEST_PEM", "pem")
	t.Setenv("TEST_SECRET", "s")
	file := configfiletest.Load(t, configYAML)
	if err := st.ApplyConfig(ctx, file); err != nil {
		t.Fatal(err)
	}
	in, _ := file.Connection("bot-ross")
	account, _ := file.Account(configfile.ForgeGitHub, "onedr0p")

	var recent, stale string
	err := st.WithAccount(ctx, account.ID(), func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE repositories SET enabled = true WHERE name = 'onedr0p/home-ops'`); err != nil {
			return err
		}
		pull := func(number int, reviewed time.Time, comments ...int64) string {
			var pr string
			if err := tx.QueryRow(ctx, `INSERT INTO pull_requests (account_id, repository_id, number, head_sha)
				SELECT $1, id, $2, 'h' FROM repositories WHERE name = 'onedr0p/home-ops' RETURNING id`, account.ID(), number).Scan(&pr); err != nil {
				t.Fatal(err)
			}
			// Two reviews, the second carrying the first's threads.
			for i := range 2 {
				var review string
				if err := tx.QueryRow(ctx, `INSERT INTO reviews (account_id, pull_request_id, head_sha, status, created_at)
					VALUES ($1, $2, $3, 'completed', $4) RETURNING id`, account.ID(), pr, fmt.Sprintf("h%d", i), reviewed).Scan(&review); err != nil {
					t.Fatal(err)
				}
				for _, id := range comments {
					if _, err := tx.Exec(ctx, `INSERT INTO findings (account_id, review_id, path, line, severity, title, explanation,
						posted_inline, forge_comment_id) VALUES ($1, $2, 'a.go', 1, 'nit', 'n', '', true, $3)`, account.ID(), review, id); err != nil {
						t.Fatal(err)
					}
				}
			}
			return pr
		}
		if _, err := tx.Exec(ctx, `DELETE FROM pull_requests WHERE number IN (70, 71)`); err != nil {
			return err
		}
		recent = pull(70, time.Now(), 9001, 9002)
		stale = pull(71, time.Now().Add(-30*24*time.Hour), 9101)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	rf := &reactionForge{inline: map[int][]forge.Comment{
		70: {{ID: 9001, ReactionsUp: 3, ReactionsDown: 1}, {ID: 9002}, {ID: 5, ReactionsUp: 9}},
		71: {{ID: 9101, ReactionsUp: 4}},
	}}
	p := &Poller{Store: st, Current: configfile.NewCurrent(file), Forges: &forges{f: rf}, Logger: logger}
	if _, err := p.Poll(ctx, file, account, in); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if !slices.Equal(rf.listed, []int{70}) {
		t.Fatalf("listed the inline comments of %v; want only the pull request reviewed lately", rf.listed)
	}
	reactions := func(pr string) map[int64][2]int {
		t.Helper()
		out := map[int64][2]int{}
		if err := st.WithAccount(ctx, account.ID(), func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `SELECT f.forge_comment_id, f.reactions_up, f.reactions_down FROM findings f
				JOIN reviews v ON v.id = f.review_id WHERE v.pull_request_id = $1`, pr)
			if err != nil {
				return err
			}
			var id int64
			var up, down int
			_, err = pgx.ForEachRow(rows, []any{&id, &up, &down}, func() error {
				if got, ok := out[id]; ok && got != [2]int{up, down} {
					return fmt.Errorf("comment %d reads %v on one review and %v on another", id, got, [2]int{up, down})
				}
				out[id] = [2]int{up, down}
				return nil
			})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := reactions(recent); got[9001] != [2]int{3, 1} || got[9002] != [2]int{0, 0} {
		t.Fatalf("reactions = %v", got)
	}
	if got := reactions(stale); got[9101] != [2]int{0, 0} {
		t.Fatalf("a pull request reviewed a month ago was read: %v", got)
	}
}
