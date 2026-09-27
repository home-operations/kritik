// Package gitlab is the forge client for GitLab, authenticated with a
// personal, group or project access token (ADR-0002).
package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/home-operations/kritik/internal/forge"
	"github.com/home-operations/kritik/internal/webhook"
)

// defaultHost is where an installation that names no host goes.
const defaultHost = "gitlab.com"

// maxErrorBody bounds how much of a non-2xx response body an apiError
// quotes.
const maxErrorBody = 512

// maxDiffBytes bounds a merge request diff read whole.
const maxDiffBytes = 8 << 20

// pageSize is the per_page every list asks for, GitLab's maximum.
const pageSize = 100

// ErrNotFound wraps any error produced by a 404 response.
var ErrNotFound = errors.New("gitlab: not found")

// apiError is returned for any non-2xx response.
type apiError struct {
	method     string
	path       string
	statusCode int
	body       string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("gitlab: %s %s: %d: %s", e.method, e.path, e.statusCode, e.body)
}

// Unwrap lets errors.Is(err, ErrNotFound) succeed for a 404 response.
func (e *apiError) Unwrap() error {
	if e.statusCode == http.StatusNotFound {
		return ErrNotFound
	}
	return nil
}

// Client is one GitLab instance's API, authenticated with one token.
type Client struct {
	httpClient *http.Client
	base       string // e.g. https://gitlab.com/api/v4
	webBase    string // e.g. https://gitlab.com
	token      string
	// FetchToken, when set, is what GitToken hands a runner instead of the
	// API token: a read-only token keeps the pod that reads untrusted
	// content from holding one that can write to the forge.
	FetchToken string

	mu    sync.Mutex
	login string // cached BotLogin result
}

