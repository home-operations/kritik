//go:build integration

package worker

import (
	"context"
	"database/sql"
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
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/executor"
	"github.com/home-operations/kritik/internal/forge"
	"github.com/home-operations/kritik/internal/ingest"
	"github.com/home-operations/kritik/internal/jobs"
	"github.com/home-operations/kritik/internal/model"
	"github.com/home-operations/kritik/internal/review"
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
	permissions map[string]forge.Permission
	replies     []string
}

func (l *localForge) setBase(base string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.base = base
}

func (l *localForge) MergeBase(context.Context, string, string, int, string, string) (string, error) {
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

func (l *localForge) GetComment(_ context.Context, _, _ string, _ int, id int64, inline bool) (forge.Comment, error) {
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

func (l *localForge) Permission(_ context.Context, _, _, login string) (forge.Permission, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if p, ok := l.permissions[login]; ok {
		return p, nil
	}
	return forge.PermissionRead, nil
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

func (l *localForge) LineRanges() bool { return true }

func (l *localForge) FileURL(owner, repo, sha, path string, line, _ int) string {
	return fmt.Sprintf("local://%s/%s/%s/%s#L%d", owner, repo, sha, path, line)
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
	mu      sync.Mutex
	calls   int
	users   []string
	systems []string
	block   bool
	started chan struct{}
	// answered, if set, runs once a review/findings call has its answer and
	// before Complete returns it.
	answered func(ctx context.Context)
}

func (f *fakeCompleter) setAnswered(fn func(ctx context.Context)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answered = fn
}

// setBlock arms or disarms blocking the review/findings model call (never
// the follow-up "reply" call) inside Complete until the caller's ctx is
// done, so a test can cancel a review while it is mid-publish. started, if
// non-nil, receives a signal once a blocked call is actually in progress.
func (f *fakeCompleter) setBlock(b bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.block = b
}

func (f *fakeCompleter) Complete(ctx context.Context, req model.CompletionRequest) (model.CompletionResponse, error) {
	f.mu.Lock()
	f.calls++
	f.users = append(f.users, req.User)
	f.systems = append(f.systems, req.System)
	block := f.block && req.SchemaName != "reply"
	started := f.started
	answered := f.answered
	f.mu.Unlock()
	if block {
		if started != nil {
			select {
			case started <- struct{}{}:
			case <-ctx.Done():
			}
		}
		<-ctx.Done()
		return model.CompletionResponse{}, context.Cause(ctx)
	}
	if req.SchemaName == "reply" {
		return model.CompletionResponse{Raw: `{"reply":"Because b is new."}`, Model: req.Model, InputTokens: 20, OutputTokens: 5}, nil
	}
	if answered != nil {
		answered(ctx)
	}
	return model.CompletionResponse{
		Raw: `{"summary":{"take":"Changes main.go.","praise":["Small and focused"]},"findings":[
		  {"path":"main.go","line":1,"severity":"important","title":"first line","explanation":"look here","suggested_fix":"do this"},
		  {"path":"main.go","line":500,"severity":"blocking","title":"off the diff","explanation":"dropped"}]}`,
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
	// checkActions later resets the branch back to base and commits again to
	// mint fresh heads (rerunHead, canceling, ...), which orphans head from
	// every branch. A lightweight tag keeps it reachable so a later fetch of
	// head (e.g. EnqueueReindex's BranchTip-resolved commit) still satisfies
	// upload-pack's allowReachableSHA1InWant check instead of failing "not
	// our ref".
	if _, err := r.CreateTag("kritik-head", plumbing.NewHash(head), nil); err != nil {
		t.Fatal(err)
	}
	return dir, base, head
}

// checkWriteBack asserts what the fake forge and completer saw for PR 1.
func checkWriteBack(t *testing.T, lf *localForge, fc *fakeCompleter) {
	t.Helper()
	lf.mu.Lock()
	comments, inline, forgeStatus := lf.comments, lf.inline, lf.status
	lf.mu.Unlock()
	sticky := comments[commentBase+1]
	if len(comments) != 1 || !strings.HasPrefix(sticky, "<!-- kritik:pr-1 -->\n") ||
		!strings.Contains(sticky, "- **[important]** [`main.go:1`](local://onedr0p/home-ops/") ||
		!strings.Contains(sticky, "/main.go#L1) first line") || !strings.Contains(sticky, "**1 finding** · 0 blocking · 1 important · 0 nit") ||
		!strings.Contains(sticky, "- Small and focused") || !strings.Contains(sticky, "_1 finding(s) were dropped (unanchored: 1)._") {
		t.Fatalf("comments = %v", comments)
	}
	if len(inline) != 1 || inline[0].Line != 1 || !strings.Contains(inline[0].Body, "**[important]** **first line**") ||
		!strings.Contains(inline[0].Body, "do this") || forgeStatus != "success: kritik: 1 finding(s)" {
		t.Fatalf("inline = %+v status = %q", inline, forgeStatus)
	}
	fc.mu.Lock()
	calls, user := fc.calls, fc.users[0]
	fc.mu.Unlock()
	if calls != 1 || !strings.Contains(user, "Pull request #1: t") || !strings.Contains(user, "diff --git a/main.go") ||
		!strings.Contains(user, "<description>\nAdds b.\n</description>") {
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
	var modelName, take, explanation, fix, fingerprint string
	var tokens int64
	err := st.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*), min(explanation), min(suggested_fix), min(fingerprint) FROM findings`).
			Scan(&findings, &explanation, &fix, &fingerprint); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT summary->>'take' FROM reviews WHERE head_sha = $1`, head).Scan(&take); err != nil {
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
	wantPrint := review.Fingerprint(review.Finding{Path: "main.go", Title: "first line"})
	if take != "Changes main.go." || explanation != "look here" || fix != "do this" || fingerprint != wantPrint {
		t.Fatalf("contract rows: take=%q explanation=%q fix=%q fingerprint=%q", take, explanation, fix, fingerprint)
	}
}

// checkContextPack dispatches a review of head, waits for it to complete,
// and asserts both the write-back/review-row contract (via checkWriteBack
// and checkReviewRows) and the context pack the run recorded: its diff,
// changed paths, empty stages array and heartbeat, joined through the
// runner_runs row the pack's completion depends on.
func checkContextPack(
	ctx context.Context, t *testing.T, appStore *store.Store, lf *localForge, fc *fakeCompleter,
	dispatch func(headSHA string, bot bool), waitReview func(headSHA string) (status, patchID, mergeBase string),
	tenantID, head, base string,
) {
	t.Helper()
	dispatch(head, true)
	status, patchID, mergeBase := waitReview(head)
	if status != "completed" || patchID == "" || mergeBase != base {
		t.Fatalf("status=%s patch=%s base=%s", status, patchID, mergeBase)
	}
	checkWriteBack(t, lf, fc)
	checkReviewRows(ctx, t, appStore, tenantID, head)
	var diff, phase, logTail, stages string
	var changed []string
	var heartbeat bool
	err := appStore.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
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
}

// checkIndexing indexes base in full, then head incrementally, and checks
// the generation, chunk and staging rows after each step.
func checkIndexing(ctx context.Context, t *testing.T, st *store.Store, queue *river.Client[pgx.Tx], tenantID, repoID, base, head string) {
	t.Helper()
	waitIndex := func(commit string) (status, mode string) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			// An incremental step advances the active generation's commit
			// before its own row finishes, so only read once nothing is
			// running; the newest finished row is then the step itself.
			err := st.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT status, mode FROM index_runs WHERE commit_sha = $1 AND finished_at IS NOT NULL
					AND NOT EXISTS (SELECT 1 FROM index_runs WHERE status = 'running')
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
	for _, want := range []string{"Thread, oldest first", "<!-- kritik:pr-1 -->", "--- onedr0p", "[answer this]", "diff --git a/main.go", "Findings kritik posted", "main.go:1 [important] first line: look here", "<description>\nAdds b.\n</description>"} {
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
	dispatchPR := func(number int, headSHA string, bot bool, labels ...string) {
		t.Helper()
		out, err := svc.Dispatch(ctx, ingest.Request{File: file, Tenant: tenant, Installation: in, Event: webhook.Event{
			Kind: webhook.KindPullRequest, Action: "synchronize", Account: "onedr0p",
			Repository:  &webhook.Repository{FullName: "onedr0p/home-ops", DefaultBranch: "main"},
			PullRequest: &webhook.PullRequest{Number: number, Title: "t", Body: "Adds b.", Author: "renovate[bot]", AuthorIsBot: bot, State: "open", HeadRef: "f", HeadSHA: headSHA, BaseRef: "main", Labels: labelled(labels)},
		}})
		if err != nil || out.Status != ingest.Enqueued {
			t.Fatalf("dispatch = %+v, %v", out, err)
		}
	}
	dispatch := func(headSHA string, bot bool) { t.Helper(); dispatchPR(1, headSHA, bot) }

	lf := &localForge{dir: dir, base: base, tip: head, permissions: map[string]forge.Permission{"onedr0p": forge.PermissionAdmin}}
	fc := &fakeCompleter{started: make(chan struct{}, 1)}
	fe := &fakeEmbedder{}
	exec := &gateExecutor{inner: &executor.Local{Store: runnerStore}, started: make(chan executor.Spec)}
	deadline := &jobDeadline{}
	workers := river.NewWorkers()
	wb := Base{Store: appStore, Current: current, Forges: &forges{f: lf}, Logger: logger}
	river.AddWorker(workers, &Review{
		Base: wb, Completers: &completers{c: fc}, Embedder: fe, EmbedModel: "fake-embed", Executor: exec, Deadline: time.Minute,
		superviseEvery: 50 * time.Millisecond,
	})
	river.AddWorker(workers, &FollowUp{Base: wb, Completers: &completers{c: fc}})
	river.AddWorker(workers, &Index{
		Base: wb, Executor: exec, Embedder: fe, EmbedModel: "fake-embed", EmbedDims: 8, Deadline: time.Minute,
	})
	client, err := river.NewClient(riverpgxv5.New(appStore.App()), &river.Config{
		Queues: map[string]river.QueueConfig{
			jobs.QueueReview: {MaxWorkers: 1}, jobs.QueueIndex: {MaxWorkers: 1}, jobs.QueueFollowUp: {MaxWorkers: 1},
		}, Workers: workers,
		FetchCooldown: 50 * time.Millisecond, FetchPollInterval: 100 * time.Millisecond,
		Middleware: []rivertype.Middleware{river.WorkerMiddlewareFunc(deadline.wrap)},
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

	// waitReviewCount is waitReview's counting counterpart, for a head SHA
	// that already has a finished review: it waits until at least min finished
	// reviews exist for headSHA before reading the latest one, so a second
	// wait for the same head cannot re-match a still-lingering earlier row
	// (the database is the sole source of truth for both the count and the
	// row, so there is no client-clock race to get wrong).
	waitReviewCount := func(headSHA string, min int) (status, patchID, mergeBase string) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			err := appStore.WithTenant(ctx, tenant.ID(), func(tx pgx.Tx) error {
				var count int
				if err := tx.QueryRow(ctx,
					`SELECT count(*) FROM reviews WHERE head_sha = $1 AND finished_at IS NOT NULL`, headSHA).
					Scan(&count); err != nil {
					return err
				}
				if count < min {
					return pgx.ErrNoRows
				}
				return tx.QueryRow(ctx, `SELECT status, patch_id, merge_base_sha FROM reviews WHERE head_sha = $1 AND finished_at IS NOT NULL ORDER BY created_at DESC LIMIT 1`, headSHA).
					Scan(&status, &patchID, &mergeBase)
			})
			if err == nil {
				return status, patchID, mergeBase
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("no %dth finished review for %s", min, headSHA)
		return "", "", ""
	}

	repoID := configfile.RepositoryID(in.ID(), "onedr0p/home-ops")
	t.Run("index builds in full, then advances incrementally", func(t *testing.T) {
		checkIndexing(ctx, t, appStore, insertOnly, tenant.ID(), repoID, base, head)
	})

	t.Run("completed with a context pack, findings and a sticky comment", func(t *testing.T) {
		checkContextPack(ctx, t, appStore, lf, fc, dispatch, waitReview, tenant.ID(), head, base)
	})

	t.Run("follow-up answers a qualifying mention and rate-limits the thread", func(t *testing.T) {
		checkFollowUps(ctx, t, appStore, svc, lf, fc, ingest.Request{File: file, Tenant: tenant, Installation: in}, tenant.ID())
	})

	t.Run("bot PR with the same patch id is skipped", func(t *testing.T) {
		checkBotPatchIDSkip(ctx, t, dir, base, lf, dispatch, waitReviewCount, insertOnly, tenant.ID(), repoID)
	})

	t.Run("the merge-base .kritik.yaml skips, instructs and templates", func(t *testing.T) {
		checkRepoConfig(ctx, t, appStore, lf, fc, dir, base, dispatchPR, waitReview, tenant.ID())
	})

	t.Run("re-reviews build on the last reviewed head", func(t *testing.T) {
		checkIncremental(ctx, t, appStore, lf, fc, dir, base, dispatchPR, waitReview, tenant.ID())
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

	t.Run("a job that ends after the runner still ends its review", func(t *testing.T) {
		checkJobEnded(ctx, t, appStore, insertOnly, exec, fc, deadline, dispatch, waitReview, dir, base, tenant.ID(), lf)
	})

	t.Run("worker actions: rerun, cancel and forced reindex", func(t *testing.T) {
		checkActions(ctx, t, appStore, insertOnly, exec, dispatch, waitReview, dir, base, head, tenant.ID(), repoID, lf, fc)
	})
}

func labelled(names []string) []webhook.Label {
	labels := make([]webhook.Label, 0, len(names))
	for _, n := range names {
		labels = append(labels, webhook.Label{Name: n, Color: "ededed"})
	}
	return labels
}

// checkBotPatchIDSkip rebases the same change onto a new head (new commit,
// identical diff) and asserts a bot PR with the same patch id is skipped on
// dispatch, then that a manual re-run of that same unchanged patch bypasses
// the skip.
func checkBotPatchIDSkip(
	ctx context.Context, t *testing.T, dir, base string, lf *localForge,
	dispatch func(headSHA string, bot bool), waitReviewCount func(headSHA string, min int) (status, patchID, mergeBase string),
	insertOnly *river.Client[pgx.Tx], tenantID, repoID string,
) {
	t.Helper()
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
	status, _, _ := waitReviewCount(newHead.String(), 1)
	if status != "skipped" {
		t.Fatalf("status = %s, want skipped for an unchanged bot patch", status)
	}

	// A manual re-run of the same unchanged bot patch must bypass the skip:
	// it inserts directly (as the web API's EnqueueRerun would) rather than
	// through the ingest dispatcher, since only the trigger -- not the diff
	// -- differs from the dispatch above. It shares newHead's SHA with the
	// bot dispatch above, so waiting for a second finished review (rather
	// than reusing waitReview, which would happily re-match the bot
	// dispatch's already-finished row) is required to observe this job's own
	// outcome rather than the previous one's.
	res, err := insertOnly.Insert(ctx, jobs.ReviewArgs{
		TenantID: tenantID, RepositoryID: repoID, Number: 1, HeadSHA: newHead.String(),
		Trigger: jobs.TriggerManual, Request: uuid.NewString(),
	}, nil)
	if err != nil || res.UniqueSkippedAsDuplicate {
		t.Fatalf("insert manual rerun = %+v, %v", res, err)
	}
	status, _, _ = waitReviewCount(newHead.String(), 2)
	if status != "completed" {
		t.Fatalf("status = %s, want completed: a manual trigger must bypass the bot patch-id skip", status)
	}
}

// checkRepoConfig commits a .kritik.yaml with a skip rule, instructions
// and a summary template onto a new merge base, then reviews pull requests
// against it.
func checkRepoConfig(
	ctx context.Context, t *testing.T, appStore *store.Store, lf *localForge, fc *fakeCompleter, dir, base string,
	dispatchPR func(int, string, bool, ...string), waitReview func(string) (string, string, string), tenantID string,
) {
	t.Helper()
	r, _ := git.PlainOpen(dir)
	wt, _ := r.Worktree()
	if err := wt.Reset(&git.ResetOptions{Commit: plumbing.NewHash(base), Mode: git.HardReset}); err != nil {
		t.Fatal(err)
	}
	commit := func(msg string, files map[string]string) string {
		t.Helper()
		for name, content := range files {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := wt.Add(name); err != nil {
				t.Fatal(err)
			}
		}
		h, err := wt.Commit(msg, &git.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@x", When: time.Now()}})
		if err != nil {
			t.Fatal(err)
		}
		return h.String()
	}
	cfgBase := commit("configure kritik", map[string]string{
		".kritik.yaml": `filter: '!pr.labels.exists(l, l.name == "skip-review")'
skip:
  onlyPaths: ["docs/**", ".kritik.yaml"]
review:
  instructions: [".kritik/rules.md"]
  templates:
    summary: ".kritik/summary.md.tmpl"
`,
		".kritik/rules.md":        "Flag every TODO left in code.\n",
		".kritik/summary.md.tmpl": "Custom summary for #{{ .Number }}: {{ .Result.Summary.Take }}\n",
	})
	docsHead := commit("docs", map[string]string{"docs/guide.md": "# Guide\n"})
	loosened := commit("drop the skip rule", map[string]string{".kritik.yaml": "review: {}\n"})
	lf.setBase(cfgBase)

	fc.mu.Lock()
	callsBefore := fc.calls
	fc.mu.Unlock()
	skipReason := func(head string) string {
		var reason string
		if err := appStore.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT skip_reason FROM reviews WHERE head_sha = $1 AND finished_at IS NOT NULL`, head).Scan(&reason)
		}); err != nil {
			t.Fatal(err)
		}
		return reason
	}
	for _, head := range []string{docsHead, loosened} {
		dispatchPR(2, head, false)
		if status, _, _ := waitReview(head); status != "skipped" {
			t.Fatalf("status = %s, want skipped: the merge-base skip rule covers every changed path", status)
		}
		lf.mu.Lock()
		forgeStatus := lf.status
		lf.mu.Unlock()
		if forgeStatus != "success: kritik: skipped (only skipped paths changed)" || skipReason(head) != "only_skipped_paths" {
			t.Fatalf("status = %q reason = %q", forgeStatus, skipReason(head))
		}
	}
	fc.mu.Lock()
	calls := fc.calls
	fc.mu.Unlock()
	if calls != callsBefore {
		t.Fatalf("a skipped review called the model %d time(s)", calls-callsBefore)
	}

	if err := wt.Reset(&git.ResetOptions{Commit: plumbing.NewHash(cfgBase), Mode: git.HardReset}); err != nil {
		t.Fatal(err)
	}
	codeHead := commit("code", map[string]string{"main.go": "package main\n\nfunc d() {}\n"})
	dispatchPR(3, codeHead, false)
	if status, _, _ := waitReview(codeHead); status != "completed" {
		t.Fatalf("status = %s, want completed", status)
	}
	fc.mu.Lock()
	system := fc.systems[len(fc.systems)-1]
	fc.mu.Unlock()
	if !strings.Contains(system, "\n\n## Repository instructions\n\n") || !strings.HasSuffix(system, "\n\nFlag every TODO left in code.") {
		t.Fatalf("system prompt does not carry the instructions:\n%s", system)
	}
	lf.mu.Lock()
	var sticky string
	for _, body := range lf.comments {
		if strings.HasPrefix(body, "<!-- kritik:pr-3 -->\n") {
			sticky = body
		}
	}
	lf.mu.Unlock()
	if !strings.HasPrefix(sticky, "<!-- kritik:pr-3 -->\nCustom summary for #3: Changes main.go.") {
		t.Fatalf("sticky comment for PR 3 = %q", sticky)
	}

	// The same kind of change carrying the label the filter excludes.
	labelledHead := commit("labelledHead", map[string]string{"main.go": "package main\n\nfunc e() {}\n"})
	dispatchPR(4, labelledHead, false, "skip-review")
	if status, _, _ := waitReview(labelledHead); status != "skipped" || skipReason(labelledHead) != "filtered" {
		t.Fatalf("status = %s reason = %q, want skipped by the label filter", status, skipReason(labelledHead))
	}
	lf.mu.Lock()
	forgeStatus := lf.status
	lf.mu.Unlock()
	if forgeStatus != "success: kritik: skipped (filtered by .kritik.yaml)" {
		t.Fatalf("status = %q", forgeStatus)
	}
}

// checkIncremental reviews a pull request, pushes a commit on top, and then
// force-pushes it away: the second review is incremental and does not post
// the repeated finding inline again, the third is full because the head it
// would build on is gone.
func checkIncremental(
	ctx context.Context, t *testing.T, appStore *store.Store, lf *localForge, fc *fakeCompleter, dir, base string,
	dispatchPR func(int, string, bool, ...string), waitReview func(string) (string, string, string), tenantID string,
) {
	t.Helper()
	const number = 5
	r, _ := git.PlainOpen(dir)
	wt, _ := r.Worktree()
	commit := func(content string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := wt.Add("main.go"); err != nil {
			t.Fatal(err)
		}
		h, err := wt.Commit("change", &git.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@x", When: time.Now()}})
		if err != nil {
			t.Fatal(err)
		}
		return h.String()
	}
	reset := func() {
		t.Helper()
		if err := wt.Reset(&git.ResetOptions{Commit: plumbing.NewHash(base), Mode: git.HardReset}); err != nil {
			t.Fatal(err)
		}
	}
	reviewHead := func(head string) (reviewScopeRow, string, int) {
		t.Helper()
		lf.mu.Lock()
		inlineBefore := len(lf.inline)
		lf.mu.Unlock()
		dispatchPR(number, head, false)
		if status, _, _ := waitReview(head); status != "completed" {
			t.Fatalf("status = %s, want completed", status)
		}
		fc.mu.Lock()
		prompt := fc.users[len(fc.users)-1]
		fc.mu.Unlock()
		lf.mu.Lock()
		newInline := len(lf.inline) - inlineBefore
		lf.mu.Unlock()
		return scopeRow(ctx, t, appStore, tenantID, head), prompt, newInline
	}

	reset()
	lf.setBase(base)
	first := commit("package main\n\nfunc f1() {}\n")
	firstRow, prompt, inline := reviewHead(first)
	if firstRow.scope != "full" || firstRow.reason != "no completed review to build on" || firstRow.prior != "" || inline != 1 {
		t.Fatalf("first review = %+v, %d inline comment(s)", firstRow, inline)
	}
	if strings.Contains(prompt, "Changed since the last review") {
		t.Fatalf("a first review has no incremental sections:\n%s", prompt)
	}
	if p := postedInline(ctx, t, appStore, tenantID, firstRow.id); len(p) != 1 || !p[0] {
		t.Fatalf("posted_inline = %v", p)
	}

	second := commit("package main\n\nfunc f1() {}\n\nfunc f2() {}\n")
	secondRow, prompt, inline := reviewHead(second)
	if secondRow.scope != "incremental" || secondRow.reason != "" || secondRow.prior != firstRow.id || inline != 0 {
		t.Fatalf("second review = %+v, %d inline comment(s); want incremental on %s with nothing posted inline again",
			secondRow, inline, firstRow.id)
	}
	for _, want := range []string{
		"Changed since the last review (" + first[:7], "+func f2() {}",
		"Findings from the last review (verify each; report again only if still present)", "- main.go:1 [important] first line: look here",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("missing %q in the incremental prompt:\n%s", want, prompt)
		}
	}
	if p := postedInline(ctx, t, appStore, tenantID, secondRow.id); len(p) != 1 || !p[0] {
		t.Fatalf("a finding carried from the last review keeps posted_inline, got %v", p)
	}
	checkIncrementalRecord(ctx, t, appStore, lf, tenantID, secondRow.id, first)

	// Force-push: the second head is no longer reachable from any ref.
	reset()
	third := commit("package main\n\nfunc f3() {}\n")
	thirdRow, prompt, inline := reviewHead(third)
	if thirdRow.scope != "full" || thirdRow.reason != "prior head unreachable" || thirdRow.prior != secondRow.id || inline != 0 {
		t.Fatalf("third review = %+v, %d inline comment(s)", thirdRow, inline)
	}
	if strings.Contains(prompt, "Changed since the last review") || strings.Contains(prompt, "Findings from the last review") {
		t.Fatalf("a full re-review has no incremental sections:\n%s", prompt)
	}

}

type reviewScopeRow struct{ id, scope, reason, prior string }

func scopeRow(ctx context.Context, t *testing.T, appStore *store.Store, tenantID, head string) reviewScopeRow {
	t.Helper()
	var out reviewScopeRow
	if err := appStore.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id, scope, scope_reason, coalesce(prior_review_id::text, '') FROM reviews
			WHERE head_sha = $1 AND status = 'completed' ORDER BY created_at DESC LIMIT 1`, head).Scan(&out.id, &out.scope, &out.reason, &out.prior)
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func postedInline(ctx context.Context, t *testing.T, appStore *store.Store, tenantID, reviewID string) []bool {
	t.Helper()
	var out []bool
	if err := appStore.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT posted_inline FROM findings WHERE review_id = $1`, reviewID)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowTo[bool])
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// checkIncrementalRecord asserts the context pack and sticky comment of an
// incremental review of PR 5 that builds on prior.
func checkIncrementalRecord(ctx context.Context, t *testing.T, appStore *store.Store, lf *localForge, tenantID, reviewID, prior string) {
	t.Helper()
	var priorHead string
	var deltaPaths []string
	if err := appStore.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT c.prior_head_sha, c.delta_paths FROM context_packs c JOIN runner_runs rr ON rr.id = c.runner_run_id
			WHERE rr.review_id = $1`, reviewID).Scan(&priorHead, &deltaPaths)
	}); err != nil || priorHead != prior || len(deltaPaths) != 1 || deltaPaths[0] != "main.go" {
		t.Fatalf("pack prior = %s delta = %v err = %v", priorHead, deltaPaths, err)
	}
	lf.mu.Lock()
	var sticky string
	for _, body := range lf.comments {
		if strings.HasPrefix(body, "<!-- kritik:pr-5 -->\n") {
			sticky = body
		}
	}
	lf.mu.Unlock()
	if !strings.Contains(sticky, "_Incremental review of the changes since `"+prior[:7]+"`._") ||
		!strings.Contains(sticky, "/main.go#L1) first line") {
		t.Fatalf("sticky comment = %q", sticky)
	}

}

// checkSupervision holds review runs open and moves the head, then stales
// the heartbeat, expecting supervision to cancel each run.
func checkSupervision(
	ctx context.Context, t *testing.T, appStore *store.Store, exec *gateExecutor, dispatch func(string, bool),
	waitReview func(string) (string, string, string), tenantID, repoID string,
) {
	exec.setBlock(true)
	t.Cleanup(func() { exec.setBlock(false) })
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

// checkActions exercises the web dashboard's enqueue helpers (internal/jobs/
// actions.go) against a real running worker: a manual rerun of a completed
// head, a cancel of a review while its runner is blocked, a cancel rejected
// once the review has finished, and a forced full reindex over an active
// incremental generation. Each scenario lives in its own helper (below) so
// this dispatcher stays trivial to read.
func checkActions(
	ctx context.Context, t *testing.T, appStore *store.Store, insertOnly *river.Client[pgx.Tx], exec *gateExecutor,
	dispatch func(string, bool), waitReview func(string) (string, string, string),
	dir, base, head, tenantID, repoID string, lf *localForge, fc *fakeCompleter,
) {
	t.Helper()

	var rerunHead string
	t.Run("EnqueueRerun produces a second completed review of the same head", func(t *testing.T) {
		rerunHead = checkEnqueueRerun(ctx, t, appStore, insertOnly, dispatch, waitReview, dir, base, tenantID, repoID)
	})
	t.Run("RequestCancel on a completed review is rejected", func(t *testing.T) {
		checkRequestCancelRejected(ctx, t, appStore, insertOnly, tenantID, rerunHead)
	})
	t.Run("RequestCancel on another tenant's review is rejected", func(t *testing.T) {
		checkRequestCancelCrossTenant(ctx, t, appStore, insertOnly, tenantID, rerunHead)
	})
	t.Run("RequestCancel ends a running review as canceled with no retry", func(t *testing.T) {
		checkRequestCancelRunning(ctx, t, appStore, insertOnly, exec, dispatch, waitReview, tenantID, lf)
	})
	t.Run("RequestCancel ends a queued (prepared) review as canceled", func(t *testing.T) {
		checkRequestCancelPrepared(ctx, t, appStore, insertOnly, fc, dispatch, waitReview, dir, base, tenantID)
	})
	t.Run("EnqueueReindex forces a full generation even when one is active", func(t *testing.T) {
		checkEnqueueReindex(ctx, t, appStore, insertOnly, tenantID, repoID, head)
	})
	t.Run("EnqueueReindex reports the sentinels it branches on", func(t *testing.T) {
		checkEnqueueReindexSentinels(ctx, t, appStore, insertOnly, tenantID, repoID)
	})
	t.Run("EnqueueRerun on a closed pull request is rejected", func(t *testing.T) {
		checkEnqueueRerunClosed(ctx, t, appStore, insertOnly, tenantID, repoID)
	})
}

// countReviewsByStatus returns how many reviews rows exist for headSHA with
// exactly status, scoped to the tenant.
func countReviewsByStatus(ctx context.Context, t *testing.T, appStore *store.Store, tenantID, headSHA, status string) int {
	t.Helper()
	var n int
	err := appStore.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM reviews WHERE head_sha = $1 AND status = $2`, headSHA, status).Scan(&n)
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// latestReviewID returns the most recently created review's id for headSHA.
func latestReviewID(ctx context.Context, t *testing.T, appStore *store.Store, tenantID, headSHA string) string {
	t.Helper()
	var id string
	err := appStore.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM reviews WHERE head_sha = $1 ORDER BY created_at DESC LIMIT 1`, headSHA).Scan(&id)
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// newTestAccount inserts a bare accounts row and returns its id, standing in
// for whichever human account RequestCancel's by should record.
func newTestAccount(ctx context.Context, t *testing.T, appStore *store.Store, tenantID string) string {
	t.Helper()
	var id string
	err := appStore.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO accounts DEFAULT VALUES RETURNING id`).Scan(&id)
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// commitOnBase resets dir's worktree to base, writes body to main.go, and
// commits it, returning the new commit's SHA.
func commitOnBase(t *testing.T, dir, base, msg, body string) string {
	t.Helper()
	r, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := r.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := wt.Reset(&git.ResetOptions{Commit: plumbing.NewHash(base), Mode: git.HardReset}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("main.go"); err != nil {
		t.Fatal(err)
	}
	c, err := wt.Commit(msg, &git.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@x", When: time.Now()}})
	if err != nil {
		t.Fatal(err)
	}
	return c.String()
}

// checkEnqueueRerun re-runs EnqueueRerun over an already-completed head and
// asserts a second completed review lands, returning the head it exercised.
func checkEnqueueRerun(
	ctx context.Context, t *testing.T, appStore *store.Store, insertOnly *river.Client[pgx.Tx],
	dispatch func(string, bool), waitReview func(string) (string, string, string), dir, base, tenantID, repoID string,
) string {
	t.Helper()
	rerunHead := commitOnBase(t, dir, base, "rerun target", "package main\n\nfunc rerun() {}\n")
	dispatch(rerunHead, false)
	if status, _, _ := waitReview(rerunHead); status != "completed" {
		t.Fatalf("status = %s, want completed", status)
	}
	if n := countReviewsByStatus(ctx, t, appStore, tenantID, rerunHead, "completed"); n != 1 {
		t.Fatalf("completed reviews for %s = %d, want 1 before the rerun", rerunHead[:7], n)
	}

	var jobID int64
	err := appStore.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		jobID, err = jobs.EnqueueRerun(ctx, tx, insertOnly, tenantID, repoID, 1)
		return err
	})
	if err != nil || jobID == 0 {
		t.Fatalf("EnqueueRerun: jobID=%d err=%v", jobID, err)
	}

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && countReviewsByStatus(ctx, t, appStore, tenantID, rerunHead, "completed") < 2 {
		time.Sleep(100 * time.Millisecond)
	}
	if n := countReviewsByStatus(ctx, t, appStore, tenantID, rerunHead, "completed"); n != 2 {
		t.Fatalf("completed reviews for %s = %d, want 2 after the rerun", rerunHead[:7], n)
	}
	return rerunHead
}

// checkRequestCancelRejected asserts RequestCancel refuses a review that has
// already finished.
func checkRequestCancelRejected(ctx context.Context, t *testing.T, appStore *store.Store, insertOnly *river.Client[pgx.Tx], tenantID, headSHA string) {
	t.Helper()
	id := latestReviewID(ctx, t, appStore, tenantID, headSHA)
	by := newTestAccount(ctx, t, appStore, tenantID)
	err := appStore.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		return jobs.RequestCancel(ctx, tx, insertOnly, id, by)
	})
	if !errors.Is(err, jobs.ErrNotCancelable) {
		t.Fatalf("RequestCancel on a completed review = %v, want ErrNotCancelable", err)
	}
}

// checkRequestCancelCrossTenant asserts RequestCancel refuses a review that
// belongs to a different tenant than the one the caller's transaction scopes
// to: row-level security hides the row entirely, so it surfaces the same as
// "not found" rather than a distinguishable authorization error.
func checkRequestCancelCrossTenant(ctx context.Context, t *testing.T, appStore *store.Store, insertOnly *river.Client[pgx.Tx], tenantID, headSHA string) {
	t.Helper()
	id := latestReviewID(ctx, t, appStore, tenantID, headSHA)
	by := newTestAccount(ctx, t, appStore, tenantID)
	other := uuid.NewString()
	err := appStore.WithTenant(ctx, other, func(tx pgx.Tx) error {
		return jobs.RequestCancel(ctx, tx, insertOnly, id, by)
	})
	if !errors.Is(err, jobs.ErrNotCancelable) {
		t.Fatalf("RequestCancel from another tenant = %v, want ErrNotCancelable", err)
	}
}

// checkRequestCancelRunning blocks a review mid-run, cancels it, and asserts
// it lands as 'canceled' exactly once (no River retry), that the underlying
// River job itself finished normally, that the commit status and reviews row
// both reflect the cancel, and that the runner run the worker was waiting on
// is recorded as failed with the remote-cancellation error.
func checkRequestCancelRunning(
	ctx context.Context, t *testing.T, appStore *store.Store, insertOnly *river.Client[pgx.Tx], exec *gateExecutor,
	dispatch func(string, bool), waitReview func(string) (string, string, string), tenantID string, lf *localForge,
) {
	t.Helper()
	const canceling = "4444444444444444444444444444444444444444"
	exec.setBlock(true)
	t.Cleanup(func() { exec.setBlock(false) })
	dispatch(canceling, false)
	select {
	case <-exec.started:
	case <-time.After(20 * time.Second):
		t.Fatal("the review runner never started")
	}

	id := latestReviewID(ctx, t, appStore, tenantID, canceling)
	by := newTestAccount(ctx, t, appStore, tenantID)
	err := appStore.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		return jobs.RequestCancel(ctx, tx, insertOnly, id, by)
	})
	if err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}

	if status, _, _ := waitReview(canceling); status != "canceled" {
		t.Fatalf("status = %s, want canceled", status)
	}
	if n := countReviewsByStatus(ctx, t, appStore, tenantID, canceling, "canceled"); n != 1 {
		t.Fatalf("canceled reviews for %s = %d, want 1 (no River retry)", canceling, n)
	}

	// The worker's Work method returns nil once it has recorded the review as
	// 'canceled' itself (review.go's Important-1 fix): from River's point of
	// view the job ran once and succeeded, so river_job lands on 'completed'
	// with attempt=1, not a retry and not River's own 'cancelled' state (that
	// state is for a job JobCancelTx marks before it ever starts running).
	// River updates river_job's own row only after Work returns, which is
	// strictly after the reviews row above already reads 'canceled', so this
	// polls rather than reading it once.
	var state string
	var attempt int
	deadline := time.Now().Add(5 * time.Second)
	for {
		err = appStore.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT state, attempt FROM river_job WHERE id = (
				SELECT river_job_id FROM reviews WHERE id = $1)`, id).Scan(&state, &attempt)
		})
		if err != nil {
			t.Fatalf("query river_job: %v", err)
		}
		if state == "completed" || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if state != "completed" || attempt != 1 {
		t.Fatalf("river_job state=%q attempt=%d, want completed/1", state, attempt)
	}

	lf.mu.Lock()
	status := lf.status
	lf.mu.Unlock()
	if status != "error: kritik: review canceled" {
		t.Fatalf("commit status = %q, want %q", status, "error: kritik: review canceled")
	}

	var requestedAt sql.NullTime
	var canceledBy sql.NullString
	err = appStore.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT cancel_requested_at, canceled_by FROM reviews WHERE id = $1`, id).
			Scan(&requestedAt, &canceledBy)
	})
	if err != nil {
		t.Fatalf("query reviews: %v", err)
	}
	if !requestedAt.Valid {
		t.Fatal("cancel_requested_at = NULL, want set")
	}
	if canceledBy.String != by {
		t.Fatalf("canceled_by = %q, want %q", canceledBy.String, by)
	}

	var phase, runErr string
	err = appStore.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT phase, error FROM runner_runs WHERE review_id = $1
			ORDER BY created_at DESC LIMIT 1`, id).Scan(&phase, &runErr)
	})
	if err != nil {
		t.Fatalf("query runner_runs: %v", err)
	}
	if phase != "failed" || !strings.Contains(runErr, "cancelled remotely") {
		t.Fatalf("runner_runs phase=%q error=%q, want phase=failed and error containing %q", phase, runErr, "cancelled remotely")
	}
}

// checkRequestCancelPrepared cancels a review in its "prepared" state, which
// RequestCancel also accepts: the runner finishes and records a successful
// run, afterRun marks the review prepared, and the model call inside publish
// is what the blocked completer holds open when the cancel arrives. The run
// itself succeeded, so unlike checkRequestCancelRunning this does not assert
// on runner_runs.
func checkRequestCancelPrepared(
	ctx context.Context, t *testing.T, appStore *store.Store, insertOnly *river.Client[pgx.Tx], fc *fakeCompleter,
	dispatch func(string, bool), waitReview func(string) (string, string, string), dir, base, tenantID string,
) {
	t.Helper()
	head := commitOnBase(t, dir, base, "cancel during prepared", "package main\n\nfunc preparedCancel() {}\n")
	fc.setBlock(true)
	t.Cleanup(func() { fc.setBlock(false) })
	dispatch(head, false)
	select {
	case <-fc.started:
	case <-time.After(20 * time.Second):
		t.Fatal("the completer never started")
	}

	id := latestReviewID(ctx, t, appStore, tenantID, head)
	by := newTestAccount(ctx, t, appStore, tenantID)
	err := appStore.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		return jobs.RequestCancel(ctx, tx, insertOnly, id, by)
	})
	if err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}

	if status, _, _ := waitReview(head); status != "canceled" {
		t.Fatalf("status = %s, want canceled", status)
	}
	if n := countReviewsByStatus(ctx, t, appStore, tenantID, head, "canceled"); n != 1 {
		t.Fatalf("canceled reviews for %s = %d, want 1 (no River retry)", head, n)
	}
}

