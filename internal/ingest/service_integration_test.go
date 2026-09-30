//go:build integration

package ingest

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/configfile/configfiletest"
	"github.com/home-operations/kritik/internal/store"
	"github.com/home-operations/kritik/internal/webhook"
)

func env(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Skipf("%s not set", key)
	}
	return v
}

// The store suite's TestMain resets the schema; this suite runs after it in
// the same `go test ./...` invocation only by package order, so it starts by
// migrating whatever state it finds and applying its own configuration.
func setupService(t *testing.T) (*Service, *store.Store, *configfile.File) {
	t.Helper()
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
	f := configfiletest.Load(t, configYAML+`repositories:
  onedr0p/*: { filterExpr: "!pr.draft" }
  onedr0p/settle: { settle: 60s }
  onedr0p/opened-only: { filterExpr: 'pr.event == "opened"' }
`)
	if err := st.ApplyConfig(ctx, f); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	// onedr0p/disabled is one an admin turned off.
	account, _ := f.Account(configfile.ForgeGitHub, "onedr0p")
	if err := st.WithAccount(ctx, account.ID(), func(tx pgx.Tx) error {
		id, _, err := store.EnsureRepository(ctx, tx, account.ID(), store.ReachedRepository{FullName: "onedr0p/disabled"})
		if err != nil {
			return err
		}
		return store.TurnOn(ctx, tx, id, false)
	}); err != nil {
		t.Fatal(err)
	}
	queue, err := river.NewClient(riverpgxv5.New(st.App()), &river.Config{})
	if err != nil {
		t.Fatalf("river: %v", err)
	}
	return NewService(st, queue), st, f
}

// request is ev as delivered to bot-ross for the account it names, or
// else the owner of its repository.
func request(f *configfile.File, ev webhook.Event) Request {
	in, _ := f.Connection("bot-ross")
	owner := ev.Account
	if owner == "" && ev.Repository != nil {
		owner, _, _ = strings.Cut(ev.Repository.FullName, "/")
	}
	account, _ := f.Account(in.Forge, owner)
	return Request{File: f, Account: account, Event: ev}
}

func repo(name string) *webhook.Repository {
	return &webhook.Repository{FullName: name, DefaultBranch: "main"}
}

