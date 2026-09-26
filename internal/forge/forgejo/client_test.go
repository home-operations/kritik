package forgejo

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/home-operations/kritik/internal/forge"
)

// testToken is the fixed token every newTestServer client authenticates
// with; no test needs a different value, so it is not a parameter.
const testToken = "tok"

// newTestServer builds an httptest server and a Client pointed at it, and
// asserts every request carries the token header.
func newTestServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *Client) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "token "+testToken {
			t.Errorf("Authorization header = %q, want %q", got, "token "+testToken)
		}
		handler(w, r)
	}))
	c, err := NewClient(srv.URL, testToken, srv.Client())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return srv, c
}

func TestPullRequestDiff(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		const diff = "diff --git a/a.go b/a.go\n+b\n"
		srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/repos/acme/widgets/pulls/7.diff" {
				t.Errorf("path = %s", r.URL.Path)
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte(diff))
		})
		defer srv.Close()
		got, err := c.PullRequestDiff(t.Context(), "acme", "widgets", 7, "base", "head")
		if err != nil || got != diff {
			t.Fatalf("PullRequestDiff = %q, %v", got, err)
		}
	})

	t.Run("a diff over the cap is an error, not a truncated diff", func(t *testing.T) {
		srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(bytes.Repeat([]byte("x"), maxDiffBytes+1))
		})
		defer srv.Close()
		if got, err := c.PullRequestDiff(t.Context(), "acme", "widgets", 7, "base", "head"); err == nil {
			t.Fatalf("PullRequestDiff = %d bytes, want an error", len(got))
		}
	})
}

