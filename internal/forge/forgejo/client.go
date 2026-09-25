// Package forgejo implements forge.Client against a Forgejo instance's REST
// API (https://<host>/api/v1), using only the standard library HTTP client:
// Forgejo has no first-party Go SDK comparable to go-github, so requests and
// responses are hand-rolled against the subset of the API kritik needs.
package forgejo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/home-operations/kritik/internal/forge"
	"github.com/home-operations/kritik/internal/webhook"
)

// statusContext is the commit status context kritik reports under.
const statusContext = "kritik/review"

// maxStatusDescription is the length Forgejo (matching GitHub) truncates a
// commit status description to.
const maxStatusDescription = 140

// maxErrorBody bounds how much of a non-2xx response body an apiError
// quotes, so a large HTML error page cannot blow up an error message.
const maxErrorBody = 512

// ErrNotFound wraps any error produced by a 404 response, so callers can
// branch on a missing resource with errors.Is(err, ErrNotFound).
var ErrNotFound = errors.New("forgejo: not found")

// ErrCommentUnknown is returned by GetComment for an inline comment id the
// cache has never seen. Forgejo has no endpoint to fetch a single inline
// review comment by id alone (unlike a conversation comment): the id is
// only ever returned nested under a review, listed per pull request. The
// cache is populated by ListInline, so a comment must be listed at least
// once before GetComment(inline=true) can resolve it.
var ErrCommentUnknown = errors.New("forgejo: inline comment unknown (not yet listed)")

// apiError is returned for any non-2xx response.
type apiError struct {
	method     string
	path       string
	statusCode int
	body       string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("forgejo: %s %s: %d: %s", e.method, e.path, e.statusCode, e.body)
}

// Unwrap lets errors.Is(err, ErrNotFound) succeed for a 404 response.
func (e *apiError) Unwrap() error {
	if e.statusCode == http.StatusNotFound {
		return ErrNotFound
	}
	return nil
}

// inlineLocation is where the cache last saw an inline comment: the pull
// request and review it belongs to, needed to answer GetComment(inline=true)
// without an id-only lookup endpoint.
type inlineLocation struct {
	number   int
	reviewID int64
}

// Client is one Forgejo instance's API, authenticated as a single account
// or app token.
type Client struct {
	httpClient *http.Client
	base       string // e.g. https://forge.example.com/api/v1
	webBase    string // e.g. https://forge.example.com
	token      string

	mu    sync.Mutex
	login string // cached BotLogin result

	inlineMu    sync.Mutex
	inlineCache map[int64]inlineLocation
}

// NewClient builds a Client against host's API. host may be a bare hostname
// (defaulting to https://) or include an explicit scheme, which tests use to
// point at an httptest server. A nil httpClient defaults to http.DefaultClient.
func NewClient(host, token string, httpClient *http.Client) (*Client, error) {
	if host == "" {
		return nil, errors.New("forgejo: host is required")
	}
	webBase := host
	if !strings.Contains(webBase, "://") {
		webBase = "https://" + webBase
	}
	webBase = strings.TrimSuffix(webBase, "/")
	if _, err := url.Parse(webBase); err != nil {
		return nil, fmt.Errorf("forgejo: invalid host %q: %w", host, err)
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{
		httpClient:  httpClient,
		base:        webBase + "/api/v1",
		webBase:     webBase,
		token:       token,
		inlineCache: make(map[int64]inlineLocation),
	}, nil
}

// do issues an API request and decodes a JSON response into out (if out is
// non-nil). body, if non-nil, is marshaled as the JSON request body.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("forgejo: encode request body: %w", err)
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return fmt.Errorf("forgejo: build request: %w", err)
	}
	req.Header.Set("Authorization", "token "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("forgejo: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return &apiError{method: method, path: path, statusCode: resp.StatusCode, body: string(raw)}
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("forgejo: decode response for %s %s: %w", method, path, err)
	}
	return nil
}

func repoPath(owner, repo string) string {
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)
}