func TestDispatchPullRequest(t *testing.T) {
	svc, st, f := setupService(t)
	ctx := context.Background()
	pr := &webhook.PullRequest{Number: 7, Title: "t", Body: "please review", Author: "devin", State: "open", HeadRef: "f", HeadSHA: "aaa", BaseRef: "main",
		Labels: []webhook.Label{{Name: "stale", Color: "ffffff"}}}

	out, err := svc.Dispatch(ctx, request(f, webhook.Event{Kind: webhook.KindPullRequest, Action: "opened", Repository: repo("onedr0p/home-ops"), PullRequest: pr}))
	if err != nil || out.Status != Enqueued || out.Job != "review" {
		t.Fatalf("first dispatch = %+v, %v", out, err)
	}
	out, err = svc.Dispatch(ctx, request(f, webhook.Event{Kind: webhook.KindPullRequest, Action: "synchronize", Repository: repo("onedr0p/home-ops"), PullRequest: pr}))
	if err != nil || out.Status != Skipped || out.Reason != "duplicate" {
		t.Fatalf("redelivery of the same head should be a duplicate: %+v, %v", out, err)
	}
	pr2 := *pr
	pr2.HeadSHA = "bbb"
	pr2.Labels = []webhook.Label{{Name: "ready", Color: "00ff00"}}
	out, err = svc.Dispatch(ctx, request(f, webhook.Event{Kind: webhook.KindPullRequest, Action: "synchronize", Repository: repo("onedr0p/home-ops"), PullRequest: &pr2}))
	if err != nil || out.Status != Enqueued {
		t.Fatalf("a new head must enqueue: %+v, %v", out, err)
	}

	account, _ := f.Account(configfile.ForgeGitHub, "onedr0p")
	var headSHA, body string
	var labels []byte
	var merged bool
	var jobsN int
	err = st.WithAccount(ctx, account.ID(), func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT head_sha, body, labels, merged FROM pull_requests WHERE number = 7`).
			Scan(&headSHA, &body, &labels, &merged); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM river_job WHERE kind = 'review'`).Scan(&jobsN)
	})
	if err != nil || headSHA != "bbb" || body != "please review" || jobsN != 2 {
		t.Fatalf("head_sha = %q body = %q jobs = %d err = %v; want bbb, %q and 2", headSHA, body, jobsN, err, "please review")
	}
	// Labels are stored in the filter's own shape and replaced on every event.
	var stored []map[string]any
	if err := json.Unmarshal(labels, &stored); err != nil || merged ||
		!reflect.DeepEqual(stored, []map[string]any{{"name": "ready", "color": "00ff00"}}) {
		t.Fatalf("labels = %s merged = %v err = %v", labels, merged, err)
	}

	t.Run("gates", func(t *testing.T) {
		draft := *pr
		draft.Draft = true
		draft.HeadSHA = "ccc"
		fork := *pr
		fork.Fork = true
		fork.HeadSHA = "ddd"
		tests := []struct {
			name   string
			action string
			repo   string
			pr     *webhook.PullRequest
			reason string
		}{
			{"draft filtered", "opened", "onedr0p/home-ops", &draft, "filter"},
			{"fork off by default", "opened", "onedr0p/home-ops", &fork, "fork"},
			{"disabled repository", "opened", "onedr0p/disabled", pr, "disabled"},
			{"filtered on the event", "synchronize", "onedr0p/opened-only", pr, "filter"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				out, err := svc.Dispatch(ctx, request(f, webhook.Event{Kind: webhook.KindPullRequest, Action: tt.action, Repository: repo(tt.repo), PullRequest: tt.pr}))
				if err != nil || out.Status != Skipped || out.Reason != tt.reason {
					t.Fatalf("out = %+v, %v; want skipped %s", out, err, tt.reason)
				}
			})
		}
	})

	t.Run("closed updates state", func(t *testing.T) {
		merged := *pr
		closedAt := time.Date(2026, 9, 24, 20, 0, 0, 0, time.UTC)
		merged.State, merged.Merged, merged.ClosedAt = "closed", true, &closedAt
		out, err := svc.Dispatch(ctx, request(f, webhook.Event{Kind: webhook.KindPullRequest, Action: "closed", Repository: repo("onedr0p/home-ops"), PullRequest: &merged}))
		if err != nil || out.Status != Ignored || out.Reason != "closed" {
			t.Fatalf("closed = %+v, %v", out, err)
		}
		var state string
		var isMerged bool
		var at *time.Time
		if err := st.WithAccount(ctx, account.ID(), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT state, merged, closed_at FROM pull_requests WHERE number = 7`).Scan(&state, &isMerged, &at)
		}); err != nil {
			t.Fatal(err)
		}
		if state != "closed" || !isMerged || at == nil || !at.Equal(closedAt) {
			t.Fatalf("state = %q, merged = %v, closed at %v; want closed and merged at %v", state, isMerged, at, closedAt)
		}
	})
}

// TestDispatchForkRecorded: a fork's pull request is recorded, so a
// maintainer can ask for its review, but no review is queued for it.
func TestDispatchForkRecorded(t *testing.T) {
	svc, st, f := setupService(t)
	ctx := context.Background()
	account, _ := f.Account(configfile.ForgeGitHub, "onedr0p")
	fork := &webhook.PullRequest{Number: 70, Title: "t", Author: "someone", State: "open", HeadRef: "f", HeadSHA: "eee", BaseRef: "main", Fork: true}
	out, err := svc.Dispatch(ctx, request(f, webhook.Event{Kind: webhook.KindPullRequest, Action: "opened", Repository: repo("onedr0p/home-ops"), PullRequest: fork}))
	if err != nil || out != (Outcome{Status: Skipped, Reason: reasonFork}) {
		t.Fatalf("out = %+v, %v; want skipped as a fork", out, err)
	}
	var isFork bool
	var jobs int
	if err := st.WithAccount(ctx, account.ID(), func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT fork FROM pull_requests WHERE number = 70`).Scan(&isFork); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM river_job WHERE kind = 'review' AND (args->>'number')::int = 70`).Scan(&jobs)
	}); err != nil || !isFork || jobs != 0 {
		t.Fatalf("fork = %v, review jobs = %d, %v; want the pull request recorded as a fork and no review queued", isFork, jobs, err)
	}
}

// A new head is enqueued at once even where the repository settles: the
// worker waits the settle time out, since .kritik.yaml may set it.
// TestDispatchSkipsArchivedAndForks: nothing runs for an archived
// repository, or a fork its own entry does not turn on, whatever the
// account's settings say.
func TestDispatchSkipsArchivedAndForks(t *testing.T) {
	svc, _, f := setupService(t)
	ctx := context.Background()
	for _, traits := range []configfile.RepoTraits{{Fork: true}, {Archived: true}} {
		r := &webhook.Repository{FullName: "onedr0p/home-ops", DefaultBranch: "main", RepoTraits: traits}
		for _, ev := range []webhook.Event{
			{Kind: webhook.KindPullRequest, Action: "opened", Repository: r, PullRequest: &webhook.PullRequest{Number: 99, HeadSHA: "x"}},
			{Kind: webhook.KindComment, Action: "created", Repository: r, Comment: &webhook.Comment{ID: 9, Number: 99, Body: "@bot why"}},
			{Kind: webhook.KindPush, Repository: r, Push: &webhook.Push{Ref: "refs/heads/main", After: "abc"}},
		} {
			if out, err := svc.Dispatch(ctx, request(f, ev)); err != nil || out != (Outcome{Status: Skipped, Reason: reasonDisabled}) {
				t.Errorf("%s in a repository that is %+v = %+v, %v; want skipped as disabled", ev.Kind, traits, out, err)
			}
		}
	}
}

// TestDispatchFollowsTheDashboard: a repository an admin turned off in the
// dashboard runs nothing whatever the configuration says, and a fork one
// turned on is reviewed.
func TestDispatchFollowsTheDashboard(t *testing.T) {
	svc, st, f := setupService(t)
	ctx := context.Background()
	account, _ := f.Account(configfile.ForgeGitHub, "onedr0p")
	turn := func(name string, fork, on bool) {
		t.Helper()
		if err := st.WithAccount(ctx, account.ID(), func(tx pgx.Tx) error {
			id, _, err := store.EnsureRepository(ctx, tx, account.ID(), store.ReachedRepository{
				FullName: name, DefaultBranch: "main", Traits: &configfile.RepoTraits{Fork: fork},
			})
			if err != nil {
				return err
			}
			return store.TurnOn(ctx, tx, id, on)
		}); err != nil {
			t.Fatal(err)
		}
	}
	turn("onedr0p/switched-off", false, false)
	off := &webhook.Repository{FullName: "onedr0p/switched-off", DefaultBranch: "main"}
	for _, ev := range []webhook.Event{
		{Kind: webhook.KindPullRequest, Action: "opened", Repository: off, PullRequest: &webhook.PullRequest{Number: 98, HeadSHA: "x"}},
		{Kind: webhook.KindComment, Action: "created", Repository: off, Comment: &webhook.Comment{ID: 8, Number: 98, Body: "@bot why"}},
		{Kind: webhook.KindPush, Repository: off, Push: &webhook.Push{Ref: "refs/heads/main", After: "abc"}},
	} {
		if out, err := svc.Dispatch(ctx, request(f, ev)); err != nil || out != (Outcome{Status: Skipped, Reason: reasonDisabled}) {
			t.Errorf("%s in a repository turned off = %+v, %v; want skipped as disabled", ev.Kind, out, err)
		}
	}
	turn("onedr0p/switched-fork", true, true)
	on := &webhook.Repository{FullName: "onedr0p/switched-fork", DefaultBranch: "main", Fork: true}
	ev := webhook.Event{Kind: webhook.KindPush, Repository: on, Push: &webhook.Push{Ref: "refs/heads/main", After: "abc"}}
	if out, err := svc.Dispatch(ctx, request(f, ev)); err != nil || out.Reason == reasonDisabled {
		t.Errorf("a push to a fork turned on = %+v, %v; want it to run", out, err)
	}
}

func TestDispatchPullRequestEnqueuesAtOnce(t *testing.T) {
	svc, st, f := setupService(t)
	ctx := context.Background()
	account, _ := f.Account(configfile.ForgeGitHub, "onedr0p")
	scheduledAt := func(headSHA string) time.Time {
		t.Helper()
		var ts time.Time
		err := st.WithAccount(ctx, account.ID(), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx,
				`SELECT scheduled_at FROM river_job WHERE kind = 'review' AND args->>'head_sha' = $1`, headSHA).Scan(&ts)
		})
		if err != nil {
			t.Fatalf("scheduled_at for %s: %v", headSHA, err)
		}
		return ts
	}

	opened := &webhook.PullRequest{Number: 9, Title: "t", Author: "devin", State: "open", HeadRef: "f", HeadSHA: "s1", BaseRef: "main"}
	before := time.Now()
	out, err := svc.Dispatch(ctx, request(f, webhook.Event{Kind: webhook.KindPullRequest, Action: "opened", Repository: repo("onedr0p/settle"), PullRequest: opened}))
	after := time.Now()
	if err != nil || out.Status != Enqueued {
		t.Fatalf("opened dispatch = %+v, %v", out, err)
	}
	if got := scheduledAt("s1"); got.Before(before.Add(-time.Second)) || got.After(after.Add(time.Second)) {
		t.Fatalf("opened scheduled_at = %v, want within [%v, %v]", got, before, after)
	}

	synced := *opened
	synced.HeadSHA = "s2"
	before = time.Now()
	out, err = svc.Dispatch(ctx, request(f, webhook.Event{Kind: webhook.KindPullRequest, Action: "synchronize", Repository: repo("onedr0p/settle"), PullRequest: &synced}))
	after = time.Now()
	if err != nil || out.Status != Enqueued {
		t.Fatalf("synchronize dispatch = %+v, %v", out, err)
	}
	if got := scheduledAt("s2"); got.Before(before.Add(-time.Second)) || got.After(after.Add(time.Second)) {
		t.Fatalf("synchronize scheduled_at = %v, want within [%v, %v]", got, before, after)
	}
}

func TestDispatchCommentPush(t *testing.T) {
	svc, st, f := setupService(t)
	ctx := context.Background()
	account, _ := f.Account(configfile.ForgeGitHub, "onedr0p")
	count := func(kind string) int {
		var n int
		_ = st.WithAccount(ctx, account.ID(), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM river_job WHERE kind = $1`, kind).Scan(&n)
		})
		return n
	}

	t.Run("comment with a mention enqueues once", func(t *testing.T) {
		c := &webhook.Comment{ID: 501, Number: 7, Author: "devin", Body: "@bot-ross why?"}
		ev := webhook.Event{Kind: webhook.KindComment, Action: "created", Repository: repo("onedr0p/home-ops"), Comment: c}
		if out, err := svc.Dispatch(ctx, request(f, ev)); err != nil || out.Status != Enqueued {
			t.Fatalf("out = %+v, %v", out, err)
		}
		if out, _ := svc.Dispatch(ctx, request(f, ev)); out.Reason != "duplicate" {
			t.Fatalf("redelivery = %+v", out)
		}
		bot := *c
		bot.ID, bot.AuthorIsBot = 502, true
		if out, _ := svc.Dispatch(ctx, request(f, webhook.Event{Kind: webhook.KindComment, Action: "created", Repository: repo("onedr0p/home-ops"), Comment: &bot})); out.Reason != "no-mention" {
			t.Fatalf("bot comment = %+v", out)
		}
		if count("followup") != 1 {
			t.Fatalf("followup jobs = %d", count("followup"))
		}
	})

	t.Run("push to an indexed default branch indexes, other branches do not", func(t *testing.T) {
		main := webhook.Event{Kind: webhook.KindPush, Repository: repo("onedr0p/home-ops"), Push: &webhook.Push{Ref: "refs/heads/main", After: "eee"}}
		if out, err := svc.Dispatch(ctx, request(f, main)); err != nil || out.Reason != "not-indexed" || count("index") != 0 {
			t.Fatalf("push before an index = %+v, %v; index jobs = %d", out, err, count("index"))
		}
		if err := st.WithAccount(ctx, account.ID(), func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `WITH g AS (
					INSERT INTO index_runs (account_id, repository_id, commit_sha, embed_model, embed_dims, mode, status)
					VALUES ($1, $2, 'ddd', 'm', 8, 'full', 'completed') RETURNING id, repository_id)
				UPDATE repositories r SET active_index_run_id = g.id FROM g WHERE r.id = g.repository_id`,
				account.ID(), configfile.RepositoryID(account.ID(), "onedr0p/home-ops"))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if out, err := svc.Dispatch(ctx, request(f, main)); err != nil || out.Status != Enqueued || out.Job != "index" {
			t.Fatalf("main push = %+v, %v", out, err)
		}
		feature := webhook.Event{Kind: webhook.KindPush, Repository: repo("onedr0p/home-ops"), Push: &webhook.Push{Ref: "refs/heads/feature", After: "fff"}}
		if out, _ := svc.Dispatch(ctx, request(f, feature)); out.Reason != "not-default-branch" {
			t.Fatalf("feature push = %+v", out)
		}
		deleted := webhook.Event{Kind: webhook.KindPush, Repository: repo("onedr0p/home-ops"), Push: &webhook.Push{Ref: "refs/heads/main", After: "0000000000000000000000000000000000000000"}}
		if out, _ := svc.Dispatch(ctx, request(f, deleted)); out.Reason != "branch-deleted" {
			t.Fatalf("deleted push = %+v", out)
		}
		if count("index") != 1 {
			t.Fatalf("index jobs = %d", count("index"))
		}
	})
}

