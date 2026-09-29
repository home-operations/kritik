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

// Event names as GitHub puts them in the X-GitHub-Event header.
const (
	evPullRequest   = "pull_request"
	evIssueComment  = "issue_comment"
	evReviewComment = "pull_request_review_comment"
	evPush          = "push"
	evRepository    = "repository"
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
	// KindRepository is a repository created, archived or unarchived; its
	// Repository says what the forge now says of it.
	KindRepository Kind = "repository"
	KindIgnored    Kind = "ignored"
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
	configfile.RepoTraits
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

// FilterVars is the map the CEL filter evaluates against, for a review
// the event (the pull request action) starts.
func (p *PullRequest) FilterVars(event string) map[string]any {
	return map[string]any{
		"event":     event,
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
		"labels":    p.LabelVars(),
	}
}

// LabelVars is the pull request's labels as a filter sees them.
func (p *PullRequest) LabelVars() []any {
	labels := make([]any, len(p.Labels))
	for i, l := range p.Labels {
		labels[i] = map[string]any{"name": l.Name, "color": l.Color}
	}
	return labels
}

// Comment is a comment on a pull request: a top-level conversation comment
// or a reply on an inline finding.
type Comment struct {
	ID          int64
	Number      int // the pull request
	Author      string
	AuthorIsBot bool
	Body        string
	// Inline is set for review comments on a diff line.
	Inline bool
}

// Push is a branch update.
type Push struct {
	Ref   string // refs/heads/<branch>
	After string
}

// Installation is a GitHub App installation change: which repositories the
// App may now see.
type Installation struct {
	// Repositories is the full list on "created", the delta on
	// "added"/"removed"; the action says which.
	Repositories []string
}

// MaxBody bounds a payload before parsing. GitHub caps deliveries at 25 MB;
// a PR event is a few hundred kilobytes at most.
const MaxBody = 4 << 20

// Parse turns a verified webhook into an Event. Unknown events are
// KindIgnored rather than an error: forges add event types, and an ignored
// event must not make a delivery fail.
func Parse(forge configfile.Forge, header http.Header, body []byte) (Event, error) {
	if len(body) > MaxBody {
		return Event{}, fmt.Errorf("webhook: payload of %d bytes exceeds %d", len(body), MaxBody)
	}
	body = unwrapFormPayload(header, body)
	switch forge {
	case configfile.ForgeGitHub:
		return parseGitHub(header.Get("X-GitHub-Event"), header.Get("X-GitHub-Delivery"), body)
	}
	return Event{}, fmt.Errorf("webhook: unsupported forge %q", forge)
}

// unwrapFormPayload returns the JSON document from a webhook body. GitHub can
// deliver application/x-www-form-urlencoded, which wraps the JSON in a
// `payload=` form field. Signature verification runs over the original
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

// ghUser is a user as GitHub payloads carry one.
type ghUser struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

func (u ghUser) isBot() bool { return IsBot(u.Type, u.Login) }

// IsBot reports whether a GitHub user of userType and login is an App or
// bot account, as the type says or the "[bot]" suffix of its login does.
func IsBot(userType, login string) bool {
	return strings.EqualFold(userType, "Bot") || strings.HasSuffix(login, "[bot]")
}

// ghRepo is a repository as GitHub payloads carry one.
type ghRepo struct {
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	Archived      bool   `json:"archived"`
	Fork          bool   `json:"fork"`
	Owner         ghUser `json:"owner"`
}

func (r ghRepo) event() *Repository {
	if r.FullName == "" {
		return nil
	}
	return &Repository{FullName: r.FullName, DefaultBranch: r.DefaultBranch, Archived: r.Archived, Fork: r.Fork}
}

// ghPR is a pull request as GitHub payloads carry one.
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
		HeadRef: p.Head.Ref, HeadSHA: p.Head.SHA, BaseRef: p.Base.Ref,
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
	case evRepository:
		return parseRepository(delivery, body)
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
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return Event{}, fmt.Errorf("webhook: issue_comment payload: %w", err)
	}
	// Comments on plain issues are not review follow-ups.
	if len(p.Issue.PullRequest) == 0 {
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
			Inline: true,
		},
	}, nil
}

func parsePush(delivery string, body []byte) (Event, error) {
	var p struct {
		Ref        string `json:"ref"`
		After      string `json:"after"`
		Repository ghRepo `json:"repository"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return Event{}, fmt.Errorf("webhook: push payload: %w", err)
	}
	return Event{
		Kind: KindPush, Delivery: delivery,
		Repository: p.Repository.event(), Account: p.Repository.Owner.Login,
		Push: &Push{Ref: p.Ref, After: p.After},
	}, nil
}

// repositoryActions are the repository event actions that change what
// kritik records of one; the rest, such as edited, change nothing it keeps.
var repositoryActions = map[string]bool{"created": true, "archived": true, "unarchived": true}

func parseRepository(delivery string, body []byte) (Event, error) {
	var p struct {
		Action     string `json:"action"`
		Repository ghRepo `json:"repository"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return Event{}, fmt.Errorf("webhook: repository payload: %w", err)
	}
	if !repositoryActions[p.Action] {
		return Event{Kind: KindIgnored, Action: evRepository, Delivery: delivery}, nil
	}
	return Event{
		Kind: KindRepository, Action: p.Action, Delivery: delivery,
		Repository: p.Repository.event(), Account: p.Repository.Owner.Login,
	}, nil
}

func parseInstallation(delivery string, body []byte) (Event, error) {
	var p struct {
		Action       string `json:"action"`
		Installation struct {
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
	inst := &Installation{}
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
