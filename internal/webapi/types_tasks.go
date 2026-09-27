package webapi

import (
	"encoding/json"
	"time"

	"github.com/home-operations/kritik/internal/store"
)

// TaskRun is one task run as lists show it. DurationMs is null until it
// finishes.
type TaskRun struct {
	ID            string              `json:"id"`
	Repository    string              `json:"repository"`
	Task          string              `json:"task"`
	SubjectKind   string              `json:"subjectKind"`
	SubjectNumber int                 `json:"subjectNumber"`
	Trigger       string              `json:"trigger"`
	Mode          string              `json:"mode"`
	Status        store.TaskRunStatus `json:"status"`
	Reason        string              `json:"reason"`
	Model         string              `json:"model"`
	ConfigSHA     string              `json:"configSha"`
	CommentID     *int64              `json:"commentId"`
	Error         string              `json:"error"`
	DroppedCount  int                 `json:"droppedCount"`
	CreatedAt     time.Time           `json:"createdAt"`
	StartedAt     *time.Time          `json:"startedAt"`
	FinishedAt    *time.Time          `json:"finishedAt"`
	DurationMs    *int64              `json:"durationMs"`
}

// TaskEvent is the delivery a task run ran on.
type TaskEvent struct {
	Forge      string    `json:"forge"`
	Event      string    `json:"event"`
	RawEvent   string    `json:"rawEvent"`
	Action     string    `json:"action"`
	Sender     string    `json:"sender"`
	Delivery   string    `json:"delivery"`
	ReceivedAt time.Time `json:"receivedAt"`
}

// TaskInline is an inline comment on a pull request's diff.
type TaskInline struct {
	Path    string `json:"path"`
	Line    int    `json:"line"`
	EndLine int    `json:"endLine"`
	Body    string `json:"body"`
}

// TaskAnswer is what the model proposed.
type TaskAnswer struct {
	Summary      string       `json:"summary"`
	Comment      string       `json:"comment"`
	AddLabels    []string     `json:"addLabels"`
	RemoveLabels []string     `json:"removeLabels"`
	State        string       `json:"state"`
	Assignees    []string     `json:"assignees"`
	Reviewers    []string     `json:"reviewers"`
	Inline       []TaskInline `json:"inline"`
}

// TaskApplied is what the forge accepted. Comment is the report comment's
// mode, "" when none was posted.
type TaskApplied struct {
	AddLabels    []string     `json:"addLabels"`
	RemoveLabels []string     `json:"removeLabels"`
	State        string       `json:"state"`
	Assignees    []string     `json:"assignees"`
	Reviewers    []string     `json:"reviewers"`
	Inline       []TaskInline `json:"inline"`
	Comment      string       `json:"comment"`
}

// TaskDrop is an action a run left out, and why.
type TaskDrop struct {
	Action string `json:"action"`
	Value  string `json:"value"`
	Reason string `json:"reason"`
}

// TaskRunDetail is one task run with its event and records. Event is null
// once retention swept it; Proposed and Applied are null until the model
// answered.
type TaskRunDetail struct {
	Run        TaskRun                    `json:"run"`
	Event      *TaskEvent                 `json:"event"`
	Fields     map[string]json.RawMessage `json:"fields"`
	Proposed   *TaskAnswer                `json:"proposed"`
	Applied    *TaskApplied               `json:"applied"`
	Dropped    []TaskDrop                 `json:"dropped"`
	ModelCalls int                        `json:"modelCalls"`
	CostUSD    float64                    `json:"costUsd"`
	Tokens     TokenCounts                `json:"tokens"`
}
