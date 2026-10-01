// Package forge is the worker's view of a forge: the few calls a review
// needs before and after the runner does its work. Each connection gets
// its own Client, authenticated as that connection.
package forge

import (
	"context"
	"errors"
	"time"
	"unicode/utf8"

	"github.com/home-operations/kritika/internal/configfile"
	"github.com/home-operations/kritika/internal/webhook"
)

// Clients builds a forge client per App and repository owner: a GitHub
// App's installation, and with it the token, is its owner's.
type Clients interface {
	For(ctx context.Context, in *configfile.Connection, repo string) (Client, error)
}

// OpenPullRequest is a pull request as the forge lists it, in the same
// shape the webhook parser produces so the poller can dispatch it as an
// event.
type OpenPullRequest struct {
	webhook.PullRequest
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
	// InReplyTo is the root inline comment this one replies to, 0 for a
	// root or a conversation comment.
	InReplyTo int64
	// ReactionsUp and ReactionsDown count the 👍 and 👎 on the comment.
	ReactionsUp, ReactionsDown int
}

// Issue is an issue of a repository, as a pull request's description links
// one it closes.
type Issue struct {
	Number int
	Title  string
	Body   string
	// URL is the issue's page in the forge's web UI.
	URL string
	// PullRequest is set when the number names a pull request, which the
	// forge files among its issues.
	PullRequest bool
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

// StatusState is the outcome a commit status reports. kritika never reports
// failure for a review that ran: a review informs, it does not block.
// StatusError is the one exception, for a review that did not run to a
// verdict at all (canceled), which is not a finding to weigh.
type StatusState string

// States kritika reports.
const (
	StatusSuccess StatusState = "success"
	StatusError   StatusState = "error"
)

// StatusContext is the commit status context kritika reports under.
const StatusContext = "kritika/review"

// MaxStatusDescription is the length, in characters, GitHub truncates a
// commit status description to.
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

// Client is one connection's access to its forge.
type Client interface {
	// MergeBase asks the forge for the merge-base of base (a branch) and
	// head (a commit), the same way the forge computes a pull request's
	// diff.
	MergeBase(ctx context.Context, owner, repo, base, head string) (string, error)
	// PullRequestDiff is a pull request's unified diff, from base, its
	// merge-base, to head. A diff too large to read whole is an error,
	// never a truncated diff.
	PullRequestDiff(ctx context.Context, owner, repo, base, head string) (string, error)
	// CloneURL is the HTTPS clone URL of a repository on this forge.
	CloneURL(owner, repo string) string
	// GitToken is the credential a runner fetches repo, a name under the
	// client's account, with: a short-lived installation token on GitHub
	// that can only read repo. It reaches a pod that reads untrusted
	// content and runs commands an agent chose.
	GitToken(ctx context.Context, repo string) (string, error)
	// BranchTip returns the commit a branch points at; an empty branch
	// means the repository's default branch, whose name is also returned.
	BranchTip(ctx context.Context, owner, repo, branch string) (sha, resolvedBranch string, err error)
	// FileAt returns the content of the file at path in commit ref. A path
	// that is not a file there is an error wrapping fs.ErrNotExist, and a
	// file over MaxFileBytes one wrapping ErrFileTooLarge.
	FileAt(ctx context.Context, owner, repo, ref, path string) ([]byte, error)
	// Issue returns issue number of the repository. A number that names
	// none is an error wrapping fs.ErrNotExist.
	Issue(ctx context.Context, owner, repo string, number int) (Issue, error)

	// BotLogin is the login comments posted through this client carry, so
	// the sticky comment can be matched by author and marker together.
	BotLogin(ctx context.Context) (string, error)
	// FindComment returns the id of the first PR conversation comment by
	// login whose body contains marker, or 0 when there is none.
	FindComment(ctx context.Context, owner, repo string, number int, login, marker string) (int64, error)
	// CreateComment posts a PR conversation comment and returns its id.
	CreateComment(ctx context.Context, owner, repo string, number int, body string) (int64, error)
	// UpdateComment replaces the body of conversation comment id.
	UpdateComment(ctx context.Context, owner, repo string, id int64, body string) error
	// CreateReview posts a non-blocking review with inline comments pinned
	// to headSHA and returns each comment's id, in order, 0 where the forge
	// did not say. ids is nil when the review was not posted; a review
	// posted whose comments could not be read back returns ids, all 0, and
	// the error.
	CreateReview(ctx context.Context, owner, repo string, number int, headSHA string, comments []InlineComment) (ids []int64, err error)
	// Approve posts an approving review of headSHA with body, unless the
	// bot's approval of that head already stands, and reports whether one
	// was posted.
	Approve(ctx context.Context, owner, repo string, number int, headSHA, body string) (bool, error)
	// DismissApprovals dismisses each of the bot's standing approvals of
	// the pull request with message, and returns how many it dismissed.
	DismissApprovals(ctx context.Context, owner, repo string, number int, message string) (int, error)
	// SetStatus sets the kritika commit status on sha.
	SetStatus(ctx context.Context, owner, repo, sha string, state StatusState, description string) error
	// FileURL links lines line through endLine (0 for line alone) of path
	// at sha in the forge's web UI.
	FileURL(owner, repo, sha, path string, line, endLine int) string
	// CommitURL links commit sha in the forge's web UI.
	CommitURL(owner, repo, sha string) string
	// ThreadURL links the thread of inline comment id on pull request
	// number in the forge's web UI.
	ThreadURL(owner, repo string, number int, id int64) string

	// GetComment fetches one comment; inline selects the review-comment
	// namespace, which the forge keeps apart from conversation comments.
	GetComment(ctx context.Context, owner, repo string, id int64, inline bool) (Comment, error)
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
	// ResolveThread resolves the review thread inline comment id opened,
	// when it is still open, and reports whether it did. With onlyOwn, a
	// thread someone else has written in is left open: it is a
	// conversation, left to its people unless one of them asked.
	ResolveThread(ctx context.Context, owner, repo string, number int, id int64, onlyOwn bool) (bool, error)
	// ListOpenPullRequests returns the open pull requests updated since a
	// time, most recently updated first.
	ListOpenPullRequests(ctx context.Context, owner, repo string, since time.Time) ([]OpenPullRequest, error)
}

// CanWrite reports whether a permission level allows pushing.
func CanWrite(permission Permission) bool {
	switch permission {
	case PermissionAdmin, PermissionMaintain, PermissionWrite:
		return true
	}
	return false
}
