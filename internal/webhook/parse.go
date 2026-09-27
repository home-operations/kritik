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

// Event names as GitHub and Forgejo put them in the event header. Forgejo's
// X-Gitea-Event folds its finer event types into these: issue label,
// assignee and milestone changes arrive as "issues", the pull request ones
// as "pull_request", told apart by the action (see forgejoActions).
const (
	evPullRequest   = "pull_request"
	evIssues        = "issues"
	evIssueComment  = "issue_comment"
	evReviewComment = "pull_request_review_comment"
	evPush          = "push"
)

// Subject kinds: what an issue, pull request or comment event is about.
const (
	SubjectIssue = "issue"
	SubjectPull  = "pull"
)

// Kind is what a webhook is about, after the forge-specific shape is gone.
type Kind string

// Event kinds kritik acts on. Anything else parses to KindIgnored.
const (
	KindPing         Kind = "ping"
	KindPullRequest  Kind = "pull_request"
	KindIssue        Kind = "issue"
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

	// Forge is the forge the delivery came from.
	Forge configfile.Forge
	// RawEvent is the event header verbatim (X-GitHub-Event, X-Gitea-Event),
	// or GitLab's object_kind.
	RawEvent string
	// Raw is the verified JSON body, unwrapped from a form payload; nil when
	// the body is not JSON.
	Raw json.RawMessage
	// Sender is the login of the user who caused the event, when the
	// payload names one.
	Sender string
	// Subject is the issue or pull request the event concerns, for
	// KindIssue, KindPullRequest and KindComment.
	Subject *Subject

	PullRequest  *PullRequest
	Issue        *Issue
	Comment      *Comment
	Push         *Push
	Installation *Installation
}

// Subject names an issue or pull request by its number.
type Subject struct {
	Kind   string // SubjectIssue or SubjectPull
	Number int
}

// Issue is a plain issue, never a pull request.
type Issue struct {
	Number    int
	Title     string
	Body      string
	State     string // open or closed
	Author    string
	Labels    []string
	Assignees []string
	URL       string
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

// Comment is a comment on an issue or pull request: a top-level
// conversation comment or a reply on an inline finding. The event's Subject
// says which; only pull request comments are review follow-ups.
type Comment struct {
	ID          int64
	Number      int // the issue or pull request
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
	var (
		ev  Event
		err error
	)
	switch forge {
	case configfile.ForgeGitHub:
		ev, err = parseGitHub(header.Get("X-GitHub-Event"), header.Get("X-GitHub-Delivery"), body)
	case configfile.ForgeForgejo, configfile.ForgeGitea:
		// Gitea uses the same X-Gitea-* headers and payload shapes as Forgejo.
		ev, err = parseForgejo(header.Get("X-Gitea-Event"), header.Get("X-Gitea-Delivery"), body)
	case configfile.ForgeGitLab:
		ev, err = parseGitLab(header.Get("X-Gitlab-Event-UUID"), body)
	default:
		return Event{}, fmt.Errorf("webhook: unsupported forge %q", forge)
	}
	if err != nil {
		return Event{}, err
	}
	ev.Forge = forge
	if json.Valid(body) {
		ev.Raw = json.RawMessage(body)
	}
	return ev, nil
}

// envelope is what every GitHub and Forgejo payload shares: enough to say
// who did what where, whatever the event.
type envelope struct {
	Action     string `json:"action"`
	Sender     ghUser `json:"sender"`
	Repository ghRepo `json:"repository"`
}

// withEnvelope stamps the raw event name and the payload's sender on a
// parsed event. Events kritik does not model also get the payload's action
// and repository, so a consumer of the raw delivery can still route them.
// A body that is not JSON leaves an ignored event bare rather than failing
// the delivery.
func withEnvelope(ev Event, err error, event string, body []byte) (Event, error) {
	if err != nil {
		return Event{}, err
	}
	ev.RawEvent = event
	var env envelope
	if json.Unmarshal(body, &env) != nil {
		return ev, nil
	}
	ev.Sender = env.Sender.Login
	if ev.Kind == KindIgnored || ev.Kind == KindPing {
		ev.Action = env.Action
		if ev.Repository == nil {
			ev.Repository = env.Repository.event()
		}
		if ev.Account == "" {
			ev.Account = env.Repository.Owner.Login
		}
	}
	return ev, nil
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
	var (
		ev  Event
		err error
	)
	switch event {
	case "ping":
		ev = Event{Kind: KindPing, Delivery: delivery}
	case evPullRequest:
		ev, err = parsePullRequestEvent(delivery, body)
	case evIssues:
		ev, err = parseIssueEvent(delivery, body)
	case evIssueComment:
		ev, err = parseIssueComment(delivery, body)
	case evReviewComment:
		ev, err = parseReviewComment(delivery, body)
	case evPush:
		ev, err = parsePush(delivery, body)
	case "installation", "installation_repositories":
		ev, err = parseInstallation(delivery, body)
	default:
		ev = Event{Kind: KindIgnored, Delivery: delivery}
	}
	return withEnvelope(ev, err, event, body)
}

// forgejoActions maps Forgejo's action spellings to GitHub's, so downstream
// action matching does not need to know which forge sent the event.
// Forgejo sends "label_updated" for a label added or removed alike, with no
// delta in the payload, so "labeled" here means "labels changed"; only
// "label_cleared" is known to be a removal.
var forgejoActions = map[string]string{
	"synchronized":  "synchronize",
	"label_updated": "labeled",
	"label_cleared": "unlabeled",
}

func parseForgejo(event, delivery string, body []byte) (Event, error) {
	var (
		ev  Event
		err error
	)
	switch event {
	case evPullRequest:
		ev, err = parsePullRequestEvent(delivery, body)
	case evIssues:
		ev, err = parseIssueEvent(delivery, body)
	case evIssueComment, "pull_request_comment":
		ev, err = parseIssueComment(delivery, body)
	case evReviewComment:
		ev, err = parseReviewComment(delivery, body)
	case evPush:
		ev, err = parsePush(delivery, body)
	default:
		ev = Event{Kind: KindIgnored, Delivery: delivery}
	}
	if a, ok := forgejoActions[ev.Action]; ok && (ev.Kind == KindPullRequest || ev.Kind == KindIssue) {
		ev.Action = a
	}
	return withEnvelope(ev, err, event, body)
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
		Subject:     &Subject{Kind: SubjectPull, Number: p.PullRequest.Number},
		PullRequest: p.PullRequest.event(),
	}, nil
}