// checkEnqueueRerunClosed asserts EnqueueRerun refuses a pull request that
// has no open head to re-review. dispatchPR always reopens PR #1 (State:
// "open"), so this closes it directly with SQL rather than through a
// dispatch, and must run after every other checkActions subtest that
// dispatches: a later dispatch would silently reopen the row.
func checkEnqueueRerunClosed(ctx context.Context, t *testing.T, appStore *store.Store, insertOnly *river.Client[pgx.Tx], tenantID, repoID string) {
	t.Helper()
	err := appStore.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE pull_requests SET state = 'closed' WHERE tenant_id = $1 AND repository_id = $2 AND number = 1`,
			tenantID, repoID)
		return err
	})
	if err != nil {
		t.Fatalf("close pull request: %v", err)
	}

	err = appStore.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := jobs.EnqueueRerun(ctx, tx, insertOnly, tenantID, repoID, 1)
		return err
	})
	if !errors.Is(err, jobs.ErrNoHead) {
		t.Fatalf("EnqueueRerun on a closed pull request = %v, want ErrNoHead", err)
	}
}

// activeIndexGeneration returns repositories.active_index_run_id for repoID.
func activeIndexGeneration(ctx context.Context, t *testing.T, appStore *store.Store, tenantID, repoID string) string {
	t.Helper()
	var id string
	err := appStore.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT coalesce(active_index_run_id::text, '') FROM repositories WHERE id = $1`, repoID).Scan(&id)
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// checkEnqueueReindex asserts a forced reindex replaces the active
// generation with a new full/completed one and supersedes the old one.
//
// A completed full generation supersedes whatever it replaces (see embed()
// in internal/worker/index.go), so the count of mode='full' AND
// status='completed' rows for a commit never exceeds 1: the prior
// generation drops to 'superseded' at essentially the same time the new one
// lands. What actually distinguishes "forced a new generation" from "no-op"
// is the repository's active_index_run_id switching to a different row.
func checkEnqueueReindex(ctx context.Context, t *testing.T, appStore *store.Store, insertOnly *river.Client[pgx.Tx], tenantID, repoID, head string) {
	t.Helper()
	before := activeIndexGeneration(ctx, t, appStore, tenantID, repoID)
	if before == "" {
		t.Fatal("no active index generation before forcing a reindex")
	}

	var jobID int64
	err := appStore.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		jobID, err = jobs.EnqueueReindex(ctx, tx, insertOnly, tenantID, repoID)
		return err
	})
	if err != nil || jobID == 0 {
		t.Fatalf("EnqueueReindex: jobID=%d err=%v", jobID, err)
	}

	deadline := time.Now().Add(20 * time.Second)
	after := before
	for time.Now().Before(deadline) {
		if after = activeIndexGeneration(ctx, t, appStore, tenantID, repoID); after != "" && after != before {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if after == before {
		t.Fatalf("active index generation for %s = %q, want a new generation distinct from %q", head[:7], after, before)
	}

	var mode, status string
	if err := appStore.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT mode, status FROM index_runs WHERE id = $1`, after).Scan(&mode, &status)
	}); err != nil {
		t.Fatal(err)
	}
	if mode != "full" || status != "completed" {
		t.Fatalf("new active generation %s mode=%s status=%s, want full/completed", after, mode, status)
	}

	var priorStatus string
	if err := appStore.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM index_runs WHERE id = $1`, before).Scan(&priorStatus)
	}); err != nil {
		t.Fatal(err)
	}
	if priorStatus != "superseded" {
		t.Fatalf("prior generation %s status = %s, want superseded", before, priorStatus)
	}
}

