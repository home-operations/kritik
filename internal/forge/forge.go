// Package forge is the worker's view of a forge: the few calls a review
// needs before and after the runner does its work. Each installation gets
// its own Client, authenticated as that installation.
package forge

import (
	"context"
	"errors"
	"time"
	"unicode/utf8"

	"github.com/home-operations/kritik/internal/webhook"
)

// OpenPullRequest is a pull request as the forge lists it, in the same
// shape the webhook parser produces so the poller can dispatch it as an
// event.
type OpenPullRequest struct {
	webhook.PullRequest
	UpdatedAt     time.Time
	DefaultBranch string
}

// Comment is a pull request comment as the forge holds it: a conversation
// comment, or an inline one on a diff line that may reply to another.
type Comment struct {
	ID          int64
	Author      string
	AuthorIsBot bool
	Body        string
	CreatedAt   time.Time
	Inline      bool
	Path        string
	Line        int
	// CommitID is the commit an inline comment is on.
	CommitID string
	// InReplyTo is the root inline comment this one replies to, 0 for a
	// root or a conversation comment.
	InReplyTo int64
}

// InlineComment is one finding attached to a line on the head side of the
// PR diff. The forge rejects lines the diff does not show, so callers anchor
// first.
type InlineComment struct {
	Path string
	// Line is the line the comment is on; with StartLine set, the last
	// line of a range that starts there.
	Line      int
	StartLine int
	Body      string
}

// Issue is an issue or pull request as the forge holds it, in the shape a
// task needs to read and act on one: it does not carry everything a pull
// request review does (no diff, no branches), only what triage-style tasks
// use.
type Issue struct {
	Number    int
	Title     string
	Body      string
	State     string // "open" or "closed"
	Author    string
	Labels    []string
	Assignees []string
	IsPull    bool
	// Draft is whether a pull request is a draft, false for an issue and
	// on a forge too old to report it.
	Draft bool
	URL   string
}

// StatusState is the outcome a commit status reports. kritik never reports
// failure for a review that ran: a review informs, it does not block.
// StatusError is the one exception, for a review that did not run to a
// verdict at all (canceled), which is not a finding to weigh.
type StatusState string

// States kritik reports.
const (
	StatusPending StatusState = "pending"
	StatusSuccess StatusState = "success"
	StatusError   StatusState = "error"
)

// StatusContext is the commit status context kritik reports under.
const StatusContext = "kritik/review"

// MaxStatusDescription is the length, in characters, GitHub, and Forgejo
// matching it, truncates a commit status description to.
const MaxStatusDescription = 140

// StatusDescription is s cut to MaxStatusDescription characters, the last
// an ellipsis when it had to be cut. It counts characters, not bytes, so it
// never splits one.
func StatusDescription(s string) string {
	if utf8.RuneCountInString(s) <= MaxStatusDescription {
		return s
	}
	return string([]rune(s)[:MaxStatusDescription-1]) + "…"
}

// Permission is a login's access level to a repository, in ascending order.
type Permission string

// Levels a forge grants a collaborator.
const (
	PermissionNone     Permission = "none"
	PermissionRead     Permission = "read"
	PermissionTriage   Permission = "triage"
	PermissionWrite    Permission = "write"
	PermissionMaintain Permission = "maintain"
	PermissionAdmin    Permission = "admin"
)

// Valid reports whether p is one of the known permission levels.
func (p Permission) Valid() bool {
	switch p {
	case PermissionNone, PermissionRead, PermissionTriage, PermissionWrite, PermissionMaintain, PermissionAdmin:
		return true
	}
	return false
}

// MaxFileBytes bounds what FileAt reads of one file: GitHub's contents API
// inlines a file only up to this size.
const MaxFileBytes = 1 << 20

// ErrFileTooLarge is FileAt refusing a file over MaxFileBytes.
var ErrFileTooLarge = errors.New("forge: file is over the size limit")

// ErrNoReadToken means the forge has no read-only credential to hand a
// runner.
var ErrNoReadToken = errors.New("forge: no read-only git token")