func TestDispatchInstallation(t *testing.T) {
	svc, st, f := setupService(t)
	ctx := context.Background()
	account, _ := f.Account(configfile.ForgeGitHub, "onedr0p")

	t.Run("connection adds and removes forge-managed repositories", func(t *testing.T) {
		added := webhook.Event{Kind: webhook.KindInstallation, Action: "added", Account: "onedr0p",
			Installation: &webhook.Installation{Repositories: []string{"onedr0p/new-repo", "onedr0p/disabled"}}}
		if out, err := svc.Dispatch(ctx, request(f, added)); err != nil || out != (Outcome{Status: Recorded, Reason: "added"}) {
			t.Fatalf("Dispatch = %+v, %v; want recorded for added", out, err)
		}
		var newEnabled bool
		var turnedOn *bool
		_ = st.WithAccount(ctx, account.ID(), func(tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `SELECT enabled FROM repositories WHERE name = 'onedr0p/new-repo'`).Scan(&newEnabled); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `SELECT turned_on FROM repositories WHERE name = 'onedr0p/disabled'`).Scan(&turnedOn)
		})
		if !newEnabled || turnedOn == nil || *turnedOn {
			t.Fatalf("new=%v turned on=%v; the App reaching a repository an admin turned off must not turn it on", newEnabled, turnedOn)
		}
		removed := webhook.Event{Kind: webhook.KindInstallation, Action: "removed", Account: "onedr0p",
			Installation: &webhook.Installation{Repositories: []string{"onedr0p/new-repo"}}}
		if _, err := svc.Dispatch(ctx, request(f, removed)); err != nil {
			t.Fatal(err)
		}
		_ = st.WithAccount(ctx, account.ID(), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT enabled FROM repositories WHERE name = 'onedr0p/new-repo'`).Scan(&newEnabled)
		})
		if newEnabled {
			t.Fatal("removed repository should be disabled")
		}
	})

	t.Run("the App leaving one account disables only that account's repositories", func(t *testing.T) {
		for _, name := range []string{"onedr0p/stays", "home-operations/goes"} {
			owner, _, _ := strings.Cut(name, "/")
			added := webhook.Event{Kind: webhook.KindInstallation, Action: "added", Account: owner,
				Installation: &webhook.Installation{Repositories: []string{name}}}
			if _, err := svc.Dispatch(ctx, request(f, added)); err != nil {
				t.Fatal(err)
			}
		}
		// The poller's tests share the database and poll every enabled
		// repository of bot-ross.
		t.Cleanup(func() {
			removed := webhook.Event{Kind: webhook.KindInstallation, Action: "removed", Account: "onedr0p",
				Installation: &webhook.Installation{Repositories: []string{"onedr0p/stays"}}}
			_, _ = svc.Dispatch(ctx, request(f, removed))
		})
		deleted := webhook.Event{Kind: webhook.KindInstallation, Action: "deleted", Account: "Home-Operations", Installation: &webhook.Installation{}}
		if _, err := svc.Dispatch(ctx, request(f, deleted)); err != nil {
			t.Fatal(err)
		}
		enabled := map[string]bool{}
		for _, owner := range []string{"onedr0p", "home-operations"} {
			a, _ := f.Account(configfile.ForgeGitHub, owner)
			if err := st.WithAccount(ctx, a.ID(), func(tx pgx.Tx) error {
				rows, err := tx.Query(ctx, `SELECT name, enabled FROM repositories WHERE name IN ('onedr0p/stays', 'home-operations/goes')`)
				if err != nil {
					return err
				}
				var name string
				var on bool
				_, err = pgx.ForEachRow(rows, []any{&name, &on}, func() error {
					enabled[name] = on
					return nil
				})
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		if !enabled["onedr0p/stays"] || enabled["home-operations/goes"] {
			t.Fatalf("enabled = %v; only home-operations' repository should go", enabled)
		}
	})
}

// TestDispatchRepository: a repository event records whether the
// repository is archived or a fork, which a later installation event,
// naming it alone, keeps.
func TestDispatchRepository(t *testing.T) {
	svc, st, f := setupService(t)
	ctx := context.Background()
	account, _ := f.Account(configfile.ForgeGitHub, "onedr0p")
	// The poller's tests share the database and poll every enabled
	// repository of bot-ross.
	t.Cleanup(func() {
		removed := webhook.Event{Kind: webhook.KindInstallation, Action: "removed", Account: "onedr0p",
			Installation: &webhook.Installation{Repositories: []string{"onedr0p/old"}}}
		_, _ = svc.Dispatch(ctx, request(f, removed))
	})
	traits := func() (archived, fork bool) {
		t.Helper()
		if err := st.WithAccount(ctx, account.ID(), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT archived, fork FROM repositories WHERE name = 'onedr0p/old'`).Scan(&archived, &fork)
		}); err != nil {
			t.Fatal(err)
		}
		return archived, fork
	}
	old := &webhook.Repository{FullName: "onedr0p/old", Archived: true, Fork: true}
	archived := webhook.Event{Kind: webhook.KindRepository, Action: "archived", Account: "onedr0p", Repository: old}
	if out, err := svc.Dispatch(ctx, request(f, archived)); err != nil || out != (Outcome{Status: Recorded, Reason: "archived"}) {
		t.Fatalf("Dispatch = %+v, %v; want recorded for archived", out, err)
	}
	if a, fk := traits(); !a || !fk {
		t.Fatalf("archived=%v fork=%v after the repository event", a, fk)
	}
	added := webhook.Event{Kind: webhook.KindInstallation, Action: "added", Account: "onedr0p",
		Installation: &webhook.Installation{Repositories: []string{"onedr0p/old"}}}
	if _, err := svc.Dispatch(ctx, request(f, added)); err != nil {
		t.Fatal(err)
	}
	if a, fk := traits(); !a || !fk {
		t.Fatalf("archived=%v fork=%v after an installation event; it names the repository alone", a, fk)
	}
	old.Archived = false
	unarchived := webhook.Event{Kind: webhook.KindRepository, Action: "unarchived", Account: "onedr0p", Repository: old}
	if _, err := svc.Dispatch(ctx, request(f, unarchived)); err != nil {
		t.Fatal(err)
	}
	if a, fk := traits(); a || !fk {
		t.Fatalf("archived=%v fork=%v after unarchiving", a, fk)
	}
}

