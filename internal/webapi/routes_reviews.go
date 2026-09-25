package webapi

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/contextpack"
	"github.com/home-operations/kritik/internal/model"
	"github.com/home-operations/kritik/internal/store"
	"github.com/home-operations/kritik/internal/transcript"
)

// reviewRecord is a review and what its newest runner run left behind;
// run, agent and pack are nil where there is none.
type reviewRecord struct {
	review store.ReviewRow
	run    *store.RunnerRunRow
	agent  *store.AgentRunRow
	meta   *store.ContextPackMeta
	bodies *store.ContextPackBodies
}

// loadReview reads the review {id} and, when withRun, its runner run,
// agent run and context pack.
func loadReview(ctx context.Context, tx pgx.Tx, r *http.Request, withRun bool) (reviewRecord, error) {
	var rec reviewRecord
	id := r.PathValue("id")
	if uuid.Validate(id) != nil {
		return rec, errNotFound("review")
	}
	v, err := store.FindReview(ctx, tx, id)
	if errors.Is(err, store.ErrNotFound) {
		return rec, errNotFound("review")
	}
	if err != nil || !withRun {
		rec.review = v
		return rec, err
	}
	rec.review = v
	run, err := store.LatestRunnerRun(ctx, tx, v.ID)
	if errors.Is(err, store.ErrNotFound) {
		return rec, nil
	}
	if err != nil {
		return rec, err
	}
	rec.run = &run
	if a, err := store.FindAgentRun(ctx, tx, run.ID); err == nil {
		rec.agent = &a
	} else if !errors.Is(err, store.ErrNotFound) {
		return rec, err
	}
	if m, b, err := store.FindContextPack(ctx, tx, run.ID); err == nil {
		rec.meta, rec.bodies = &m, &b
	} else if !errors.Is(err, store.ErrNotFound) {
		return rec, err
	}
	return rec, nil
}

func (s *Server) getReview(w http.ResponseWriter, r *http.Request, t *tenantScope) error {
	ctx := r.Context()
	var d ReviewDetail
	if err := s.read(ctx, t, func(tx pgx.Tx) error {
		rec, err := loadReview(ctx, tx, r, true)
		if err != nil {
			return err
		}
		findings, err := store.ListFindings(ctx, tx, rec.review.ID)
		if err != nil {
			return err
		}
		usage, err := store.ListReviewUsage(ctx, tx, rec.review.ID)
		if err != nil {
			return err
		}
		d = reviewDetail(rec, findings, usage)
		return nil
	}); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, d)
	return nil
}

func reviewDetail(rec reviewRecord, findings []store.FindingRow, usage []store.UsageRow) ReviewDetail {
	v := rec.review
	d := ReviewDetail{
		Review: ReviewInfo{
			Review: reviewItem(v), Pull: PullRef{Repository: v.Repository, Number: v.Number, Title: v.Title},
			ScopeReason: v.ScopeReason, MergeBaseSHA: v.MergeBaseSHA, PatchID: v.PatchID, PriorReviewID: v.PriorReviewID,
			CancelRequestedAt: v.CancelRequestedAt,
		},
		Findings: make([]Finding, len(findings)), Usage: make([]UsageRow, len(usage)),
	}
	if v.Summary != nil {
		d.Summary = &Summary{Take: v.Summary.Take, Praise: nonNil(v.Summary.Praise)}
	}
	for i, f := range findings {
		d.Findings[i] = Finding{
			ID: f.ID, Path: f.Path, Line: f.Line, EndLine: f.EndLine, Severity: f.Severity, Title: f.Title, Explanation: f.Explanation,
			SuggestedFix: f.SuggestedFix, Replacement: f.Replacement, AgentPrompt: f.AgentPrompt, Fingerprint: f.Fingerprint,
			PostedInline: f.PostedInline, ForgeCommentID: f.ForgeCommentID, CreatedAt: f.CreatedAt,
		}
	}
	for i, u := range usage {
		d.Usage[i] = UsageRow{
			Role: u.Role, Model: u.Model, Upstream: u.Upstream, InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
			CostUSD: u.CostUSD, CreatedAt: u.CreatedAt,
		}
	}
	if x := rec.run; x != nil {
		d.RunnerRun = &RunnerRun{
			ID: x.ID, Phase: x.Phase, JobName: x.JobName, PodName: x.PodName, NodeName: x.NodeName, CreatedAt: x.CreatedAt,
			ScheduledAt: x.ScheduledAt, StartedAt: x.StartedAt, FinishedAt: x.FinishedAt, HeartbeatAt: x.HeartbeatAt,
			ExitCode: x.ExitCode, TerminationReason: x.TerminationReason, DeadlineExceeded: x.DeadlineExceeded, Error: x.Error,
			LogTail: x.LogTail,
		}
	}
	if a := rec.agent; a != nil {
		d.AgentRun = agentRun(a)
	}
	if m := rec.meta; m != nil {
		d.ContextPack = contextPack(m)
	}
	return d
}

