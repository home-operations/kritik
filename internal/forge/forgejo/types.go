package forgejo

import "time"

// user is the REST API's user shape. Forgejo has no bot/type flag on this
// model (unlike GitHub's), so callers detect bots with a login-suffix
// heuristic only.
type user struct {
	Login string `json:"login"`
}

// label is a repository or PR label.
type label struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

// repository is the subset of Forgejo's Repository model kritik reads.
type repository struct {
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	Fork          bool   `json:"fork"`
}

// branchInfo is a pull request's head or base side.
type branchInfo struct {
	Ref  string     `json:"ref"`
	SHA  string     `json:"sha"`
	Repo repository `json:"repo"`
}

// pullRequest is the subset of Forgejo's PullRequest model kritik reads.
type pullRequest struct {
	Number    int        `json:"number"`
	Title     string     `json:"title"`
	User      user       `json:"user"`
	State     string     `json:"state"`
	Merged    bool       `json:"merged"`
	Draft     bool       `json:"draft"`
	HTMLURL   string     `json:"html_url"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	MergeBase string     `json:"merge_base"`
	Head      branchInfo `json:"head"`
	Base      branchInfo `json:"base"`
	Labels    []label    `json:"labels"`
}

// repoInfo is the subset of GET /repos/{owner}/{repo} kritik reads.
type repoInfo struct {
	DefaultBranch string `json:"default_branch"`
}

// commit is a branch's tip commit.
type commit struct {
	ID string `json:"id"`
}

// branch is the subset of GET /repos/{owner}/{repo}/branches/{branch}
// kritik reads.
type branch struct {
	Commit commit `json:"commit"`
}

// comment is a PR conversation comment (issues/{n}/comments,
// issues/comments/{id}).
type comment struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	User      user      `json:"user"`
	CreatedAt time.Time `json:"created_at"`
}

// createCommentOption is the request body for creating or editing a
// conversation comment.
type createCommentOption struct {
	Body string `json:"body"`
}

// pullReview is one review, as listed under a pull request.
type pullReview struct {
	ID        int64     `json:"id"`
	CommitID  string    `json:"commit_id"`
	CreatedAt time.Time `json:"submitted_at"`
}

// pullReviewComment is one inline comment attached to a review. Forgejo's
// model carries no reply-linkage field: there is no in_reply_to_id or
// equivalent, so every comment mapped from this type is necessarily a root.
type pullReviewComment struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	Path      string    `json:"path"`
	LineNum   uint64    `json:"position"`
	CommitID  string    `json:"commit_id"`
	User      user      `json:"user"`
	CreatedAt time.Time `json:"created_at"`
}

// createPullReviewComment is one inline comment in a review creation
// request.
type createPullReviewComment struct {
	Path       string `json:"path"`
	Body       string `json:"body"`
	NewLineNum int64  `json:"new_position"`
}

// createPullReviewOptions is the request body for POST
// /pulls/{index}/reviews.
type createPullReviewOptions struct {
	CommitID string                    `json:"commit_id"`
	Event    string                    `json:"event"`
	Body     string                    `json:"body"`
	Comments []createPullReviewComment `json:"comments"`
}

// collaboratorPermission is the response of
// GET /repos/{owner}/{repo}/collaborators/{collaborator}/permission.
type collaboratorPermission struct {
	Permission string `json:"permission"`
	RoleName   string `json:"role_name"`
}

// createStatusOption is the request body for POST
// /repos/{owner}/{repo}/statuses/{sha}.
type createStatusOption struct {
	State       string `json:"state"`
	Context     string `json:"context"`
	Description string `json:"description"`
}
