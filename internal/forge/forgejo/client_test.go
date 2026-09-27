package forgejo

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
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

func TestFileAt(t *testing.T) {
	srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() + "?" + r.URL.RawQuery {
		case "/api/v1/repos/acme/widgets/raw/.kritik/my%20rules.md?ref=deadbeef":
			_, _ = w.Write([]byte("rules"))
		case "/api/v1/repos/acme/widgets/raw/big.bin?ref=deadbeef":
			_, _ = w.Write(bytes.Repeat([]byte("x"), forge.MaxFileBytes+1))
		default:
			http.NotFound(w, r)
		}
	})
	defer srv.Close()
	got, err := c.FileAt(t.Context(), "acme", "widgets", "deadbeef", ".kritik/my rules.md")
	if err != nil || string(got) != "rules" {
		t.Fatalf("FileAt = %q, %v", got, err)
	}
	if _, err := c.FileAt(t.Context(), "acme", "widgets", "deadbeef", "gone.yaml"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("FileAt of a missing file = %v, want fs.ErrNotExist", err)
	}
	if _, err := c.FileAt(t.Context(), "acme", "widgets", "deadbeef", "big.bin"); !errors.Is(err, forge.ErrFileTooLarge) {
		t.Fatalf("FileAt of a file over the cap = %v, want forge.ErrFileTooLarge", err)
	}
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

func TestReadGitToken(t *testing.T) {
	tests := []struct {
		name, fetch, want string
		wantErr           error
	}{
		{"the gitToken", "ro", "ro", nil},
		{"never the API token", "", "", forge.ErrNoReadToken},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := NewClient("forge.example.com", "tok", nil)
			if err != nil {
				t.Fatal(err)
			}
			c.FetchToken = tt.fetch
			got, err := c.ReadGitToken(t.Context(), "acme", "widgets")
			if got != tt.want || !errors.Is(err, tt.wantErr) {
				t.Fatalf("ReadGitToken = %q, %v; want %q, %v", got, err, tt.want, tt.wantErr)
			}
		})
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
			{"id":2,"body":"kritik-marker: v1","user":{"login":"kritik-bot"},"created_at":"2026-01-02T00:00:00Z"},
			{"id":3,"body":"kritik-marker: v2","user":{"login":"kritik-bot"},"created_at":"2026-01-03T00:00:00Z"}
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
	if cm.ID != 200 || cm.Path != "a.go" || cm.CommitID != "sha1" {
		t.Fatalf("GetComment(inline=true) = %+v", cm)
	}

	// GetComment(inline=true) for an id no review comment carries.
	if _, err := c.GetComment(t.Context(), "acme", "widgets", 9, 999, true); !errors.Is(err, ErrCommentUnknown) {
		t.Fatalf("GetComment unknown id error = %v, want ErrCommentUnknown", err)
	}

	id, err := c.ReplyInline(t.Context(), "acme", "widgets", 9, cm, "reply body")
	if err != nil {
		t.Fatalf("ReplyInline: %v", err)
	}
	if id != 0 {
		t.Fatalf("ReplyInline id = %d, want 0 (Forgejo reviews carry no per-comment id)", id)
	}
}