func TestMergeBase(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/repos/acme/widgets/pulls/7" {
				t.Errorf("path = %s", r.URL.Path)
			}
			_, _ = w.Write([]byte(`{"merge_base":"deadbeef"}`))
		})
		defer srv.Close()
		sha, err := c.MergeBase(t.Context(), "acme", "widgets", 7, "main", "feature")
		if err != nil || sha != "deadbeef" {
			t.Fatalf("MergeBase = %q, %v", sha, err)
		}
	})

	t.Run("empty merge base is an error", func(t *testing.T) {
		srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"merge_base":""}`))
		})
		defer srv.Close()
		if _, err := c.MergeBase(t.Context(), "acme", "widgets", 7, "main", "feature"); err == nil {
			t.Fatal("expected an error for an empty merge_base")
		}
	})
}

func TestCloneURL(t *testing.T) {
	c, err := NewClient("forge.example.com", "tok", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := c.CloneURL("acme", "widgets"), "https://forge.example.com/acme/widgets.git"; got != want {
		t.Fatalf("CloneURL = %q, want %q", got, want)
	}
}

func TestGitToken(t *testing.T) {
	c, err := NewClient("forge.example.com", "tok", nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.GitToken(t.Context())
	if err != nil || got != "tok" {
		t.Fatalf("GitToken = %q, %v", got, err)
	}
}

func TestBranchTip(t *testing.T) {
	t.Run("default branch", func(t *testing.T) {
		srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v1/repos/acme/widgets":
				_, _ = w.Write([]byte(`{"default_branch":"main"}`))
			case "/api/v1/repos/acme/widgets/branches/main":
				_, _ = w.Write([]byte(`{"commit":{"id":"abc123"}}`))
			default:
				t.Errorf("unexpected path %s", r.URL.Path)
			}
		})
		defer srv.Close()
		sha, branch, err := c.BranchTip(t.Context(), "acme", "widgets", "")
		if err != nil || sha != "abc123" || branch != "main" {
			t.Fatalf("BranchTip = %q, %q, %v", sha, branch, err)
		}
	})

	t.Run("named branch", func(t *testing.T) {
		srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/repos/acme/widgets/branches/feature" {
				t.Errorf("unexpected path %s", r.URL.Path)
			}
			_, _ = w.Write([]byte(`{"commit":{"id":"def456"}}`))
		})
		defer srv.Close()
		sha, branch, err := c.BranchTip(t.Context(), "acme", "widgets", "feature")
		if err != nil || sha != "def456" || branch != "feature" {
			t.Fatalf("BranchTip = %q, %q, %v", sha, branch, err)
		}
	})
}

func TestBotLoginCaches(t *testing.T) {
	hits := 0
	srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/user" {
			t.Errorf("path = %s", r.URL.Path)
		}
		hits++
		_, _ = w.Write([]byte(`{"login":"kritik-bot"}`))
	})
	defer srv.Close()

	first, err := c.BotLogin(t.Context())
	if err != nil || first != "kritik-bot" {
		t.Fatalf("BotLogin = %q, %v", first, err)
	}
	second, err := c.BotLogin(t.Context())
	if err != nil || second != "kritik-bot" || hits != 1 {
		t.Fatalf("BotLogin second = %q, %v, hits = %d, want 1", second, err, hits)
	}
}

func TestFindComment(t *testing.T) {
	srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/acme/widgets/issues/9/comments" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`[
			{"id":1,"body":"unrelated","user":{"login":"someone"},"created_at":"2026-01-01T00:00:00Z"},
			{"id":2,"body":"kritik-marker: v1","user":{"login":"kritik-bot"},"created_at":"2026-01-02T00:00:00Z"}
		]`))
	})
	defer srv.Close()

	id, err := c.FindComment(t.Context(), "acme", "widgets", 9, "kritik-bot", "kritik-marker")
	if err != nil || id != 2 {
		t.Fatalf("FindComment = %d, %v", id, err)
	}

	id, err = c.FindComment(t.Context(), "acme", "widgets", 9, "kritik-bot", "nope")
	if err != nil || id != 0 {
		t.Fatalf("FindComment (no match) = %d, %v", id, err)
	}
}

func TestCreateUpdateGetComment(t *testing.T) {
	srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/acme/widgets/issues/9/comments":
			_, _ = w.Write([]byte(`{"id":42,"body":"hello","user":{"login":"kritik-bot"},"created_at":"2026-01-01T00:00:00Z"}`))
		case r.Method == http.MethodPatch && r.URL.Path == "/api/v1/repos/acme/widgets/issues/comments/42":
			_, _ = w.Write([]byte(`{"id":42,"body":"updated","user":{"login":"kritik-bot"},"created_at":"2026-01-01T00:00:00Z"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/widgets/issues/comments/42":
			_, _ = w.Write([]byte(`{"id":42,"body":"updated","user":{"login":"kritik-bot"},"created_at":"2026-01-01T00:00:00Z"}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})
	defer srv.Close()

	id, err := c.CreateComment(t.Context(), "acme", "widgets", 9, "hello")
	if err != nil || id != 42 {
		t.Fatalf("CreateComment = %d, %v", id, err)
	}
	if err := c.UpdateComment(t.Context(), "acme", "widgets", 42, "updated"); err != nil {
		t.Fatalf("UpdateComment: %v", err)
	}
	cm, err := c.GetComment(t.Context(), "acme", "widgets", 9, 42, false)
	if err != nil {
		t.Fatalf("GetComment: %v", err)
	}
	if cm.ID != 42 || cm.Body != "updated" || cm.Author != "kritik-bot" || cm.Inline {
		t.Fatalf("GetComment = %+v", cm)
	}
}

func TestCreateReview(t *testing.T) {
	t.Run("no-op on empty comments", func(t *testing.T) {
		called := false
		srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			called = true
		})
		defer srv.Close()
		if err := c.CreateReview(t.Context(), "acme", "widgets", 9, "sha1", nil); err != nil {
			t.Fatalf("CreateReview: %v", err)
		}
		if called {
			t.Fatal("CreateReview made a request for an empty comment slice")
		}
	})

	t.Run("posts the review body shape", func(t *testing.T) {
		var gotBody string
		srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/repos/acme/widgets/pulls/9/reviews" {
				t.Errorf("path = %s", r.URL.Path)
			}
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
			_, _ = w.Write([]byte(`{"id":1}`))
		})
		defer srv.Close()
		err := c.CreateReview(t.Context(), "acme", "widgets", 9, "sha1", []forge.InlineComment{
			{Path: "a.go", Line: 12, Body: "nit"},
		})
		if err != nil {
			t.Fatalf("CreateReview: %v", err)
		}
		for _, want := range []string{`"commit_id":"sha1"`, `"event":"COMMENT"`, `"new_position":12`, `"path":"a.go"`, `"body":"nit"`} {
			if !strings.Contains(gotBody, want) {
				t.Errorf("review body %q missing %q", gotBody, want)
			}
		}
	})

	t.Run("surfaces a 422 error", func(t *testing.T) {
		srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"message":"bad diff line"}`))
		})
		defer srv.Close()
		err := c.CreateReview(t.Context(), "acme", "widgets", 9, "sha1", []forge.InlineComment{{Path: "a.go", Line: 1, Body: "x"}})
		if err == nil {
			t.Fatal("expected an error for a 422 response")
		}
		if !strings.Contains(err.Error(), "bad diff line") {
			t.Fatalf("error = %q, want it to contain the forge's message %q", err.Error(), "bad diff line")
		}
	})
}