// MergeBase implements forge.Client. base and head are accepted for
// interface parity with other forges but unused: Forgejo's pull request
// resource already reports the merge base it computed against its current
// base branch.
func (c *Client) MergeBase(ctx context.Context, owner, repo string, number int, base, head string) (string, error) {
	var pr pullRequest
	path := fmt.Sprintf("%s/pulls/%d", repoPath(owner, repo), number)
	if err := c.do(ctx, http.MethodGet, path, nil, &pr); err != nil {
		return "", fmt.Errorf("forgejo: merge base for %s/%s#%d: %w", owner, repo, number, err)
	}
	if pr.MergeBase == "" {
		return "", fmt.Errorf("forgejo: %s/%s#%d: merge_base is empty", owner, repo, number)
	}
	return pr.MergeBase, nil
}

// CloneURL implements forge.Client.
func (c *Client) CloneURL(owner, repo string) string {
	return fmt.Sprintf("%s/%s/%s.git", c.webBase, owner, repo)
}

// GitToken implements forge.Client: Forgejo authenticates git operations
// with the same static token used for the API.
func (c *Client) GitToken(_ context.Context) (string, error) {
	return c.token, nil
}

// BranchTip implements forge.Client. An empty branch resolves the
// repository's default branch first.
func (c *Client) BranchTip(ctx context.Context, owner, repo, ref string) (string, string, error) {
	resolved := ref
	if resolved == "" {
		var info repoInfo
		if err := c.do(ctx, http.MethodGet, repoPath(owner, repo), nil, &info); err != nil {
			return "", "", fmt.Errorf("forgejo: default branch for %s/%s: %w", owner, repo, err)
		}
		resolved = info.DefaultBranch
	}
	var b branch
	path := repoPath(owner, repo) + "/branches/" + url.PathEscape(resolved)
	if err := c.do(ctx, http.MethodGet, path, nil, &b); err != nil {
		return "", "", fmt.Errorf("forgejo: branch tip for %s/%s@%s: %w", owner, repo, resolved, err)
	}
	return b.Commit.ID, resolved, nil
}

// BotLogin implements forge.Client, caching the result: the account behind
// a token never changes mid-process.
func (c *Client) BotLogin(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.login != "" {
		return c.login, nil
	}
	var u user
	if err := c.do(ctx, http.MethodGet, "/user", nil, &u); err != nil {
		return "", fmt.Errorf("forgejo: bot login: %w", err)
	}
	c.login = u.Login
	return c.login, nil
}

// FindComment implements forge.Client, returning the id of the newest
// conversation comment by login whose body contains marker, or 0 if none
// matches.
func (c *Client) FindComment(ctx context.Context, owner, repo string, number int, login, marker string) (int64, error) {
	comments, err := c.ListConversation(ctx, owner, repo, number)
	if err != nil {
		return 0, err
	}
	var found int64
	for _, cm := range comments {
		if cm.Author == login && strings.Contains(cm.Body, marker) {
			found = cm.ID
		}
	}
	return found, nil
}

