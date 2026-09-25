package webhook

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/home-operations/kritik/internal/configfile"
)

// stateOpen is the pull request state kritik acts on; everything else is
// closed, merged or not.
const stateOpen = "open"

// Event names as GitHub and Forgejo put them in the event header.
const (
	evPullRequest   = "pull_request"
	evIssueComment  = "issue_comment"
	evReviewComment = "pull_request_review_comment"
	evPush          = "push"
)

// Kind is what a webhook is about, after the forge-specific shape is gone.
type Kind string

// Event kinds kritik acts on. Anything else parses to KindIgnored.
const (
	KindPing         Kind = "ping"
	KindPullRequest  Kind = "pull_request"
	KindComment      Kind = "comment"
	KindPush         Kind = "push"
	KindInstallation Kind = "installation"
	KindIgnored      Kind = "ignored"
)

// Event is the forge-neutral view of a verified webhook.
type Event struct {
	Kind Kind
	// Action is the forge's own action string: opened, synchronize, created,
	// added, ... Empty when the forge has none for the kind.
	Action string
	// Delivery is the forge's delivery identifier, for logs.
	Delivery string
	// Repository is the repository the event concerns, when it has one.
	Repository *Repository
	// Account is the forge account the event concerns: the repository owner,
	// or the installation account for installation events.
	Account string

	PullRequest  *PullRequest
	Comment      *Comment
	Push         *Push
	Installation *Installation
}

// Repository identifies a repository as the forge names it.
type Repository struct {
	// FullName is "owner/repo".
	FullName      string
	DefaultBranch string
	Private       bool
	CloneURL      string
}

// PullRequest carries the fields the filter and the review pipeline need.
// Every field here is also a key of the CEL `pr` variable.
type PullRequest struct {
	Number      int
	Title       string
	Author      string
	AuthorIsBot bool
	State       string // open or closed
	Merged      bool
	Draft       bool
	Fork        bool
	HeadRef     string
	HeadSHA     string
	BaseRef     string
	BaseSHA     string
	URL         string
	Body        string
	CreatedAt   time.Time
	Labels      []Label
}

// Label is a PR label.
type Label struct {
	Name  string
	Color string
}

// FilterVars is the map the CEL filter evaluates against.
func (p *PullRequest) FilterVars() map[string]any {
	labels := make([]any, len(p.Labels))
	for i, l := range p.Labels {
		labels[i] = map[string]any{"name": l.Name, "color": l.Color}
	}
	return map[string]any{
		"number":    p.Number,
		"title":     p.Title,
		"author":    p.Author,
		"state":     p.State,
		"open":      p.State == stateOpen,
		"merged":    p.Merged,
		"draft":     p.Draft,
		"fork":      p.Fork,
		"headRef":   p.HeadRef,
		"headSha":   p.HeadSHA,
		"baseRef":   p.BaseRef,
		"url":       p.URL,
		"body":      p.Body,
		"createdAt": p.CreatedAt,
		"labels":    labels,
	}
}

// Comment is a comment on a pull request: a top-level conversation comment
// or a reply on an inline finding.
type Comment struct {
	ID          int64
	Number      int // the pull request
	Author      string
	AuthorIsBot bool
	Body        string
	// Inline is set for review comments on a diff line; Path and Line then
	// identify the finding the reply belongs to.
	Inline bool
	Path   string
	Line   int
}

// Push is a branch update.
type Push struct {
	Ref    string // refs/heads/<branch>
	Before string
	After  string
}

// Installation is a GitHub App installation change: which repositories the
// App may now see.
type Installation struct {
	ID int64
	// Repositories is the full list on "created", the delta on
	// "added"/"removed"; the action says which.
	Repositories []string
}

// maxBody bounds a payload before parsing. GitHub caps deliveries at 25 MB;
// a PR event is a few hundred kilobytes at most.
const maxBody = 4 << 20

// Parse turns a verified webhook into an Event. Unknown events are
// KindIgnored rather than an error: forges add event types, and an ignored
// event must not make a delivery fail.
func Parse(forge configfile.Forge, header http.Header, body []byte) (Event, error) {
	if len(body) > maxBody {
		return Event{}, fmt.Errorf("webhook: payload of %d bytes exceeds %d", len(body), maxBody)
	}
	body = unwrapFormPayload(header, body)
	switch forge {
	case configfile.ForgeGitHub:
		return parseGitHub(header.Get("X-GitHub-Event"), header.Get("X-GitHub-Delivery"), body)
	case configfile.ForgeForgejo:
		return parseForgejo(header.Get("X-Gitea-Event"), header.Get("X-Gitea-Delivery"), body)
	case configfile.ForgeGitLab:
		return parseGitLab(header.Get("X-Gitlab-Event-UUID"), body)
	}
	return Event{}, fmt.Errorf("webhook: unsupported forge %q", forge)
}