func TestSetStatus(t *testing.T) {
	var gotBody string
	srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/acme/widgets/statuses/deadbeef" {
			t.Errorf("path = %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
	})
	defer srv.Close()

	long := strings.Repeat("x", 200)
	if err := c.SetStatus(t.Context(), "acme", "widgets", "deadbeef", forge.StatusPending, long); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	if !strings.Contains(gotBody, `"state":"pending"`) || !strings.Contains(gotBody, `"context":"kritik/review"`) {
		t.Fatalf("status body = %q", gotBody)
	}
	if strings.Count(gotBody, "x") >= 200 {
		t.Fatalf("status description was not truncated: %q", gotBody)
	}
}

func TestPermission(t *testing.T) {
	cases := []struct {
		forgejo string
		want    forge.Permission
	}{
		{"owner", forge.PermissionAdmin},
		{"admin", forge.PermissionAdmin},
		{"write", forge.PermissionWrite},
		{"read", forge.PermissionRead},
		{"none", forge.PermissionNone},
	}
	for _, tc := range cases {
		t.Run(tc.forgejo, func(t *testing.T) {
			srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/repos/acme/widgets/collaborators/alice/permission" {
					t.Errorf("path = %s", r.URL.Path)
				}
				_, _ = w.Write([]byte(`{"permission":"` + tc.forgejo + `"}`))
			})
			defer srv.Close()
			got, err := c.Permission(t.Context(), "acme", "widgets", "alice")
			if err != nil || got != tc.want {
				t.Fatalf("Permission(%s) = %q, %v; want %q", tc.forgejo, got, err, tc.want)
			}
		})
	}

	t.Run("unknown permission is an error", func(t *testing.T) {
		srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"permission":"nonsense"}`))
		})
		defer srv.Close()
		if _, err := c.Permission(t.Context(), "acme", "widgets", "alice"); err == nil {
			t.Fatal("expected an error for an unrecognized permission string")
		}
	})
}

func TestListOpenPullRequests(t *testing.T) {
	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/acme/widgets/pulls" {
			t.Errorf("path = %s", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("state") != "open" || q.Get("sort") != "recentupdate" || q.Get("limit") != "50" {
			t.Errorf("query = %q, want state=open&sort=recentupdate&limit=50", r.URL.RawQuery)
		}
		page := q.Get("page")
		switch page {
		case "", "1":
			w.Header().Set("Link", `<`+"http://"+r.Host+r.URL.Path+`?page=2>; rel="next"`)
			_, _ = w.Write([]byte(`[
				{"number":3,"title":"newest","body":"Fixes the widget.","user":{"login":"alice"},"state":"open","draft":false,
				 "updated_at":"2026-01-15T00:00:00Z","created_at":"2026-01-15T00:00:00Z","html_url":"https://forge.example.com/acme/widgets/pulls/3",
				 "head":{"ref":"feature","sha":"h1","repo":{"full_name":"acme/widgets","fork":false}},
				 "base":{"ref":"main","sha":"b1","repo":{"full_name":"acme/widgets","default_branch":"main"}},
				 "labels":[{"name":"bug","color":"f00"}]},
				{"number":4,"title":"bot draft","user":{"login":"bob[bot]"},"state":"open","draft":true,
				 "updated_at":"2026-01-16T00:00:00Z","created_at":"2026-01-16T00:00:00Z","html_url":"https://forge.example.com/acme/widgets/pulls/4",
				 "head":{"ref":"fork-feature","sha":"h4","repo":{"full_name":"someone/widgets","fork":true}},
				 "base":{"ref":"main","sha":"b4","repo":{"full_name":"acme/widgets","default_branch":"main"}},
				 "labels":[]}
			]`))
		case "2":
			_, _ = w.Write([]byte(`[
				{"number":2,"title":"too old","user":{"login":"bob[bot]"},"state":"open","draft":true,
				 "updated_at":"2025-12-01T00:00:00Z","created_at":"2025-12-01T00:00:00Z","html_url":"https://forge.example.com/acme/widgets/pulls/2",
				 "head":{"ref":"fork-feature","sha":"h2","repo":{"full_name":"someone/widgets","fork":true}},
				 "base":{"ref":"main","sha":"b2","repo":{"full_name":"acme/widgets","default_branch":"main"}},
				 "labels":[]}
			]`))
		default:
			t.Errorf("unexpected page %q", page)
		}
	})
	defer srv.Close()

	prs, err := c.ListOpenPullRequests(t.Context(), "acme", "widgets", since)
	if err != nil {
		t.Fatalf("ListOpenPullRequests: %v", err)
	}
	if len(prs) != 2 {
		t.Fatalf("got %d pull requests, want 2 (page 2 is older than since)", len(prs))
	}
	pr := prs[0]
	if pr.Number != 3 || pr.Body != "Fixes the widget." || pr.DefaultBranch != "main" || pr.Fork || pr.AuthorIsBot || pr.Draft || len(pr.Labels) != 1 || pr.Labels[0].Name != "bug" {
		t.Fatalf("mapped PR = %+v", pr)
	}
	bot := prs[1]
	if bot.Number != 4 || !bot.Fork || !bot.AuthorIsBot || !bot.Draft {
		t.Fatalf("mapped bot/draft/fork PR = %+v", bot)
	}
}

func TestListInlineAndReplyAndGetComment(t *testing.T) {
	srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/widgets/pulls/9/reviews":
			_, _ = w.Write([]byte(`[{"id":100,"commit_id":"sha1","submitted_at":"2026-01-01T00:00:00Z"}]`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/widgets/pulls/9/reviews/100/comments":
			_, _ = w.Write([]byte(`[
				{"id":200,"body":"first","path":"a.go","position":5,"commit_id":"sha1","user":{"login":"kritik-bot"},"created_at":"2026-01-01T00:00:01Z"},
				{"id":201,"body":"second","path":"b.go","position":9,"commit_id":"sha1","user":{"login":"alice"},"created_at":"2026-01-01T00:00:02Z"}
			]`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/acme/widgets/pulls/9/reviews":
			_, _ = w.Write([]byte(`{"id":101}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})
	defer srv.Close()

	comments, err := c.ListInline(t.Context(), "acme", "widgets", 9)
	if err != nil {
		t.Fatalf("ListInline: %v", err)
	}
	if len(comments) != 2 {
		t.Fatalf("got %d comments, want 2", len(comments))
	}
	for _, cm := range comments {
		if cm.InReplyTo != 0 {
			t.Errorf("comment %d has InReplyTo = %d, want 0 (Forgejo has no reply linkage)", cm.ID, cm.InReplyTo)
		}
		if !cm.Inline {
			t.Errorf("comment %d Inline = false, want true", cm.ID)
		}
	}
	if comments[0].ID != 200 || comments[1].ID != 201 {
		t.Fatalf("comments not oldest-first: %+v", comments)
	}

	// GetComment(inline=true) finds the comment by traversing reviews, with
	// no cache and no dependency on the prior ListInline call above.
	cm, err := c.GetComment(t.Context(), "acme", "widgets", 9, 200, true)
	if err != nil {
		t.Fatalf("GetComment(inline=true): %v", err)
	}
	if cm.ID != 200 || cm.Path != "a.go" {
		t.Fatalf("GetComment(inline=true) = %+v", cm)
	}

	// GetComment(inline=true) for an id no review comment carries.
	if _, err := c.GetComment(t.Context(), "acme", "widgets", 9, 999, true); !errors.Is(err, ErrCommentUnknown) {
		t.Fatalf("GetComment unknown id error = %v, want ErrCommentUnknown", err)
	}

	id, err := c.ReplyInline(t.Context(), "acme", "widgets", 9, 200, "reply body")
	if err != nil {
		t.Fatalf("ReplyInline: %v", err)
	}
	if id != 0 {
		t.Fatalf("ReplyInline id = %d, want 0 (Forgejo reviews carry no per-comment id)", id)
	}
}