// gateExecutor runs the real runner, or once blocked holds each review run
// until supervision cancels it, the way a Job runs until it is deleted.
type gateExecutor struct {
	inner   executor.Executor
	started chan executor.Spec

	mu    sync.Mutex
	block bool
	// after, if set, runs once a review runner has finished unblocked.
	after func()
}

func (g *gateExecutor) setAfter(fn func()) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.after = fn
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
	block, after := g.block, g.after
	g.mu.Unlock()
	if spec.Job.Kind != runner.KindReview {
		return g.inner.Run(ctx, spec)
	}
	if !block {
		res := g.inner.Run(ctx, spec)
		if after != nil {
			after()
		}
		return res
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

// jobDeadline stands in for River's job timeout, which cancels a job's ctx
// with context.DeadlineExceeded as its cause: armed, it ends the running
// review job's ctx that way the moment fire is called.
type jobDeadline struct {
	mu     sync.Mutex
	armed  bool
	cancel context.CancelCauseFunc
}

func (d *jobDeadline) wrap(ctx context.Context, job *rivertype.JobRow, doInner func(context.Context) error) error {
	if job.Kind != (jobs.ReviewArgs{}).Kind() {
		return doInner(ctx)
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	d.mu.Lock()
	d.cancel = cancel
	d.mu.Unlock()
	return doInner(ctx)
}

func (d *jobDeadline) arm() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.armed = true
}

func (d *jobDeadline) fire() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.armed && d.cancel != nil {
		d.armed = false
		d.cancel(context.DeadlineExceeded)
	}
}