func agentRun(a *store.AgentRunRow) *AgentRun {
	out := &AgentRun{
		StopReason: a.StopReason, Steps: a.Steps, ToolCalls: a.ToolCalls, Timeline: make([]TimelineStep, len(a.Timeline)),
		Sources: a.Sources, Usage: usageOf(a.Usage), CostUSD: a.CostUSD, Model: a.Model, Error: a.Error, CreatedAt: a.CreatedAt,
		Result: a.Result,
	}
	for i, st := range a.Timeline {
		out.Timeline[i] = TimelineStep{
			Index: st.Index, Tools: nonNil(st.Tools), DurationMs: st.DurationMS, OutputBytes: st.OutputBytes,
			InputTokens: st.InputTokens, OutputTokens: st.OutputTokens,
		}
	}
	return out
}

func contextPack(m *store.ContextPackMeta) *ContextPack {
	out := &ContextPack{
		HeadSHA: m.HeadSHA, BaseSHA: m.BaseSHA, PatchID: m.PatchID, ChangedPaths: nonNil(m.ChangedPaths),
		DeltaPaths: nonNil(m.DeltaPaths), PriorHeadSHA: m.PriorHeadSHA, RepoNotes: nonNil(m.RepoNotes),
		Stages: make([]Stage, len(m.Stages)), RepoFiles: make([]RepoFile, 0, len(m.RepoFiles)), CreatedAt: m.CreatedAt,
	}
	for i, c := range m.Stages {
		out.Stages[i] = Stage{
			Stage: c.Stage, Path: c.Path, Language: c.Language, Symbol: c.Symbol, Kind: c.Kind, Scope: c.Scope,
			StartLine: c.StartLine, EndLine: c.EndLine, Ref: c.Ref, Bytes: m.StageBytes[i],
		}
	}
	for path, size := range m.RepoFiles {
		out.RepoFiles = append(out.RepoFiles, RepoFile{Path: path, Size: size})
	}
	slices.SortFunc(out.RepoFiles, func(a, b RepoFile) int { return cmp.Compare(a.Path, b.Path) })
	return out
}

func (s *Server) getReviewDiff(w http.ResponseWriter, r *http.Request, t *tenantScope) error {
	ctx := r.Context()
	d := ReviewDiff{}
	if err := s.read(ctx, t, func(tx pgx.Tx) error {
		rec, err := loadReview(ctx, tx, r, true)
		if err != nil {
			return err
		}
		if rec.bodies != nil {
			d = ReviewDiff{Diff: rec.bodies.Diff, DeltaDiff: rec.bodies.DeltaDiff}
		}
		return nil
	}); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, d)
	return nil
}