// ListConversation implements forge.Client.
func (c *Client) ListConversation(ctx context.Context, owner, repo string, number int) ([]forge.Comment, error) {
	var raw []comment
	path := fmt.Sprintf("%s/issues/%d/comments", repoPath(owner, repo), number)
	if err := c.do(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, fmt.Errorf("forgejo: list conversation for %s/%s#%d: %w", owner, repo, number, err)
	}
	out := make([]forge.Comment, 0, len(raw))
	for _, cm := range raw {
		out = append(out, conversationComment(cm))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func conversationComment(cm comment) forge.Comment {
	return forge.Comment{
		ID:          cm.ID,
		Author:      cm.User.Login,
		AuthorIsBot: isBot(cm.User.Login),
		Body:        cm.Body,
		CreatedAt:   cm.CreatedAt,
	}
}

// CreateComment implements forge.Client.
func (c *Client) CreateComment(ctx context.Context, owner, repo string, number int, body string) (int64, error) {
	var cm comment
	path := fmt.Sprintf("%s/issues/%d/comments", repoPath(owner, repo), number)
	if err := c.do(ctx, http.MethodPost, path, createCommentOption{Body: body}, &cm); err != nil {
		return 0, fmt.Errorf("forgejo: create comment on %s/%s#%d: %w", owner, repo, number, err)
	}
	return cm.ID, nil
}

// UpdateComment implements forge.Client.
func (c *Client) UpdateComment(ctx context.Context, owner, repo string, id int64, body string) error {
	path := fmt.Sprintf("%s/issues/comments/%d", repoPath(owner, repo), id)
	if err := c.do(ctx, http.MethodPatch, path, createCommentOption{Body: body}, nil); err != nil {
		return fmt.Errorf("forgejo: update comment %d on %s/%s: %w", id, owner, repo, err)
	}
	return nil
}

// GetComment implements forge.Client. inline=false uses the id-only
// conversation-comment endpoint. inline=true has no such endpoint on
// Forgejo, so it resolves via the cache ListInline populates, returning
// ErrCommentUnknown on a cold miss.
func (c *Client) GetComment(ctx context.Context, owner, repo string, id int64, inline bool) (forge.Comment, error) {
	if !inline {
		var cm comment
		path := fmt.Sprintf("%s/issues/comments/%d", repoPath(owner, repo), id)
		if err := c.do(ctx, http.MethodGet, path, nil, &cm); err != nil {
			return forge.Comment{}, fmt.Errorf("forgejo: get comment %d on %s/%s: %w", id, owner, repo, err)
		}
		return conversationComment(cm), nil
	}

	c.inlineMu.Lock()
	loc, ok := c.inlineCache[id]
	c.inlineMu.Unlock()
	if !ok {
		return forge.Comment{}, fmt.Errorf("forgejo: get inline comment %d on %s/%s: %w", id, owner, repo, ErrCommentUnknown)
	}
	comments, err := c.listReviewComments(ctx, owner, repo, loc.number, loc.reviewID)
	if err != nil {
		return forge.Comment{}, err
	}
	for _, cm := range comments {
		if cm.ID == id {
			return cm, nil
		}
	}
	return forge.Comment{}, fmt.Errorf("forgejo: get inline comment %d on %s/%s: %w", id, owner, repo, ErrCommentUnknown)
}

// CreateReview implements forge.Client. A no comments is a no-op: Forgejo
// rejects a review with no body and no comments as meaningless.
func (c *Client) CreateReview(ctx context.Context, owner, repo string, number int, headSHA string, comments []forge.InlineComment) error {
	if len(comments) == 0 {
		return nil
	}
	opts := createPullReviewOptions{
		CommitID: headSHA,
		Event:    "COMMENT",
		Comments: make([]createPullReviewComment, 0, len(comments)),
	}
	for _, cm := range comments {
		opts.Comments = append(opts.Comments, createPullReviewComment{
			Path:       cm.Path,
			Body:       cm.Body,
			NewLineNum: int64(cm.Line),
		})
	}
	path := fmt.Sprintf("%s/pulls/%d/reviews", repoPath(owner, repo), number)
	if err := c.do(ctx, http.MethodPost, path, opts, nil); err != nil {
		return fmt.Errorf("forgejo: create review on %s/%s#%d: %w", owner, repo, number, err)
	}
	return nil
}

// SetStatus implements forge.Client.
func (c *Client) SetStatus(ctx context.Context, owner, repo, sha string, state forge.StatusState, description string) error {
	opts := createStatusOption{
		State:       string(state),
		Context:     statusContext,
		Description: truncate(description, maxStatusDescription),
	}
	path := fmt.Sprintf("%s/statuses/%s", repoPath(owner, repo), sha)
	if err := c.do(ctx, http.MethodPost, path, opts, nil); err != nil {
		return fmt.Errorf("forgejo: set status on %s/%s@%s: %w", owner, repo, sha, err)
	}
	return nil
}

// Permission implements forge.Client, mapping Forgejo's collaborator
// permission (which reports "owner" for the repository owner, distinct
// from its own "admin" collaborator level) onto forge.Permission's
// ascending scale.
func (c *Client) Permission(ctx context.Context, owner, repo, login string) (forge.Permission, error) {
	var p collaboratorPermission
	path := fmt.Sprintf("%s/collaborators/%s/permission", repoPath(owner, repo), url.PathEscape(login))
	if err := c.do(ctx, http.MethodGet, path, nil, &p); err != nil {
		return "", fmt.Errorf("forgejo: permission for %s on %s/%s: %w", login, owner, repo, err)
	}
	level := forge.Permission(p.Permission)
	if p.Permission == "owner" {
		level = forge.PermissionAdmin
	}
	if !level.Valid() {
		return "", fmt.Errorf("forgejo: unrecognized permission %q for %s on %s/%s", p.Permission, login, owner, repo)
	}
	return level, nil
}

// ReplyInline implements forge.Client. Forgejo carries no reply-linkage
// field on an inline comment, so a "reply" is a new single-comment review
// on the same line, reusing the original comment's commit and path.
// Forgejo's review-creation response carries no per-comment id, so this
// always returns 0.
func (c *Client) ReplyInline(ctx context.Context, owner, repo string, number int, rootID int64, body string) (int64, error) {
	root, err := c.GetComment(ctx, owner, repo, rootID, true)
	if err != nil {
		return 0, fmt.Errorf("forgejo: reply to inline comment %d on %s/%s#%d: %w", rootID, owner, repo, number, err)
	}
	opts := createPullReviewOptions{
		Event: "COMMENT",
		Comments: []createPullReviewComment{
			{Path: root.Path, Body: body, NewLineNum: int64(root.Line)},
		},
	}
	// forge.Comment carries no commit id of its own, so it is looked up
	// separately from the cached review location.
	commitID, err := c.commentCommitID(ctx, owner, repo, rootID)
	if err != nil {
		return 0, err
	}
	opts.CommitID = commitID
	path := fmt.Sprintf("%s/pulls/%d/reviews", repoPath(owner, repo), number)
	if err := c.do(ctx, http.MethodPost, path, opts, nil); err != nil {
		return 0, fmt.Errorf("forgejo: reply to inline comment %d on %s/%s#%d: %w", rootID, owner, repo, number, err)
	}
	return 0, nil
}

// commentCommitID looks up the commit_id of a cached inline comment, since
// forge.Comment does not carry it but a reply review must reuse it.
func (c *Client) commentCommitID(ctx context.Context, owner, repo string, id int64) (string, error) {
	c.inlineMu.Lock()
	loc, ok := c.inlineCache[id]
	c.inlineMu.Unlock()
	if !ok {
		return "", fmt.Errorf("forgejo: reply to inline comment %d on %s/%s: %w", id, owner, repo, ErrCommentUnknown)
	}
	var raw []pullReviewComment
	path := fmt.Sprintf("%s/pulls/%d/reviews/%d/comments", repoPath(owner, repo), loc.number, loc.reviewID)
	if err := c.do(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return "", fmt.Errorf("forgejo: reply to inline comment %d on %s/%s: %w", id, owner, repo, err)
	}
	for _, cm := range raw {
		if cm.ID == id {
			return cm.CommitID, nil
		}
	}
	return "", fmt.Errorf("forgejo: reply to inline comment %d on %s/%s: %w", id, owner, repo, ErrCommentUnknown)
}

// ListInline implements forge.Client: it lists every review on the pull
// request, then every comment under each review, populating the inline
// cache as it goes. Every returned comment's InReplyTo is 0: Forgejo's
// model has no reply-linkage field, so a "thread" always degenerates to a
// flat list of root comments.
func (c *Client) ListInline(ctx context.Context, owner, repo string, number int) ([]forge.Comment, error) {
	reviews, err := c.listReviews(ctx, owner, repo, number)
	if err != nil {
		return nil, err
	}
	var out []forge.Comment
	cacheUpdates := make(map[int64]inlineLocation)
	for _, rv := range reviews {
		comments, err := c.listReviewComments(ctx, owner, repo, number, rv.ID)
		if err != nil {
			return nil, err
		}
		for _, cm := range comments {
			cacheUpdates[cm.ID] = inlineLocation{number: number, reviewID: rv.ID}
			out = append(out, cm)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })

	c.inlineMu.Lock()
	for id, loc := range cacheUpdates {
		c.inlineCache[id] = loc
	}
	c.inlineMu.Unlock()
	return out, nil
}

// reviewsPageSize bounds each GET /pulls/{n}/reviews page. listReviews
// passes it explicitly as the limit query param rather than relying on the
// server's default, so a short page (fewer results than requested) is an
// unambiguous, server-independent "no more pages" signal instead of
// requiring an exactly-empty page, which a resource with an exact multiple
// of the page size worth of reviews would never produce.
const reviewsPageSize = 50

// listReviews paginates GET /pulls/{n}/reviews.
func (c *Client) listReviews(ctx context.Context, owner, repo string, number int) ([]pullReview, error) {
	var out []pullReview
	page := 1
	for {
		var batch []pullReview
		path := fmt.Sprintf("%s/pulls/%d/reviews?page=%d&limit=%d", repoPath(owner, repo), number, page, reviewsPageSize)
		if err := c.do(ctx, http.MethodGet, path, nil, &batch); err != nil {
			return nil, fmt.Errorf("forgejo: list reviews for %s/%s#%d: %w", owner, repo, number, err)
		}
		out = append(out, batch...)
		if len(batch) < reviewsPageSize {
			return out, nil
		}
		page++
	}
}

// listReviewComments fetches GET /pulls/{n}/reviews/{id}/comments, which is
// not paginated, and maps the results to forge.Comment with InReplyTo
// always 0.
func (c *Client) listReviewComments(ctx context.Context, owner, repo string, number int, reviewID int64) ([]forge.Comment, error) {
	var raw []pullReviewComment
	path := fmt.Sprintf("%s/pulls/%d/reviews/%d/comments", repoPath(owner, repo), number, reviewID)
	if err := c.do(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, fmt.Errorf("forgejo: list review comments for %s/%s#%d review %d: %w", owner, repo, number, reviewID, err)
	}
	out := make([]forge.Comment, 0, len(raw))
	for _, cm := range raw {
		out = append(out, forge.Comment{
			ID:          cm.ID,
			Author:      cm.User.Login,
			AuthorIsBot: isBot(cm.User.Login),
			Body:        cm.Body,
			CreatedAt:   cm.CreatedAt,
			Inline:      true,
			Path:        cm.Path,
			Line:        int(cm.LineNum),
		})
	}
	return out, nil
}

// ListOpenPullRequests implements forge.Client. Forgejo sorts by
// recentupdate server-side, so pagination stops as soon as a page's pull
// requests are older than since.
func (c *Client) ListOpenPullRequests(ctx context.Context, owner, repo string, since time.Time) ([]forge.OpenPullRequest, error) {
	var out []forge.OpenPullRequest
	page := 1
	for {
		var batch []pullRequest
		path := fmt.Sprintf("%s/pulls?state=open&sort=recentupdate&page=%d", repoPath(owner, repo), page)
		if err := c.do(ctx, http.MethodGet, path, nil, &batch); err != nil {
			return nil, fmt.Errorf("forgejo: list open pull requests for %s/%s: %w", owner, repo, err)
		}
		if len(batch) == 0 {
			return out, nil
		}
		for _, pr := range batch {
			if pr.UpdatedAt.Before(since) {
				return out, nil
			}
			out = append(out, openPullRequest(pr))
		}
		page++
	}
}

func openPullRequest(pr pullRequest) forge.OpenPullRequest {
	out := forge.OpenPullRequest{
		UpdatedAt:     pr.UpdatedAt,
		DefaultBranch: pr.Base.Repo.DefaultBranch,
	}
	out.Number = pr.Number
	out.Title = pr.Title
	out.Author = pr.User.Login
	out.AuthorIsBot = isBot(pr.User.Login)
	out.State = pr.State
	out.Merged = pr.Merged
	out.Draft = pr.Draft
	out.Fork = pr.Head.Repo.Fork
	out.HeadRef = pr.Head.Ref
	out.HeadSHA = pr.Head.SHA
	out.BaseRef = pr.Base.Ref
	out.BaseSHA = pr.Base.SHA
	out.URL = pr.HTMLURL
	out.CreatedAt = pr.CreatedAt
	for _, l := range pr.Labels {
		out.Labels = append(out.Labels, webhook.Label{Name: l.Name, Color: l.Color})
	}
	return out
}

// isBot heuristically detects a bot account from its login, since Forgejo's
// user model has no bot/type flag (unlike GitHub's).
func isBot(login string) bool {
	return strings.HasSuffix(login, "[bot]") || strings.HasSuffix(login, "-bot")
}

// truncate shortens s to at most n bytes, matching the cap Forgejo (like
// GitHub) enforces on a commit status description.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