// NewClient builds a Client against host's API: gitlab.com when host is
// empty, https unless host names a scheme (as tests do). A nil httpClient
// defaults to http.DefaultClient.
func NewClient(host, token string, httpClient *http.Client) (*Client, error) {
	if host == "" {
		host = defaultHost
	}
	webBase := host
	if !strings.Contains(webBase, "://") {
		webBase = "https://" + webBase
	}
	webBase = strings.TrimSuffix(webBase, "/")
	if u, err := url.Parse(webBase); err != nil || u.Host == "" {
		return nil, fmt.Errorf("gitlab: invalid host %q", host)
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{httpClient: httpClient, base: webBase + "/api/v4", webBase: webBase, token: token}, nil
}

// do issues an API request, JSON-encoding body and decoding the response
// into out when they are non-nil. It returns the next page a paginated
// response names, 0 on the last.
func (c *Client) do(ctx context.Context, method, path string, body, out any) (int, error) {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("gitlab: encode request body: %w", err)
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return 0, fmt.Errorf("gitlab: build request: %w", err)
	}
	req.Header.Set("PRIVATE-TOKEN", c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("gitlab: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return 0, &apiError{method: method, path: path, statusCode: resp.StatusCode, body: string(raw)}
	}
	next, _ := strconv.Atoi(resp.Header.Get("X-Next-Page"))
	if out == nil {
		return next, nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return 0, fmt.Errorf("gitlab: decode response for %s %s: %w", method, path, err)
	}
	return next, nil
}

// list reads every page of the list path names, which may carry a query.
func list[T any](ctx context.Context, c *Client, path string) ([]T, error) {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	var out []T
	for page := 1; page > 0; {
		var batch []T
		next, err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s%sper_page=%d&page=%d", path, sep, pageSize, page), nil, &batch)
		if err != nil {
			return nil, err
		}
		out = append(out, batch...)
		page = next
	}
	return out, nil
}

// projectPath addresses a project by its full path, owner being its top
// namespace and repo the rest, subgroups included.
func projectPath(owner, repo string) string {
	return "/projects/" + url.PathEscape(owner+"/"+repo)
}

func mrPath(owner, repo string, number int) string {
	return fmt.Sprintf("%s/merge_requests/%d", projectPath(owner, repo), number)
}

// LineRanges implements forge.Client: a diff note sits on one line here.
func (c *Client) LineRanges() bool { return false }

// FileURL implements forge.Client.
func (c *Client) FileURL(owner, repo, sha, path string, line, endLine int) string {
	u := fmt.Sprintf("%s/%s/%s/-/blob/%s/%s#L%d", c.webBase, owner, repo, sha, path, line)
	if endLine > line {
		u += fmt.Sprintf("-%d", endLine)
	}
	return u
}

// CloneURL implements forge.Client.
func (c *Client) CloneURL(owner, repo string) string {
	return fmt.Sprintf("%s/%s/%s.git", c.webBase, owner, repo)
}

// GitToken implements forge.Client: FetchToken when one is set, the API
// token otherwise.
func (c *Client) GitToken(context.Context) (string, error) {
	if c.FetchToken != "" {
		return c.FetchToken, nil
	}
	return c.token, nil
}

// MergeBase implements forge.Client: the merge base of the target branch
// and the head commit, as GitLab computes it.
func (c *Client) MergeBase(ctx context.Context, owner, repo string, number int, base, head string) (string, error) {
	var commit struct {
		ID string `json:"id"`
	}
	path := fmt.Sprintf("%s/repository/merge_base?refs[]=%s&refs[]=%s", projectPath(owner, repo), url.QueryEscape(base), url.QueryEscape(head))
	if _, err := c.do(ctx, http.MethodGet, path, nil, &commit); err != nil {
		return "", fmt.Errorf("gitlab: merge base for %s/%s!%d: %w", owner, repo, number, err)
	}
	if commit.ID == "" {
		return "", fmt.Errorf("gitlab: %s/%s!%d: no merge base", owner, repo, number)
	}
	return commit.ID, nil
}

// PullRequestDiff implements forge.Client: the diff of the merge request
// version whose head is head, from the merge base GitLab diffed that
// version from, so base goes unused. The compare API would do for two
// commits, but it collapses a file over GitLab's safe diff limits, a
// lockfile's easily; a version keeps its diff whole up to GitLab's hard
// limits, and one past them is an error.
func (c *Client) PullRequestDiff(ctx context.Context, owner, repo string, number int, _, head string) (string, error) {
	v, err := c.version(ctx, owner, repo, number, head)
	if err != nil {
		return "", err
	}
	if v.State == "overflow" {
		return "", fmt.Errorf("gitlab: diff of %s/%s!%d is over GitLab's diff limits", owner, repo, number)
	}
	var b strings.Builder
	for _, d := range v.Diffs {
		if d.TooLarge || d.Collapsed {
			return "", fmt.Errorf("gitlab: diff of %s/%s!%d: %s is too large to diff whole", owner, repo, number, d.NewPath)
		}
		writeFileDiff(&b, d)
		if b.Len() > maxDiffBytes {
			return "", fmt.Errorf("gitlab: diff of %s/%s!%d is over %d bytes", owner, repo, number, maxDiffBytes)
		}
	}
	return b.String(), nil
}

// writeFileDiff writes one file as git diff prints it, less the index
// line.
func writeFileDiff(b *strings.Builder, d fileDiff) {
	fmt.Fprintf(b, "diff --git a/%s b/%s\n", d.OldPath, d.NewPath)
	oldName, newName := "a/"+d.OldPath, "b/"+d.NewPath
	switch {
	case d.NewFile:
		fmt.Fprintf(b, "new file mode %s\n", d.BMode)
		oldName = "/dev/null"
	case d.DeletedFile:
		fmt.Fprintf(b, "deleted file mode %s\n", d.AMode)
		newName = "/dev/null"
	case d.RenamedFile:
		fmt.Fprintf(b, "rename from %s\nrename to %s\n", d.OldPath, d.NewPath)
	}
	if !strings.HasPrefix(d.Diff, "@@") {
		b.WriteString(d.Diff)
		return
	}
	fmt.Fprintf(b, "--- %s\n+++ %s\n%s", oldName, newName, d.Diff)
	if !strings.HasSuffix(d.Diff, "\n") {
		b.WriteByte('\n')
	}
}

// BranchTip implements forge.Client. An empty branch resolves the
// project's default branch first.
func (c *Client) BranchTip(ctx context.Context, owner, repo, ref string) (string, string, error) {
	resolved := ref
	if resolved == "" {
		var p project
		if _, err := c.do(ctx, http.MethodGet, projectPath(owner, repo), nil, &p); err != nil {
			return "", "", fmt.Errorf("gitlab: default branch for %s/%s: %w", owner, repo, err)
		}
		resolved = p.DefaultBranch
	}
	var b branch
	if _, err := c.do(ctx, http.MethodGet, projectPath(owner, repo)+"/repository/branches/"+url.PathEscape(resolved), nil, &b); err != nil {
		return "", "", fmt.Errorf("gitlab: tip of %s/%s@%s: %w", owner, repo, resolved, err)
	}
	return b.Commit.ID, resolved, nil
}

// FileAt implements forge.Client.
func (c *Client) FileAt(ctx context.Context, owner, repo, ref, path string) ([]byte, error) {
	u := fmt.Sprintf("%s%s/repository/files/%s/raw?ref=%s", c.base, projectPath(owner, repo), url.PathEscape(path), url.QueryEscape(ref))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("gitlab: build request: %w", err)
	}
	req.Header.Set("PRIVATE-TOKEN", c.token)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gitlab: read %s at %s: %w", path, ref, err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("gitlab: %s at %s: %w", path, ref, fs.ErrNotExist)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return nil, &apiError{method: http.MethodGet, path: path, statusCode: resp.StatusCode, body: string(raw)}
	}
	content, err := io.ReadAll(io.LimitReader(resp.Body, forge.MaxFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("gitlab: read %s at %s: %w", path, ref, err)
	}
	if len(content) > forge.MaxFileBytes {
		return nil, fmt.Errorf("gitlab: %s at %s: %w", path, ref, forge.ErrFileTooLarge)
	}
	return content, nil
}

