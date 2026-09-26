package github

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/home-operations/kritik/internal/forge"
)

// newTestClient builds a Client whose underlying go-github API calls, and
// whose installation-token minting, both hit srv. It mirrors
// TestInstallationTokensMintOnceAndRefresh's server setup but leaves the
// caller free to install its own handler for the actual API request under
// test; the token mint itself is answered directly here since callers don't
// care about it.
func newTestClient(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *Client) {
	t.Helper()
	_, pemKey := testKeyPEM(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/app/installations/42/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"ghs_test","expires_at":"` + exp + `"}`))
	})
	mux.HandleFunc("/", handler)
	srv := httptest.NewServer(mux)

	app, err := NewApp("Iv1.abc", pemKey, srv.URL+"/api/v3")
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewClient(app, 42, "")
	if err != nil {
		t.Fatal(err)
	}
	return srv, c
}

func TestPermission(t *testing.T) {
	respond := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasSuffix(r.URL.Path, "/collaborators/alice/permission") {
				t.Errorf("path = %s", r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}
	}

	cases := []struct {
		name string
		body string
		want forge.Permission
	}{
		{"admin permission", `{"permission":"admin"}`, forge.PermissionAdmin},
		{"write permission", `{"permission":"write"}`, forge.PermissionWrite},
		{"read permission", `{"permission":"read"}`, forge.PermissionRead},
		{"none permission", `{"permission":"none"}`, forge.PermissionNone},
		{"maintain role_name overrides write permission", `{"permission":"write","role_name":"maintain"}`, forge.PermissionMaintain},
		{"triage role_name overrides read permission", `{"permission":"read","role_name":"triage"}`, forge.PermissionTriage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, c := newTestClient(t, respond(tc.body))
			defer srv.Close()
			got, err := c.Permission(t.Context(), "acme", "widgets", "alice")
			if err != nil {
				t.Fatalf("Permission: %v", err)
			}
			if got != tc.want {
				t.Fatalf("Permission = %q, want %q", got, tc.want)
			}
		})
	}

	t.Run("unrecognized permission is an error", func(t *testing.T) {
		srv, c := newTestClient(t, respond(`{"permission":"bogus"}`))
		defer srv.Close()
		if _, err := c.Permission(t.Context(), "acme", "widgets", "alice"); err == nil {
			t.Fatal("expected an error for an unrecognized permission string")
		}
	})
}

// fakeAPI serves canned GitHub responses and records requests.
type fakeAPI struct {
	t        *testing.T
	mux      *http.ServeMux
	requests []string
	bodies   map[string]any
}

func newFakeAPI(t *testing.T) (*fakeAPI, *Client) {
	t.Helper()
	f := &fakeAPI{t: t, mux: http.NewServeMux(), bodies: map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests = append(f.requests, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		if r.Body != nil {
			var body any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body != nil {
				f.bodies[r.Method+" "+r.URL.Path] = body
			}
		}
		f.mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	api, err := newClient(http.DefaultTransport, srv.URL+"/api/v3")
	if err != nil {
		t.Fatal(err)
	}
	return f, &Client{api: api, webBase: srv.URL}
}

func (f *fakeAPI) reply(pattern string, status int, body string) {
	f.mux.HandleFunc(pattern, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
}

func (f *fakeAPI) saw(prefix string) bool {
	for _, r := range f.requests {
		if strings.HasPrefix(r, prefix) {
			return true
		}
	}
	return false
}

func TestMergeBaseAndBranchTip(t *testing.T) {
	f, c := newFakeAPI(t)
	f.reply("GET /api/v3/repos/o/r/compare/main...abc", 200, `{"merge_base_commit":{"sha":"base123"}}`)
	f.reply("GET /api/v3/repos/o/r", 200, `{"default_branch":"trunk"}`)
	f.reply("GET /api/v3/repos/o/r/branches/trunk", 200, `{"name":"trunk","commit":{"sha":"tip456"}}`)
	sha, err := c.MergeBase(t.Context(), "o", "r", 7, "main", "abc")
	if err != nil || sha != "base123" {
		t.Fatalf("MergeBase = %q, %v", sha, err)
	}
	tip, branch, err := c.BranchTip(t.Context(), "o", "r", "")
	if err != nil || tip != "tip456" || branch != "trunk" {
		t.Fatalf("BranchTip = %q %q, %v", tip, branch, err)
	}
	if c.CloneURL("o", "r") != c.webBase+"/o/r.git" {
		t.Fatalf("CloneURL = %s", c.CloneURL("o", "r"))
	}
	f.reply("GET /api/v3/repos/o/r/compare/main...none", 200, `{}`)
	if _, err := c.MergeBase(t.Context(), "o", "r", 7, "main", "none"); err == nil {
		t.Fatal("a compare without a merge base must error")
	}
}

func TestPullRequestDiff(t *testing.T) {
	f, c := newFakeAPI(t)
	const diff = "diff --git a/a.go b/a.go\n+b\n"
	f.mux.HandleFunc("GET /api/v3/repos/o/r/compare/base123...abc", func(w http.ResponseWriter, r *http.Request) {
		if accept := r.Header.Get("Accept"); accept != "application/vnd.github.v3.diff" {
			t.Errorf("Accept = %q, want the diff media type", accept)
		}
		_, _ = w.Write([]byte(diff))
	})
	got, err := c.PullRequestDiff(t.Context(), "o", "r", 7, "base123", "abc")
	if err != nil || got != diff {
		t.Fatalf("PullRequestDiff = %q, %v", got, err)
	}
	f.reply("GET /api/v3/repos/o/r/compare/base123...big", 406, `{"message":"diff too large"}`)
	if _, err := c.PullRequestDiff(t.Context(), "o", "r", 7, "base123", "big"); err == nil {
		t.Fatal("a diff the forge refuses must be an error")
	}
}

func TestFindCommentPaginatesAndMatchesAuthorPlusMarker(t *testing.T) {
	f, c := newFakeAPI(t)
	f.mux.HandleFunc("GET /api/v3/repos/o/r/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "2" {
			_, _ = w.Write([]byte(`[{"id":30,"body":"<!-- kritik:pr-7 --> real","user":{"login":"bot[bot]","type":"Bot"}}]`))
			return
		}
		w.Header().Set("Link", `<`+"http://x"+r.URL.Path+`?page=2>; rel="next"`)
		_, _ = w.Write([]byte(`[{"id":10,"body":"<!-- kritik:pr-7 --> planted","user":{"login":"attacker","type":"User"}},
			{"id":20,"body":"unrelated","user":{"login":"bot[bot]","type":"Bot"}}]`))
	})
	id, err := c.FindComment(t.Context(), "o", "r", 7, "bot[bot]", "<!-- kritik:pr-7 -->")
	if err != nil || id != 30 {
		t.Fatalf("FindComment = %d, %v; a planted marker by another author must not match", id, err)
	}
}

func TestWriteBackCalls(t *testing.T) {
	f, c := newFakeAPI(t)
	f.reply("POST /api/v3/repos/o/r/issues/7/comments", 201, `{"id":100}`)
	f.reply("PATCH /api/v3/repos/o/r/issues/comments/100", 200, `{"id":100}`)
	f.reply("POST /api/v3/repos/o/r/pulls/7/reviews", 200, `{"id":5}`)
	f.reply("POST /api/v3/repos/o/r/statuses/abc", 201, `{"state":"success"}`)
	f.reply("POST /api/v3/repos/o/r/pulls/7/comments", 201, `{"id":200}`)

	id, err := c.CreateComment(t.Context(), "o", "r", 7, "hello")
	if err != nil || id != 100 {
		t.Fatalf("CreateComment = %d, %v", id, err)
	}
	if err := c.UpdateComment(t.Context(), "o", "r", 100, "edited"); err != nil {
		t.Fatal(err)
	}
	if err := c.CreateReview(t.Context(), "o", "r", 7, "abc", nil); err != nil || f.saw("POST /api/v3/repos/o/r/pulls/7/reviews") {
		t.Fatal("a review with no comments must not be posted")
	}
	if err := c.CreateReview(t.Context(), "o", "r", 7, "abc", []forge.InlineComment{{Path: "a.go", Line: 3, Body: "b"}}); err != nil {
		t.Fatal(err)
	}
	review := f.bodies["POST /api/v3/repos/o/r/pulls/7/reviews"].(map[string]any)
	if review["event"] != "COMMENT" || review["commit_id"] != "abc" {
		t.Fatalf("review body = %v; must be a COMMENT review pinned to the head", review)
	}
	if cm := review["comments"].([]any)[0].(map[string]any); cm["side"] != "RIGHT" || cm["line"] != float64(3) {
		t.Fatalf("inline comment = %v", cm)
	}
	long := strings.Repeat("x", 200)
	if err := c.SetStatus(t.Context(), "o", "r", "abc", forge.StatusSuccess, long); err != nil {
		t.Fatal(err)
	}
	status := f.bodies["POST /api/v3/repos/o/r/statuses/abc"].(map[string]any)
	// GitHub's limit is 140 characters, not bytes; the ellipsis is 3 bytes.
	if status["context"] != statusContext || utf8.RuneCountInString(status["description"].(string)) != 140 {
		t.Fatalf("status body = %v", status)
	}
	rid, err := c.ReplyInline(t.Context(), "o", "r", 7, 50, "reply")
	if err != nil || rid != 200 || f.bodies["POST /api/v3/repos/o/r/pulls/7/comments"].(map[string]any)["in_reply_to"] != float64(50) {
		t.Fatalf("ReplyInline = %d, %v, body %v", rid, err, f.bodies["POST /api/v3/repos/o/r/pulls/7/comments"])
	}
}

func TestCommentsPermissionAndOpenPullRequests(t *testing.T) {
	f, c := newFakeAPI(t)
	f.reply("GET /api/v3/repos/o/r/issues/comments/1", 200, `{"id":1,"body":"hi","user":{"login":"u","type":"User"},"created_at":"2026-09-24T20:00:00Z"}`)
	f.reply("GET /api/v3/repos/o/r/pulls/comments/2", 200, `{"id":2,"body":"inline","path":"a.go","line":4,"in_reply_to_id":1,"user":{"login":"b[bot]","type":"Bot"}}`)
	f.reply("GET /api/v3/repos/o/r/issues/7/comments", 200, `[{"id":1,"body":"a","user":{"login":"u"}},{"id":2,"body":"b","user":{"login":"v"}}]`)
	f.reply("GET /api/v3/repos/o/r/pulls/7/comments", 200, `[{"id":3,"body":"c","path":"a.go","line":1,"user":{"login":"u"}}]`)
	f.reply("GET /api/v3/repos/o/r/collaborators/u/permission", 200, `{"permission":"write","role_name":"maintain"}`)
	f.reply("GET /api/v3/repos/o/r/pulls", 200, `[
		{"number":2,"title":"new","state":"open","updated_at":"2026-09-24T22:00:00Z","user":{"login":"x[bot]","type":"Bot"},
		 "head":{"ref":"f","sha":"h2","repo":{"full_name":"fork/r"}},"base":{"ref":"main","sha":"b2","repo":{"full_name":"o/r","default_branch":"main"}},
		 "labels":[{"name":"l"}]},
		{"number":1,"title":"old","state":"open","updated_at":"2026-09-24T10:00:00Z","user":{"login":"u"},
		 "head":{"ref":"g","sha":"h1","repo":{"full_name":"o/r"}},"base":{"ref":"main","sha":"b1","repo":{"full_name":"o/r"}}}]`)

	cm, err := c.GetComment(t.Context(), "o", "r", 7, 1, false)
	if err != nil || cm.Author != "u" || cm.AuthorIsBot || cm.CreatedAt.IsZero() {
		t.Fatalf("GetComment = %+v, %v", cm, err)
	}
	inline, err := c.GetComment(t.Context(), "o", "r", 7, 2, true)
	if err != nil || !inline.Inline || inline.Path != "a.go" || inline.Line != 4 || inline.InReplyTo != 1 || !inline.AuthorIsBot {
		t.Fatalf("inline GetComment = %+v, %v", inline, err)
	}
	conv, err := c.ListConversation(t.Context(), "o", "r", 7)
	if err != nil || len(conv) != 2 || conv[1].Author != "v" {
		t.Fatalf("ListConversation = %+v, %v", conv, err)
	}
	inl, err := c.ListInline(t.Context(), "o", "r", 7)
	if err != nil || len(inl) != 1 || !inl[0].Inline {
		t.Fatalf("ListInline = %+v, %v", inl, err)
	}
	perm, err := c.Permission(t.Context(), "o", "r", "u")
	if err != nil || perm != forge.PermissionMaintain || !forge.CanWrite(perm) || forge.CanWrite(forge.PermissionRead) {
		t.Fatalf("Permission = %q, %v", perm, err)
	}
	since := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	prs, err := c.ListOpenPullRequests(t.Context(), "o", "r", since)
	if err != nil || len(prs) != 1 || prs[0].Number != 2 {
		t.Fatalf("ListOpenPullRequests = %+v, %v; the older PR is past since", prs, err)
	}
	pr := prs[0]
	if !pr.Fork || !pr.AuthorIsBot || pr.HeadSHA != "h2" || pr.DefaultBranch != "main" || len(pr.Labels) != 1 {
		t.Fatalf("open PR = %+v", pr)
	}
}

func TestAPIBaseAndTruncate(t *testing.T) {
	if APIBase("") != "" || APIBase("ghe.example.com/") != "https://ghe.example.com/api/v3" {
		t.Fatal("APIBase")
	}
	if got := truncate(strings.Repeat("a", 150), 140); utf8.RuneCountInString(got) != 140 || !strings.HasSuffix(got, "…") {
		t.Fatalf("truncate = %q", got)
	}
	if truncate("short", 140) != "short" {
		t.Fatal("truncate must leave short text alone")
	}
}
