package github

import (
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v92/github"

	"github.com/home-operations/kritik/internal/forge"
	"github.com/home-operations/kritik/internal/webhook"
)

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
// merge_base_commit is exactly what GitHub diffs a PR against. number is
// unused: GitHub's compare API needs only the two refs.
func (c *Client) MergeBase(ctx context.Context, owner, repo string, number int, base, head string) (string, error) {
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

// PullRequestDiff implements forge.Client from the compare of base and
// head, both commits, so the diff is of exactly those two.
func (c *Client) PullRequestDiff(ctx context.Context, owner, repo string, _ int, base, head string) (string, error) {
	diff, _, err := c.api.Repositories.CompareCommitsRaw(ctx, owner, repo, base, head, gh.RawOptions{Type: gh.Diff})
	if err != nil {
		return "", fmt.Errorf("github: diff %s...%s: %w", base, head, err)
	}
	return diff, nil
}

// CloneURL implements forge.Client.
func (c *Client) CloneURL(owner, repo string) string {
	return c.webBase + "/" + owner + "/" + repo + ".git"
}

// LineRanges implements forge.Client: GitHub review comments take a
// start_line.
func (c *Client) LineRanges() bool { return true }

// FileURL implements forge.Client.
func (c *Client) FileURL(owner, repo, sha, path string, line, endLine int) string {
	u := fmt.Sprintf("%s/%s/%s/blob/%s/%s#L%d", c.webBase, owner, repo, sha, path, line)
	if endLine > line {
		u += fmt.Sprintf("-L%d", endLine)
	}
	return u
}

// GitToken implements forge.Client with the installation token.
func (c *Client) GitToken(ctx context.Context) (string, error) {
	return c.tokens.Token(ctx)
}

// ReadGitToken implements forge.Client with an installation token minted
// for repo alone that can only read its contents.
func (c *Client) ReadGitToken(ctx context.Context, _, repo string) (string, error) {
	return c.tokens.ReadOnly(ctx, repo)
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

// FileAt implements forge.Client through the contents API, which inlines
// a file up to forge.MaxFileBytes. A symlink or submodule is not a file.
func (c *Client) FileAt(ctx context.Context, owner, repo, ref, path string) ([]byte, error) {
	fc, _, resp, err := c.api.Repositories.GetContents(ctx, owner, repo, path, &gh.RepositoryContentGetOptions{Ref: ref})
	if resp != nil && resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("github: %s of %s/%s at %s: %w", path, owner, repo, ref, fs.ErrNotExist)
	}
	if err != nil {
		return nil, fmt.Errorf("github: %s of %s/%s at %s: %w", path, owner, repo, ref, err)
	}
	if fc == nil || fc.GetType() != "file" {
		return nil, fmt.Errorf("github: %s of %s/%s at %s is not a file: %w", path, owner, repo, ref, fs.ErrNotExist)
	}
	if fc.GetSize() > forge.MaxFileBytes {
		return nil, fmt.Errorf("github: %s of %s/%s at %s: %w", path, owner, repo, ref, forge.ErrFileTooLarge)
	}
	content, err := fc.GetContent()
	if err != nil {
		return nil, fmt.Errorf("github: %s of %s/%s at %s: %w", path, owner, repo, ref, err)
	}
	return []byte(content), nil
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
	for cm, err := range c.api.Issues.ListCommentsIter(ctx, owner, repo, number, &gh.IssueListCommentsOptions{PerPage: 100}) {
		if err != nil {
			return 0, fmt.Errorf("github: list comments on #%d: %w", number, err)
		}
		if cm.GetUser().GetLogin() == login && strings.Contains(cm.GetBody(), marker) {
			return cm.GetID(), nil
		}
	}
	return 0, nil
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
		c := &gh.DraftReviewComment{Path: new(cm.Path), Line: new(cm.Line), Side: new("RIGHT"), Body: new(cm.Body)}
		if cm.StartLine > 0 && cm.StartLine < cm.Line {
			c.StartLine, c.StartSide = new(cm.StartLine), new("RIGHT")
		}
		req.Comments = append(req.Comments, c)
	}
	if _, _, err := c.api.PullRequests.CreateReview(ctx, owner, repo, number, req); err != nil {
		return fmt.Errorf("github: review #%d: %w", number, err)
	}
	return nil
}

// GetComment implements forge.Client. GitHub resolves a comment by id alone,
// so number (the pull request it belongs to) is unused.
func (c *Client) GetComment(ctx context.Context, owner, repo string, _ int, id int64, inline bool) (forge.Comment, error) {
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
	for cm, err := range c.api.Issues.ListCommentsIter(ctx, owner, repo, number, opts) {
		if err != nil {
			return nil, fmt.Errorf("github: list comments on #%d: %w", number, err)
		}
		out = append(out, conversationComment(cm))
	}
	return out, nil
}

