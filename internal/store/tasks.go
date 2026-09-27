package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// TaskRunStatus is where a task run is, as task_runs.status spells it.
type TaskRunStatus string

// Task run statuses.
const (
	TaskQueued    TaskRunStatus = "queued"
	TaskRunning   TaskRunStatus = "running"
	TaskSucceeded TaskRunStatus = "succeeded"
	TaskFailed    TaskRunStatus = "failed"
	TaskSkipped   TaskRunStatus = "skipped"
)

// Terminal reports whether a run in status s is over.
func (s TaskRunStatus) Terminal() bool {
	return s == TaskSucceeded || s == TaskFailed || s == TaskSkipped
}

// TaskEvent is one row of task_events: a delivery tasks may run on.
type TaskEvent struct {
	ID             string
	TenantID       string
	InstallationID string
	RepositoryID   string
	Forge          string
	// Event is the normalized event (tasks.EventIssue and so on), "" when
	// the delivery has none.
	Event         string
	RawEvent      string
	Action        string
	Sender        string
	Delivery      string
	SubjectKind   string
	SubjectNumber int
	// Payload is the delivery's JSON body; nil stores an empty object.
	Payload    json.RawMessage
	ReceivedAt time.Time
}

// InsertTaskEvent records e in tx, which must be scoped to e's tenant, and
// returns its id, or "" when the installation's delivery is already
// recorded: a redelivery.
func InsertTaskEvent(ctx context.Context, tx pgx.Tx, e TaskEvent) (string, error) {
	payload := e.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	var id string
	err := tx.QueryRow(ctx, `INSERT INTO task_events (tenant_id, installation_id, repository_id, forge, event, raw_event, action,
		sender, delivery, subject_kind, subject_number, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12::jsonb)
		ON CONFLICT (installation_id, delivery) WHERE delivery <> '' DO NOTHING RETURNING id`,
		e.TenantID, e.InstallationID, e.RepositoryID, e.Forge, e.Event, e.RawEvent, e.Action, e.Sender, e.Delivery,
		e.SubjectKind, e.SubjectNumber, string(payload)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: insert task event: %w", err)
	}
	return id, nil
}

// ErrTaskEventGone is a task event retention has already deleted, or one
// that never matched a task.
var ErrTaskEventGone = errors.New("store: task event is gone")

// LoadTaskEvent reads the task event id in tx.
func LoadTaskEvent(ctx context.Context, tx pgx.Tx, id string) (TaskEvent, error) {
	e := TaskEvent{ID: id}
	var payload []byte
	err := tx.QueryRow(ctx, `SELECT tenant_id, installation_id, repository_id, forge, event, raw_event, action, sender, delivery,
		subject_kind, subject_number, payload, received_at FROM task_events WHERE id = $1`, id).
		Scan(&e.TenantID, &e.InstallationID, &e.RepositoryID, &e.Forge, &e.Event, &e.RawEvent, &e.Action, &e.Sender, &e.Delivery,
			&e.SubjectKind, &e.SubjectNumber, &payload, &e.ReceivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return e, ErrTaskEventGone
	}
	if err != nil {
		return e, fmt.Errorf("store: load task event: %w", err)
	}
	e.Payload = payload
	return e, nil
}

// DeleteTaskEvent deletes the task event id in tx: one no task matched.
func DeleteTaskEvent(ctx context.Context, tx pgx.Tx, id string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM task_events WHERE id = $1`, id); err != nil {
		return fmt.Errorf("store: delete task event: %w", err)
	}
	return nil
}

// SweepTaskEvents deletes every tenant's task events received more than
// olderThan ago and returns how many it deleted; their runs keep their
// record. It runs on the owner connection, which row-level security does
// not restrict. Leader only.
func (s *Store) SweepTaskEvents(ctx context.Context, olderThan time.Duration) (int64, error) {
	if s.owner == nil {
		return 0, errors.New("store: SweepTaskEvents needs the owner connection")
	}
	if olderThan <= 0 {
		return 0, fmt.Errorf("store: task event retention %s is not positive", olderThan)
	}
	tag, err := s.owner.Exec(ctx, `DELETE FROM task_events WHERE received_at < now() - make_interval(secs => $1)`, olderThan.Seconds())
	if err != nil {
		return 0, fmt.Errorf("store: sweep task events: %w", err)
	}
	return tag.RowsAffected(), nil
}

// TaskRun is one row of task_runs.
type TaskRun struct {
	ID            string
	TenantID      string
	RepositoryID  string
	Task          string
	EventID       string
	SubjectKind   string
	SubjectNumber int
	Trigger       string
	Mode          string
	Status        TaskRunStatus
	Reason        string
	ConfigSHA     string
	CreatedAt     time.Time
	// StartedAt is when a job first ran the run, nil before; AnsweredAt
	// when its model's answer was charged, nil before.
	StartedAt  *time.Time
	AnsweredAt *time.Time
}

// QueueTaskRun records r as queued in tx, once per event and task, and
// reports whether it inserted it: a redelivered dispatch finds the run
// already there.
func QueueTaskRun(ctx context.Context, tx pgx.Tx, r TaskRun) (bool, error) {
	tag, err := tx.Exec(ctx, `INSERT INTO task_runs (tenant_id, repository_id, task, event_id, subject_kind, subject_number, trigger,
		mode, status, config_sha)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'queued', $9) ON CONFLICT (event_id, task) DO NOTHING`,
		r.TenantID, r.RepositoryID, r.Task, r.EventID, r.SubjectKind, r.SubjectNumber, r.Trigger, r.Mode, r.ConfigSHA)
	if err != nil {
		return false, fmt.Errorf("store: queue task run: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// ErrTaskRunGone is a task run that does not exist.
var ErrTaskRunGone = errors.New("store: task run is gone")

// LoadTaskRun reads the run of task on the event eventID in tx.
func LoadTaskRun(ctx context.Context, tx pgx.Tx, eventID, task string) (TaskRun, error) {
	r := TaskRun{EventID: eventID, Task: task}
	err := tx.QueryRow(ctx, `SELECT id, tenant_id, repository_id, subject_kind, subject_number, trigger, mode, status, reason,
		config_sha, created_at, started_at, answered_at FROM task_runs WHERE event_id = $1 AND task = $2`, eventID, task).
		Scan(&r.ID, &r.TenantID, &r.RepositoryID, &r.SubjectKind, &r.SubjectNumber, &r.Trigger, &r.Mode, &r.Status, &r.Reason,
			&r.ConfigSHA, &r.CreatedAt, &r.StartedAt, &r.AnsweredAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrTaskRunGone
	}
	if err != nil {
		return r, fmt.Errorf("store: load task run: %w", err)
	}
	return r, nil
}

// StartTaskRun marks the run id running in tx.
func StartTaskRun(ctx context.Context, tx pgx.Tx, id string) error {
	_, err := tx.Exec(ctx, `UPDATE task_runs SET status = 'running', started_at = coalesce(started_at, now()) WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: start task run: %w", err)
	}
	return nil
}

