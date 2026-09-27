package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/transcript"
)

// Valid reports whether s is one of the task run statuses.
func (s TaskRunStatus) Valid() bool {
	switch s {
	case TaskQueued, TaskRunning, TaskSucceeded, TaskFailed, TaskSkipped:
		return true
	}
	return false
}

// TaskRunRow is one task_runs row as the dashboard lists it.
type TaskRunRow struct {
	ID            string
	RepositoryID  string
	Repository    string
	Task          string
	SubjectKind   string
	SubjectNumber int
	Trigger       string
	Mode          string
	Status        TaskRunStatus
	Reason        string
	ConfigSHA     string
	Model         string
	Error         string
	CommentID     *int64
	CreatedAt     time.Time
	StartedAt     *time.Time
	FinishedAt    *time.Time
	// DroppedCount counts the actions the run left out.
	DroppedCount int
}

// TaskRunFilter narrows ListTaskRuns; zero fields match everything.
type TaskRunFilter struct {
	RepositoryID string
	Task         string
	Status       TaskRunStatus
}

const taskRunColumns = `t.id, t.repository_id, r.name, t.task, t.subject_kind, t.subject_number, t.trigger, t.mode, t.status,
	t.reason, t.config_sha, t.model, t.error, t.comment_id, t.created_at, t.started_at, t.finished_at,
	CASE WHEN jsonb_typeof(t.dropped) = 'array' THEN jsonb_array_length(t.dropped) ELSE 0 END`

func scanTaskRun(row pgx.CollectableRow) (TaskRunRow, error) {
	var x TaskRunRow
	var status string
	err := row.Scan(&x.ID, &x.RepositoryID, &x.Repository, &x.Task, &x.SubjectKind, &x.SubjectNumber, &x.Trigger, &x.Mode, &status,
		&x.Reason, &x.ConfigSHA, &x.Model, &x.Error, &x.CommentID, &x.CreatedAt, &x.StartedAt, &x.FinishedAt, &x.DroppedCount)
	x.Status = TaskRunStatus(status)
	return x, err
}

// ListTaskRuns returns a page of task runs, newest first.
func ListTaskRuns(ctx context.Context, tx pgx.Tx, f TaskRunFilter, p Page) ([]TaskRunRow, *Cursor, error) {
	if err := p.check(); err != nil {
		return nil, nil, err
	}
	if f.Status != "" && !f.Status.Valid() {
		return nil, nil, ErrFilter
	}
	rows, err := tx.Query(ctx, `SELECT `+taskRunColumns+`
		FROM task_runs t JOIN repositories r ON r.id = t.repository_id
		WHERE ($1::uuid IS NULL OR t.repository_id = $1) AND ($2 = '' OR t.task = $2) AND ($3 = '' OR t.status = $3)
			AND ($4 OR (t.created_at, t.id) < ($5, $6::uuid))
		ORDER BY t.created_at DESC, t.id DESC LIMIT $7`,
		uuidParam(f.RepositoryID), f.Task, string(f.Status), p.After.First(), p.After.T, p.afterID(), p.Limit+1)
	if err != nil {
		return nil, nil, fmt.Errorf("store: list task runs: %w", err)
	}
	out, err := pgx.CollectRows(rows, scanTaskRun)
	if err != nil {
		return nil, nil, fmt.Errorf("store: list task runs: %w", err)
	}
	items, next := paged(out, p.Limit, func(x TaskRunRow) Cursor { return Cursor{T: x.CreatedAt, ID: x.ID} })
	return items, next, nil
}

// TaskEventRow is the delivery a task run ran on, without its payload.
type TaskEventRow struct {
	ID         string
	Forge      string
	Event      string
	RawEvent   string
	Action     string
	Sender     string
	Delivery   string
	ReceivedAt time.Time
}

// TaskRunDetail is one task run with what it recorded and what its model
// calls cost.
type TaskRunDetail struct {
	TaskRunRow
	// Event is nil once retention swept the run's event.
	Event *TaskEventRow
	// Fields, Proposed, Applied and Dropped are the run's JSON records as
	// stored, nil for NULL.
	Fields, Proposed, Applied, Dropped json.RawMessage
	ModelCalls                         int
	CostUSD                            float64
	InputTokens                        int64
	OutputTokens                       int64
}

// FindTaskRun returns one task run, or ErrNotFound.
func FindTaskRun(ctx context.Context, tx pgx.Tx, id string) (TaskRunDetail, error) {
	var d TaskRunDetail
	if uuid.Validate(id) != nil {
		return d, ErrNotFound
	}
	rows, err := tx.Query(ctx, `SELECT `+taskRunColumns+`, t.fields, t.proposed, t.applied, t.dropped,
		e.id::text, e.forge, e.event, e.raw_event, e.action, e.sender, e.delivery, e.received_at,
		mc.calls, mc.cost, mc.input, mc.output
		FROM task_runs t JOIN repositories r ON r.id = t.repository_id
		LEFT JOIN task_events e ON e.id = t.event_id
		LEFT JOIN LATERAL (SELECT count(*) AS calls, coalesce(sum(cost_usd), 0)::float8 AS cost,
			coalesce(sum(input_tokens), 0)::bigint AS input, coalesce(sum(output_tokens), 0)::bigint AS output
			FROM model_calls WHERE task_run_id = t.id) mc ON true
		WHERE t.id = $1::uuid`, id)
	if err != nil {
		return d, fmt.Errorf("store: find task run: %w", err)
	}
	d, err = pgx.CollectExactlyOneRow(rows, func(row pgx.CollectableRow) (TaskRunDetail, error) {
		var x TaskRunDetail
		var status string
		var fields, proposed, applied, dropped []byte
		var eventID, forge, event, rawEvent, action, sender, delivery *string
		var receivedAt *time.Time
		err := row.Scan(&x.ID, &x.RepositoryID, &x.Repository, &x.Task, &x.SubjectKind, &x.SubjectNumber, &x.Trigger, &x.Mode, &status,
			&x.Reason, &x.ConfigSHA, &x.Model, &x.Error, &x.CommentID, &x.CreatedAt, &x.StartedAt, &x.FinishedAt, &x.DroppedCount,
			&fields, &proposed, &applied, &dropped,
			&eventID, &forge, &event, &rawEvent, &action, &sender, &delivery, &receivedAt,
			&x.ModelCalls, &x.CostUSD, &x.InputTokens, &x.OutputTokens)
		if err != nil {
			return x, err
		}
		x.Status = TaskRunStatus(status)
		x.Fields, x.Proposed, x.Applied, x.Dropped = rawOrNil(fields), rawOrNil(proposed), rawOrNil(applied), rawOrNil(dropped)
		if eventID != nil {
			x.Event = &TaskEventRow{
				ID: *eventID, Forge: *forge, Event: *event, RawEvent: *rawEvent, Action: *action, Sender: *sender, Delivery: *delivery,
				ReceivedAt: *receivedAt,
			}
		}
		return x, nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return d, ErrNotFound
	}
	if err != nil {
		return d, fmt.Errorf("store: find task run: %w", err)
	}
	return d, nil
}

func rawOrNil(b []byte) json.RawMessage {
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	return json.RawMessage(b)
}

// TaskRunModelCalls returns a task run's model calls in the order they
// were recorded.
func TaskRunModelCalls(ctx context.Context, tx pgx.Tx, taskRunID string) ([]transcript.StoredRow, error) {
	if uuid.Validate(taskRunID) != nil {
		return nil, ErrNotFound
	}
	return modelCallsWhere(ctx, tx, `task_run_id = $1::uuid`, taskRunID)
}
