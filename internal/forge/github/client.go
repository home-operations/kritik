package github

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v92/github"

	"github.com/home-operations/kritik/internal/forge"
	"github.com/home-operations/kritik/internal/webhook"
)

// statusContext is the name of kritik's commit status.
const statusContext = "kritik/review"

// userTypeBot is how GitHub types App and bot accounts.
const userTypeBot = "Bot"

// Client is one App installation's access to GitHub.
type Client struct {
	app    *App
	api    *gh.Client
	tokens *InstallationTokens
	// webBase is "https://github.com" or the GHES host.
	webBase string

	mu    sync.Mutex
	login string
}

// NewClient builds a Client for an installation. host is empty for
// github.com, or the GHES hostname.
func NewClient(app *App, installationID int64, host string) (*Client, error) {
	tokens := app.InstallationTokens(installationID)
	api, err := app.Client(tokens)
	if err != nil {
		return nil, err
	}
	web := "https://github.com"
	if host != "" {
		web = "https://" + strings.TrimRight(host, "/")
	}
	return &Client{app: app, api: api, tokens: tokens, webBase: web}, nil
}

// MergeBase implements forge.Client through the compare API, whose
// merge_base_commit is exactly what GitHub diffs a PR against.
func (c *Client) MergeBase(ctx context.Context, owner, repo, base, head string) (string, error) {
	cmp, _, err := c.api.Repositories.CompareCommits(ctx, owner, repo, base, head, &gh.ListOptions{PerPage: 1})
	if err != nil {
		return "", fmt.Errorf("github: compare %s...%s: %w", base, head, err)
	}
	sha := cmp.GetMergeBaseCommit().GetSHA()
	if sha == "" {
		return "", fmt.Errorf("github: compare %s...%s returned no merge base", base, head)
	}
	return sha, nil
}

// CloneURL implements forge.Client.
func (c *Client) CloneURL(owner, repo string) string {
	return c.webBase + "/" + owner + "/" + repo + ".git"
}

// GitToken implements forge.Client with the installation token.
func (c *Client) GitToken(ctx context.Context) (string, error) {
	return c.tokens.Token(ctx)
}

// BranchTip implements forge.Client.
func (c *Client) BranchTip(ctx context.Context, owner, repo, branch string) (string, string, error) {
	if branch == "" {
		r, _, err := c.api.Repositories.Get(ctx, owner, repo)
		if err != nil {
			return "", "", fmt.Errorf("github: repository %s/%s: %w", owner, repo, err)
		}
		branch = r.GetDefaultBranch()
	}
	b, _, err := c.api.Repositories.GetBranch(ctx, owner, repo, branch, 1)
	if err != nil {
		return "", "", fmt.Errorf("github: branch %s of %s/%s: %w", branch, owner, repo, err)
	}
	if b.GetCommit().GetSHA() == "" {
		return "", "", fmt.Errorf("github: branch %s of %s/%s has no commit", branch, owner, repo)
	}
	return b.GetCommit().GetSHA(), branch, nil
}

// BotLogin implements forge.Client. An App's comments are authored by the
// user "<slug>[bot]"; the slug comes from the App itself, so nothing in the
// configuration has to repeat it.
func (c *Client) BotLogin(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.login != "" {
		return c.login, nil
	}
	slug, err := c.app.Slug(ctx)
	if err != nil {
		return "", err
	}
	c.login = slug + "[bot]"
	return c.login, nil
}

// FindComment implements forge.Client.
func (c *Client) FindComment(ctx context.Context, owner, repo string, number int, login, marker string) (int64, error) {
	opts := &gh.IssueListCommentsOptions{PerPage: 100}
	for {
		comments, resp, err := c.api.Issues.ListComments(ctx, owner, repo, number, opts)
		if err != nil {
			return 0, fmt.Errorf("github: list comments on #%d: %w", number, err)
		}
		for _, cm := range comments {
			if cm.GetUser().GetLogin() == login && strings.Contains(cm.GetBody(), marker) {
				return cm.GetID(), nil
			}
		}
		if resp.NextPage == 0 {
			return 0, nil
		}
		opts.Page = resp.NextPage
	}
}