// TestGetCommentInCodeConversation covers a mention in a code conversation:
// Forgejo's webhook calls it a conversation comment, but the conversation
// endpoint answers it with 204, so it is found under its review instead.
func TestGetCommentInCodeConversation(t *testing.T) {
	srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/repos/acme/widgets/issues/comments/"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/widgets/pulls/9/reviews":
			_, _ = w.Write([]byte(`[{"id":100,"commit_id":"sha1","submitted_at":"2026-01-01T00:00:00Z"}]`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/widgets/pulls/9/reviews/100/comments":
			_, _ = w.Write([]byte(`[{"id":200,"body":"@kritik why?","path":"a.go","position":5,"commit_id":"sha1","user":{"login":"alice"},"created_at":"2026-01-01T00:00:01Z"}]`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})
	defer srv.Close()

	t.Run("found under its review", func(t *testing.T) {
		cm, err := c.GetComment(t.Context(), "acme", "widgets", 9, 200, false)
		if err != nil {
			t.Fatalf("GetComment: %v", err)
		}
		if cm.ID != 200 || !cm.Inline || cm.Path != "a.go" || cm.Line != 5 || cm.Author != "alice" {
			t.Fatalf("GetComment = %+v, want the inline comment", cm)
		}
	})
	t.Run("under no review", func(t *testing.T) {
		if _, err := c.GetComment(t.Context(), "acme", "widgets", 9, 999, false); !errors.Is(err, ErrCommentUnknown) {
			t.Fatalf("GetComment error = %v, want ErrCommentUnknown", err)
		}
	})
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

// TestGetCommentWithoutPriorListInline: an inline reply can arrive as the
// very first request this process makes for a PR (e.g. right after a
// restart), so GetComment(inline=true) must resolve the comment by
// traversing reviews directly, not depend on an earlier ListInline call.
func TestGetCommentWithoutPriorListInline(t *testing.T) {
	srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/widgets/pulls/9/reviews":
			_, _ = w.Write([]byte(`[{"id":100,"commit_id":"sha1","submitted_at":"2026-01-01T00:00:00Z"}]`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/widgets/pulls/9/reviews/100/comments":
			_, _ = w.Write([]byte(`[{"id":200,"body":"first","path":"a.go","position":5,"commit_id":"sha1","user":{"login":"kritik-bot"},"created_at":"2026-01-01T00:00:01Z"}]`))
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
}

// TestReplyInline: the reply is a review on the answered comment's commit,
// path and line, which the comment carries, so nothing is looked up first.
func TestReplyInline(t *testing.T) {
	var got createPullReviewOptions
	srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/repos/acme/widgets/pulls/9/reviews" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode review: %v", err)
		}
		_, _ = w.Write([]byte(`{"id":101}`))
	})
	defer srv.Close()

	to := forge.Comment{ID: 200, Inline: true, Path: "a.go", Line: 5, CommitID: "sha1"}
	if _, err := c.ReplyInline(t.Context(), "acme", "widgets", 9, to, "reply body"); err != nil {
		t.Fatalf("ReplyInline: %v", err)
	}
	want := createPullReviewOptions{CommitID: "sha1", Event: "COMMENT",
		Comments: []createPullReviewComment{{Path: "a.go", Body: "reply body", NewLineNum: 5}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("review = %+v, want %+v", got, want)
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

func TestIssue(t *testing.T) {
	t.Run("issue", func(t *testing.T) {
		srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/repos/acme/widgets/issues/5" {
				t.Errorf("path = %s", r.URL.Path)
			}
			_, _ = w.Write([]byte(`{"number":5,"title":"bug report","body":"it broke","state":"open",
				"user":{"login":"alice"},"labels":[{"name":"bug","color":"f00"}],
				"assignees":[{"login":"bob"}],"pull_request":null,
				"html_url":"https://forge.example.com/acme/widgets/issues/5"}`))
		})
		defer srv.Close()
		got, err := c.Issue(t.Context(), "acme", "widgets", 5)
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}
		want := forge.Issue{
			Number: 5, Title: "bug report", Body: "it broke", State: "open",
			Author: "alice", Labels: []string{"bug"}, Assignees: []string{"bob"},
			IsPull: false, URL: "https://forge.example.com/acme/widgets/issues/5",
		}
		if !issueEqual(got, want) {
			t.Fatalf("Issue = %+v, want %+v", got, want)
		}
	})

	t.Run("pull request is distinguished by a non-null pull_request field", func(t *testing.T) {
		srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"number":6,"title":"a pr","state":"open","user":{"login":"alice"},
				"pull_request":{},"html_url":"https://forge.example.com/acme/widgets/pulls/6"}`))
		})
		defer srv.Close()
		got, err := c.Issue(t.Context(), "acme", "widgets", 6)
		if err != nil || !got.IsPull {
			t.Fatalf("Issue.IsPull = %v, %v; want true", got.IsPull, err)
		}
	})

	t.Run("a draft pull request says so", func(t *testing.T) {
		srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"number":7,"title":"wip","state":"open","user":{"login":"alice"},
				"pull_request":{"draft":true,"merged":false}}`))
		})
		defer srv.Close()
		got, err := c.Issue(t.Context(), "acme", "widgets", 7)
		if err != nil || !got.IsPull || !got.Draft {
			t.Fatalf("Issue = %+v, %v; want a draft pull request", got, err)
		}
	})

	t.Run("wraps ErrNotFound", func(t *testing.T) {
		srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"not found"}`))
		})
		defer srv.Close()
		if _, err := c.Issue(t.Context(), "acme", "widgets", 9); !errors.Is(err, ErrNotFound) {
			t.Fatalf("error = %v, want errors.Is(err, ErrNotFound)", err)
		}
	})
}

// issueEqual compares two forge.Issue values field by field since it
// contains slices, which == cannot compare.
func issueEqual(a, b forge.Issue) bool {
	if a.Number != b.Number || a.Title != b.Title || a.Body != b.Body || a.State != b.State ||
		a.Author != b.Author || a.IsPull != b.IsPull || a.URL != b.URL {
		return false
	}
	return slices.Equal(a.Labels, b.Labels) && slices.Equal(a.Assignees, b.Assignees)
}

func TestRepoLabels(t *testing.T) {
	srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/acme/widgets/labels" {
			t.Errorf("path = %s", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("limit") != "50" {
			t.Errorf("limit = %q, want 50", q.Get("limit"))
		}
		switch q.Get("page") {
		case "1":
			labels := make([]string, repoLabelsPageSize)
			for i := range labels {
				labels[i] = fmt.Sprintf(`{"name":"l%d","color":"f00"}`, i)
			}
			_, _ = w.Write([]byte("[" + strings.Join(labels, ",") + "]"))
		case "2":
			_, _ = w.Write([]byte(`[{"name":"last","color":"0f0"}]`))
		default:
			t.Errorf("unexpected page %q", q.Get("page"))
		}
	})
	defer srv.Close()

	got, err := c.RepoLabels(t.Context(), "acme", "widgets")
	if err != nil {
		t.Fatalf("RepoLabels: %v", err)
	}
	if len(got) != repoLabelsPageSize+1 || got[len(got)-1] != "last" {
		t.Fatalf("RepoLabels returned %d labels, last %q; want %d labels ending in %q", len(got), got[len(got)-1], repoLabelsPageSize+1, "last")
	}
}

func TestAddLabels(t *testing.T) {
	serve := func(t *testing.T, posted *string) (*httptest.Server, *Client) {
		return newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			switch r.Method + " " + r.URL.Path {
			case "GET /api/v1/repos/acme/widgets/labels":
				_, _ = w.Write([]byte(`[{"id":42,"name":"bug"},{"id":7,"name":"triage"},{"id":9,"name":"docs"}]`))
			case "POST /api/v1/repos/acme/widgets/issues/5/labels":
				b, _ := io.ReadAll(r.Body)
				*posted = string(b)
			default:
				t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			}
		})
	}
	tests := []struct {
		name, wantBody, wantErr string
		labels                  []string
	}{
		{name: "names are sent as ids", labels: []string{"bug", "triage"}, wantBody: `{"labels":[42,7]}`},
		{name: "an unknown name adds nothing", labels: []string{"bug", "nope"}, wantErr: `the repository has no label "nope"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var posted string
			srv, c := serve(t, &posted)
			defer srv.Close()
			err := c.AddLabels(t.Context(), "acme", "widgets", 5, tt.labels)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) || posted != "" {
					t.Fatalf("AddLabels = %v, posted %q; want %q and nothing posted", err, posted, tt.wantErr)
				}
				return
			}
			if err != nil || strings.TrimSpace(posted) != tt.wantBody {
				t.Fatalf("AddLabels = %v, posted %q; want %s", err, posted, tt.wantBody)
			}
		})
	}

	t.Run("no labels sends no request", func(t *testing.T) {
		srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			t.Error("unexpected request for an empty label list")
		})
		defer srv.Close()
		if err := c.AddLabels(t.Context(), "acme", "widgets", 5, nil); err != nil {
			t.Fatalf("AddLabels(nil): %v", err)
		}
	})
}