// checkJobEnded asserts a review job whose ctx ends after its runner has
// finished leaves a terminal review and no River retry: a timeout during
// afterRun fails the review, and a cancel after the model answered keeps
// the published review completed with its usage recorded.
func checkJobEnded(
	ctx context.Context, t *testing.T, appStore *store.Store, insertOnly *river.Client[pgx.Tx], exec *gateExecutor,
	fc *fakeCompleter, deadline *jobDeadline, dispatch func(string, bool), waitReview func(string) (string, string, string),
	dir, base, tenantID string, lf *localForge,
) {
	t.Run("a timeout during afterRun fails the review", func(t *testing.T) {
		head := commitOnBase(t, dir, base, "timed out after the runner", "package main\n\nfunc timedOut() {}\n")
		exec.setAfter(deadline.fire)
		t.Cleanup(func() { exec.setAfter(nil) })
		deadline.arm()
		dispatch(head, false)
		if status, _, _ := waitReview(head); status != statusFailed {
			t.Fatalf("status = %s, want failed", status)
		}
		id := latestReviewID(ctx, t, appStore, tenantID, head)
		var errText string
		err := appStore.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT error FROM reviews WHERE id = $1`, id).Scan(&errText)
		})
		if err != nil || errText != "review timed out: context deadline exceeded" {
			t.Fatalf("error = %q, %v; want review timed out: context deadline exceeded", errText, err)
		}
		waitRiverJobCompleted(ctx, t, appStore, tenantID, id)
		if n := countReviewsByStatus(ctx, t, appStore, tenantID, head, "running"); n != 0 {
			t.Fatalf("running reviews for %s = %d, want 0", head, n)
		}
		if got := lf.lastStatus(); got != "error: kritik: review timed out" {
			t.Fatalf("commit status = %q, want error: kritik: review timed out", got)
		}
	})

	t.Run("a cancel after the model answered keeps the review completed", func(t *testing.T) {
		head := commitOnBase(t, dir, base, "canceled after the answer", "package main\n\nfunc answered() {}\n")
		by := newTestAccount(ctx, t, appStore, tenantID)
		fc.setAnswered(func(cctx context.Context) {
			id := latestReviewID(ctx, t, appStore, tenantID, head)
			err := appStore.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
				return jobs.RequestCancel(ctx, tx, insertOnly, id, by)
			})
			if err != nil {
				t.Errorf("RequestCancel: %v", err)
				return
			}
			select {
			case <-cctx.Done():
			case <-time.After(10 * time.Second):
				t.Error("the job's ctx never saw the cancel")
			}
		})
		t.Cleanup(func() { fc.setAnswered(nil) })
		dispatch(head, false)
		if status, _, _ := waitReview(head); status != statusCompleted {
			t.Fatalf("status = %s, want completed", status)
		}
		id := latestReviewID(ctx, t, appStore, tenantID, head)
		var usage int
		err := appStore.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM usage WHERE review_id = $1 AND role = 'review'`, id).Scan(&usage)
		})
		if err != nil || usage != 1 {
			t.Fatalf("review usage rows = %d, %v; want 1", usage, err)
		}
		waitRiverJobCompleted(ctx, t, appStore, tenantID, id)
		if got := lf.lastStatus(); !strings.HasPrefix(got, "success: ") {
			t.Fatalf("commit status = %q, want success", got)
		}
	})
}