// CreateComment implements forge.Client.
func (c *Client) CreateComment(ctx context.Context, owner, repo string, number int, body string) (int64, error) {
	cm, _, err := c.api.Issues.CreateComment(ctx, owner, repo, number, gh.IssueCommentRequest{Body: body})
	if err != nil {
		return 0, fmt.Errorf("github: comment on #%d: %w", number, err)
	}
	return cm.GetID(), nil
}

// UpdateComment implements forge.Client.
func (c *Client) UpdateComment(ctx context.Context, owner, repo string, id int64, body string) error {
	if _, _, err := c.api.Issues.UpdateComment(ctx, owner, repo, id, gh.IssueCommentRequest{Body: body}); err != nil {
		return fmt.Errorf("github: edit comment %d: %w", id, err)
	}
	return nil
}

// CreateReview implements forge.Client with a COMMENT review: visible in
// the Files tab, never a required approval or a request for changes.
func (c *Client) CreateReview(ctx context.Context, owner, repo string, number int, headSHA string, comments []forge.InlineComment) error {
	if len(comments) == 0 {
		return nil
	}
	req := &gh.PullRequestReviewRequest{CommitID: new(headSHA), Event: new("COMMENT")}
	for _, cm := range comments {
		req.Comments = append(req.Comments, &gh.DraftReviewComment{
			Path: new(cm.Path), Line: new(cm.Line), Side: new("RIGHT"), Body: new(cm.Body),
		})
	}
	if _, _, err := c.api.PullRequests.CreateReview(ctx, owner, repo, number, req); err != nil {
		return fmt.Errorf("github: review #%d: %w", number, err)
	}
	return nil
}

// GetComment implements forge.Client.
func (c *Client) GetComment(ctx context.Context, owner, repo string, id int64, inline bool) (forge.Comment, error) {
	if inline {
		cm, _, err := c.api.PullRequests.GetComment(ctx, owner, repo, id)
		if err != nil {
			return forge.Comment{}, fmt.Errorf("github: review comment %d: %w", id, err)
		}
		return inlineComment(cm), nil
	}
	cm, _, err := c.api.Issues.GetComment(ctx, owner, repo, id)
	if err != nil {
		return forge.Comment{}, fmt.Errorf("github: comment %d: %w", id, err)
	}
	return conversationComment(cm), nil
}

