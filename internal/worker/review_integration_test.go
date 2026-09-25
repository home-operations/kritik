//go:build integration

package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/executor"
	"github.com/home-operations/kritik/internal/forge"
	"github.com/home-operations/kritik/internal/ingest"
	"github.com/home-operations/kritik/internal/jobs"
	"github.com/home-operations/kritik/internal/model"
	"github.com/home-operations/kritik/internal/runner"
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

const configYAML = `
providers:
  test:
    type: openai
    baseUrl: http://unused.invalid/v1
    apiKey: { env: TEST_SECRET }
defaults:
  models:
    review: test/reviewer
  limits:
    concurrency: 1
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

// localForge stands in for GitHub: the merge-base is known from the test
// repository, the clone URL is a path, and the token is empty.
type localForge struct {
	dir string

	mu       sync.Mutex
	base     string
	tip      string
	comments map[int64]string
	authors  map[int64]string
	inline   []forge.InlineComment
	status   string
	// permissions by login; unknown logins have read access.
	permissions map[string]string
	replies     []string
}

func (l *localForge) setBase(base string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.base = base
}

func (l *localForge) MergeBase(context.Context, string, string, string, string) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.base, nil
}
func (l *localForge) CloneURL(string, string) string           { return l.dir }
func (l *localForge) GitToken(context.Context) (string, error) { return "", nil }
func (l *localForge) BotLogin(context.Context) (string, error) { return "kritik[bot]", nil }

func (l *localForge) BranchTip(context.Context, string, string, string) (string, string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.tip, "main", nil
}

// fakeEmbedder maps text to an 8-dimensional vector of character-bigram
// counts, so similar text gets similar vectors without a model.
type fakeEmbedder struct {
	mu    sync.Mutex
	calls int
}

func (f *fakeEmbedder) Embed(_ context.Context, inputs []string) ([][]float32, int64, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	out := make([][]float32, len(inputs))
	var tokens int64
	for i, s := range inputs {
		v := make([]float32, 8)
		for j := 0; j+1 < len(s); j++ {
			v[(int(s[j])*31+int(s[j+1]))%8]++
		}
		var norm float32
		for _, x := range v {
			norm += x * x
		}
		if norm > 0 {
			norm = float32(math.Sqrt(float64(norm)))
			for k := range v {
				v[k] /= norm
			}
		}
		out[i] = v
		tokens += int64(len(s) / 4)
	}
	return out, tokens, nil
}

func (l *localForge) FindComment(_ context.Context, _, _ string, _ int, _, marker string) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for id, body := range l.comments {
		if strings.Contains(body, marker) {
			return id, nil
		}
	}
	return 0, nil
}

func (l *localForge) CreateComment(_ context.Context, _, _ string, _ int, body string) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.addComment("kritik[bot]", body), nil
}

// commentBase puts fake comment ids where GitHub's are: beyond int32.
const commentBase int64 = 5_800_000_000

// addComment stores a conversation comment; the caller holds the lock.
func (l *localForge) addComment(author, body string) int64 {
	if l.comments == nil {
		l.comments = map[int64]string{}
		l.authors = map[int64]string{}
	}
	id := commentBase + int64(len(l.comments)+1)
	l.comments[id] = body
	l.authors[id] = author
	return id
}

func (l *localForge) GetComment(_ context.Context, _, _ string, id int64, inline bool) (forge.Comment, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	body, ok := l.comments[id]
	if !ok || inline {
		return forge.Comment{}, fmt.Errorf("comment %d does not exist", id)
	}
	return forge.Comment{ID: id, Author: l.authors[id], Body: body, CreatedAt: time.Unix(id-commentBase, 0)}, nil
}

func (l *localForge) ListConversation(context.Context, string, string, int) ([]forge.Comment, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]forge.Comment, 0, len(l.comments))
	for i := int64(1); i <= int64(len(l.comments)); i++ {
		id := commentBase + i
		out = append(out, forge.Comment{ID: id, Author: l.authors[id], Body: l.comments[id], CreatedAt: time.Unix(i, 0)})
	}
	return out, nil
}

func (l *localForge) ListInline(context.Context, string, string, int) ([]forge.Comment, error) {
	return nil, nil
}

func (l *localForge) Permission(_ context.Context, _, _, login string) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if p, ok := l.permissions[login]; ok {
		return p, nil
	}
	return "read", nil
}

func (l *localForge) ListOpenPullRequests(context.Context, string, string, time.Time) ([]forge.OpenPullRequest, error) {
	return nil, nil
}

func (l *localForge) ReplyInline(_ context.Context, _, _ string, _ int, _ int64, body string) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.replies = append(l.replies, body)
	return int64(len(l.replies)), nil
}

func (l *localForge) UpdateComment(_ context.Context, _, _ string, id int64, body string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.comments[id]; !ok {
		return fmt.Errorf("comment %d does not exist", id)
	}
	l.comments[id] = body
	return nil
}

func (l *localForge) CreateReview(_ context.Context, _, _ string, _ int, _ string, comments []forge.InlineComment) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.inline = append(l.inline, comments...)
	return nil
}

func (l *localForge) SetStatus(_ context.Context, _, _, _ string, state forge.StatusState, desc string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.status = string(state) + ": " + desc
	return nil
}

// fakeCompleter answers a review with one finding on the first added line
// of main.go and one that cannot be anchored, and a follow-up with a fixed
// reply.
type fakeCompleter struct {
	mu    sync.Mutex
	calls int
	users []string
}

func (f *fakeCompleter) Complete(_ context.Context, req model.CompletionRequest) (model.CompletionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.users = append(f.users, req.User)
	if req.SchemaName == "reply" {
		return model.CompletionResponse{Raw: `{"reply":"Because b is new."}`, Model: req.Model, InputTokens: 20, OutputTokens: 5}, nil
	}
	return model.CompletionResponse{
		Raw: `{"summary":"Changes main.go.","findings":[
		  {"path":"main.go","line":1,"severity":"warning","title":"first line","body":"look here"},
		  {"path":"main.go","line":500,"severity":"error","title":"off the diff","body":"dropped"}]}`,
		Model: req.Model, Upstream: "test", InputTokens: 10, OutputTokens: 5, CostUSD: 0.001,
	}, nil
}

type completers struct{ c model.Completer }

func (c *completers) For(*configfile.File, string) (model.Completer, error) { return c.c, nil }

type forges struct{ f forge.Client }

func (f *forges) For(context.Context, *configfile.Installation, int64, string) (forge.Client, error) {
	return f.f, nil
}

// testRepo builds a repository with a base commit and a head commit.
func testRepo(t *testing.T) (dir, base, head string) {
	t.Helper()
	dir = t.TempDir()
	r, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	// A depth-one fetch of a bare SHA needs the server to allow it. Real git
	// serves a local path and only advertises the capability when told to,
	// which is also what GitHub, GitLab and Forgejo do server-side.
	allowSHAFetch(t, r)
	wt, _ := r.Worktree()
	commit := func(name, content, msg string) string {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		_, _ = wt.Add(name)
		h, err := wt.Commit(msg, &git.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@x", When: time.Now()}})
		if err != nil {
			t.Fatal(err)
		}
		return h.String()
	}
	commit("other.go", "package main\n\nfunc c() {}\n", "other")
	base = commit("main.go", "package main\n", "base")
	head = commit("main.go", "package main\n\nfunc b() {}\n", "head")
	return dir, base, head
}

// checkWriteBack asserts what the fake forge and completer saw for PR 1.
func checkWriteBack(t *testing.T, lf *localForge, fc *fakeCompleter) {
	t.Helper()
	lf.mu.Lock()
	comments, inline, forgeStatus := lf.comments, lf.inline, lf.status
	lf.mu.Unlock()
	if len(comments) != 1 || !strings.Contains(comments[commentBase+1], "<!-- kritik:pr-1 -->") ||
		!strings.Contains(comments[commentBase+1], "`main.go:1` first line") {
		t.Fatalf("comments = %v", comments)
	}
	if len(inline) != 1 || inline[0].Line != 1 || forgeStatus != "success: kritik: 1 finding(s)" {
		t.Fatalf("inline = %+v status = %q", inline, forgeStatus)
	}
	fc.mu.Lock()
	calls, user := fc.calls, fc.users[0]
	fc.mu.Unlock()
	if calls != 1 || !strings.Contains(user, "Pull request #1: t") || !strings.Contains(user, "diff --git a/main.go") {
		t.Fatalf("calls = %d, prompt:\n%s", calls, user)
	}
	// Stage 4: other.go looks like the diff and is not a changed path.
	if !strings.Contains(user, "### similar: other.go") || strings.Contains(user, "### similar: main.go") {
		t.Fatalf("similar chunks missing or wrong in prompt:\n%s", user)
	}
}

// checkReviewRows asserts the findings, usage, lease and model rows a
// completed review leaves behind.
func checkReviewRows(ctx context.Context, t *testing.T, st *store.Store, tenantID, head string) {
	t.Helper()
	var findings, usage, leasesHeld int
	var modelName string
	var tokens int64
	err := st.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM findings`).Scan(&findings); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*), coalesce(sum(input_tokens + output_tokens), 0) FROM usage`).Scan(&usage, &tokens); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM model_leases WHERE job_id IS NOT NULL`).Scan(&leasesHeld); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT model FROM reviews WHERE head_sha = $1`, head).Scan(&modelName)
	})
	// Two usage rows for the review: the completion and the stage 4
	// embedding; index runs add their own.
	if err != nil || findings != 1 || usage < 2 || tokens < 15 || leasesHeld != 0 || modelName != "reviewer" {
		t.Fatalf("rows: err=%v findings=%d usage=%d tokens=%d leases=%d model=%s", err, findings, usage, tokens, leasesHeld, modelName)
	}
}

// checkIndexing indexes base in full, then head incrementally, and checks
// the generation, chunk and staging rows after each step.
func checkIndexing(ctx context.Context, t *testing.T, st *store.Store, queue *river.Client[pgx.Tx], tenantID, repoID, base, head string) {
	t.Helper()
	waitIndex := func(commit string) (status, mode string) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			err := st.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT status, mode FROM index_runs WHERE commit_sha = $1 AND finished_at IS NOT NULL
					ORDER BY created_at DESC LIMIT 1`, commit).Scan(&status, &mode)
			})
			if err == nil {
				return status, mode
			}
			time.Sleep(100 * time.Millisecond)
		}
		// Say why: the job's recorded errors and the run rows.
		var errs, runs string
		_ = st.App().QueryRow(ctx, `SELECT coalesce(string_agg(e->>'error', ' | '), '') FROM river_job j, jsonb_array_elements(j.errors) e WHERE j.kind = 'index'`).Scan(&errs)
		_ = st.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT coalesce(string_agg(status || '/' || mode || ' ' || error, ' | '), '') FROM index_runs`).Scan(&runs)
		})
		t.Fatalf("index of %s never finished; job errors: %s; runs: %s", commit[:7], errs, runs)
		return "", ""
	}
	indexRows := func() (active, activeCommit string, chunks int, mainChunks []string) {
		t.Helper()
		err := st.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `SELECT coalesce(r.active_index_run_id::text, ''), coalesce(g.commit_sha, '') FROM repositories r
				LEFT JOIN index_runs g ON g.id = r.active_index_run_id WHERE r.id = $1`, repoID).Scan(&active, &activeCommit); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM index_chunks WHERE repository_id = $1`, repoID).Scan(&chunks); err != nil {
				return err
			}
			rows, err := tx.Query(ctx, `SELECT text FROM index_chunks WHERE repository_id = $1 AND path = 'main.go' ORDER BY start_line`, repoID)
			if err != nil {
				return err
			}
			mainChunks, err = pgx.CollectRows(rows, pgx.RowTo[string])
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return active, activeCommit, chunks, mainChunks
	}
	// Index base by name first, so the incremental step has somewhere to
	// go; the second job leaves the commit to the worker, which asks the
	// fake forge for the branch tip and gets head.
	if _, err := queue.Insert(ctx, jobs.IndexArgs{TenantID: tenantID, RepositoryID: repoID, CommitSHA: base, Trigger: "onboard"}, nil); err != nil {
		t.Fatal(err)
	}
	if status, mode := waitIndex(base); status != "completed" || mode != "full" {
		t.Fatalf("base index = %s/%s", status, mode)
	}
	active, activeCommit, chunks, mainChunks := indexRows()
	if active == "" || activeCommit != base || chunks < 2 || len(mainChunks) != 1 || strings.Contains(mainChunks[0], "func b") {
		t.Fatalf("after full build: active=%q commit=%s chunks=%d main=%q", active, activeCommit, chunks, mainChunks)
	}
	if _, err := queue.Insert(ctx, jobs.IndexArgs{TenantID: tenantID, RepositoryID: repoID, Trigger: "push"}, nil); err != nil {
		t.Fatal(err)
	}
	if status, mode := waitIndex(head); status != "completed" || mode != "incremental" {
		t.Fatalf("head index = %s/%s", status, mode)
	}
	active2, activeCommit, chunks, mainChunks := indexRows()
	if active2 != active || activeCommit != head || chunks < 2 || !strings.Contains(strings.Join(mainChunks, ""), "func b") {
		var packs, logs string
		_ = st.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
			_ = tx.QueryRow(ctx, `SELECT string_agg(mode || ':' || array_to_string(changed_paths, ','), ' | ') FROM index_packs`).Scan(&packs)
			return tx.QueryRow(ctx, `SELECT string_agg(right(log_tail, 600), ' || ') FROM runner_runs WHERE kind = 'index'`).Scan(&logs)
		})
		t.Fatalf("after incremental: active=%q (was %q) commit=%s chunks=%d main=%q\npacks: %s\nlogs: %s", active2, active, activeCommit, chunks, mainChunks, packs, logs)
	}
	var staged int
	_ = st.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM index_staging`).Scan(&staged)
	})
	if staged != 0 {
		t.Fatalf("staging rows left behind: %d", staged)
	}
}

// checkFollowUps posts mentions as the forge would deliver them and checks
// qualification, the reply, the thread in the prompt, and the rate limit.
func checkFollowUps(
	ctx context.Context, t *testing.T, st *store.Store, svc *ingest.Service, lf *localForge, fc *fakeCompleter, req ingest.Request, tenantID string,
) {
	t.Helper()
	mention := func(author, body string) int64 {
		t.Helper()
		lf.mu.Lock()
		id := lf.addComment(author, body)
		lf.mu.Unlock()
		req.Event = webhook.Event{
			Kind: webhook.KindComment, Action: "created", Account: "onedr0p",
			Repository: &webhook.Repository{FullName: "onedr0p/home-ops", DefaultBranch: "main"},
			Comment:    &webhook.Comment{ID: id, Number: 1, Author: author, Body: body},
		}
		out, err := svc.Dispatch(ctx, req)
		if err != nil || out.Status != ingest.Enqueued {
			t.Fatalf("dispatch = %+v, %v", out, err)
		}
		return id
	}
	waitFollowUp := func(id int64) (status, reason string) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			err := st.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT status, reason FROM followups WHERE comment_id = $1`, id).Scan(&status, &reason)
			})
			if err == nil {
				return status, reason
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("follow-up for comment %d never recorded", id)
		return "", ""
	}
	lastComment := func() (int, string) {
		lf.mu.Lock()
		defer lf.mu.Unlock()
		return len(lf.comments), lf.comments[commentBase+int64(len(lf.comments))]
	}

	id := mention("onedr0p", "@kritik why is b here?")
	if status, reason := waitFollowUp(id); status != "answered" {
		t.Fatalf("status = %s (%s), want answered", status, reason)
	}
	if _, body := lastComment(); !strings.Contains(body, "Because b is new.") || !strings.Contains(body, "kritik follow-up with reviewer") {
		t.Fatalf("reply = %q", body)
	}
	fc.mu.Lock()
	prompt := fc.users[len(fc.users)-1]
	fc.mu.Unlock()
	for _, want := range []string{"Thread, oldest first", "<!-- kritik:pr-1 -->", "--- onedr0p", "[answer this]", "diff --git a/main.go", "Findings kritik posted"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("follow-up prompt missing %q:\n%s", want, prompt)
		}
	}
	var replyID int64
	_ = st.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT reply_comment_id FROM followups WHERE comment_id = $1`, id).Scan(&replyID)
	})
	if replyID <= commentBase {
		t.Fatalf("reply id %d not recorded as a bigint", replyID)
	}

	id = mention("outsider", "@kritik and me?")
	if status, reason := waitFollowUp(id); status != "ignored" || !strings.Contains(reason, "write is required") {
		t.Fatalf("outsider: status = %s (%s)", status, reason)
	}
	id = mention("onedr0p", "no mention here @someoneelse")
	if status, reason := waitFollowUp(id); status != "ignored" || !strings.Contains(reason, "does not mention") {
		t.Fatalf("no mention: status = %s (%s)", status, reason)
	}

	// Four more answers exhaust the hourly allowance; the next mention gets
	// one notice, the one after that nothing new.
	err := st.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO followups (tenant_id, pull_request_id, comment_id, status)
			SELECT $1, f.pull_request_id, f.comment_id + 1000 + s, 'answered' FROM followups f, generate_series(1, 4) AS s WHERE f.comment_id = $2`,
			tenantID, id)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	id = mention("onedr0p", "@kritik again?")
	if status, _ := waitFollowUp(id); status != "limited" {
		t.Fatalf("status = %s, want limited", status)
	}
	before, body := lastComment()
	if !strings.Contains(body, "limit of follow-ups") {
		t.Fatalf("limit notice not posted, last comment = %q", body)
	}
	id = mention("onedr0p", "@kritik and again?")
	status, _ := waitFollowUp(id)
	after, _ := lastComment()
	// The mention itself is one comment; no notice follows it.
	if status != "limited" || after != before+1 {
		t.Fatalf("second limited mention: status = %s, comments %d -> %d", status, before, after)
	}
}