func TestRemoveLabel(t *testing.T) {
	const labelsPath = "/api/v1/repos/acme/widgets/issues/5/labels"

	t.Run("present label is deleted by its resolved numeric id", func(t *testing.T) {
		var gotDeletePath string
		srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				if r.URL.Path != labelsPath {
					t.Errorf("GET path = %s", r.URL.Path)
				}
				_, _ = w.Write([]byte(`[{"id":42,"name":"bug","color":"f00"},{"id":7,"name":"triage","color":"0f0"}]`))
			case http.MethodDelete:
				gotDeletePath = r.URL.Path
				w.WriteHeader(http.StatusNoContent)
			default:
				t.Errorf("unexpected method %s", r.Method)
			}
		})
		defer srv.Close()
		if err := c.RemoveLabel(t.Context(), "acme", "widgets", 5, "bug"); err != nil {
			t.Fatalf("RemoveLabel: %v", err)
		}
		if want := labelsPath + "/42"; gotDeletePath != want {
			t.Fatalf("DELETE path = %q, want %q (the resolved numeric id, not the name)", gotDeletePath, want)
		}
	})

	t.Run("a label not currently applied is not an error and issues no DELETE", func(t *testing.T) {
		srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				t.Errorf("unexpected %s request; a label absent from the issue must not be DELETEd", r.Method)
				return
			}
			_, _ = w.Write([]byte(`[{"id":7,"name":"triage","color":"0f0"}]`))
		})
		defer srv.Close()
		if err := c.RemoveLabel(t.Context(), "acme", "widgets", 5, "bug"); err != nil {
			t.Fatalf("RemoveLabel of an absent label: %v", err)
		}
	})

	t.Run("a label removed concurrently is not an error", func(t *testing.T) {
		srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				_, _ = w.Write([]byte(`[{"id":42,"name":"bug","color":"f00"}]`))
			case http.MethodDelete:
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"not found"}`))
			}
		})
		defer srv.Close()
		if err := c.RemoveLabel(t.Context(), "acme", "widgets", 5, "bug"); err != nil {
			t.Fatalf("RemoveLabel: %v", err)
		}
	})

	t.Run("a label lookup failure is returned, not swallowed", func(t *testing.T) {
		srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"boom"}`))
		})
		defer srv.Close()
		if err := c.RemoveLabel(t.Context(), "acme", "widgets", 5, "bug"); err == nil {
			t.Fatal("RemoveLabel: want an error when the label lookup fails, got nil")
		}
	})
}