// ListOpenPullRequests implements forge.Client: GitLab filters by update
// time itself, newest first.
func (c *Client) ListOpenPullRequests(ctx context.Context, owner, repo string, since time.Time) ([]forge.OpenPullRequest, error) {
	var p project
	if _, err := c.do(ctx, http.MethodGet, projectPath(owner, repo), nil, &p); err != nil {
		return nil, fmt.Errorf("gitlab: read %s/%s: %w", owner, repo, err)
	}
	path := fmt.Sprintf("%s/merge_requests?state=opened&order_by=updated_at&sort=desc&with_labels_details=true&updated_after=%s",
		projectPath(owner, repo), url.QueryEscape(since.UTC().Format(time.RFC3339)))
	mrs, err := list[mergeRequest](ctx, c, path)
	if err != nil {
		return nil, fmt.Errorf("gitlab: list open merge requests for %s/%s: %w", owner, repo, err)
	}
	out := make([]forge.OpenPullRequest, 0, len(mrs))
	for _, mr := range mrs {
		out = append(out, openPullRequest(mr, p.DefaultBranch))
	}
	return out, nil
}

// openPullRequest maps a merge request to the webhook parser's shape. One
// from another project is a fork's.
func openPullRequest(mr mergeRequest, defaultBranch string) forge.OpenPullRequest {
	pr := webhook.PullRequest{
		Number: mr.IID, Title: mr.Title, Author: mr.Author.Username, AuthorIsBot: mr.Author.Bot,
		State: "open", Draft: mr.Draft, Fork: mr.SourceProjectID != mr.TargetProjectID,
		HeadRef: mr.SourceBranch, HeadSHA: mr.SHA, BaseRef: mr.TargetBranch,
		URL: mr.WebURL, Body: mr.Description, CreatedAt: mr.CreatedAt,
	}
	for _, l := range mr.Labels {
		pr.Labels = append(pr.Labels, webhook.Label{Name: l.Name, Color: strings.TrimPrefix(l.Color, "#")})
	}
	return forge.OpenPullRequest{PullRequest: pr, UpdatedAt: mr.UpdatedAt, DefaultBranch: defaultBranch}
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
	if _, err := c.do(ctx, http.MethodGet, "/user", nil, &u); err != nil {
		return "", fmt.Errorf("gitlab: bot login: %w", err)
	}
	c.login = u.Username
	return c.login, nil
}

// FindComment implements forge.Client.
func (c *Client) FindComment(ctx context.Context, owner, repo string, number int, login, marker string) (int64, error) {
	comments, err := c.ListConversation(ctx, owner, repo, number)
	if err != nil {
		return 0, err
	}
	for _, cm := range comments {
		if cm.Author == login && strings.Contains(cm.Body, marker) {
			return cm.ID, nil
		}
	}
	return 0, nil
}

