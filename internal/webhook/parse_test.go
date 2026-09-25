package webhook

import (
	"net/http"
	"testing"

	"github.com/home-operations/kritik/internal/configfile"
)

const ghPullRequest = `{
  "action": "synchronize",
  "number": 42,
  "pull_request": {
    "number": 42, "title": "feat: thing", "body": "a body", "state": "open", "draft": false, "merged": false,
    "html_url": "https://github.com/onedr0p/home-ops/pull/42", "created_at": "2026-09-24T10:00:00Z",
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

func hdr(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
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
	if pr.Number != 42 || pr.HeadSHA != "aaa111" || pr.BaseRef != "main" || !pr.AuthorIsBot || pr.Fork {
		t.Fatalf("pr = %+v", pr)
	}
	if ev.Repository.FullName != "onedr0p/home-ops" || ev.Repository.DefaultBranch != "main" {
		t.Fatalf("repo = %+v", ev.Repository)
	}
	vars := pr.FilterVars()
	if vars["open"] != true || vars["author"] != "renovate[bot]" || vars["body"] != "a body" {
		t.Fatalf("vars = %v", vars)
	}
	labels := vars["labels"].([]any)
	if len(labels) != 1 || labels[0].(map[string]any)["name"] != "area/storage" {
		t.Fatalf("labels = %v", labels)
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
			if tt.inline && (ev.Comment.Path != "main.go" || ev.Comment.Line != 12) {
				t.Fatalf("inline position = %+v", ev.Comment)
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
	if err != nil || ev.Kind != KindInstallation || ev.Account != "onedr0p" || ev.Installation.ID != 42 || len(ev.Installation.Repositories) != 1 {
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
	if _, err := Parse(configfile.ForgeGitHub, gh("push"), make([]byte, maxBody+1)); err == nil {
		t.Fatal("oversized payload must error")
	}
}

func TestParseGitLab(t *testing.T) {
	mr := `{"object_kind":"merge_request","user":{"username":"devin","bot":false},
	  "project":{"path_with_namespace":"group/repo","default_branch":"main","git_http_url":"https://gl/group/repo.git"},
	  "object_attributes":{"iid":5,"title":"t","description":"mr body","state":"opened","action":"update","draft":false,"url":"u",
	    "created_at":"2026-09-24 10:00:00 UTC","source_branch":"f","target_branch":"main",
	    "source_project_id":1,"target_project_id":1,"last_commit":{"id":"abc"}},
	  "labels":[{"title":"x","color":"#fff"}]}`
	ev, err := Parse(configfile.ForgeGitLab, hdr("X-Gitlab-Event-UUID", "u-1"), []byte(mr))
	if err != nil || ev.Kind != KindPullRequest || ev.Account != "group" || ev.PullRequest.Number != 5 ||
		ev.PullRequest.State != "open" || ev.PullRequest.HeadSHA != "abc" || ev.PullRequest.Fork || len(ev.PullRequest.Labels) != 1 ||
		ev.PullRequest.Body != "mr body" {
		t.Fatalf("gitlab mr = %+v %+v %v", ev, ev.PullRequest, err)
	}
	//nolint:misspell // GitLab's field is spelled noteable
	note := `{"object_kind":"note","user":{"username":"devin"},"project":{"path_with_namespace":"group/repo"},
	  "merge_request":{"iid":5},"object_attributes":{"id":77,"note":"@bot","noteable_type":"MergeRequest",
	  "position":{"new_path":"a.go","new_line":3}}}`
	ev, err = Parse(configfile.ForgeGitLab, http.Header{}, []byte(note))
	if err != nil || ev.Kind != KindComment || !ev.Comment.Inline || ev.Comment.Number != 5 || ev.Comment.Path != "a.go" {
		t.Fatalf("gitlab note = %+v %+v %v", ev, ev.Comment, err)
	}
	push := `{"object_kind":"push","ref":"refs/heads/main","after":"9","project":{"path_with_namespace":"group/repo","default_branch":"main"}}`
	ev, err = Parse(configfile.ForgeGitLab, http.Header{}, []byte(push))
	if err != nil || ev.Kind != KindPush || ev.Push.After != "9" {
		t.Fatalf("gitlab push = %+v %v", ev, err)
	}
}

func TestParseForgejo(t *testing.T) {
	h := hdr("X-Gitea-Event", "pull_request", "X-Gitea-Delivery", "f-1")
	ev, err := Parse(configfile.ForgeForgejo, h, []byte(ghPullRequest))
	if err != nil || ev.Kind != KindPullRequest || ev.Delivery != "f-1" || ev.PullRequest.Number != 42 || ev.PullRequest.Body != "a body" || ev.Action != "synchronize" {
		t.Fatalf("forgejo pr = %+v %v", ev, err)
	}
	h.Set("X-Gitea-Event", "issue_comment")
	ev, err = Parse(configfile.ForgeForgejo, h, []byte(`{"action":"created","is_pull":true,"issue":{"number":3},
	  "comment":{"id":5,"body":"@bot","user":{"login":"x"}},"repository":{"full_name":"a/b","owner":{"login":"a"}}}`))
	if err != nil || ev.Kind != KindComment || ev.Comment.Number != 3 {
		t.Fatalf("forgejo comment = %+v %v", ev, err)
	}
}

// TestParseForgejoSynchronizedAction covers the past-tense "synchronized"
// spelling Forgejo sends for this action, which must normalize to GitHub's
// "synchronize" so callers can match on one action string regardless of
// forge.
func TestParseForgejoSynchronizedAction(t *testing.T) {
	h := hdr("X-Gitea-Event", "pull_request", "X-Gitea-Delivery", "f-2")
	body := `{
	  "action": "synchronized",
	  "number": 7,
	  "pull_request": {
	    "number": 7, "title": "feat: thing", "state": "open",
	    "user": {"login": "alice"},
	    "head": {"ref": "topic", "sha": "aaa111", "repo": {"full_name": "acme/widgets"}},
	    "base": {"ref": "main", "sha": "bbb222", "repo": {"full_name": "acme/widgets"}}
	  },
	  "repository": {"full_name": "acme/widgets", "owner": {"login": "acme"}}
	}`
	ev, err := Parse(configfile.ForgeForgejo, h, []byte(body))
	if err != nil || ev.Kind != KindPullRequest || ev.Action != "synchronize" {
		t.Fatalf("forgejo synchronized pr = %+v %v", ev, err)
	}
	// The same raw action, parsed as a GitHub payload, must be left alone:
	// only the Forgejo path normalizes it.
	gh := hdr("X-GitHub-Event", "pull_request", "X-GitHub-Delivery", "f-3")
	ev, err = Parse(configfile.ForgeGitHub, gh, []byte(body))
	if err != nil || ev.Action != "synchronized" {
		t.Fatalf("github synchronized pr = %+v %v", ev, err)
	}
}