// unwrapFormPayload returns the JSON document from a webhook body. GitHub and
// Forgejo can deliver application/x-www-form-urlencoded, which wraps the JSON
// in a `payload=` form field. Signature verification runs over the original
// body upstream, so unwrapping here never affects authentication.
func unwrapFormPayload(header http.Header, body []byte) []byte {
	if !strings.HasPrefix(header.Get("Content-Type"), "application/x-www-form-urlencoded") &&
		!bytes.HasPrefix(body, []byte("payload=")) {
		return body
	}
	if v, err := url.ParseQuery(string(body)); err == nil {
		if p := v.Get("payload"); p != "" {
			return []byte(p)
		}
	}
	return body
}

// ghUser is the user shape shared by GitHub and Forgejo payloads.
type ghUser struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

func (u ghUser) isBot() bool {
	return strings.EqualFold(u.Type, "Bot") || strings.HasSuffix(u.Login, "[bot]")
}

// ghRepo is the repository shape shared by GitHub and Forgejo payloads.
type ghRepo struct {
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	Private       bool   `json:"private"`
	CloneURL      string `json:"clone_url"`
	Owner         ghUser `json:"owner"`
}

func (r ghRepo) event() *Repository {
	if r.FullName == "" {
		return nil
	}
	return &Repository{FullName: r.FullName, DefaultBranch: r.DefaultBranch, Private: r.Private, CloneURL: r.CloneURL}
}

// ghPR is the pull request shape shared by GitHub and Forgejo payloads.
type ghPR struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	State     string    `json:"state"`
	Merged    bool      `json:"merged"`
	Draft     bool      `json:"draft"`
	HTMLURL   string    `json:"html_url"`
	CreatedAt time.Time `json:"created_at"`
	User      ghUser    `json:"user"`
	Head      struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"base"`
	Labels []struct {
		Name  string `json:"name"`
		Color string `json:"color"`
	} `json:"labels"`
}

func (p ghPR) event() *PullRequest {
	pr := &PullRequest{
		Number: p.Number, Title: p.Title, Author: p.User.Login, AuthorIsBot: p.User.isBot(),
		State: cmp.Or(p.State, stateOpen), Merged: p.Merged, Draft: p.Draft,
		HeadRef: p.Head.Ref, HeadSHA: p.Head.SHA, BaseRef: p.Base.Ref, BaseSHA: p.Base.SHA,
		URL: p.HTMLURL, Body: p.Body, CreatedAt: p.CreatedAt,
	}
	// A fork PR's head lives in a different repository than its base. A
	// deleted fork leaves head.repo null, which is also not the base repo.
	pr.Fork = p.Head.Repo == nil || p.Head.Repo.FullName != p.Base.Repo.FullName
	for _, l := range p.Labels {
		pr.Labels = append(pr.Labels, Label{Name: l.Name, Color: l.Color})
	}
	return pr
}

func parseGitHub(event, delivery string, body []byte) (Event, error) {
	switch event {
	case "ping":
		return Event{Kind: KindPing, Delivery: delivery}, nil
	case evPullRequest:
		return parsePullRequestEvent(delivery, body)
	case evIssueComment:
		return parseIssueComment(delivery, body)
	case evReviewComment:
		return parseReviewComment(delivery, body)
	case evPush:
		return parsePush(delivery, body)
	case "installation", "installation_repositories":
		return parseInstallation(delivery, body)
	default:
		return Event{Kind: KindIgnored, Delivery: delivery, Action: event}, nil
	}
}

func parseForgejo(event, delivery string, body []byte) (Event, error) {
	switch event {
	case evPullRequest:
		ev, err := parsePullRequestEvent(delivery, body)
		// Forgejo spells the synchronize action "synchronized" (past
		// tense), unlike GitHub's "synchronize"; normalize so downstream
		// action-string matching doesn't need to know which forge sent it.
		if err == nil && ev.Action == "synchronized" {
			ev.Action = "synchronize"
		}
		return ev, err
	case evIssueComment, "pull_request_comment":
		return parseIssueComment(delivery, body)
	case evReviewComment:
		return parseReviewComment(delivery, body)
	case evPush:
		return parsePush(delivery, body)
	default:
		return Event{Kind: KindIgnored, Delivery: delivery, Action: event}, nil
	}
}

func parsePullRequestEvent(delivery string, body []byte) (Event, error) {
	var p struct {
		Action      string `json:"action"`
		Repository  ghRepo `json:"repository"`
		PullRequest ghPR   `json:"pull_request"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return Event{}, fmt.Errorf("webhook: pull_request payload: %w", err)
	}
	return Event{
		Kind: KindPullRequest, Action: p.Action, Delivery: delivery,
		Repository: p.Repository.event(), Account: p.Repository.Owner.Login,
		PullRequest: p.PullRequest.event(),
	}, nil
}