// ListConversation implements forge.Client: the merge request's notes that
// are neither system notes nor on the diff.
func (c *Client) ListConversation(ctx context.Context, owner, repo string, number int) ([]forge.Comment, error) {
	notes, err := list[note](ctx, c, mrPath(owner, repo, number)+"/notes?sort=asc&order_by=created_at")
	if err != nil {
		return nil, fmt.Errorf("gitlab: list notes of %s/%s!%d: %w", owner, repo, number, err)
	}
	var out []forge.Comment
	for _, n := range notes {
		if !n.System && n.Position == nil {
			out = append(out, comment(n))
		}
	}
	return out, nil
}

// CreateComment implements forge.Client.
func (c *Client) CreateComment(ctx context.Context, owner, repo string, number int, body string) (int64, error) {
	var n note
	if _, err := c.do(ctx, http.MethodPost, mrPath(owner, repo, number)+"/notes", noteBody{Body: noQuickActions(body)}, &n); err != nil {
		return 0, fmt.Errorf("gitlab: comment on %s/%s!%d: %w", owner, repo, number, err)
	}
	return n.ID, nil
}

// UpdateComment implements forge.Client.
func (c *Client) UpdateComment(ctx context.Context, owner, repo string, number int, id int64, body string) error {
	path := fmt.Sprintf("%s/notes/%d", mrPath(owner, repo, number), id)
	if _, err := c.do(ctx, http.MethodPut, path, noteBody{Body: noQuickActions(body)}, nil); err != nil {
		return fmt.Errorf("gitlab: update note %d on %s/%s!%d: %w", id, owner, repo, number, err)
	}
	return nil
}

// GetComment implements forge.Client. A note on the diff is found among
// the diff threads, which say which note it replies to.
func (c *Client) GetComment(ctx context.Context, owner, repo string, number int, id int64, inline bool) (forge.Comment, error) {
	if !inline {
		var n note
		path := fmt.Sprintf("%s/notes/%d", mrPath(owner, repo, number), id)
		if _, err := c.do(ctx, http.MethodGet, path, nil, &n); err != nil {
			return forge.Comment{}, fmt.Errorf("gitlab: get note %d on %s/%s!%d: %w", id, owner, repo, number, err)
		}
		return comment(n), nil
	}
	threads, err := c.discussions(ctx, owner, repo, number)
	if err != nil {
		return forge.Comment{}, err
	}
	for _, d := range threads {
		for _, cm := range diffComments(d) {
			if cm.ID == id {
				return cm, nil
			}
		}
	}
	return forge.Comment{}, fmt.Errorf("gitlab: diff note %d on %s/%s!%d: %w", id, owner, repo, number, ErrNotFound)
}