// Client is one installation's access to its forge.
type Client interface {
	// MergeBase asks the forge for the merge-base of base (a branch) and
	// head (a commit) of pull request number, the same way the forge
	// computes the PR diff.
	MergeBase(ctx context.Context, owner, repo string, number int, base, head string) (string, error)
	// PullRequestDiff is the unified diff of pull request number: from
	// base, its merge-base, to head on a forge that diffs two commits, and
	// from the pull request's own merge base to its current head on one
	// whose API diffs only pull requests. A diff too large to read whole is
	// an error, never a truncated diff.
	PullRequestDiff(ctx context.Context, owner, repo string, number int, base, head string) (string, error)
	// CloneURL is the HTTPS clone URL of a repository on this forge.
	CloneURL(owner, repo string) string
	// GitToken is the credential a runner fetches with: a short-lived
	// installation token on GitHub, and on Forgejo a static token, the
	// installation's gitToken when configured and its API token otherwise.
	// It reaches a pod that reads untrusted content.
	GitToken(ctx context.Context) (string, error)
	// ReadGitToken is a credential that can only read owner/repo, for a
	// runner whose agent a task drives over untrusted input: on GitHub an
	// installation token minted for that repository alone with contents
	// read, and on Forgejo the installation's gitToken, which the operator
	// vouches is read-only. ErrNoReadToken when the forge has none.
	ReadGitToken(ctx context.Context, owner, repo string) (string, error)
	// BranchTip returns the commit a branch points at; an empty branch
	// means the repository's default branch, whose name is also returned.
	BranchTip(ctx context.Context, owner, repo, branch string) (sha, resolvedBranch string, err error)
	// FileAt returns the content of the file at path in commit ref. A path
	// that is not a file there is an error wrapping fs.ErrNotExist, and a
	// file over MaxFileBytes one wrapping ErrFileTooLarge.
	FileAt(ctx context.Context, owner, repo, ref, path string) ([]byte, error)

	// BotLogin is the login comments posted through this client carry, so
	// the sticky comment can be matched by author and marker together.
	BotLogin(ctx context.Context) (string, error)
	// FindComment returns the id of the first PR conversation comment by
	// login whose body contains marker, or 0 when there is none.
	FindComment(ctx context.Context, owner, repo string, number int, login, marker string) (int64, error)
	// CreateComment posts a PR conversation comment and returns its id.
	CreateComment(ctx context.Context, owner, repo string, number int, body string) (int64, error)
	// UpdateComment replaces the body of a PR conversation comment.
	UpdateComment(ctx context.Context, owner, repo string, id int64, body string) error
	// CreateReview posts a non-blocking review with inline comments pinned
	// to headSHA.
	CreateReview(ctx context.Context, owner, repo string, number int, headSHA string, comments []InlineComment) error
	// SetStatus sets the kritik commit status on sha.
	SetStatus(ctx context.Context, owner, repo, sha string, state StatusState, description string) error
	// LineRanges reports whether inline comments may span a range of
	// lines, so a suggestion can replace more than one.
	LineRanges() bool
	// FileURL links lines line through endLine (0 for line alone) of path
	// at sha in the forge's web UI.
	FileURL(owner, repo, sha, path string, line, endLine int) string

	// GetComment fetches one comment; inline selects the review-comment
	// namespace, which the forge keeps apart from conversation comments.
	// number is the pull request the comment belongs to; forges that can
	// resolve a comment by id alone (GitHub) ignore it.
	GetComment(ctx context.Context, owner, repo string, number int, id int64, inline bool) (Comment, error)
	// ListConversation returns the PR's conversation comments, oldest first.
	ListConversation(ctx context.Context, owner, repo string, number int) ([]Comment, error)
	// ListInline returns the PR's inline review comments, oldest first.
	ListInline(ctx context.Context, owner, repo string, number int) ([]Comment, error)
	// Permission is the login's access to the repository: admin, maintain,
	// write, triage, read or none.
	Permission(ctx context.Context, owner, repo, login string) (Permission, error)
	// ReplyInline posts a reply in inline comment to's thread and returns
	// its id, 0 when the forge does not say.
	ReplyInline(ctx context.Context, owner, repo string, number int, to Comment, body string) (int64, error)
	// ListOpenPullRequests returns the open pull requests updated since a
	// time, most recently updated first.
	ListOpenPullRequests(ctx context.Context, owner, repo string, since time.Time) ([]OpenPullRequest, error)

	// Issue fetches one issue or pull request by number.
	Issue(ctx context.Context, owner, repo string, number int) (Issue, error)
	// RepoLabels lists the names of every label defined on the repository.
	RepoLabels(ctx context.Context, owner, repo string) ([]string, error)
	// AddLabels adds labels, by name, to an issue or pull request. A label
	// already applied is not an error.
	AddLabels(ctx context.Context, owner, repo string, number int, labels []string) error
	// RemoveLabel removes one label, by name, from an issue or pull
	// request. A label not currently applied is not an error.
	RemoveLabel(ctx context.Context, owner, repo string, number int, label string) error
	// SetState opens or closes an issue or pull request.
	SetState(ctx context.Context, owner, repo string, number int, open bool) error
	// AddAssignees adds logins as assignees of an issue or pull request,
	// alongside any already assigned.
	AddAssignees(ctx context.Context, owner, repo string, number int, logins []string) error
	// RequestReviewers requests review from logins on a pull request.
	RequestReviewers(ctx context.Context, owner, repo string, number int, logins []string) error
	// SearchIssues searches issues and pull requests in one repository,
	// returning at most limit results.
	SearchIssues(ctx context.Context, owner, repo, query string, limit int) ([]Issue, error)
}

// CanWrite reports whether a permission level allows pushing.
func CanWrite(permission Permission) bool {
	switch permission {
	case PermissionAdmin, PermissionMaintain, PermissionWrite:
		return true
	}
	return false
}