// ListInline implements forge.Client.
func (c *Client) ListInline(ctx context.Context, owner, repo string, number int) ([]forge.Comment, error) {
	opts := &gh.PullRequestListCommentsOptions{Sort: "created", Direction: "asc", PerPage: 100}
	var out []forge.Comment
	for cm, err := range c.api.PullRequests.ListCommentsIter(ctx, owner, repo, number, opts) {
		if err != nil {
			return nil, fmt.Errorf("github: list review comments on #%d: %w", number, err)
		}
		out = append(out, inlineComment(cm))
	}
	return out, nil
}

// Permission implements forge.Client.
func (c *Client) Permission(ctx context.Context, owner, repo, login string) (forge.Permission, error) {
	level, _, err := c.api.Repositories.GetPermissionLevel(ctx, owner, repo, login)
	if err != nil {
		return "", fmt.Errorf("github: permission of %s on %s/%s: %w", login, owner, repo, err)
	}
	// role_name carries maintain and triage, which permission folds into
	// write and read.
	p := forge.Permission(level.GetRoleName())
	if p == "" {
		p = forge.Permission(level.GetPermission())
	}
	if !p.Valid() {
		return "", fmt.Errorf("github: unrecognized permission %q for %s on %s/%s", p, login, owner, repo)
	}
	return p, nil
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
	for pr, err := range c.api.PullRequests.ListIter(ctx, owner, repo, opts) {
		if err != nil {
			return nil, fmt.Errorf("github: list open pull requests of %s/%s: %w", owner, repo, err)
		}
		if pr.GetUpdatedAt().Before(since) {
			break
		}
		out = append(out, openPullRequest(pr))
	}
	return out, nil
}

func openPullRequest(pr *gh.PullRequest) forge.OpenPullRequest {
	head, base := pr.GetHead(), pr.GetBase()
	out := forge.OpenPullRequest{
		Number: pr.GetNumber(), Title: pr.GetTitle(), Author: pr.GetUser().GetLogin(),
		AuthorIsBot: pr.GetUser().GetType() == userTypeBot || strings.HasSuffix(pr.GetUser().GetLogin(), "[bot]"),
		State:       pr.GetState(), Merged: pr.GetMerged(), Draft: pr.GetDraft(),
		Fork:    head.GetRepo().GetFullName() != "" && head.GetRepo().GetFullName() != base.GetRepo().GetFullName(),
		HeadRef: head.GetRef(), HeadSHA: head.GetSHA(), BaseRef: base.GetRef(), BaseSHA: base.GetSHA(),
		URL: pr.GetHTMLURL(), Body: pr.GetBody(), CreatedAt: pr.GetCreatedAt().Time,
		UpdatedAt: pr.GetUpdatedAt().Time, DefaultBranch: base.GetRepo().GetDefaultBranch(),
	}
	for _, l := range pr.Labels {
		out.Labels = append(out.Labels, webhook.Label{Name: l.GetName(), Color: l.GetColor()})
	}
	return out
}

// SetStatus implements forge.Client.
func (c *Client) SetStatus(ctx context.Context, owner, repo, sha string, state forge.StatusState, description string) error {
	status := gh.RepoStatus{
		State: new(string(state)), Context: new(forge.StatusContext), Description: new(truncate(description, forge.MaxStatusDescription)),
	}
	if _, _, err := c.api.Repositories.CreateStatus(ctx, owner, repo, sha, status); err != nil {
		return fmt.Errorf("github: status on %s: %w", sha, err)
	}
	return nil
}

// Issue implements forge.Client.
func (c *Client) Issue(ctx context.Context, owner, repo string, number int) (forge.Issue, error) {
	iss, _, err := c.api.Issues.Get(ctx, owner, repo, number)
	if err != nil {
		return forge.Issue{}, fmt.Errorf("github: issue %s/%s#%d: %w", owner, repo, number, err)
	}
	return issueFrom(iss), nil
}

// RepoLabels implements forge.Client.
func (c *Client) RepoLabels(ctx context.Context, owner, repo string) ([]string, error) {
	var out []string
	for l, err := range c.api.Issues.ListLabelsIter(ctx, owner, repo, &gh.ListOptions{PerPage: 100}) {
		if err != nil {
			return nil, fmt.Errorf("github: list labels of %s/%s: %w", owner, repo, err)
		}
		out = append(out, l.GetName())
	}
	return out, nil
}

// AddLabels implements forge.Client.
func (c *Client) AddLabels(ctx context.Context, owner, repo string, number int, labels []string) error {
	if len(labels) == 0 {
		return nil
	}
	if _, _, err := c.api.Issues.AddLabelsToIssue(ctx, owner, repo, number, labels); err != nil {
		return fmt.Errorf("github: add labels to %s/%s#%d: %w", owner, repo, number, err)
	}
	return nil
}