func TestRecordDelivery(t *testing.T) {
	svc, st, f := setupService(t)
	ctx := context.Background()
	in, _ := f.Connection("bot-ross")
	account, _ := f.Account(configfile.ForgeGitHub, "onedr0p")
	exec := func(sql string) {
		t.Helper()
		if err := st.WithAccount(ctx, account.ID(), func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, sql, in.ID())
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	last := func() time.Time {
		t.Helper()
		var at *time.Time
		if err := st.WithAccount(ctx, account.ID(), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT last_webhook_at FROM connections WHERE id = $1`, in.ID()).Scan(&at)
		}); err != nil {
			t.Fatal(err)
		}
		if at == nil {
			t.Fatal("no delivery recorded")
		}
		return *at
	}
	record := func() {
		t.Helper()
		if err := svc.RecordDelivery(ctx, in.ID()); err != nil {
			t.Fatalf("RecordDelivery: %v", err)
		}
	}
	exec(`UPDATE connections SET last_webhook_at = NULL WHERE id = $1`)
	record()
	first := last()
	record()
	if again := last(); !again.Equal(first) {
		t.Fatalf("a delivery within the minute moved the time from %s to %s", first, again)
	}
	exec(`UPDATE connections SET last_webhook_at = now() - interval '2 minutes' WHERE id = $1`)
	stale := last()
	record()
	if now := last(); !now.After(stale.Add(time.Minute)) {
		t.Fatalf("a delivery after the minute left the time at %s", now)
	}
}

// TestRecordUnsigned: an unsigned delivery is recorded apart from verified
// ones, and read back with them.
func TestRecordUnsigned(t *testing.T) {
	svc, st, f := setupService(t)
	ctx := context.Background()
	in, _ := f.Connection("bot-ross")
	account, _ := f.Account(configfile.ForgeGitHub, "onedr0p")
	read := func() store.WebhookDeliveries {
		t.Helper()
		var got map[string]store.WebhookDeliveries
		if err := st.WithAccount(ctx, account.ID(), func(tx pgx.Tx) error {
			var err error
			got, err = store.ReadWebhookDeliveries(ctx, tx)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return got[in.ID()]
	}
	// The database is shared with the other packages' tests, whose state
	// last_webhook_at is part of, so only the unsigned time is reset.
	if err := st.WithAccount(ctx, account.ID(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE connections SET last_unsigned_webhook_at = NULL WHERE id = $1`, in.ID())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	before := read()
	if before.Unsigned != nil {
		t.Fatalf("unsigned before any = %v", before.Unsigned)
	}
	if err := svc.RecordUnsigned(ctx, in.ID()); err != nil {
		t.Fatalf("RecordUnsigned: %v", err)
	}
	after := read()
	if after.Unsigned == nil || (before.Verified == nil) != (after.Verified == nil) ||
		(before.Verified != nil && !after.Verified.Equal(*before.Verified)) {
		t.Fatalf("deliveries %+v then %+v; want only the unsigned time set", before, after)
	}
}
