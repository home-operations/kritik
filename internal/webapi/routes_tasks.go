package webapi

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/store"
	"github.com/home-operations/kritik/internal/tasks"
	"github.com/home-operations/kritik/internal/transcript"
)

func (s *Server) registerTasks(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/tenants/{slug}/task-runs", s.tenant(s.listTaskRuns))
	mux.HandleFunc("GET /api/v1/tenants/{slug}/task-runs/{id}", s.tenant(s.getTaskRun))
	mux.HandleFunc("GET /api/v1/tenants/{slug}/task-runs/{id}/transcript", s.tenant(s.getTaskRunTranscript))
}

func (s *Server) listTaskRuns(w http.ResponseWriter, r *http.Request, t *tenantScope) error {
	page, err := parsePage(r)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	f := store.TaskRunFilter{Task: q.Get("task"), Status: store.TaskRunStatus(q.Get("status"))}
	if f.Status != "" && !f.Status.Valid() {
		return errBadRequest(CodeBadRequest, "status must be queued, running, succeeded, failed or skipped")
	}
	ctx := r.Context()
	var rows []store.TaskRunRow
	var next *store.Cursor
	if err := s.read(ctx, t, func(tx pgx.Tx) error {
		if f.RepositoryID, err = repoFilter(ctx, tx, r); err != nil {
			return err
		}
		rows, next, err = store.ListTaskRuns(ctx, tx, f, page)
		return err
	}); err != nil {
		return err
	}
	items := make([]TaskRun, len(rows))
	for i, row := range rows {
		items[i] = taskRun(row)
	}
	writeJSON(w, http.StatusOK, newPage(items, next))
	return nil
}

func taskRun(x store.TaskRunRow) TaskRun {
	out := TaskRun{
		ID: x.ID, Repository: x.Repository, Task: x.Task, SubjectKind: x.SubjectKind, SubjectNumber: x.SubjectNumber,
		Trigger: x.Trigger, Mode: x.Mode, Status: x.Status, Reason: x.Reason, Model: x.Model, ConfigSHA: x.ConfigSHA,
		CommentID: x.CommentID, Error: x.Error, DroppedCount: x.DroppedCount, CreatedAt: x.CreatedAt, StartedAt: x.StartedAt,
		FinishedAt: x.FinishedAt,
	}
	if x.FinishedAt != nil {
		start := x.CreatedAt
		if x.StartedAt != nil {
			start = *x.StartedAt
		}
		ms := x.FinishedAt.Sub(start).Milliseconds()
		out.DurationMs = &ms
	}
	return out
}

func (s *Server) getTaskRun(w http.ResponseWriter, r *http.Request, t *tenantScope) error {
	ctx := r.Context()
	var d store.TaskRunDetail
	if err := s.read(ctx, t, func(tx pgx.Tx) error {
		var err error
		d, err = store.FindTaskRun(ctx, tx, r.PathValue("id"))
		return err
	}); err != nil {
		return err
	}
	out, err := taskRunDetail(d)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func taskRunDetail(d store.TaskRunDetail) (TaskRunDetail, error) {
	out := TaskRunDetail{
		Run: taskRun(d.TaskRunRow), Fields: map[string]json.RawMessage{}, Dropped: []TaskDrop{}, ModelCalls: d.ModelCalls,
		CostUSD: d.CostUSD, Tokens: TokenCounts{Input: d.InputTokens, Output: d.OutputTokens},
	}
	if e := d.Event; e != nil {
		out.Event = &TaskEvent{
			Forge: e.Forge, Event: e.Event, RawEvent: e.RawEvent, Action: e.Action, Sender: e.Sender, Delivery: e.Delivery,
			ReceivedAt: e.ReceivedAt,
		}
	}
	if err := decodeRecord("fields", d.Fields, &out.Fields); err != nil {
		return out, err
	}
	if d.Proposed != nil {
		var a tasks.Answer
		if err := decodeRecord("proposed", d.Proposed, &a); err != nil {
			return out, err
		}
		out.Proposed = &TaskAnswer{
			Summary: a.Summary, Comment: a.Comment, AddLabels: nonNil(a.Labels.Add), RemoveLabels: nonNil(a.Labels.Remove),
			State: a.State, Assignees: nonNil(a.Assignees), Reviewers: nonNil(a.Reviewers), Inline: taskInlines(a.Inline),
		}
	}
	if d.Applied != nil {
		var a store.TaskRunApplied
		if err := decodeRecord("applied", d.Applied, &a); err != nil {
			return out, err
		}
		out.Applied = &TaskApplied{
			AddLabels: nonNil(a.AddLabels), RemoveLabels: nonNil(a.RemoveLabels), State: a.State, Assignees: nonNil(a.Assignees),
			Reviewers: nonNil(a.Reviewers), Inline: taskInlines(a.Inline), Comment: a.Comment,
		}
	}
	if err := decodeRecord("dropped", d.Dropped, &out.Dropped); err != nil {
		return out, err
	}
	out.Dropped = nonNil(out.Dropped)
	return out, nil
}

func decodeRecord(what string, raw json.RawMessage, v any) error {
	if raw == nil {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("webapi: decode task run %s: %w", what, err)
	}
	return nil
}

func taskInlines(in []tasks.Inline) []TaskInline {
	out := make([]TaskInline, len(in))
	for i, c := range in {
		out[i] = TaskInline{Path: c.Path, Line: c.Line, EndLine: c.EndLine, Body: c.Body}
	}
	return out
}

func (s *Server) getTaskRunTranscript(w http.ResponseWriter, r *http.Request, t *tenantScope) error {
	ctx := r.Context()
	var rows []transcript.StoredRow
	if err := s.read(ctx, t, func(tx pgx.Tx) error {
		// The run must exist in this tenant: an empty transcript and a
		// missing run are different answers.
		d, err := store.FindTaskRun(ctx, tx, r.PathValue("id"))
		if err != nil {
			return err
		}
		rows, err = store.TaskRunModelCalls(ctx, tx, d.ID)
		return err
	}); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, transcriptOf(transcript.Rebuild(rows)))
	return nil
}
