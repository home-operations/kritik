//go:build integration

package ingest

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/home-operations/kritik/internal/configfile"
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
	t.Setenv("TEST_SECRET", "s3cret")
	f, err := configfile.Parse([]byte(configYAML + `    filter: "!pr.draft"
    repositories:
      - name: onedr0p/disabled
        enabled: false
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyConfig(ctx, f, "test"); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	queue, err := river.NewClient(riverpgxv5.New(st.App()), &river.Config{})
	if err != nil {
		t.Fatalf("river: %v", err)
	}
	return NewService(st, queue), st, f
}

func request(f *configfile.File, ev webhook.Event) Request {
	in, tenant, _ := f.Installation("bot-ross")
	return Request{File: f, Tenant: tenant, Installation: in, Event: ev}
}

func repo(name string) *webhook.Repository {
	return &webhook.Repository{FullName: name, DefaultBranch: "main"}
}

func TestDispatchPullRequest(t *testing.T) {
	svc, st, f := setupService(t)
	ctx := context.Background()
	pr := &webhook.PullRequest{Number: 7, Title: "t", Body: "please review", Author: "devin", State: "open", HeadRef: "f", HeadSHA: "aaa", BaseRef: "main"}

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
	out, err = svc.Dispatch(ctx, request(f, webhook.Event{Kind: webhook.KindPullRequest, Action: "synchronize", Repository: repo("onedr0p/home-ops"), PullRequest: &pr2}))
	if err != nil || out.Status != Enqueued {
		t.Fatalf("a new head must enqueue: %+v, %v", out, err)
	}

	tenant, _ := f.Tenant("onedr0p")
	var headSHA, body string
	var jobsN int
	err = st.WithTenant(ctx, tenant.ID(), func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT head_sha, body FROM pull_requests WHERE number = 7`).Scan(&headSHA, &body); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM river_job WHERE kind = 'review'`).Scan(&jobsN)
	})
	if err != nil || headSHA != "bbb" || body != "please review" || jobsN != 2 {
		t.Fatalf("head_sha = %q body = %q jobs = %d err = %v; want bbb, %q and 2", headSHA, body, jobsN, err, "please review")
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
			repo   string
			pr     *webhook.PullRequest
			reason string
		}{
			{"draft filtered", "onedr0p/home-ops", &draft, "filter"},
			{"fork off by default", "onedr0p/home-ops", &fork, "fork"},
			{"disabled repository", "onedr0p/disabled", pr, "disabled"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				out, err := svc.Dispatch(ctx, request(f, webhook.Event{Kind: webhook.KindPullRequest, Action: "opened", Repository: repo(tt.repo), PullRequest: tt.pr}))
				if err != nil || out.Status != Skipped || out.Reason != tt.reason {
					t.Fatalf("out = %+v, %v; want skipped %s", out, err, tt.reason)
				}
			})
		}
	})

	t.Run("closed updates state", func(t *testing.T) {
		out, err := svc.Dispatch(ctx, request(f, webhook.Event{Kind: webhook.KindPullRequest, Action: "closed", Repository: repo("onedr0p/home-ops"), PullRequest: pr}))
		if err != nil || out.Status != Ignored || out.Reason != "closed" {
			t.Fatalf("closed = %+v, %v", out, err)
		}
		var state string
		_ = st.WithTenant(ctx, tenant.ID(), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT state FROM pull_requests WHERE number = 7`).Scan(&state)
		})
		if state != "closed" {
			t.Fatalf("state = %q", state)
		}
	})
}

func TestDispatchCommentPushInstallation(t *testing.T) {
	svc, st, f := setupService(t)
	ctx := context.Background()
	tenant, _ := f.Tenant("onedr0p")
	count := func(kind string) int {
		var n int
		_ = st.WithTenant(ctx, tenant.ID(), func(tx pgx.Tx) error {
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

	t.Run("push to default branch indexes, other branches do not", func(t *testing.T) {
		main := webhook.Event{Kind: webhook.KindPush, Repository: repo("onedr0p/home-ops"), Push: &webhook.Push{Ref: "refs/heads/main", After: "eee"}}
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

	t.Run("installation adds and removes forge-managed repositories", func(t *testing.T) {
		added := webhook.Event{Kind: webhook.KindInstallation, Action: "added", Account: "onedr0p",
			Installation: &webhook.Installation{ID: 4242, Repositories: []string{"onedr0p/new-repo", "onedr0p/disabled"}}}
		if _, err := svc.Dispatch(ctx, request(f, added)); err != nil {
			t.Fatal(err)
		}
		var newEnabled, disabledEnabled bool
		var extID int64
		_ = st.WithTenant(ctx, tenant.ID(), func(tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `SELECT enabled FROM repositories WHERE name = 'onedr0p/new-repo'`).Scan(&newEnabled); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, `SELECT enabled FROM repositories WHERE name = 'onedr0p/disabled'`).Scan(&disabledEnabled); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `SELECT external_id FROM installations WHERE name = 'bot-ross'`).Scan(&extID)
		})
		if !newEnabled || disabledEnabled || extID != 4242 {
			t.Fatalf("new=%v file-disabled=%v external_id=%d; a file-managed row must keep its flag", newEnabled, disabledEnabled, extID)
		}
		removed := webhook.Event{Kind: webhook.KindInstallation, Action: "removed", Account: "onedr0p",
			Installation: &webhook.Installation{ID: 4242, Repositories: []string{"onedr0p/new-repo"}}}
		if _, err := svc.Dispatch(ctx, request(f, removed)); err != nil {
			t.Fatal(err)
		}
		_ = st.WithTenant(ctx, tenant.ID(), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT enabled FROM repositories WHERE name = 'onedr0p/new-repo'`).Scan(&newEnabled)
		})
		if newEnabled {
			t.Fatal("removed repository should be disabled")
		}
	})
}