// waitRiverJobCompleted waits for the review's River job to settle as
// completed on its first attempt: River writes its own row only after Work
// returns, and a job that returned an error would be retryable instead.
func waitRiverJobCompleted(ctx context.Context, t *testing.T, appStore *store.Store, tenantID, reviewID string) {
	t.Helper()
	var state string
	var attempt int
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := appStore.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT state, attempt FROM river_job WHERE id = (
				SELECT river_job_id FROM reviews WHERE id = $1)`, reviewID).Scan(&state, &attempt)
		})
		if err != nil {
			t.Fatalf("query river_job: %v", err)
		}
		if state == "completed" || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if state != "completed" || attempt != 1 {
		t.Fatalf("river_job state=%q attempt=%d, want completed/1", state, attempt)
	}
}

func (l *localForge) lastStatus() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.status
}

// checkEnqueueReindexSentinels asserts EnqueueReindex's two sentinels. Each
// case runs in a transaction it rolls back, so no job it inserts outlives it.
func checkEnqueueReindexSentinels(
	ctx context.Context, t *testing.T, appStore *store.Store, insertOnly *river.Client[pgx.Tx], tenantID, repoID string,
) {
	errRollback := errors.New("roll back")
	t.Run("ErrRepositoryNotFound for a repository the tenant does not have", func(t *testing.T) {
		err := appStore.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
			_, err := jobs.EnqueueReindex(ctx, tx, insertOnly, tenantID, uuid.NewString())
			return errors.Join(err, errRollback)
		})
		if !errors.Is(err, jobs.ErrRepositoryNotFound) {
			t.Fatalf("EnqueueReindex = %v, want ErrRepositoryNotFound", err)
		}
	})
	t.Run("ErrReindexQueued when an onboard index job is already queued", func(t *testing.T) {
		err := appStore.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
			res, err := insertOnly.InsertTx(ctx, tx, jobs.IndexArgs{TenantID: tenantID, RepositoryID: repoID, Trigger: "onboard"}, nil)
			if err != nil || res.UniqueSkippedAsDuplicate {
				return fmt.Errorf("insert onboard job = %+v, %w", res, err)
			}
			_, err = jobs.EnqueueReindex(ctx, tx, insertOnly, tenantID, repoID)
			return errors.Join(err, errRollback)
		})
		if !errors.Is(err, jobs.ErrReindexQueued) {
			t.Fatalf("EnqueueReindex = %v, want ErrReindexQueued", err)
		}
	})
}
