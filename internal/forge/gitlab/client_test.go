package gitlab

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/home-operations/kritik/internal/forge"
	"github.com/home-operations/kritik/internal/webhook"
)

// testToken is the fixed token every newTestServer client authenticates
// with.
const testToken = "tok"

// projectAPI and mr are the escaped API paths of acme/widgets and its
// merge request 7.
const (
	projectAPI = "/api/v4/projects/acme%2Fwidgets"
	mr         = projectAPI + "/merge_requests/7"
)

// newTestServer builds an httptest server and a Client pointed at it, and
// asserts every request carries the token header.
func newTestServer(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("PRIVATE-TOKEN"); got != testToken {
			t.Errorf("PRIVATE-TOKEN = %q, want %q", got, testToken)
		}
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, testToken, srv.Client())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

// route is a request's method and escaped path, which keeps a project
// path's %2F.
func route(r *http.Request) string {
	return r.Method + " " + r.URL.EscapedPath()
}

func decodeBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Errorf("decode %s body: %v", route(r), err)
	}
	return body
}

func TestNewClient(t *testing.T) {
	for _, tc := range []struct{ host, base, web string }{
		{"", "https://gitlab.com/api/v4", "https://gitlab.com"},
		{"gitlab.example.com", "https://gitlab.example.com/api/v4", "https://gitlab.example.com"},
		{"http://127.0.0.1:8080/", "http://127.0.0.1:8080/api/v4", "http://127.0.0.1:8080"},
	} {
		c, err := NewClient(tc.host, "t", nil)
		if err != nil {
			t.Fatalf("NewClient(%q): %v", tc.host, err)
		}
		if c.base != tc.base || c.webBase != tc.web {
			t.Errorf("NewClient(%q) = %q, %q; want %q, %q", tc.host, c.base, c.webBase, tc.base, tc.web)
		}
	}
	if _, err := NewClient("http://", "t", nil); err == nil {
		t.Error("NewClient accepted a URL without a host")
	}
}

