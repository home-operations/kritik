package webhook

import (
	"net/http"
	"testing"
	"time"

	"github.com/home-operations/kritik/internal/configfile"
)

const ghPullRequest = `{
  "action": "synchronize",
  "number": 42,
  "pull_request": {
    "number": 42, "title": "feat: thing", "body": "a body", "state": "open", "draft": false, "merged": false,
    "html_url": "https://github.com/onedr0p/home-ops/pull/42", "created_at": "2026-09-24T10:00:00Z", "updated_at": "2026-09-24T11:30:00Z",
    "user": {"login": "renovate[bot]", "type": "Bot"},
    "head": {"ref": "renovate/x", "sha": "aaa111", "repo": {"full_name": "onedr0p/home-ops"}},
    "base": {"ref": "main", "sha": "bbb222", "repo": {"full_name": "onedr0p/home-ops"}},
    "labels": [{"name": "area/storage", "color": "0e8a16"}]
  },
  "repository": {"full_name": "onedr0p/home-ops", "default_branch": "main", "private": false,
    "clone_url": "https://github.com/onedr0p/home-ops.git", "owner": {"login": "onedr0p", "type": "User"}}
}`

func gh(event string) http.Header {
	h := http.Header{}
	h.Set("X-GitHub-Event", event)
	h.Set("X-GitHub-Delivery", "d-1")
	h.Set("Content-Type", "application/json")
	return h
}

func TestParseGitHubPullRequest(t *testing.T) {
	ev, err := Parse(configfile.ForgeGitHub, gh("pull_request"), []byte(ghPullRequest))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != KindPullRequest || ev.Action != "synchronize" || ev.Delivery != "d-1" || ev.Account != "onedr0p" {
		t.Fatalf("event = %+v", ev)
	}
	pr := ev.PullRequest
	if pr.Number != 42 || pr.HeadSHA != "aaa111" || pr.BaseRef != "main" || !pr.AuthorIsBot || pr.Fork ||
		!pr.UpdatedAt.Equal(time.Date(2026, 9, 24, 11, 30, 0, 0, time.UTC)) {
		t.Fatalf("pr = %+v", pr)
	}
	if ev.Repository.FullName != "onedr0p/home-ops" || ev.Repository.DefaultBranch != "main" {
		t.Fatalf("repo = %+v", ev.Repository)
	}
	vars := pr.FilterVars("opened")
	if vars["open"] != true || vars["author"] != "renovate[bot]" || vars["body"] != "a body" || vars["event"] != "opened" {
		t.Fatalf("vars = %v", vars)
	}
	labels := vars["labels"].([]any)
	if len(labels) != 1 || labels[0].(map[string]any)["name"] != "area/storage" {
		t.Fatalf("labels = %v", labels)
	}
	if pr.ClosedAt != nil {
		t.Fatalf("an open pull request's ClosedAt = %v", pr.ClosedAt)
	}
	closed := []byte(`{"action":"closed","pull_request":{"number":1,"state":"closed","merged":true,
	  "closed_at":"2026-09-24T20:00:00Z","user":{"login":"x"}},
	  "repository":{"full_name":"onedr0p/home-ops","owner":{"login":"onedr0p"}}}`)
	ev, err = Parse(configfile.ForgeGitHub, gh("pull_request"), closed)
	if err != nil || ev.PullRequest.ClosedAt == nil || !ev.PullRequest.ClosedAt.Equal(time.Date(2026, 9, 24, 20, 0, 0, 0, time.UTC)) {
		t.Fatalf("closed PR = %+v, %v; want its closed_at", ev.PullRequest, err)
	}
}

func TestParseGitHubForkAndDeletedFork(t *testing.T) {
	fork := []byte(`{"action":"opened","pull_request":{"number":1,"user":{"login":"x"},
	  "head":{"ref":"f","sha":"1","repo":{"full_name":"someone/home-ops"}},
	  "base":{"ref":"main","sha":"2","repo":{"full_name":"onedr0p/home-ops"}}},
	  "repository":{"full_name":"onedr0p/home-ops","owner":{"login":"onedr0p"}}}`)
	ev, err := Parse(configfile.ForgeGitHub, gh("pull_request"), fork)
	if err != nil || !ev.PullRequest.Fork {
		t.Fatalf("fork PR not detected: %+v %v", ev.PullRequest, err)
	}
	deleted := []byte(`{"action":"opened","pull_request":{"number":1,"user":{"login":"x"},
	  "head":{"ref":"f","sha":"1","repo":null},
	  "base":{"ref":"main","sha":"2","repo":{"full_name":"onedr0p/home-ops"}}},
	  "repository":{"full_name":"onedr0p/home-ops","owner":{"login":"onedr0p"}}}`)
	ev, err = Parse(configfile.ForgeGitHub, gh("pull_request"), deleted)
	if err != nil || !ev.PullRequest.Fork {
		t.Fatalf("deleted-fork PR should count as a fork: %+v %v", ev.PullRequest, err)
	}
}