func TestSetState(t *testing.T) {
	cases := []struct {
		name string
		open bool
		want string
	}{
		{"open", true, `"state":"open"`},
		{"closed", false, `"state":"closed"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotBody string
			srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPatch || r.URL.Path != "/api/v1/repos/acme/widgets/issues/5" {
					t.Errorf("method/path = %s %s", r.Method, r.URL.Path)
				}
				b, _ := io.ReadAll(r.Body)
				gotBody = string(b)
			})
			defer srv.Close()
			if err := c.SetState(t.Context(), "acme", "widgets", 5, tc.open); err != nil {
				t.Fatalf("SetState: %v", err)
			}
			if !strings.Contains(gotBody, tc.want) {
				t.Fatalf("request body = %q, want it to contain %q", gotBody, tc.want)
			}
		})
	}
}

func TestAddAssignees(t *testing.T) {
	t.Run("merges with, rather than replaces, current assignees", func(t *testing.T) {
		var gotBody string
		srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				_, _ = w.Write([]byte(`{"number":5,"title":"t","state":"open","user":{"login":"alice"},
					"assignees":[{"login":"alice"}],"html_url":"https://forge.example.com/acme/widgets/issues/5"}`))
			case http.MethodPatch:
				b, _ := io.ReadAll(r.Body)
				gotBody = string(b)
			default:
				t.Errorf("unexpected method %s", r.Method)
			}
		})
		defer srv.Close()
		// alice is already assigned, so requesting alice again must not
		// duplicate her in the merged list sent back to the API.
		if err := c.AddAssignees(t.Context(), "acme", "widgets", 5, []string{"alice", "bob"}); err != nil {
			t.Fatalf("AddAssignees: %v", err)
		}
		if !strings.Contains(gotBody, `"assignees":["alice","bob"]`) {
			t.Fatalf("request body = %q, want assignees [alice bob] with no duplicate", gotBody)
		}
	})

	t.Run("no logins sends no request", func(t *testing.T) {
		srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			t.Error("unexpected request for an empty login list")
		})
		defer srv.Close()
		if err := c.AddAssignees(t.Context(), "acme", "widgets", 5, nil); err != nil {
			t.Fatalf("AddAssignees(nil): %v", err)
		}
	})
}

func TestRequestReviewers(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		var gotBody string
		srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/repos/acme/widgets/pulls/5/requested_reviewers" {
				t.Errorf("path = %s", r.URL.Path)
			}
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
		})
		defer srv.Close()
		if err := c.RequestReviewers(t.Context(), "acme", "widgets", 5, []string{"carol"}); err != nil {
			t.Fatalf("RequestReviewers: %v", err)
		}
		if !strings.Contains(gotBody, `"reviewers":["carol"]`) {
			t.Fatalf("request body = %q", gotBody)
		}
	})

	t.Run("no logins sends no request", func(t *testing.T) {
		srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			t.Error("unexpected request for an empty login list")
		})
		defer srv.Close()
		if err := c.RequestReviewers(t.Context(), "acme", "widgets", 5, nil); err != nil {
			t.Fatalf("RequestReviewers(nil): %v", err)
		}
	})
}

func TestSearchIssues(t *testing.T) {
	srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/acme/widgets/issues" {
			t.Errorf("path = %s", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("q") != "crash on start" || q.Get("limit") != "1" {
			t.Errorf("query = %q, want q=crash+on+start&limit=1", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`[
			{"number":10,"title":"crash on startup","state":"open","user":{"login":"alice"},"html_url":"https://forge.example.com/acme/widgets/issues/10"},
			{"number":11,"title":"another crash","state":"open","user":{"login":"bob"},"html_url":"https://forge.example.com/acme/widgets/issues/11"}
		]`))
	})
	defer srv.Close()

	got, err := c.SearchIssues(t.Context(), "acme", "widgets", "crash on start", 1)
	if err != nil {
		t.Fatalf("SearchIssues: %v", err)
	}
	if len(got) != 1 || got[0].Number != 10 {
		t.Fatalf("SearchIssues = %+v, want 1 result truncated to the limit", got)
	}
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