func TestURLs(t *testing.T) {
	c, err := NewClient("gitlab.example.com", "t", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := c.CloneURL("acme", "team/widgets"), "https://gitlab.example.com/acme/team/widgets.git"; got != want {
		t.Errorf("CloneURL = %q, want %q", got, want)
	}
	if got, want := c.FileURL("acme", "widgets", "abc", "a/b.go", 3, 0), "https://gitlab.example.com/acme/widgets/-/blob/abc/a/b.go#L3"; got != want {
		t.Errorf("FileURL = %q, want %q", got, want)
	}
	if got, want := c.FileURL("acme", "widgets", "abc", "a.go", 3, 9), "https://gitlab.example.com/acme/widgets/-/blob/abc/a.go#L3-9"; got != want {
		t.Errorf("FileURL range = %q, want %q", got, want)
	}
}

func TestGitToken(t *testing.T) {
	c, err := NewClient("", "api", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := c.GitToken(t.Context()); got != "api" {
		t.Errorf("GitToken = %q, want the API token", got)
	}
	c.FetchToken = "read"
	if got, _ := c.GitToken(t.Context()); got != "read" {
		t.Errorf("GitToken = %q, want the fetch token", got)
	}
}

func TestPullRequestDiff(t *testing.T) {
	// versionServer serves two versions of merge request 7, the older at
	// head, and detail as that version's own read.
	versionServer := func(t *testing.T, detail string) *Client {
		return newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			switch route(r) {
			case "GET " + mr + "/versions":
				_, _ = io.WriteString(w, `[{"id":3,"head_commit_sha":"newer"},{"id":2,"head_commit_sha":"head"}]`)
			case "GET " + mr + "/versions/2":
				_, _ = io.WriteString(w, detail)
			default:
				t.Errorf("request = %s", route(r))
				http.NotFound(w, r)
			}
		})
	}

	t.Run("the version's files under git's headers", func(t *testing.T) {
		c := versionServer(t, `{"id":2,"head_commit_sha":"head","state":"collected","diffs":[
			{"old_path":"a.go","new_path":"a.go","a_mode":"100644","b_mode":"100644","diff":"@@ -1 +1 @@\n-a\n+b"},
			{"old_path":"new.go","new_path":"new.go","a_mode":"0","b_mode":"100644","new_file":true,"diff":"@@ -0,0 +1 @@\n+n\n"},
			{"old_path":"gone.go","new_path":"gone.go","a_mode":"100644","b_mode":"0","deleted_file":true,"diff":"@@ -1 +0,0 @@\n-g\n"},
			{"old_path":"old.go","new_path":"moved.go","a_mode":"100644","b_mode":"100644","renamed_file":true,"diff":""},
			{"old_path":"logo.png","new_path":"logo.png","a_mode":"100644","b_mode":"100644","diff":"Binary files logo.png and logo.png differ\n"}]}`)
		got, err := c.PullRequestDiff(t.Context(), "acme", "widgets", 7, "base", "head")
		want := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-a\n+b\n" +
			"diff --git a/new.go b/new.go\nnew file mode 100644\n--- /dev/null\n+++ b/new.go\n@@ -0,0 +1 @@\n+n\n" +
			"diff --git a/gone.go b/gone.go\ndeleted file mode 100644\n--- a/gone.go\n+++ /dev/null\n@@ -1 +0,0 @@\n-g\n" +
			"diff --git a/old.go b/moved.go\nrename from old.go\nrename to moved.go\n" +
			"diff --git a/logo.png b/logo.png\nBinary files logo.png and logo.png differ\n"
		if err != nil || got != want {
			t.Fatalf("PullRequestDiff = %q, %v\nwant %q", got, err, want)
		}
	})

	for name, detail := range map[string]string{
		"a version GitLab cut short": `{"id":2,"state":"overflow","diffs":[]}`,
		"a file too large to keep":   `{"id":2,"diffs":[{"old_path":"a","new_path":"a","too_large":true,"diff":""}]}`,
		"a file collapsed":           `{"id":2,"diffs":[{"old_path":"a","new_path":"a","collapsed":true,"diff":""}]}`,
		"a diff over the cap":        fmt.Sprintf(`{"id":2,"diffs":[{"old_path":"a","new_path":"a","diff":"@@ -1 +1 @@\n+%s\n"}]}`, strings.Repeat("x", maxDiffBytes)),
	} {
		t.Run(name+" is an error, not a partial diff", func(t *testing.T) {
			if got, err := versionServer(t, detail).PullRequestDiff(t.Context(), "acme", "widgets", 7, "base", "head"); err == nil {
				t.Fatalf("PullRequestDiff = %d bytes, want an error", len(got))
			}
		})
	}

	t.Run("a head GitLab has no version of yet is an error", func(t *testing.T) {
		if _, err := versionServer(t, `{}`).PullRequestDiff(t.Context(), "acme", "widgets", 7, "base", "pushed"); err == nil {
			t.Fatal("PullRequestDiff succeeded without a version")
		}
	})
}

func TestMergeBase(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if route(r) != "GET "+projectAPI+"/repository/merge_base" {
			t.Errorf("request = %s", route(r))
		}
		if refs := r.URL.Query()["refs[]"]; !reflect.DeepEqual(refs, []string{"main", "abc"}) {
			t.Errorf("refs = %q", refs)
		}
		_, _ = io.WriteString(w, `{"id":"base1"}`)
	})
	if got, err := c.MergeBase(t.Context(), "acme", "widgets", 7, "main", "abc"); err != nil || got != "base1" {
		t.Fatalf("MergeBase = %q, %v", got, err)
	}
}