func (s *Server) getReviewRaw(w http.ResponseWriter, r *http.Request, t *tenantScope) error {
	ctx := r.Context()
	raw := ReviewRaw{RepoFiles: map[string]string{}, Stages: []ContextChunk{}}
	if err := s.read(ctx, t, func(tx pgx.Tx) error {
		rec, err := loadReview(ctx, tx, r, true)
		if err != nil {
			return err
		}
		if rec.run != nil {
			raw.LogTail = rec.run.LogTail
		}
		if rec.agent != nil {
			raw.Result = rec.agent.Result
		}
		if b := rec.bodies; b != nil {
			raw.RepoFiles, raw.Stages = b.RepoFiles, contextChunks(b.Stages)
		}
		return nil
	}); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, raw)
	return nil
}

func contextChunks(in []contextpack.Chunk) []ContextChunk {
	out := make([]ContextChunk, len(in))
	for i, c := range in {
		out[i] = ContextChunk{
			Stage: c.Stage, Path: c.Path, Language: c.Language, Symbol: c.Symbol, Kind: c.Kind, Scope: c.Scope,
			StartLine: c.StartLine, EndLine: c.EndLine, Ref: c.Ref, Text: c.Text,
		}
	}
	return out
}

func (s *Server) getReviewTranscript(w http.ResponseWriter, r *http.Request, t *tenantScope) error {
	ctx := r.Context()
	var rows []transcript.StoredRow
	if err := s.read(ctx, t, func(tx pgx.Tx) error {
		rec, err := loadReview(ctx, tx, r, false)
		if err != nil {
			return err
		}
		rows, err = store.ReviewModelCalls(ctx, tx, rec.review.ID)
		return err
	}); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, transcriptOf(transcript.Rebuild(rows)))
	return nil
}

func transcriptOf(c transcript.Conversation) Transcript {
	out := Transcript{System: c.System, Tools: toolDefs(c.Tools), Turns: make([]Turn, len(c.Turns))}
	for i, t := range c.Turns {
		turn := Turn{
			Index: i, ID: t.ID, Kind: t.Kind, Step: t.Step, Model: t.Model, Upstream: t.Upstream, System: t.System,
			Reset: t.Reset, MessagesFrom: t.MessagesFrom, Messages: make([]Message, len(t.Messages)),
			Response: Response{Text: t.Response.Text, ToolCalls: toolCalls(t.Response.ToolCalls), Stop: t.Response.Stop},
			Usage:    usageOf(t.Usage), CostUSD: t.CostUSD, DurationMs: t.Duration.Milliseconds(), Error: t.Error,
			Truncated: t.Truncated, CreatedAt: t.CreatedAt, RunnerRunID: t.RunnerRunID,
		}
		if t.Tools != nil {
			turn.Tools = toolDefs(*t.Tools)
		}
		for j, m := range t.Messages {
			msg := Message{Role: m.Role, Text: m.Text, ToolCalls: toolCalls(m.ToolCalls), ToolResults: make([]ToolResult, len(m.ToolResults))}
			for k, res := range m.ToolResults {
				msg.ToolResults[k] = ToolResult{CallID: res.CallID, Content: res.Content, IsError: res.IsError, TruncatedBytes: res.TruncatedBytes}
			}
			turn.Messages[j] = msg
		}
		out.Turns[i] = turn
	}
	return out
}

func toolDefs(in []model.ToolDef) []ToolDef {
	out := make([]ToolDef, len(in))
	for i, d := range in {
		out[i] = ToolDef{Name: d.Name, Description: d.Description, InputSchema: d.InputSchema}
	}
	return out
}

func toolCalls(in []transcript.ToolCall) []ToolCall {
	out := make([]ToolCall, len(in))
	for i, c := range in {
		out[i] = ToolCall{ID: c.ID, Name: c.Name, Input: c.Input}
	}
	return out
}

func usageOf(u model.Usage) Usage {
	return Usage{Input: u.Input, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite, Output: u.Output}
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