// ListInline implements forge.Client.
func (c *Client) ListInline(ctx context.Context, owner, repo string, number int) ([]forge.Comment, error) {
	threads, err := c.discussions(ctx, owner, repo, number)
	if err != nil {
		return nil, err
	}
	var out []forge.Comment
	for _, d := range threads {
		out = append(out, diffComments(d)...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// ReplyInline implements forge.Client, adding a note to the thread to is
// in.
func (c *Client) ReplyInline(ctx context.Context, owner, repo string, number int, to forge.Comment, body string) (int64, error) {
	threads, err := c.discussions(ctx, owner, repo, number)
	if err != nil {
		return 0, err
	}
	for _, d := range threads {
		if !slices.ContainsFunc(d.Notes, func(n note) bool { return n.ID == to.ID }) {
			continue
		}
		var n note
		path := fmt.Sprintf("%s/discussions/%s/notes", mrPath(owner, repo, number), url.PathEscape(d.ID))
		if _, err := c.do(ctx, http.MethodPost, path, noteBody{Body: noQuickActions(body)}, &n); err != nil {
			return 0, fmt.Errorf("gitlab: reply to note %d on %s/%s!%d: %w", to.ID, owner, repo, number, err)
		}
		return n.ID, nil
	}
	return 0, fmt.Errorf("gitlab: thread of note %d on %s/%s!%d: %w", to.ID, owner, repo, number, ErrNotFound)
}

func (c *Client) discussions(ctx context.Context, owner, repo string, number int) ([]discussion, error) {
	threads, err := list[discussion](ctx, c, mrPath(owner, repo, number)+"/discussions")
	if err != nil {
		return nil, fmt.Errorf("gitlab: list threads of %s/%s!%d: %w", owner, repo, number, err)
	}
	return threads, nil
}

// diffComments is a thread's notes on the diff, each after the first
// replying to it.
func diffComments(d discussion) []forge.Comment {
	var out []forge.Comment
	for i, n := range d.Notes {
		if n.System || n.Position == nil {
			continue
		}
		cm := comment(n)
		if i > 0 {
			cm.InReplyTo = d.Notes[0].ID
		}
		out = append(out, cm)
	}
	return out
}

func comment(n note) forge.Comment {
	cm := forge.Comment{ID: n.ID, Author: n.Author.Username, AuthorIsBot: n.Author.Bot, Body: n.Body, CreatedAt: n.CreatedAt}
	if p := n.Position; p != nil {
		cm.Inline, cm.Path, cm.Line, cm.CommitID = true, p.NewPath, p.NewLine, p.HeadSHA
	}
	return cm
}

// CreateReview implements forge.Client: a thread per comment, on the merge
// request version whose head is headSHA. GitLab has no review that posts
// several at once, so one refused comment does not stop the rest.
func (c *Client) CreateReview(ctx context.Context, owner, repo string, number int, headSHA string, comments []forge.InlineComment) error {
	if len(comments) == 0 {
		return nil
	}
	v, err := c.version(ctx, owner, repo, number, headSHA)
	if err != nil {
		return err
	}
	files := make(map[string]fileDiff, len(v.Diffs))
	for _, d := range v.Diffs {
		files[d.NewPath] = d
	}
	path := mrPath(owner, repo, number) + "/discussions"
	var errs []error
	for _, cm := range comments {
		f, ok := files[cm.Path]
		if !ok {
			f.OldPath = cm.Path
		}
		pos := position{
			PositionType: "text", BaseSHA: v.BaseCommitSHA, StartSHA: v.StartCommitSHA, HeadSHA: v.HeadCommitSHA,
			OldPath: f.OldPath, NewPath: cm.Path, OldLine: unchangedLine(f.Diff, cm.Line), NewLine: cm.Line,
		}
		if _, err := c.do(ctx, http.MethodPost, path, newThread{Body: noQuickActions(cm.Body), Position: pos}, nil); err != nil {
			errs = append(errs, fmt.Errorf("gitlab: comment on %s:%d of %s/%s!%d: %w", cm.Path, cm.Line, owner, repo, number, err))
		}
	}
	return errors.Join(errs...)
}

// version is the merge request version whose head is headSHA, with its
// diff. GitLab makes a version for each push, soon after it.
func (c *Client) version(ctx context.Context, owner, repo string, number int, headSHA string) (version, error) {
	versions, err := list[version](ctx, c, mrPath(owner, repo, number)+"/versions")
	if err != nil {
		return version{}, fmt.Errorf("gitlab: list versions of %s/%s!%d: %w", owner, repo, number, err)
	}
	i := slices.IndexFunc(versions, func(v version) bool { return v.HeadCommitSHA == headSHA })
	if i < 0 {
		return version{}, fmt.Errorf("gitlab: %s/%s!%d has no version at %s yet", owner, repo, number, headSHA)
	}
	var v version
	path := fmt.Sprintf("%s/versions/%d", mrPath(owner, repo, number), versions[i].ID)
	if _, err := c.do(ctx, http.MethodGet, path, nil, &v); err != nil {
		return version{}, fmt.Errorf("gitlab: read version %d of %s/%s!%d: %w", versions[i].ID, owner, repo, number, err)
	}
	return v, nil
}

// unchangedLine is the old line number of new line line when the file's
// hunks show it unchanged, and 0 when they add it or do not show it: GitLab
// places a comment on an unchanged line only by both numbers.
func unchangedLine(hunks string, line int) int {
	var oldLine, newLine int
	for l := range strings.SplitSeq(hunks, "\n") {
		switch {
		case strings.HasPrefix(l, "@@"):
			oldLine, newLine = hunkStarts(l)
		case oldLine == 0 && newLine == 0:
		case strings.HasPrefix(l, "+"):
			newLine++
		case strings.HasPrefix(l, "-"):
			oldLine++
		case strings.HasPrefix(l, " "):
			if newLine == line {
				return oldLine
			}
			oldLine++
			newLine++
		}
	}
	return 0
}

// hunkStarts reads "@@ -a,b +c,d @@" and returns a and c.
func hunkStarts(header string) (oldStart, newStart int) {
	fields := strings.Fields(header)
	if len(fields) < 3 {
		return 0, 0
	}
	return startOf(fields[1]), startOf(fields[2])
}

func startOf(field string) int {
	field, _, _ = strings.Cut(strings.TrimLeft(field, "-+"), ",")
	n, _ := strconv.Atoi(field)
	return n
}

// statusStates maps kritik's states onto GitLab's: a review that did not
// run to a verdict was canceled rather than failed.
var statusStates = map[forge.StatusState]string{
	forge.StatusPending: "pending",
	forge.StatusSuccess: "success",
	forge.StatusError:   "canceled",
}

// SetStatus implements forge.Client.
func (c *Client) SetStatus(ctx context.Context, owner, repo, sha string, state forge.StatusState, description string) error {
	opts := commitStatus{State: statusStates[state], Name: forge.StatusContext, Description: forge.StatusDescription(description)}
	path := fmt.Sprintf("%s/statuses/%s", projectPath(owner, repo), url.PathEscape(sha))
	if _, err := c.do(ctx, http.MethodPost, path, opts, nil); err != nil {
		return fmt.Errorf("gitlab: set status on %s/%s@%s: %w", owner, repo, sha, err)
	}
	return nil
}

// Permission implements forge.Client from the login's membership of the
// project, inherited ones included; a login that is no member has none.
func (c *Client) Permission(ctx context.Context, owner, repo, login string) (forge.Permission, error) {
	var users []user
	if _, err := c.do(ctx, http.MethodGet, "/users?username="+url.QueryEscape(login), nil, &users); err != nil {
		return "", fmt.Errorf("gitlab: look up %s: %w", login, err)
	}
	if len(users) == 0 {
		return forge.PermissionNone, nil
	}
	var m member
	_, err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s/members/all/%d", projectPath(owner, repo), users[0].ID), nil, &m)
	if errors.Is(err, ErrNotFound) {
		return forge.PermissionNone, nil
	}
	if err != nil {
		return "", fmt.Errorf("gitlab: permission for %s on %s/%s: %w", login, owner, repo, err)
	}
	return permission(m.AccessLevel), nil
}

// permission maps a member's access level onto forge.Permission's scale:
// Owner administers, Maintainer maintains, Developer pushes, Reporter
// triages, and Planner and Guest read.
func permission(accessLevel int) forge.Permission {
	switch {
	case accessLevel >= 50:
		return forge.PermissionAdmin
	case accessLevel >= 40:
		return forge.PermissionMaintain
	case accessLevel >= 30:
		return forge.PermissionWrite
	case accessLevel >= 20:
		return forge.PermissionTriage
	case accessLevel >= 10:
		return forge.PermissionRead
	}
	return forge.PermissionNone
}

// noQuickActions keeps GitLab from running a line of body as a quick
// action. GitLab runs a paragraph line that starts with a slash and a
// command's name, /merge or /approve among them, as whoever posted the
// note, and a body carries model output that a merge request's content
// can steer. A backslash before the slash renders the same. Fenced code is
// left as it is: GitLab runs nothing there, and a suggestion must stay
// exact.
func noQuickActions(body string) string {
	lines := strings.Split(body, "\n")
	fence := ""
	for i, l := range lines {
		trimmed := strings.TrimLeft(l, " ")
		if f := fenceOf(trimmed); f != "" && len(l)-len(trimmed) <= 3 {
			switch {
			case fence == "":
				fence = f
			case f[0] == fence[0] && len(f) >= len(fence) && strings.TrimSpace(trimmed[len(f):]) == "":
				fence = ""
			}
			continue
		}
		if fence == "" && strings.HasPrefix(l, "/") {
			lines[i] = `\` + l
		}
	}
	return strings.Join(lines, "\n")
}

// fenceOf is the run of three or more backticks or tildes a code fence
// line starts with, or "".
func fenceOf(line string) string {
	for _, c := range "`~" {
		if n := len(line) - len(strings.TrimLeft(line, string(c))); n >= 3 {
			return line[:n]
		}
	}
	return ""
}