func TestBranchTip(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch route(r) {
		case "GET " + projectAPI:
			_, _ = io.WriteString(w, `{"default_branch":"trunk"}`)
		case "GET " + projectAPI + "/repository/branches/trunk", "GET " + projectAPI + "/repository/branches/feat%2Fx":
			_, _ = io.WriteString(w, `{"commit":{"id":"tip1"}}`)
		default:
			t.Errorf("request = %s", route(r))
			http.NotFound(w, r)
		}
	})
	if sha, branch, err := c.BranchTip(t.Context(), "acme", "widgets", ""); err != nil || sha != "tip1" || branch != "trunk" {
		t.Errorf("BranchTip default = %q, %q, %v", sha, branch, err)
	}
	if sha, branch, err := c.BranchTip(t.Context(), "acme", "widgets", "feat/x"); err != nil || sha != "tip1" || branch != "feat/x" {
		t.Errorf("BranchTip feat/x = %q, %q, %v", sha, branch, err)
	}
}

func TestFileAt(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("ref") != "abc" {
			t.Errorf("ref = %q", r.URL.Query().Get("ref"))
		}
		switch route(r) {
		case "GET " + projectAPI + "/repository/files/dir%2Fa.go/raw":
			_, _ = io.WriteString(w, "package a\n")
		case "GET " + projectAPI + "/repository/files/big/raw":
			_, _ = io.WriteString(w, strings.Repeat("x", forge.MaxFileBytes+1))
		case "GET " + projectAPI + "/repository/files/secret/raw":
			http.Error(w, "forbidden", http.StatusForbidden)
		default:
			http.Error(w, `{"message":"404 File Not Found"}`, http.StatusNotFound)
		}
	})
	if got, err := c.FileAt(t.Context(), "acme", "widgets", "abc", "dir/a.go"); err != nil || string(got) != "package a\n" {
		t.Errorf("FileAt = %q, %v", got, err)
	}
	for path, want := range map[string]error{"missing": fs.ErrNotExist, "big": forge.ErrFileTooLarge} {
		if _, err := c.FileAt(t.Context(), "acme", "widgets", "abc", path); !errors.Is(err, want) {
			t.Errorf("FileAt(%s) = %v, want %v", path, err, want)
		}
	}
	if _, err := c.FileAt(t.Context(), "acme", "widgets", "abc", "secret"); err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Errorf("FileAt(secret) = %v, want an error that is not ErrNotExist", err)
	}
}

func TestBotLoginIsCached(t *testing.T) {
	calls := 0
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if route(r) != "GET /api/v4/user" {
			t.Errorf("request = %s", route(r))
		}
		_, _ = io.WriteString(w, `{"id":9,"username":"project_1_bot_x","bot":true}`)
	})
	for range 2 {
		if got, err := c.BotLogin(t.Context()); err != nil || got != "project_1_bot_x" {
			t.Fatalf("BotLogin = %q, %v", got, err)
		}
	}
	if calls != 1 {
		t.Errorf("GET /user %d times, want 1", calls)
	}
}

func TestConversation(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if route(r) != "GET "+mr+"/notes" {
			t.Errorf("request = %s", route(r))
		}
		q := r.URL.Query()
		if q.Get("sort") != "asc" || q.Get("order_by") != "created_at" || q.Get("per_page") != "100" {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		if q.Get("page") == "1" {
			w.Header().Set("X-Next-Page", "2")
			_, _ = io.WriteString(w, `[
				{"id":1,"body":"added 1 commit","author":{"username":"alice"},"system":true,"created_at":"2026-09-27T10:00:00Z"},
				{"id":2,"body":"looks good","author":{"username":"alice"},"created_at":"2026-09-27T10:01:00Z"}]`)
			return
		}
		w.Header().Set("X-Next-Page", "")
		_, _ = io.WriteString(w, `[
			{"id":3,"body":"on a line","author":{"username":"bob"},"created_at":"2026-09-27T10:02:00Z","position":{"new_path":"a.go","new_line":4}},
			{"id":4,"body":"<!-- kritik --> summary","author":{"username":"kritik"},"created_at":"2026-09-27T10:03:00Z"}]`)
	})
	got, err := c.ListConversation(t.Context(), "acme", "widgets", 7)
	if err != nil {
		t.Fatal(err)
	}
	want := []forge.Comment{
		{ID: 2, Author: "alice", Body: "looks good", CreatedAt: time.Date(2026, 9, 27, 10, 1, 0, 0, time.UTC)},
		{ID: 4, Author: "kritik", Body: "<!-- kritik --> summary", CreatedAt: time.Date(2026, 9, 27, 10, 3, 0, 0, time.UTC)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ListConversation = %+v\nwant %+v", got, want)
	}
	for login, want := range map[string]int64{"kritik": 4, "alice": 0} {
		if id, err := c.FindComment(t.Context(), "acme", "widgets", 7, login, "<!-- kritik -->"); err != nil || id != want {
			t.Errorf("FindComment(%s) = %d, %v; want %d", login, id, err, want)
		}
	}
}