func TestNotFoundWrapsSentinel(t *testing.T) {
	srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"not found"}`))
	})
	defer srv.Close()
	_, err := c.GetComment(t.Context(), "acme", "widgets", 9, 1, false)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want errors.Is(err, ErrNotFound)", err)
	}
}

func TestNewClientAcceptsHostWithOrWithoutScheme(t *testing.T) {
	c1, err := NewClient("forge.example.com", "tok", nil)
	if err != nil {
		t.Fatal(err)
	}
	if c1.CloneURL("a", "b") != "https://forge.example.com/a/b.git" {
		t.Fatalf("bare host CloneURL = %q", c1.CloneURL("a", "b"))
	}

	c2, err := NewClient("http://127.0.0.1:1234", "tok", nil)
	if err != nil {
		t.Fatal(err)
	}
	if c2.CloneURL("a", "b") != "http://127.0.0.1:1234/a/b.git" {
		t.Fatalf("scheme host CloneURL = %q", c2.CloneURL("a", "b"))
	}
}

// TestGetCommentAndReplyInlineWithoutPriorListInline is the critical-fix
// regression test: an inline reply can arrive as the very first request
// this process makes for a PR (e.g. right after a restart), so
// GetComment(inline=true) and ReplyInline must resolve the comment by
// traversing reviews directly, not depend on an earlier ListInline call
// having populated some cache.
func TestGetCommentAndReplyInlineWithoutPriorListInline(t *testing.T) {
	srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/widgets/pulls/9/reviews":
			_, _ = w.Write([]byte(`[{"id":100,"commit_id":"sha1","submitted_at":"2026-01-01T00:00:00Z"}]`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/widgets/pulls/9/reviews/100/comments":
			_, _ = w.Write([]byte(`[{"id":200,"body":"first","path":"a.go","position":5,"commit_id":"sha1","user":{"login":"kritik-bot"},"created_at":"2026-01-01T00:00:01Z"}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/acme/widgets/pulls/9/reviews":
			_, _ = w.Write([]byte(`{"id":101}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})
	defer srv.Close()

	// No ListInline call before this: c has never seen this PR, yet
	// GetComment(inline=true) must still resolve comment 200.
	cm, err := c.GetComment(t.Context(), "acme", "widgets", 9, 200, true)
	if err != nil {
		t.Fatalf("GetComment(inline=true) on a fresh client: %v", err)
	}
	if cm.ID != 200 || cm.Path != "a.go" {
		t.Fatalf("GetComment(inline=true) = %+v", cm)
	}

	if _, err := c.ReplyInline(t.Context(), "acme", "widgets", 9, 200, "reply body"); err != nil {
		t.Fatalf("ReplyInline on a fresh client: %v", err)
	}
}

func TestOpenPullRequestForkDetection(t *testing.T) {
	base := repository{FullName: "acme/widgets", DefaultBranch: "main"}

	t.Run("deleted head repo is treated as a fork", func(t *testing.T) {
		pr := pullRequest{
			Number: 5,
			Head:   branchInfo{Ref: "gone", SHA: "h1", Repo: nil},
			Base:   branchInfo{Ref: "main", SHA: "b1", Repo: &base},
		}
		out := openPullRequest(pr)
		if !out.Fork {
			t.Fatalf("Fork = false, want true for a nil head.repo (deleted fork)")
		}
	})

	t.Run("same-repo head is not a fork", func(t *testing.T) {
		pr := pullRequest{
			Number: 6,
			Head:   branchInfo{Ref: "topic", SHA: "h2", Repo: &base},
			Base:   branchInfo{Ref: "main", SHA: "b2", Repo: &base},
		}
		out := openPullRequest(pr)
		if out.Fork {
			t.Fatalf("Fork = true, want false when head.repo == base.repo")
		}
	})
}

func TestIsBot(t *testing.T) {
	cases := []struct {
		login string
		want  bool
	}{
		{"kritik-bot", false},
		{"some-bot", false},
		{"dependabot[bot]", true},
		{"renovate[bot]", true},
		{"alice", false},
	}
	for _, tc := range cases {
		if got := isBot(tc.login); got != tc.want {
			t.Errorf("isBot(%q) = %v, want %v", tc.login, got, tc.want)
		}
	}
}