func parseIssueComment(delivery string, body []byte) (Event, error) {
	var p struct {
		Action     string `json:"action"`
		Repository ghRepo `json:"repository"`
		Issue      struct {
			Number      int             `json:"number"`
			PullRequest json.RawMessage `json:"pull_request"`
		} `json:"issue"`
		Comment struct {
			ID   int64  `json:"id"`
			Body string `json:"body"`
			User ghUser `json:"user"`
		} `json:"comment"`
		// Forgejo puts the pull request under is_pull on the issue instead.
		IsPull bool `json:"is_pull"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return Event{}, fmt.Errorf("webhook: issue_comment payload: %w", err)
	}
	// Comments on plain issues are not review follow-ups.
	if len(p.Issue.PullRequest) == 0 && !p.IsPull {
		return Event{Kind: KindIgnored, Action: evIssueComment, Delivery: delivery}, nil
	}
	return Event{
		Kind: KindComment, Action: p.Action, Delivery: delivery,
		Repository: p.Repository.event(), Account: p.Repository.Owner.Login,
		Comment: &Comment{
			ID: p.Comment.ID, Number: p.Issue.Number, Author: p.Comment.User.Login,
			AuthorIsBot: p.Comment.User.isBot(), Body: p.Comment.Body,
		},
	}, nil
}

func parseReviewComment(delivery string, body []byte) (Event, error) {
	var p struct {
		Action      string `json:"action"`
		Repository  ghRepo `json:"repository"`
		PullRequest struct {
			Number int `json:"number"`
		} `json:"pull_request"`
		Comment struct {
			ID   int64  `json:"id"`
			Body string `json:"body"`
			Path string `json:"path"`
			Line int    `json:"line"`
			User ghUser `json:"user"`
		} `json:"comment"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return Event{}, fmt.Errorf("webhook: pull_request_review_comment payload: %w", err)
	}
	return Event{
		Kind: KindComment, Action: p.Action, Delivery: delivery,
		Repository: p.Repository.event(), Account: p.Repository.Owner.Login,
		Comment: &Comment{
			ID: p.Comment.ID, Number: p.PullRequest.Number, Author: p.Comment.User.Login,
			AuthorIsBot: p.Comment.User.isBot(), Body: p.Comment.Body,
			Inline: true, Path: p.Comment.Path, Line: p.Comment.Line,
		},
	}, nil
}

func parsePush(delivery string, body []byte) (Event, error) {
	var p struct {
		Ref        string `json:"ref"`
		Before     string `json:"before"`
		After      string `json:"after"`
		Repository ghRepo `json:"repository"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return Event{}, fmt.Errorf("webhook: push payload: %w", err)
	}
	return Event{
		Kind: KindPush, Delivery: delivery,
		Repository: p.Repository.event(), Account: p.Repository.Owner.Login,
		Push: &Push{Ref: p.Ref, Before: p.Before, After: p.After},
	}, nil
}

func parseInstallation(delivery string, body []byte) (Event, error) {
	var p struct {
		Action       string `json:"action"`
		Installation struct {
			ID      int64  `json:"id"`
			Account ghUser `json:"account"`
		} `json:"installation"`
		Repositories []struct {
			FullName string `json:"full_name"`
		} `json:"repositories"`
		RepositoriesAdded []struct {
			FullName string `json:"full_name"`
		} `json:"repositories_added"`
		RepositoriesRemoved []struct {
			FullName string `json:"full_name"`
		} `json:"repositories_removed"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return Event{}, fmt.Errorf("webhook: installation payload: %w", err)
	}
	inst := &Installation{ID: p.Installation.ID}
	for _, r := range p.Repositories {
		inst.Repositories = append(inst.Repositories, r.FullName)
	}
	for _, r := range p.RepositoriesAdded {
		inst.Repositories = append(inst.Repositories, r.FullName)
	}
	for _, r := range p.RepositoriesRemoved {
		inst.Repositories = append(inst.Repositories, r.FullName)
	}
	return Event{
		Kind: KindInstallation, Action: p.Action, Delivery: delivery,
		Account: p.Installation.Account.Login, Installation: inst,
	}, nil
}