func TestCreateAndUpdateComment(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if body := decodeBody(t, r); body["body"] != "hello\n\\/merge" {
			t.Errorf("%s body = %v, want its quick action escaped", route(r), body)
		}
		switch route(r) {
		case "POST " + mr + "/notes":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":41,"body":"hello"}`)
		case "PUT " + mr + "/notes/41":
			_, _ = io.WriteString(w, `{"id":41,"body":"hello"}`)
		default:
			t.Errorf("request = %s", route(r))
		}
	})
	id, err := c.CreateComment(t.Context(), "acme", "widgets", 7, "hello\n/merge")
	if err != nil || id != 41 {
		t.Fatalf("CreateComment = %d, %v", id, err)
	}
	if err := c.UpdateComment(t.Context(), "acme", "widgets", 7, 41, "hello\n/merge"); err != nil {
		t.Fatalf("UpdateComment: %v", err)
	}
}

// threads serves two diff threads, the first kritik's with a reply, and
// one thread on the overview.
const threads = `[
	{"id":"d1","notes":[
		{"id":10,"body":"finding","author":{"username":"kritik"},"created_at":"2026-09-27T10:00:00Z",
			"position":{"base_sha":"b","start_sha":"s","head_sha":"h1","old_path":"a.go","new_path":"a.go","new_line":4}},
		{"id":12,"body":"@kritik why?","author":{"username":"alice"},"created_at":"2026-09-27T10:05:00Z",
			"position":{"base_sha":"b","start_sha":"s","head_sha":"h1","old_path":"a.go","new_path":"a.go","new_line":4}}]},
	{"id":"d2","notes":[
		{"id":11,"body":"typo","author":{"username":"bob"},"created_at":"2026-09-27T10:02:00Z",
			"position":{"base_sha":"b","start_sha":"s","head_sha":"h1","old_path":"b.go","new_path":"b.go","old_line":7,"new_line":8}}]},
	{"id":"d3","notes":[{"id":13,"body":"overview","author":{"username":"bob"},"created_at":"2026-09-27T10:03:00Z"}]}]`