// parseIssueEvent decodes an "issues" event. Forgejo's issue payload also
// fits here, with its label and assignee changes as actions. An issue that
// is really a pull request is ignored: its changes also arrive as
// pull_request events, which are what kritik models.
func parseIssueEvent(delivery string, body []byte) (Event, error) {
	var p struct {
		Action     string `json:"action"`
		Repository ghRepo `json:"repository"`
		Issue      struct {
			Number int    `json:"number"`
			Title  string `json:"title"`
			Body   string `json:"body"`
			State  string `json:"state"`
			URL    string `json:"html_url"`
			User   ghUser `json:"user"`
			Labels []struct {
				Name string `json:"name"`
			} `json:"labels"`
			Assignees   []ghUser        `json:"assignees"`
			PullRequest json.RawMessage `json:"pull_request"`
		} `json:"issue"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return Event{}, fmt.Errorf("webhook: issues payload: %w", err)
	}
	if isPull(p.Issue.PullRequest) {
		return Event{Kind: KindIgnored, Delivery: delivery}, nil
	}
	is := &Issue{
		Number: p.Issue.Number, Title: p.Issue.Title, Body: p.Issue.Body,
		State: cmp.Or(p.Issue.State, stateOpen), Author: p.Issue.User.Login, URL: p.Issue.URL,
	}
	for _, l := range p.Issue.Labels {
		is.Labels = append(is.Labels, l.Name)
	}
	for _, a := range p.Issue.Assignees {
		is.Assignees = append(is.Assignees, a.Login)
	}
	return Event{
		Kind: KindIssue, Action: p.Action, Delivery: delivery,
		Repository: p.Repository.event(), Account: p.Repository.Owner.Login,
		Subject: &Subject{Kind: SubjectIssue, Number: is.Number},
		Issue:   is,
	}, nil
}

// isPull reports whether an issue's pull_request field marks it as a pull
// request: GitHub omits the field on plain issues, Forgejo sends null.
func isPull(field json.RawMessage) bool {
	return len(field) > 0 && string(field) != "null"
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
	subject := &Subject{Kind: SubjectIssue, Number: p.Issue.Number}
	if isPull(p.Issue.PullRequest) || p.IsPull {
		subject.Kind = SubjectPull
	}
	return Event{
		Kind: KindComment, Action: p.Action, Delivery: delivery,
		Repository: p.Repository.event(), Account: p.Repository.Owner.Login,
		Subject: subject,
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
		Subject: &Subject{Kind: SubjectPull, Number: p.PullRequest.Number},
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
	base := Event{Delivery: delivery, Repository: repo, Account: account, RawEvent: probe.Kind, Sender: probe.User.Username}
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
		base.Subject = &Subject{Kind: SubjectPull, Number: a.IID}
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
		base.Subject = &Subject{Kind: SubjectPull, Number: p.MergeRequest.IID}
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