// RemoveLabel implements forge.Client. A label not currently applied is not
// an error.
func (c *Client) RemoveLabel(ctx context.Context, owner, repo string, number int, label string) error {
	resp, err := c.api.Issues.RemoveLabelForIssue(ctx, owner, repo, number, label)
	if resp != nil && resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if err != nil {
		return fmt.Errorf("github: remove label %q from %s/%s#%d: %w", label, owner, repo, number, err)
	}
	return nil
}

// SetState implements forge.Client.
func (c *Client) SetState(ctx context.Context, owner, repo string, number int, open bool) error {
	state := "closed"
	if open {
		state = "open"
	}
	if _, _, err := c.api.Issues.Update(ctx, owner, repo, number, gh.UpdateIssueRequest{State: new(state)}); err != nil {
		return fmt.Errorf("github: set state of %s/%s#%d: %w", owner, repo, number, err)
	}
	return nil
}

// AddAssignees implements forge.Client.
func (c *Client) AddAssignees(ctx context.Context, owner, repo string, number int, logins []string) error {
	if len(logins) == 0 {
		return nil
	}
	if _, _, err := c.api.Issues.AddAssignees(ctx, owner, repo, number, logins); err != nil {
		return fmt.Errorf("github: add assignees to %s/%s#%d: %w", owner, repo, number, err)
	}
	return nil
}

// RequestReviewers implements forge.Client.
func (c *Client) RequestReviewers(ctx context.Context, owner, repo string, number int, logins []string) error {
	if len(logins) == 0 {
		return nil
	}
	if _, _, err := c.api.PullRequests.RequestReviewers(ctx, owner, repo, number, gh.ReviewersRequest{Reviewers: logins}); err != nil {
		return fmt.Errorf("github: request reviewers on %s/%s#%d: %w", owner, repo, number, err)
	}
	return nil
}

// SearchIssues implements forge.Client. The query is free text, often
// rendered from an issue's title, so every qualifier-shaped term (one with
// a colon) is dropped before the search is scoped to the repository: a
// planted "repo:" would otherwise widen it, since GitHub ORs repo
// qualifiers. GitHub requires one of is:issue and is:pull-request, so
// issues and pull requests are searched apart and interleaved, and a
// result from another repository is dropped all the same.
func (c *Client) SearchIssues(ctx context.Context, owner, repo, query string, limit int) ([]forge.Issue, error) {
	var terms []string
	for t := range strings.FieldsSeq(query) {
		if !strings.Contains(t, ":") {
			terms = append(terms, t)
		}
	}
	if len(terms) == 0 || limit <= 0 {
		return nil, nil
	}
	var found [2][]forge.Issue
	for i, kind := range []string{"is:issue", "is:pull-request"} {
		q := fmt.Sprintf("repo:%s/%s %s %s", owner, repo, kind, strings.Join(terms, " "))
		//nolint:modernize // embedlit's elision is array/slice/map-only per the spec; it doesn't compile for a struct field
		opts := &gh.SearchOptions{ListOptions: gh.ListOptions{PerPage: limit}}
		result, _, err := c.api.Search.Issues(ctx, q, opts)
		if err != nil {
			return nil, fmt.Errorf("github: search %q in %s/%s: %w", query, owner, repo, err)
		}
		for _, iss := range result.Issues {
			if inRepository(iss, owner, repo) {
				found[i] = append(found[i], issueFrom(iss))
			}
		}
	}
	out := make([]forge.Issue, 0, limit)
	for i := 0; len(out) < limit && i < max(len(found[0]), len(found[1])); i++ {
		for _, f := range found {
			if i < len(f) && len(out) < limit {
				out = append(out, f[i])
			}
		}
	}
	return out, nil
}

// inRepository reports whether iss, a search result, is in owner/repo.
func inRepository(iss *gh.Issue, owner, repo string) bool {
	u := iss.GetRepositoryURL()
	return strings.HasSuffix(strings.ToLower(u), strings.ToLower("/repos/"+owner+"/"+repo))
}

func issueFrom(iss *gh.Issue) forge.Issue {
	out := forge.Issue{
		Number: iss.GetNumber(), Title: iss.GetTitle(), Body: iss.GetBody(),
		State: iss.GetState(), Author: iss.GetUser().GetLogin(),
		IsPull: iss.IsPullRequest(), Draft: iss.GetDraft(), URL: iss.GetHTMLURL(),
	}
	for _, l := range iss.Labels {
		out.Labels = append(out.Labels, l.GetName())
	}
	for _, a := range iss.Assignees {
		out.Assignees = append(out.Assignees, a.GetLogin())
	}
	return out
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