func TestDiffThreads(t *testing.T) {
	var replied map[string]any
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch route(r) {
		case "GET " + mr + "/discussions":
			_, _ = io.WriteString(w, threads)
		case "POST " + mr + "/discussions/d1/notes":
			replied = decodeBody(t, r)
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":14}`)
		default:
			t.Errorf("request = %s", route(r))
		}
	})
	at := func(minute int) time.Time { return time.Date(2026, 9, 27, 10, minute, 0, 0, time.UTC) }
	finding := forge.Comment{ID: 10, Author: "kritik", Body: "finding", CreatedAt: at(0), Inline: true, Path: "a.go", Line: 4, CommitID: "h1"}
	typo := forge.Comment{ID: 11, Author: "bob", Body: "typo", CreatedAt: at(2), Inline: true, Path: "b.go", Line: 8, CommitID: "h1"}
	reply := forge.Comment{ID: 12, Author: "alice", Body: "@kritik why?", CreatedAt: at(5), Inline: true, Path: "a.go", Line: 4, CommitID: "h1", InReplyTo: 10}

	got, err := c.ListInline(t.Context(), "acme", "widgets", 7)
	if err != nil {
		t.Fatal(err)
	}
	if want := []forge.Comment{finding, typo, reply}; !reflect.DeepEqual(got, want) {
		t.Errorf("ListInline = %+v\nwant %+v", got, want)
	}

	if got, err := c.GetComment(t.Context(), "acme", "widgets", 7, 12, true); err != nil || !reflect.DeepEqual(got, reply) {
		t.Errorf("GetComment(12) = %+v, %v; want %+v", got, err, reply)
	}
	if _, err := c.GetComment(t.Context(), "acme", "widgets", 7, 13, true); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetComment(13) inline = %v, want ErrNotFound: 13 is not on the diff", err)
	}

	if id, err := c.ReplyInline(t.Context(), "acme", "widgets", 7, reply, "because\n/close"); err != nil || id != 14 {
		t.Fatalf("ReplyInline = %d, %v", id, err)
	}
	if replied["body"] != "because\n\\/close" {
		t.Errorf("reply body = %v", replied)
	}
	if _, err := c.ReplyInline(t.Context(), "acme", "widgets", 7, forge.Comment{ID: 99}, "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("ReplyInline to an unknown note = %v, want ErrNotFound", err)
	}
}

func TestGetConversationComment(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if route(r) != "GET "+mr+"/notes/13" {
			t.Errorf("request = %s", route(r))
		}
		_, _ = io.WriteString(w, `{"id":13,"body":"@kritik explain","author":{"username":"bob"},"created_at":"2026-09-27T10:03:00Z"}`)
	})
	got, err := c.GetComment(t.Context(), "acme", "widgets", 7, 13, false)
	want := forge.Comment{ID: 13, Author: "bob", Body: "@kritik explain", CreatedAt: time.Date(2026, 9, 27, 10, 3, 0, 0, time.UTC)}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("GetComment = %+v, %v; want %+v", got, err, want)
	}
}

func TestCreateReview(t *testing.T) {
	const hunks = "@@ -1,4 +1,5 @@\n a\n-b\n+c\n+d\n e\n f\n@@ -20,2 +21,3 @@\n x\n+y\n z\n"
	var posted []map[string]any
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch route(r) {
		case "GET " + mr + "/versions":
			_, _ = io.WriteString(w, `[{"id":3,"head_commit_sha":"h2"},{"id":2,"head_commit_sha":"h1"}]`)
		case "GET " + mr + "/versions/2":
			_, _ = fmt.Fprintf(w, `{"id":2,"head_commit_sha":"h1","base_commit_sha":"b1","start_commit_sha":"s1","diffs":[
				{"old_path":"old.go","new_path":"a.go","renamed_file":true,"diff":%q}]}`, hunks)
		case "POST " + mr + "/discussions":
			body := decodeBody(t, r)
			posted = append(posted, body)
			if body["body"] == "refused" {
				http.Error(w, `{"message":"400 Bad request - Note {:line_code=>[\"can't be blank\"]}"}`, http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":"d9"}`)
		default:
			t.Errorf("request = %s", route(r))
		}
	})
	err := c.CreateReview(t.Context(), "acme", "widgets", 7, "h1", []forge.InlineComment{
		{Path: "a.go", Line: 2, Body: "added\n/approve"},
		{Path: "a.go", Line: 4, Body: "refused"},
		{Path: "a.go", Line: 23, Body: "unchanged"},
	})
	if err == nil || !strings.Contains(err.Error(), "a.go:4") {
		t.Errorf("CreateReview = %v, want the refused comment's error", err)
	}
	position := func(oldLine, newLine int) map[string]any {
		p := map[string]any{"position_type": "text", "base_sha": "b1", "start_sha": "s1", "head_sha": "h1",
			"old_path": "old.go", "new_path": "a.go", "new_line": float64(newLine)}
		if oldLine > 0 {
			p["old_line"] = float64(oldLine)
		}
		return p
	}
	want := []map[string]any{
		{"body": "added\n\\/approve", "position": position(0, 2)},
		{"body": "refused", "position": position(3, 4)},
		{"body": "unchanged", "position": position(21, 23)},
	}
	if !reflect.DeepEqual(posted, want) {
		t.Errorf("posted threads = %v\nwant %v", posted, want)
	}

	if err := c.CreateReview(t.Context(), "acme", "widgets", 7, "gone", []forge.InlineComment{{Path: "a.go", Line: 2}}); err == nil {
		t.Error("CreateReview on a head with no version succeeded")
	}
	if err := c.CreateReview(t.Context(), "acme", "widgets", 7, "h1", nil); err != nil {
		t.Errorf("CreateReview without comments = %v", err)
	}
}

