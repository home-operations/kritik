// Package forge is the worker's view of a forge: the few calls a review
// needs before and after the runner does its work. Each installation gets
// its own Client, authenticated as that installation.
package forge

import (
	"context"
	"time"

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
	// InReplyTo is the root inline comment this one replies to, 0 for a
	// root or a conversation comment.
	InReplyTo int64
}

// InlineComment is one finding attached to a line on the head side of the
// PR diff. The forge rejects lines the diff does not show, so callers anchor
// first.
type InlineComment struct {
	Path string
	Line int
	Body string
}

// StatusState is the outcome a commit status reports. kritik never reports
// failure: a review informs, it does not block.
type StatusState string

// States kritik reports.
const (
	StatusPending StatusState = "pending"
	StatusSuccess StatusState = "success"
)

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

// Client is one installation's access to its forge.
type Client interface {
	// MergeBase asks the forge for the merge-base of base (a branch) and
	// head (a commit) of pull request number, the same way the forge
	// computes the PR diff.
	MergeBase(ctx context.Context, owner, repo string, number int, base, head string) (string, error)
	// CloneURL is the HTTPS clone URL of a repository on this forge.
	CloneURL(owner, repo string) string
	// GitToken is the credential a runner fetches with: a short-lived
	// installation token on GitHub, and on Forgejo a static token, the
	// installation's gitToken when configured and its API token otherwise.
	// It reaches a pod that reads untrusted content.
	GitToken(ctx context.Context) (string, error)
	// BranchTip returns the commit a branch points at; an empty branch
	// means the repository's default branch, whose name is also returned.
	BranchTip(ctx context.Context, owner, repo, branch string) (sha, resolvedBranch string, err error)

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
	// ReplyInline posts a reply under a root inline comment.
	ReplyInline(ctx context.Context, owner, repo string, number int, rootID int64, body string) (int64, error)
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