// TaskUsage is one model call charged to a task run.
type TaskUsage struct {
	Model, Upstream string
	Input, Output   int64
	CostUSD         float64
}

// ChargeTaskRun records the usage of the run id's model calls in tx, which
// must be scoped to tenantID, where the tenant's caps count it, and, when
// answered, marks the run answered: from then on it is never run again.
func ChargeTaskRun(ctx context.Context, tx pgx.Tx, tenantID, repositoryID, id string, usage []TaskUsage, answered bool) error {
	for _, u := range usage {
		if _, err := tx.Exec(ctx, `INSERT INTO usage (tenant_id, repository_id, role, model, upstream, input_tokens, output_tokens, cost_usd)
			VALUES ($1, $2, 'task', $3, $4, $5, $6, $7)`, tenantID, repositoryID, u.Model, u.Upstream, u.Input, u.Output, u.CostUSD); err != nil {
			return fmt.Errorf("store: charge task run: %w", err)
		}
	}
	if !answered {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE task_runs SET answered_at = now() WHERE id = $1`, id); err != nil {
		return fmt.Errorf("store: mark task run answered: %w", err)
	}
	return nil
}

// TaskRunResult is how a task run ended. The JSON fields are stored as
// given, nil as NULL.
type TaskRunResult struct {
	Status    TaskRunStatus
	Reason    string
	Model     string
	Fields    json.RawMessage
	Proposed  json.RawMessage
	Applied   json.RawMessage
	Dropped   json.RawMessage
	CommentID int64
	Error     string
	// Notes say what the run's context left out, and why.
	Notes []string
}

// FinishTaskRun records how the run id ended in tx.
func FinishTaskRun(ctx context.Context, tx pgx.Tx, id string, r TaskRunResult) error {
	if !r.Status.Terminal() {
		return fmt.Errorf("store: task run status %q is not terminal", r.Status)
	}
	_, err := tx.Exec(ctx, `UPDATE task_runs SET status = $2, reason = left($3, 500), model = $4, fields = $5::jsonb,
		proposed = $6::jsonb, applied = $7::jsonb, dropped = $8::jsonb, comment_id = nullif($9::bigint, 0), error = left($10, 2000),
		notes = $11, finished_at = now() WHERE id = $1`,
		id, string(r.Status), r.Reason, r.Model, jsonOrNil(r.Fields), jsonOrNil(r.Proposed), jsonOrNil(r.Applied),
		jsonOrNil(r.Dropped), r.CommentID, r.Error, notesOrEmpty(r.Notes))
	if err != nil {
		return fmt.Errorf("store: finish task run: %w", err)
	}
	return nil
}

// notesOrEmpty is notes for a NOT NULL text[] column.
func notesOrEmpty(notes []string) []string {
	if notes == nil {
		return []string{}
	}
	return notes
}

func jsonOrNil(raw json.RawMessage) any {
	if raw == nil {
		return nil
	}
	return string(raw)
}

// CountTaskRuns counts the runs of task on one subject of a repository
// that got as far as running within the last window, the run except left
// out.
func CountTaskRuns(
	ctx context.Context, tx pgx.Tx, repositoryID, task string, subjectNumber int, window time.Duration, except string,
) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `SELECT count(*) FROM task_runs WHERE repository_id = $1 AND task = $2 AND subject_number = $3
		AND started_at IS NOT NULL AND created_at > now() - make_interval(secs => $4) AND id <> $5`,
		repositoryID, task, subjectNumber, window.Seconds(), except).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count task runs: %w", err)
	}
	return n, nil
}

// InsertTaskRunner records a runner run of kind task in tx for the task run
// id, links the task run to it and returns its id. A retried job links the
// run it makes last.
func InsertTaskRunner(ctx context.Context, tx pgx.Tx, tenantID, taskRunID string) (string, error) {
	var runID string
	err := tx.QueryRow(ctx, `INSERT INTO runner_runs (tenant_id, kind) VALUES ($1, 'task') RETURNING id`, tenantID).Scan(&runID)
	if err != nil {
		return "", fmt.Errorf("store: insert task runner run: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE task_runs SET runner_run_id = $2 WHERE id = $1`, taskRunID, runID); err != nil {
		return "", fmt.Errorf("store: link task runner run: %w", err)
	}
	return runID, nil
}