func TestUnchangedLine(t *testing.T) {
	const hunks = "@@ -1,4 +1,5 @@ func a()\n a\n-b\n+c\n+d\n e\n\\ No newline at end of file\n@@ -40 +41 @@\n x\n"
	for _, tc := range []struct{ line, want int }{
		{1, 1},   // context before the change
		{2, 0},   // added
		{3, 0},   // added
		{4, 3},   // context after one removed and two added lines
		{41, 40}, // a later hunk
		{9, 0},   // not in the diff
	} {
		if got := unchangedLine(hunks, tc.line); got != tc.want {
			t.Errorf("unchangedLine(%d) = %d, want %d", tc.line, got, tc.want)
		}
	}
}

func TestNoQuickActions(t *testing.T) {
	for _, tt := range []struct{ name, in, want string }{
		{"a command", "/merge", `\/merge`},
		{"one line of several", "looks off\n/approve\nsee above", "looks off\n\\/approve\nsee above"},
		{"indented, which GitLab does not run", "  /approve", "  /approve"},
		{"in a quote", "> /approve", "> /approve"},
		{"in fenced code", "```go\n/x\n```\n/close", "```go\n/x\n```\n\\/close"},
		{"in a suggestion", "```suggestion:-0+0\n// keep\n```", "```suggestion:-0+0\n// keep\n```"},
		{"in a tilde fence", "~~~\n/x\n~~~\n/y", "~~~\n/x\n~~~\n\\/y"},
		{"past a shorter fence inside a longer one", "````\n```\n/x\n````\n/y", "````\n```\n/x\n````\n\\/y"},
		{"in a fence left open", "```\n/x", "```\n/x"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := noQuickActions(tt.in); got != tt.want {
				t.Errorf("noQuickActions(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestSetStatus(t *testing.T) {
	var got []map[string]any
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if route(r) != "POST "+projectAPI+"/statuses/abc" {
			t.Errorf("request = %s", route(r))
		}
		got = append(got, decodeBody(t, r))
		w.WriteHeader(http.StatusCreated)
	})
	long := strings.Repeat("é", forge.MaxStatusDescription+5)
	for _, s := range []forge.StatusState{forge.StatusPending, forge.StatusSuccess, forge.StatusError} {
		if err := c.SetStatus(t.Context(), "acme", "widgets", "abc", s, long); err != nil {
			t.Fatalf("SetStatus(%s): %v", s, err)
		}
	}
	for i, state := range []string{"pending", "success", "canceled"} {
		want := map[string]any{"state": state, "name": forge.StatusContext, "description": forge.StatusDescription(long)}
		if !reflect.DeepEqual(got[i], want) {
			t.Errorf("status %d = %v, want %v", i, got[i], want)
		}
	}
}

func TestPermission(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch route(r) {
		case "GET /api/v4/users":
			switch r.URL.Query().Get("username") {
			case "ghost":
				_, _ = io.WriteString(w, `[]`)
			case "stranger":
				_, _ = io.WriteString(w, `[{"id":404,"username":"stranger"}]`)
			default:
				_, _ = fmt.Fprintf(w, `[{"id":%s,"username":"x"}]`, strings.TrimPrefix(r.URL.Query().Get("username"), "level"))
			}
		case "GET " + projectAPI + "/members/all/404":
			http.Error(w, `{"message":"404 Not found"}`, http.StatusNotFound)
		default:
			level := strings.TrimPrefix(r.URL.EscapedPath(), projectAPI+"/members/all/")
			_, _ = fmt.Fprintf(w, `{"id":%s,"access_level":%s}`, level, level)
		}
	})
	for login, want := range map[string]forge.Permission{
		"level50": forge.PermissionAdmin, "level40": forge.PermissionMaintain, "level30": forge.PermissionWrite,
		"level20": forge.PermissionTriage, "level15": forge.PermissionRead, "level10": forge.PermissionRead,
		"level5": forge.PermissionNone, "ghost": forge.PermissionNone, "stranger": forge.PermissionNone,
	} {
		if got, err := c.Permission(t.Context(), "acme", "widgets", login); err != nil || got != want {
			t.Errorf("Permission(%s) = %q, %v; want %q", login, got, err, want)
		}
	}
}