func TestReviewWorkerEndToEnd(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	appStore, err := store.Open(ctx, store.Options{
		AppURL: env(t, "KRITIK_TEST_APP_URL"), OwnerURL: env(t, "KRITIK_TEST_OWNER_URL"),
		AppRole: "kritik_app", RunnerRole: "kritik_runner", Logger: logger,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(appStore.Close)
	if err := appStore.Migrate(ctx, "kritik_app", "kritik_runner"); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if err := appStore.EnsureIndexSchema(ctx, "kritik_app", "fake-embed", 8, false); err != nil {
		t.Fatalf("EnsureIndexSchema: %v", err)
	}
	if err := appStore.EnsureIndexSchema(ctx, "kritik_app", "other-model", 8, false); !errors.Is(err, store.ErrIndexSchemaMismatch) {
		t.Fatalf("a different embedding model must be refused without reindex, got %v", err)
	}
	runnerStore, err := store.Open(ctx, store.Options{AppURL: env(t, "KRITIK_TEST_RUNNER_URL"), Logger: logger})
	if err != nil {
		t.Fatalf("Open runner: %v", err)
	}
	t.Cleanup(runnerStore.Close)

	t.Setenv("TEST_PEM", "pem")
	t.Setenv("TEST_SECRET", "s")
	file, err := configfile.Parse([]byte(configYAML))
	if err != nil {
		t.Fatal(err)
	}
	if err := appStore.ApplyConfig(ctx, file, "test"); err != nil {
		t.Fatal(err)
	}
	current := configfile.NewCurrent(file)
	dir, base, head := testRepo(t)

	// Enqueue through the real ingest dispatcher so the rows look exactly
	// as they would from a webhook.
	insertOnly, err := river.NewClient(riverpgxv5.New(appStore.App()), &river.Config{})
	if err != nil {
		t.Fatal(err)
	}
	svc := ingest.NewService(appStore, insertOnly)
	in, tenant, _ := file.Installation("bot-ross")
	dispatch := func(headSHA string, bot bool) {
		t.Helper()
		out, err := svc.Dispatch(ctx, ingest.Request{File: file, Tenant: tenant, Installation: in, Event: webhook.Event{
			Kind: webhook.KindPullRequest, Action: "synchronize", Account: "onedr0p",
			Repository:  &webhook.Repository{FullName: "onedr0p/home-ops", DefaultBranch: "main"},
			PullRequest: &webhook.PullRequest{Number: 1, Title: "t", Author: "renovate[bot]", AuthorIsBot: bot, State: "open", HeadRef: "f", HeadSHA: headSHA, BaseRef: "main"},
		}})
		if err != nil || out.Status != ingest.Enqueued {
			t.Fatalf("dispatch = %+v, %v", out, err)
		}
	}

	lf := &localForge{dir: dir, base: base, tip: head, permissions: map[string]string{"onedr0p": "admin"}}
	fc := &fakeCompleter{}
	fe := &fakeEmbedder{}
	exec := &gateExecutor{inner: &executor.Local{Store: runnerStore}, started: make(chan executor.Spec)}
	workers := river.NewWorkers()
	river.AddWorker(workers, &Review{
		Store: appStore, Current: current, Forges: &forges{f: lf}, Completers: &completers{c: fc},
		Embedder: fe, EmbedModel: "fake-embed",
		Executor: exec, Deadline: time.Minute, Logger: logger, superviseEvery: 50 * time.Millisecond,
	})
	river.AddWorker(workers, &FollowUp{
		Store: appStore, Current: current, Forges: &forges{f: lf}, Completers: &completers{c: fc}, Logger: logger,
	})
	river.AddWorker(workers, &Index{
		Store: appStore, Current: current, Forges: &forges{f: lf}, Executor: exec,
		Embedder: fe, EmbedModel: "fake-embed", EmbedDims: 8, Deadline: time.Minute, Logger: logger,
	})
	client, err := river.NewClient(riverpgxv5.New(appStore.App()), &river.Config{
		Queues: map[string]river.QueueConfig{
			jobs.QueueReview: {MaxWorkers: 1}, jobs.QueueIndex: {MaxWorkers: 1}, jobs.QueueFollowUp: {MaxWorkers: 1},
		}, Workers: workers,
		FetchCooldown: 50 * time.Millisecond, FetchPollInterval: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Stop(context.Background()) })

	waitReview := func(headSHA string) (status, patchID, mergeBase string) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			err := appStore.WithTenant(ctx, tenant.ID(), func(tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT status, patch_id, merge_base_sha FROM reviews WHERE head_sha = $1 AND finished_at IS NOT NULL ORDER BY created_at DESC LIMIT 1`, headSHA).
					Scan(&status, &patchID, &mergeBase)
			})
			if err == nil {
				return status, patchID, mergeBase
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("no finished review for %s", headSHA)
		return "", "", ""
	}

	repoID := configfile.RepositoryID(in.ID(), "onedr0p/home-ops")
	t.Run("index builds in full, then advances incrementally", func(t *testing.T) {
		checkIndexing(ctx, t, appStore, insertOnly, tenant.ID(), repoID, base, head)
	})

	t.Run("completed with a context pack, findings and a sticky comment", func(t *testing.T) {
		dispatch(head, true)
		status, patchID, mergeBase := waitReview(head)
		if status != "completed" || patchID == "" || mergeBase != base {
			t.Fatalf("status=%s patch=%s base=%s", status, patchID, mergeBase)
		}
		checkWriteBack(t, lf, fc)
		checkReviewRows(ctx, t, appStore, tenant.ID(), head)
		var diff, phase, logTail, stages string
		var changed []string
		var heartbeat bool
		err := appStore.WithTenant(ctx, tenant.ID(), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT c.diff, c.changed_paths, c.stages::text, r.phase, r.log_tail, r.heartbeat_at IS NOT NULL FROM context_packs c
				JOIN runner_runs r ON r.id = c.runner_run_id WHERE c.head_sha = $1`, head).Scan(&diff, &changed, &stages, &phase, &logTail, &heartbeat)
		})
		if err != nil || phase != "done" || len(changed) != 1 || changed[0] != "main.go" || logTail == "" || !heartbeat {
			t.Fatalf("pack: err=%v phase=%s changed=%v log=%q heartbeat=%v", err, phase, changed, logTail, heartbeat)
		}
		// The whole three-line file is shown by the diff, so the pack is an
		// empty array rather than the pre-context '{}' default.
		if stages != "[]" {
			t.Fatalf("stages = %s", stages)
		}
		if diff == "" {
			t.Fatal("diff should not be empty")
		}
	})

	t.Run("follow-up answers a qualifying mention and rate-limits the thread", func(t *testing.T) {
		checkFollowUps(ctx, t, appStore, svc, lf, fc, ingest.Request{File: file, Tenant: tenant, Installation: in}, tenant.ID())
	})

	t.Run("bot PR with the same patch id is skipped", func(t *testing.T) {
		// Rebase the same change: new head commit, identical diff.
		r, _ := git.PlainOpen(dir)
		wt, _ := r.Worktree()
		_ = wt.Reset(&git.ResetOptions{Commit: plumbing.NewHash(base), Mode: git.HardReset})
		_ = os.WriteFile(filepath.Join(dir, "other.txt"), []byte("x\n"), 0o644)
		_, _ = wt.Add("other.txt")
		newBase, _ := wt.Commit("unrelated", &git.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@x", When: time.Now()}})
		_ = os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc b() {}\n"), 0o644)
		_, _ = wt.Add("main.go")
		newHead, _ := wt.Commit("head again", &git.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@x", When: time.Now()}})

		// The fake forge reports the new merge-base for the rebased head.
		lf.setBase(newBase.String())
		dispatch(newHead.String(), true)
		status, _, _ := waitReview(newHead.String())
		if status != "skipped" {
			t.Fatalf("status = %s, want skipped for an unchanged bot patch", status)
		}
	})

	t.Run("superseded when the head moves before the job runs", func(t *testing.T) {
		// Insert a job for a head that is no longer the PR's head.
		res, err := insertOnly.Insert(ctx, jobs.ReviewArgs{TenantID: tenant.ID(), RepositoryID: configfile.RepositoryID(in.ID(), "onedr0p/home-ops"), Number: 1, HeadSHA: "0000000000000000000000000000000000000000", Trigger: "poll"}, nil)
		if err != nil || res.UniqueSkippedAsDuplicate {
			t.Fatalf("insert = %+v, %v", res, err)
		}
		status, _, _ := waitReview("0000000000000000000000000000000000000000")
		if status != "superseded" {
			t.Fatalf("status = %s, want superseded", status)
		}
	})

	t.Run("supervision ends a running review", func(t *testing.T) {
		checkSupervision(ctx, t, appStore, exec, dispatch, waitReview, tenant.ID(), repoID)
	})
}

// checkSupervision holds review runs open and moves the head, then stales
// the heartbeat, expecting supervision to cancel each run.
func checkSupervision(
	ctx context.Context, t *testing.T, appStore *store.Store, exec *gateExecutor, dispatch func(string, bool),
	waitReview func(string) (string, string, string), tenantID, repoID string,
) {
	exec.setBlock(true)
	started := func() executor.Spec {
		t.Helper()
		select {
		case spec := <-exec.started:
			return spec
		case <-time.After(20 * time.Second):
			t.Fatal("the review runner never started")
			return executor.Spec{}
		}
	}
	reviewError := func(headSHA string) string {
		t.Helper()
		var text string
		err := appStore.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT error FROM reviews WHERE head_sha = $1 ORDER BY created_at DESC LIMIT 1`, headSHA).Scan(&text)
		})
		if err != nil {
			t.Fatal(err)
		}
		return text
	}

	t.Run("superseded when the head moves while the runner works", func(t *testing.T) {
		const running, newer = "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222"
		dispatch(running, false)
		spec := started()
		if spec.Job.Version != runner.SpecVersion || spec.Job.Kind != runner.KindReview || spec.Job.Head != running || spec.Job.Base == "" {
			t.Fatalf("job = %+v", spec.Job)
		}
		err := appStore.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE pull_requests SET head_sha = $2 WHERE repository_id = $1 AND number = 1`, repoID, newer)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if status, _, _ := waitReview(running); status != "superseded" {
			t.Fatalf("status = %s, want superseded", status)
		}
	})

	t.Run("failed when the runner heartbeat goes stale", func(t *testing.T) {
		const running = "3333333333333333333333333333333333333333"
		dispatch(running, false)
		spec := started()
		err := appStore.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE runner_runs SET heartbeat_at = now() - interval '5 minutes' WHERE id = $1`, spec.RunID)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if status, _, _ := waitReview(running); status != "failed" {
			t.Fatalf("status = %s, want failed", status)
		}
		if text := reviewError(running); text != "runner heartbeat lost" {
			t.Fatalf("error = %q", text)
		}
	})
}

// gateExecutor runs the real runner, or once blocked holds each review run
// until supervision cancels it, the way a Job runs until it is deleted.
type gateExecutor struct {
	inner   executor.Executor
	started chan executor.Spec

	mu    sync.Mutex
	block bool
}

func (g *gateExecutor) setBlock(b bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.block = b
}

func (g *gateExecutor) Run(ctx context.Context, spec executor.Spec) executor.Result {
	if err := spec.Job.Validate(); err != nil {
		return executor.Result{Err: err}
	}
	g.mu.Lock()
	block := g.block
	g.mu.Unlock()
	if !block || spec.Job.Kind != runner.KindReview {
		return g.inner.Run(ctx, spec)
	}
	select {
	case g.started <- spec:
	case <-ctx.Done():
	}
	<-ctx.Done()
	return executor.Result{JobName: "kritik-run-blocked", Err: context.Cause(ctx)}
}

func allowSHAFetch(t *testing.T, r *git.Repository) {
	t.Helper()
	cfg, err := r.Config()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Raw.SetOption("uploadpack", "", "allowReachableSHA1InWant", "true")
	if err := r.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
}
