package gitlab

import "time"

// user is a GitLab user as the API embeds it.
type user struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Bot      bool   `json:"bot"`
}

// project is the part of a project the client reads.
type project struct {
	DefaultBranch string `json:"default_branch"`
}

// label is a merge request label, as with_labels_details returns it.
type label struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

// mergeRequest is the part of a merge request the client reads.
type mergeRequest struct {
	IID             int       `json:"iid"`
	Title           string    `json:"title"`
	Description     string    `json:"description"`
	State           string    `json:"state"`
	Draft           bool      `json:"draft"`
	SHA             string    `json:"sha"`
	SourceBranch    string    `json:"source_branch"`
	TargetBranch    string    `json:"target_branch"`
	SourceProjectID int64     `json:"source_project_id"`
	TargetProjectID int64     `json:"target_project_id"`
	Author          user      `json:"author"`
	WebURL          string    `json:"web_url"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	Labels          []label   `json:"labels"`
}

// version is one diff version of a merge request: a push makes a new one.
// Only a single version's own read carries its diffs. State is overflow
// when GitLab stored the diff cut short at its limits.
type version struct {
	ID             int64      `json:"id"`
	HeadCommitSHA  string     `json:"head_commit_sha"`
	BaseCommitSHA  string     `json:"base_commit_sha"`
	StartCommitSHA string     `json:"start_commit_sha"`
	State          string     `json:"state"`
	Diffs          []fileDiff `json:"diffs"`
}

// position is where a diff note sits: lines of one version's diff. A line
// the change added has no old line, and one it removed no new line.
type position struct {
	PositionType string `json:"position_type"`
	BaseSHA      string `json:"base_sha"`
	StartSHA     string `json:"start_sha"`
	HeadSHA      string `json:"head_sha"`
	OldPath      string `json:"old_path"`
	NewPath      string `json:"new_path"`
	OldLine      int    `json:"old_line,omitempty"`
	NewLine      int    `json:"new_line,omitempty"`
}

// note is a merge request comment: a conversation note, or a diff note
// with a position.
type note struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	Author    user      `json:"author"`
	System    bool      `json:"system"`
	CreatedAt time.Time `json:"created_at"`
	Position  *position `json:"position"`
}

// discussion is a thread of notes, the first of them its root.
type discussion struct {
	ID    string `json:"id"`
	Notes []note `json:"notes"`
}

// noteBody is a note to create or the text to replace one's with.
type noteBody struct {
	Body string `json:"body"`
}

// newThread starts a thread on a line of the diff.
type newThread struct {
	Body     string   `json:"body"`
	Position position `json:"position"`
}

// commitStatus is a commit status to set.
type commitStatus struct {
	State       string `json:"state"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// fileDiff is one file of a version's diff: diff holds its hunks without
// the file headers.
type fileDiff struct {
	OldPath     string `json:"old_path"`
	NewPath     string `json:"new_path"`
	AMode       string `json:"a_mode"`
	BMode       string `json:"b_mode"`
	Diff        string `json:"diff"`
	NewFile     bool   `json:"new_file"`
	RenamedFile bool   `json:"renamed_file"`
	DeletedFile bool   `json:"deleted_file"`
	TooLarge    bool   `json:"too_large"`
	Collapsed   bool   `json:"collapsed"`
}

// branch is the part of a branch the client reads.
type branch struct {
	Commit struct {
		ID string `json:"id"`
	} `json:"commit"`
}

// member is a user's membership of a project, inherited ones included.
type member struct {
	AccessLevel int `json:"access_level"`
}