func TestListOpenPullRequests(t *testing.T) {
	since := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch route(r) {
		case "GET " + projectAPI:
			_, _ = io.WriteString(w, `{"default_branch":"main"}`)
		case "GET " + projectAPI + "/merge_requests":
			q := r.URL.Query()
			if q.Get("state") != "opened" || q.Get("order_by") != "updated_at" || q.Get("sort") != "desc" ||
				q.Get("with_labels_details") != "true" || q.Get("updated_after") != "2026-09-20T00:00:00Z" {
				t.Errorf("query = %s", r.URL.RawQuery)
			}
			_, _ = io.WriteString(w, `[
				{"iid":7,"title":"Add x","description":"why","state":"opened","draft":true,"sha":"h1",
					"source_branch":"feat","target_branch":"main","source_project_id":1,"target_project_id":1,
					"author":{"username":"alice"},"web_url":"https://gitlab.example.com/acme/widgets/-/merge_requests/7",
					"created_at":"2026-09-21T00:00:00Z","updated_at":"2026-09-22T00:00:00Z",
					"labels":[{"name":"bug","color":"#d9534f"}]},
				{"iid":8,"title":"From a fork","state":"opened","sha":"h2","source_branch":"main","target_branch":"main",
					"source_project_id":2,"target_project_id":1,"author":{"username":"renovate","bot":true},
					"created_at":"2026-09-21T00:00:00Z","updated_at":"2026-09-21T12:00:00Z","labels":[]}]`)
		default:
			t.Errorf("request = %s", route(r))
		}
	})
	got, err := c.ListOpenPullRequests(t.Context(), "acme", "widgets", since)
	if err != nil {
		t.Fatal(err)
	}
	day := func(d, h int) time.Time { return time.Date(2026, 9, d, h, 0, 0, 0, time.UTC) }
	want := []forge.OpenPullRequest{
		{
			Number: 7, Title: "Add x", Author: "alice", State: "open", Draft: true, HeadRef: "feat", HeadSHA: "h1",
			BaseRef: "main", URL: "https://gitlab.example.com/acme/widgets/-/merge_requests/7", Body: "why",
			CreatedAt: day(21, 0), Labels: []webhook.Label{{Name: "bug", Color: "d9534f"}}, UpdatedAt: day(22, 0), DefaultBranch: "main",
		},
		{
			Number: 8, Title: "From a fork", Author: "renovate", AuthorIsBot: true, State: "open", Fork: true,
			HeadRef: "main", HeadSHA: "h2", BaseRef: "main", CreatedAt: day(21, 0), UpdatedAt: day(21, 12), DefaultBranch: "main",
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ListOpenPullRequests = %+v\nwant %+v", got, want)
	}
}