// parseGitLab routes by object_kind. GitLab's shapes differ from the other
// two forges throughout, so it has its own decoders.
func parseGitLab(delivery string, body []byte) (Event, error) {
	var probe struct {
		Kind    string `json:"object_kind"`
		Project struct {
			PathWithNamespace string `json:"path_with_namespace"`
			DefaultBranch     string `json:"default_branch"`
			HTTPURL           string `json:"git_http_url"`
			Namespace         string `json:"namespace"`
		} `json:"project"`
		User struct {
			Username string `json:"username"`
			Bot      bool   `json:"bot"`
		} `json:"user"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return Event{}, fmt.Errorf("webhook: gitlab payload: %w", err)
	}
	repo := &Repository{FullName: probe.Project.PathWithNamespace, DefaultBranch: probe.Project.DefaultBranch, CloneURL: probe.Project.HTTPURL}
	account, _, _ := strings.Cut(probe.Project.PathWithNamespace, "/")
	base := Event{Delivery: delivery, Repository: repo, Account: account}
	switch probe.Kind {
	case "merge_request":
		var p struct {
			ObjectAttributes struct {
				IID          int    `json:"iid"`
				Title        string `json:"title"`
				Description  string `json:"description"`
				State        string `json:"state"` // opened, closed, merged
				Action       string `json:"action"`
				Draft        bool   `json:"draft"`
				URL          string `json:"url"`
				CreatedAt    string `json:"created_at"`
				SourceBranch string `json:"source_branch"`
				TargetBranch string `json:"target_branch"`
				SourceProjID int    `json:"source_project_id"`
				TargetProjID int    `json:"target_project_id"`
				LastCommit   struct {
					ID string `json:"id"`
				} `json:"last_commit"`
			} `json:"object_attributes"`
			Labels []struct {
				Title string `json:"title"`
				Color string `json:"color"`
			} `json:"labels"`
		}
		if err := json.Unmarshal(body, &p); err != nil {
			return Event{}, fmt.Errorf("webhook: merge_request payload: %w", err)
		}
		a := p.ObjectAttributes
		created, _ := time.Parse("2006-01-02 15:04:05 MST", a.CreatedAt)
		pr := &PullRequest{
			Number: a.IID, Title: a.Title, Author: probe.User.Username, AuthorIsBot: probe.User.Bot,
			State: map[bool]string{true: stateOpen, false: "closed"}[a.State == "opened"], Merged: a.State == "merged",
			Draft: a.Draft, Fork: a.SourceProjID != a.TargetProjID,
			HeadRef: a.SourceBranch, HeadSHA: a.LastCommit.ID, BaseRef: a.TargetBranch, URL: a.URL, Body: a.Description, CreatedAt: created,
		}
		for _, l := range p.Labels {
			pr.Labels = append(pr.Labels, Label{Name: l.Title, Color: l.Color})
		}
		base.Kind, base.Action, base.PullRequest = KindPullRequest, a.Action, pr
		return base, nil
	case "note":
		var p struct {
			ObjectAttributes struct {
				ID           int64  `json:"id"`
				Note         string `json:"note"`
				NoteableType string `json:"noteable_type"` //nolint:misspell // GitLab's field is spelled noteable
				Position     *struct {
					NewPath string `json:"new_path"`
					NewLine int    `json:"new_line"`
				} `json:"position"`
			} `json:"object_attributes"`
			MergeRequest struct {
				IID int `json:"iid"`
			} `json:"merge_request"`
		}
		if err := json.Unmarshal(body, &p); err != nil {
			return Event{}, fmt.Errorf("webhook: note payload: %w", err)
		}
		if p.ObjectAttributes.NoteableType != "MergeRequest" {
			base.Kind, base.Action = KindIgnored, "note"
			return base, nil
		}
		c := &Comment{
			ID: p.ObjectAttributes.ID, Number: p.MergeRequest.IID, Author: probe.User.Username,
			AuthorIsBot: probe.User.Bot, Body: p.ObjectAttributes.Note,
		}
		if pos := p.ObjectAttributes.Position; pos != nil {
			c.Inline, c.Path, c.Line = true, pos.NewPath, pos.NewLine
		}
		base.Kind, base.Action, base.Comment = KindComment, "created", c
		return base, nil
	case "push":
		var p struct {
			Ref    string `json:"ref"`
			Before string `json:"before"`
			After  string `json:"after"`
		}
		if err := json.Unmarshal(body, &p); err != nil {
			return Event{}, fmt.Errorf("webhook: push payload: %w", err)
		}
		base.Kind, base.Push = KindPush, &Push{Ref: p.Ref, Before: p.Before, After: p.After}
		return base, nil
	default:
		base.Kind, base.Action = KindIgnored, probe.Kind
		return base, nil
	}
}