func TestParseGitHubComments(t *testing.T) {
	tests := []struct {
		name   string
		event  string
		body   string
		kind   Kind
		inline bool
		number int
	}{
		{"issue comment on a PR", "issue_comment", `{"action":"created","issue":{"number":7,"pull_request":{"url":"x"}},
		  "comment":{"id":99,"body":"@bot look","user":{"login":"devin","type":"User"}},
		  "repository":{"full_name":"a/b","owner":{"login":"a"}}}`, KindComment, false, 7},
		{"issue comment on a plain issue", "issue_comment", `{"action":"created","issue":{"number":7},
		  "comment":{"id":99,"body":"hi","user":{"login":"devin"}},"repository":{"full_name":"a/b","owner":{"login":"a"}}}`, KindIgnored, false, 0},
		{"review comment", "pull_request_review_comment", `{"action":"created","pull_request":{"number":8},
		  "comment":{"id":100,"body":"@bot why","path":"main.go","line":12,"user":{"login":"devin"}},
		  "repository":{"full_name":"a/b","owner":{"login":"a"}}}`, KindComment, true, 8},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev, err := Parse(configfile.ForgeGitHub, gh(tt.event), []byte(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			if ev.Kind != tt.kind {
				t.Fatalf("kind = %s, want %s", ev.Kind, tt.kind)
			}
			if tt.kind == KindComment && (ev.Comment.Inline != tt.inline || ev.Comment.Number != tt.number) {
				t.Fatalf("comment = %+v", ev.Comment)
			}
		})
	}
}

func TestParseGitHubPushInstallationPingAndUnknown(t *testing.T) {
	push := `{"ref":"refs/heads/main","before":"1","after":"2","repository":{"full_name":"a/b","default_branch":"main","owner":{"login":"a"}}}`
	ev, err := Parse(configfile.ForgeGitHub, gh("push"), []byte(push))
	if err != nil || ev.Kind != KindPush || ev.Push.After != "2" || ev.Repository.DefaultBranch != "main" {
		t.Fatalf("push = %+v %v", ev, err)
	}
	inst := `{"action":"added","installation":{"id":42,"account":{"login":"onedr0p","type":"User"}},
	  "repositories_added":[{"full_name":"onedr0p/home-ops"}],"repositories_removed":[]}`
	ev, err = Parse(configfile.ForgeGitHub, gh("installation_repositories"), []byte(inst))
	if err != nil || ev.Kind != KindInstallation || ev.Account != "onedr0p" || len(ev.Installation.Repositories) != 1 {
		t.Fatalf("installation = %+v %v", ev, err)
	}
	ev, _ = Parse(configfile.ForgeGitHub, gh("ping"), []byte(`{"zen":"x"}`))
	if ev.Kind != KindPing {
		t.Fatalf("ping kind = %s", ev.Kind)
	}
	ev, _ = Parse(configfile.ForgeGitHub, gh("workflow_run"), []byte(`{}`))
	if ev.Kind != KindIgnored || ev.Action != "workflow_run" {
		t.Fatalf("unknown event = %+v", ev)
	}
}

func TestParseGitHubRepositoryTraits(t *testing.T) {
	tests := []struct {
		name, event, body string
		kind              Kind
		traits            configfile.RepoTraits
	}{
		{"pull request in an archived fork", "pull_request", `{"action":"opened","pull_request":{"number":1,"user":{"login":"x"}},
		  "repository":{"full_name":"a/b","archived":true,"fork":true,"owner":{"login":"a"}}}`,
			KindPullRequest, configfile.RepoTraits{Archived: true, Fork: true}},
		{"push to a fork", "push", `{"ref":"refs/heads/main","after":"2","repository":{"full_name":"a/b","fork":true,"owner":{"login":"a"}}}`,
			KindPush, configfile.RepoTraits{Fork: true}},
		{"repository archived", "repository", `{"action":"archived","repository":{"full_name":"a/b","archived":true,"owner":{"login":"a"}}}`,
			KindRepository, configfile.RepoTraits{Archived: true}},
		{"repository unarchived", "repository", `{"action":"unarchived","repository":{"full_name":"a/b","owner":{"login":"a"}}}`,
			KindRepository, configfile.RepoTraits{}},
		{"fork created", "repository", `{"action":"created","repository":{"full_name":"a/b","fork":true,"owner":{"login":"a"}}}`,
			KindRepository, configfile.RepoTraits{Fork: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev, err := Parse(configfile.ForgeGitHub, gh(tt.event), []byte(tt.body))
			if err != nil || ev.Kind != tt.kind || ev.Account != "a" || ev.Repository == nil || ev.Repository.RepoTraits != tt.traits {
				t.Fatalf("event = %+v, repository %+v, %v", ev, ev.Repository, err)
			}
		})
	}
	ev, err := Parse(configfile.ForgeGitHub, gh("repository"), []byte(`{"action":"edited","repository":{"full_name":"a/b"}}`))
	if err != nil || ev.Kind != KindIgnored || ev.Action != "repository" {
		t.Fatalf("an edit changes nothing kritik keeps: %+v %v", ev, err)
	}
}

func TestParseFormEncodedPayload(t *testing.T) {
	h := gh("push")
	h.Set("Content-Type", "application/x-www-form-urlencoded")
	body := []byte("payload=" + `{"ref":"refs/heads/main","after":"9","repository":{"full_name":"a/b","owner":{"login":"a"}}}`)
	ev, err := Parse(configfile.ForgeGitHub, h, body)
	if err != nil || ev.Kind != KindPush || ev.Push.After != "9" {
		t.Fatalf("form payload = %+v %v", ev, err)
	}
}

func TestParseRejectsMalformedAndOversized(t *testing.T) {
	if _, err := Parse(configfile.ForgeGitHub, gh("pull_request"), []byte(`{not json`)); err == nil {
		t.Fatal("malformed JSON must error")
	}
	if _, err := Parse(configfile.ForgeGitHub, gh("push"), make([]byte, MaxBody+1)); err == nil {
		t.Fatal("oversized payload must error")
	}
}