// ListConversation implements forge.Client.
func (c *Client) ListConversation(ctx context.Context, owner, repo string, number int) ([]forge.Comment, error) {
	opts := &gh.IssueListCommentsOptions{Sort: new("created"), Direction: new("asc"), PerPage: 100}
	var out []forge.Comment
	for {
		comments, resp, err := c.api.Issues.ListComments(ctx, owner, repo, number, opts)
		if err != nil {
			return nil, fmt.Errorf("github: list comments on #%d: %w", number, err)
		}
		for _, cm := range comments {
			out = append(out, conversationComment(cm))
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
}

// ListInline implements forge.Client.
func (c *Client) ListInline(ctx context.Context, owner, repo string, number int) ([]forge.Comment, error) {
	opts := &gh.PullRequestListCommentsOptions{Sort: "created", Direction: "asc", PerPage: 100}
	var out []forge.Comment
	for {
		comments, resp, err := c.api.PullRequests.ListComments(ctx, owner, repo, number, opts)
		if err != nil {
			return nil, fmt.Errorf("github: list review comments on #%d: %w", number, err)
		}
		for _, cm := range comments {
			out = append(out, inlineComment(cm))
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
}

// Permission implements forge.Client.
func (c *Client) Permission(ctx context.Context, owner, repo, login string) (string, error) {
	level, _, err := c.api.Repositories.GetPermissionLevel(ctx, owner, repo, login)
	if err != nil {
		return "", fmt.Errorf("github: permission of %s on %s/%s: %w", login, owner, repo, err)
	}
	// role_name carries maintain and triage, which permission folds into
	// write and read.
	if name := level.GetRoleName(); name != "" {
		return name, nil
	}
	return level.GetPermission(), nil
}

// ReplyInline implements forge.Client.
func (c *Client) ReplyInline(ctx context.Context, owner, repo string, number int, rootID int64, body string) (int64, error) {
	cm, _, err := c.api.PullRequests.CreateCommentInReplyTo(ctx, owner, repo, number, body, rootID)
	if err != nil {
		return 0, fmt.Errorf("github: reply to review comment %d: %w", rootID, err)
	}
	return cm.GetID(), nil
}

func conversationComment(cm *gh.IssueComment) forge.Comment {
	return forge.Comment{
		ID: cm.GetID(), Author: cm.GetUser().GetLogin(), AuthorIsBot: cm.GetUser().GetType() == userTypeBot,
		Body: cm.GetBody(), CreatedAt: cm.GetCreatedAt().Time,
	}
}

func inlineComment(cm *gh.PullRequestComment) forge.Comment {
	return forge.Comment{
		ID: cm.GetID(), Author: cm.GetUser().GetLogin(), AuthorIsBot: cm.GetUser().GetType() == userTypeBot,
		Body: cm.GetBody(), CreatedAt: cm.GetCreatedAt().Time,
		Inline: true, Path: cm.GetPath(), Line: cm.GetLine(), InReplyTo: cm.GetInReplyTo(),
	}
}

// ListOpenPullRequests implements forge.Client. GitHub sorts by update
// time server-side, so the walk stops at the first page item older than
// since.
func (c *Client) ListOpenPullRequests(ctx context.Context, owner, repo string, since time.Time) ([]forge.OpenPullRequest, error) {
	opts := &gh.PullRequestListOptions{State: "open", Sort: "updated", Direction: "desc", PerPage: 100}
	var out []forge.OpenPullRequest
	for {
		prs, resp, err := c.api.PullRequests.List(ctx, owner, repo, opts)
		if err != nil {
			return nil, fmt.Errorf("github: list open pull requests of %s/%s: %w", owner, repo, err)
		}
		for _, pr := range prs {
			if pr.GetUpdatedAt().Before(since) {
				return out, nil
			}
			out = append(out, openPullRequest(pr))
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
}

func openPullRequest(pr *gh.PullRequest) forge.OpenPullRequest {
	head, base := pr.GetHead(), pr.GetBase()
	out := forge.OpenPullRequest{UpdatedAt: pr.GetUpdatedAt().Time, DefaultBranch: base.GetRepo().GetDefaultBranch(),
		PullRequest: webhook.PullRequest{
			Number: pr.GetNumber(), Title: pr.GetTitle(), Author: pr.GetUser().GetLogin(),
			AuthorIsBot: pr.GetUser().GetType() == userTypeBot || strings.HasSuffix(pr.GetUser().GetLogin(), "[bot]"),
			State:       pr.GetState(), Merged: pr.GetMerged(), Draft: pr.GetDraft(),
			Fork:    head.GetRepo().GetFullName() != "" && head.GetRepo().GetFullName() != base.GetRepo().GetFullName(),
			HeadRef: head.GetRef(), HeadSHA: head.GetSHA(), BaseRef: base.GetRef(), BaseSHA: base.GetSHA(),
			URL: pr.GetHTMLURL(), CreatedAt: pr.GetCreatedAt().Time,
		}}
	for _, l := range pr.Labels {
		out.Labels = append(out.Labels, webhook.Label{Name: l.GetName(), Color: l.GetColor()})
	}
	return out
}

// SetStatus implements forge.Client.
func (c *Client) SetStatus(ctx context.Context, owner, repo, sha string, state forge.StatusState, description string) error {
	status := gh.RepoStatus{
		State: new(string(state)), Context: new(statusContext), Description: new(truncate(description, 140)),
	}
	if _, _, err := c.api.Repositories.CreateStatus(ctx, owner, repo, sha, status); err != nil {
		return fmt.Errorf("github: status on %s: %w", sha, err)
	}
	return nil
}

// truncate keeps a description within GitHub's 140-character limit.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// APIBase derives the REST base for a host: empty for github.com, the
// Enterprise Server path otherwise.
func APIBase(host string) string {
	if host == "" {
		return ""
	}
	return "https://" + strings.TrimRight(host, "/") + "/api/v3"
}
