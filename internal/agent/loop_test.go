package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/home-operations/kritik/internal/model"
)

// fakeTool is a Tool whose Run returns a fixed output or error, for
// exercising the loop without depending on tools.go/tree.go.
type fakeTool struct {
	name   string
	output string
	err    error
}

func (f *fakeTool) Def() model.ToolDef {
	return model.ToolDef{Name: f.name, Description: "fake tool", InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (f *fakeTool) Run(context.Context, json.RawMessage) (string, error) {
	return f.output, f.err
}

// scriptedStepper replays a fixed sequence of responses, recording every
// request it was called with. Calling it past the end of the script is a
// test bug, not a loop bug, so it errors rather than panicking.
type scriptedStepper struct {
	steps []model.StepResponse
	calls []model.StepRequest
}

func (s *scriptedStepper) Step(_ context.Context, req model.StepRequest) (model.StepResponse, error) {
	s.calls = append(s.calls, req)
	if len(s.calls) > len(s.steps) {
		return model.StepResponse{}, fmt.Errorf("scriptedStepper: no script for call %d", len(s.calls))
	}
	return s.steps[len(s.calls)-1], nil
}

// ctxCancelStepper cancels its own ctx, then returns ctx.Err(), simulating a
// Stepper that observes and reports the Run's own cancellation rather than
// an unrelated failure.
type ctxCancelStepper struct {
	cancel context.CancelFunc
}

func (s *ctxCancelStepper) Step(ctx context.Context, _ model.StepRequest) (model.StepResponse, error) {
	s.cancel()
	return model.StepResponse{}, ctx.Err()
}

// ctxDeadlineStepper blocks until ctx ends, then returns ctx.Err(),
// simulating a Stepper whose call was still in flight when a job deadline
// arrived as context.DeadlineExceeded.
type ctxDeadlineStepper struct{}

func (ctxDeadlineStepper) Step(ctx context.Context, _ model.StepRequest) (model.StepResponse, error) {
	<-ctx.Done()
	return model.StepResponse{}, ctx.Err()
}

var testSubmitDef = model.ToolDef{
	Name:        "submit_review",
	Description: "submit the review",
	InputSchema: json.RawMessage(`{"type":"object"}`),
}

const validSubmitInput = `{"verdict":"approve"}`

func toolCall(id, name, input string) model.ToolCall {
	return model.ToolCall{ID: id, Name: name, Input: json.RawMessage(input)}
}

// runCase is one TestRun table row. setup builds this case's Stepper and
// context (a nil context means t.Context()); scripted is the same value
// again when the Stepper is a *scriptedStepper, so check can inspect its
// recorded calls, and nil for the ctx-focused cases that use a different
// Stepper double.
//
// setup and check are named functions rather than inline closures so each
// case's assertions are counted, by tooling such as gocyclo, against that
// function alone rather than against TestRun as a whole.
type runCase struct {
	name      string
	tools     []Tool
	limits    Limits
	setup     func(t *testing.T) (stepper model.Stepper, ctx context.Context, scripted *scriptedStepper)
	wantStop  StopReason
	wantSteps int
	check     func(t *testing.T, result Result, events []StepEvent, scripted *scriptedStepper)
}

func setupGrepReadSubmit(t *testing.T) (model.Stepper, context.Context, *scriptedStepper) {
	st := &scriptedStepper{steps: []model.StepResponse{
		{ToolCalls: []model.ToolCall{toolCall("1", "grep", `{"pattern":"x"}`)}},
		{ToolCalls: []model.ToolCall{toolCall("2", "read_file", `{"path":"widget.go"}`)}},
		{ToolCalls: []model.ToolCall{toolCall("3", "submit_review", validSubmitInput)}},
	}}
	return st, nil, st
}

func checkGrepReadSubmit(t *testing.T, result Result, _ []StepEvent, _ *scriptedStepper) {
	if string(result.Submitted) != validSubmitInput {
		t.Fatalf("Submitted = %s, want %s", result.Submitted, validSubmitInput)
	}
	want := map[string]int{"grep": 1, "read_file": 1, "submit_review": 1}
	if len(result.ToolCalls) != len(want) {
		t.Fatalf("ToolCalls = %v, want %v", result.ToolCalls, want)
	}
	for name, n := range want {
		if result.ToolCalls[name] != n {
			t.Fatalf("ToolCalls[%q] = %d, want %d (full: %v)", name, result.ToolCalls[name], n, result.ToolCalls)
		}
	}
}

func setupUnknownToolThenSubmit(t *testing.T) (model.Stepper, context.Context, *scriptedStepper) {
	st := &scriptedStepper{steps: []model.StepResponse{
		{ToolCalls: []model.ToolCall{toolCall("1", "does_not_exist", `{}`)}},
		{ToolCalls: []model.ToolCall{toolCall("2", "submit_review", validSubmitInput)}},
	}}
	return st, nil, st
}

func checkUnknownToolThenSubmit(t *testing.T, result Result, _ []StepEvent, scripted *scriptedStepper) {
	if result.ToolCalls["does_not_exist"] != 1 {
		t.Fatalf("ToolCalls = %v, want does_not_exist:1", result.ToolCalls)
	}
	// The unknown-tool step's error result must have reached the model as
	// the next turn's input, not silently vanished.
	last := scripted.calls[len(scripted.calls)-1]
	lastMsg := last.Messages[len(last.Messages)-1]
	if len(lastMsg.ToolResults) != 1 || !lastMsg.ToolResults[0].IsError {
		t.Fatalf("expected an error ToolResult for the unknown tool, got %+v", lastMsg.ToolResults)
	}
}

func setupInvalidSubmitJSONThenValid(t *testing.T) (model.Stepper, context.Context, *scriptedStepper) {
	st := &scriptedStepper{steps: []model.StepResponse{
		{ToolCalls: []model.ToolCall{toolCall("1", "submit_review", `{not json`)}},
		{ToolCalls: []model.ToolCall{toolCall("2", "submit_review", validSubmitInput)}},
	}}
	return st, nil, st
}

func checkInvalidSubmitJSONThenValid(t *testing.T, result Result, _ []StepEvent, _ *scriptedStepper) {
	if string(result.Submitted) != validSubmitInput {
		t.Fatalf("Submitted = %s, want %s", result.Submitted, validSubmitInput)
	}
}

func setupForcedInvalidSubmitStopsNoSubmit(t *testing.T) (model.Stepper, context.Context, *scriptedStepper) {
	st := &scriptedStepper{steps: []model.StepResponse{
		{ToolCalls: []model.ToolCall{toolCall("1", "submit_review", `{not json`)}},
	}}
	return st, nil, st
}

func checkForcedInvalidSubmitStopsNoSubmit(t *testing.T, result Result, _ []StepEvent, scripted *scriptedStepper) {
	if result.Submitted != nil {
		t.Fatalf("Submitted = %s, want nil", result.Submitted)
	}
	if len(scripted.calls) != 1 {
		t.Fatalf("expected exactly 1 call to the stepper, got %d", len(scripted.calls))
	}
}

func setupTextOnlyTwiceStopsNoSubmit(t *testing.T) (model.Stepper, context.Context, *scriptedStepper) {
	st := &scriptedStepper{steps: []model.StepResponse{
		{Text: "thinking..."},
		{Text: "still thinking..."},
	}}
	return st, nil, st
}

func checkTextOnlyTwiceStopsNoSubmit(t *testing.T, _ Result, events []StepEvent, scripted *scriptedStepper) {
	if len(events) != 2 || events[0].Index != 0 || events[1].Index != 1 {
		t.Fatalf("events = %+v", events)
	}
	// The nudge must have been sent as a user message after the first
	// text-only turn, or the second turn is not really giving the model a
	// chance to submit.
	last := scripted.calls[len(scripted.calls)-1]
	found := false
	for _, m := range last.Messages {
		if m.Role == model.RoleUser && m.Text == nudgeText {
			found = true
		}
	}
	if !found {
		t.Fatalf("nudge message %q not found in %+v", nudgeText, last.Messages)
	}
}

func setupNudgeOncePerRunNotResetByToolTurn(t *testing.T) (model.Stepper, context.Context, *scriptedStepper) {
	st := &scriptedStepper{steps: []model.StepResponse{
		{Text: "thinking..."},
		{ToolCalls: []model.ToolCall{toolCall("1", "noop", `{}`)}},
		{Text: "still thinking..."},
	}}
	return st, nil, st
}

func setupBudgetForcesToolChoice(t *testing.T) (model.Stepper, context.Context, *scriptedStepper) {
	st := &scriptedStepper{steps: []model.StepResponse{
		{ToolCalls: []model.ToolCall{toolCall("1", "noop", `{}`)}, Usage: model.Usage{Input: 80, Output: 10}},
		{ToolCalls: []model.ToolCall{toolCall("2", "submit_review", validSubmitInput)}},
	}}
	return st, nil, st
}

func checkBudgetForcesToolChoice(t *testing.T, _ Result, _ []StepEvent, scripted *scriptedStepper) {
	if len(scripted.calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(scripted.calls))
	}
	forcedReq := scripted.calls[1]
	if forcedReq.ToolChoice.Mode != model.ToolChoiceTool || forcedReq.ToolChoice.Name != "submit_review" {
		t.Fatalf("ToolChoice = %+v, want a forced submit_review", forcedReq.ToolChoice)
	}
	// The first request must NOT have been forced: the budget only crosses
	// the 90% threshold after step 0's usage lands.
	if scripted.calls[0].ToolChoice.Mode == model.ToolChoiceTool {
		t.Fatalf("first request was forced, want unforced: %+v", scripted.calls[0].ToolChoice)
	}
}

func setupBudgetExhaustedStopsImmediately(t *testing.T) (model.Stepper, context.Context, *scriptedStepper) {
	st := &scriptedStepper{steps: []model.StepResponse{
		{ToolCalls: []model.ToolCall{toolCall("1", "noop", `{}`)}, Usage: model.Usage{Input: 60}},
		{ToolCalls: []model.ToolCall{toolCall("2", "submit_review", validSubmitInput)}},
	}}
	return st, nil, st
}

func checkBudgetExhaustedStopsImmediately(t *testing.T, _ Result, _ []StepEvent, scripted *scriptedStepper) {
	if len(scripted.calls) != 1 {
		t.Fatalf("expected the loop to stop before a second call, got %d calls", len(scripted.calls))
	}
}

func setupMaxStepsReachedWithoutSubmit(t *testing.T) (model.Stepper, context.Context, *scriptedStepper) {
	st := &scriptedStepper{steps: []model.StepResponse{
		{ToolCalls: []model.ToolCall{toolCall("1", "noop", `{}`)}},
		{ToolCalls: []model.ToolCall{toolCall("2", "noop", `{}`)}},
	}}
	return st, nil, st
}

func checkMaxStepsReachedWithoutSubmit(t *testing.T, _ Result, _ []StepEvent, scripted *scriptedStepper) {
	// The final step must still have been forced to offer submit_review,
	// even though the model chose not to take it.
	last := scripted.calls[len(scripted.calls)-1]
	if last.ToolChoice.Mode != model.ToolChoiceTool || last.ToolChoice.Name != "submit_review" {
		t.Fatalf("final ToolChoice = %+v, want a forced submit_review", last.ToolChoice)
	}
}

func setupContextCanceledBeforeFirstStep(t *testing.T) (model.Stepper, context.Context, *scriptedStepper) {
	st := &scriptedStepper{}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	return st, ctx, st
}

func checkContextCanceledBeforeFirstStep(t *testing.T, _ Result, _ []StepEvent, scripted *scriptedStepper) {
	if len(scripted.calls) != 0 {
		t.Fatalf("stepper was called %d times, want 0", len(scripted.calls))
	}
}

func setupStepperError(t *testing.T) (model.Stepper, context.Context, *scriptedStepper) {
	st := &scriptedStepper{}
	return st, nil, st
}

func checkStepperError(t *testing.T, result Result, _ []StepEvent, _ *scriptedStepper) {
	if result.Err == "" {
		t.Fatal("Err is empty, want the stepper's error message")
	}
}

func setupStepperErrorWhileCtxCanceled(t *testing.T) (model.Stepper, context.Context, *scriptedStepper) {
	ctx, cancel := context.WithCancel(t.Context())
	return &ctxCancelStepper{cancel: cancel}, ctx, nil
}

func setupStepperErrorOnCtxDeadlineExceeded(t *testing.T) (model.Stepper, context.Context, *scriptedStepper) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	t.Cleanup(cancel)
	return ctxDeadlineStepper{}, ctx, nil
}

// checkCanceledRunHasNoErr is shared by both ctx-cancellation cases: a Run
// that ends StopCanceled must never also carry a Stepper error message.
func checkCanceledRunHasNoErr(t *testing.T, result Result, _ []StepEvent, _ *scriptedStepper) {
	if result.Err != "" {
		t.Fatalf("Err = %q, want empty for a canceled run", result.Err)
	}
}

func setupUsageSummedAcrossSteps(t *testing.T) (model.Stepper, context.Context, *scriptedStepper) {
	st := &scriptedStepper{steps: []model.StepResponse{
		{ToolCalls: []model.ToolCall{toolCall("1", "noop", `{}`)}, Usage: model.Usage{Input: 100, Output: 10}, CostUSD: 0.01},
		{ToolCalls: []model.ToolCall{toolCall("2", "submit_review", validSubmitInput)}, Usage: model.Usage{Input: 50, Output: 5}, CostUSD: 0.02},
	}}
	return st, nil, st
}

func checkUsageSummedAcrossSteps(t *testing.T, result Result, _ []StepEvent, _ *scriptedStepper) {
	want := model.Usage{Input: 150, Output: 15}
	if result.Usage != want {
		t.Fatalf("Usage = %+v, want %+v", result.Usage, want)
	}
	if result.CostUSD != 0.03 {
		t.Fatalf("CostUSD = %v, want 0.03", result.CostUSD)
	}
}

func setupToolOutputTruncated(t *testing.T) (model.Stepper, context.Context, *scriptedStepper) {
	st := &scriptedStepper{steps: []model.StepResponse{
		{ToolCalls: []model.ToolCall{toolCall("1", "big", `{}`)}},
		{ToolCalls: []model.ToolCall{toolCall("2", "submit_review", validSubmitInput)}},
	}}
	return st, nil, st
}

func checkToolOutputTruncated(t *testing.T, _ Result, _ []StepEvent, scripted *scriptedStepper) {
	last := scripted.calls[len(scripted.calls)-1]
	lastMsg := last.Messages[len(last.Messages)-1]
	if len(lastMsg.ToolResults) != 1 {
		t.Fatalf("ToolResults = %+v, want exactly 1", lastMsg.ToolResults)
	}
	content := lastMsg.ToolResults[0].Content
	if !strings.HasPrefix(content, "abcde") || !strings.Contains(content, "[truncated 5 bytes]") {
		t.Fatalf("content = %q, want a 5-byte prefix plus a truncation marker", content)
	}
}

func setupSubmitEndsMultiCallTurnEarly(t *testing.T) (model.Stepper, context.Context, *scriptedStepper) {
	st := &scriptedStepper{steps: []model.StepResponse{
		{ToolCalls: []model.ToolCall{
			toolCall("1", "noop", `{}`),
			toolCall("2", "submit_review", validSubmitInput),
			toolCall("3", "noop2", `{}`),
		}},
	}}
	return st, nil, st
}

func checkSubmitEndsMultiCallTurnEarly(t *testing.T, result Result, _ []StepEvent, _ *scriptedStepper) {
	want := map[string]int{"noop": 1, "submit_review": 1}
	if len(result.ToolCalls) != len(want) {
		t.Fatalf("ToolCalls = %v, want %v (noop2 must not be reached)", result.ToolCalls, want)
	}
	for name, n := range want {
		if result.ToolCalls[name] != n {
			t.Fatalf("ToolCalls[%q] = %d, want %d", name, result.ToolCalls[name], n)
		}
	}
}

func setupMultipleToolCallsGetMatchingCallIDs(t *testing.T) (model.Stepper, context.Context, *scriptedStepper) {
	st := &scriptedStepper{steps: []model.StepResponse{
		{ToolCalls: []model.ToolCall{
			toolCall("a1", "alpha", `{}`),
			toolCall("b2", "beta", `{}`),
			toolCall("c3", "gamma", `{}`),
		}},
		{ToolCalls: []model.ToolCall{toolCall("4", "submit_review", validSubmitInput)}},
	}}
	return st, nil, st
}

func checkMultipleToolCallsGetMatchingCallIDs(t *testing.T, _ Result, _ []StepEvent, scripted *scriptedStepper) {
	last := scripted.calls[len(scripted.calls)-1]
	lastMsg := last.Messages[len(last.Messages)-1]
	want := map[string]string{"a1": "alpha-out", "b2": "beta-out", "c3": "gamma-out"}
	if len(lastMsg.ToolResults) != len(want) {
		t.Fatalf("ToolResults = %+v, want %d entries", lastMsg.ToolResults, len(want))
	}
	for _, tr := range lastMsg.ToolResults {
		wantContent, ok := want[tr.CallID]
		if !ok {
			t.Fatalf("unexpected CallID %q in %+v", tr.CallID, lastMsg.ToolResults)
		}
		if tr.Content != wantContent {
			t.Fatalf("ToolResult[%q].Content = %q, want %q", tr.CallID, tr.Content, wantContent)
		}
	}
}

func setupStepEventToolsAndOutputBytesPopulated(t *testing.T) (model.Stepper, context.Context, *scriptedStepper) {
	st := &scriptedStepper{steps: []model.StepResponse{
		{ToolCalls: []model.ToolCall{toolCall("1", "alpha", `{}`), toolCall("2", "beta", `{}`)}},
		{ToolCalls: []model.ToolCall{toolCall("3", "submit_review", validSubmitInput)}},
	}}
	return st, nil, st
}

func checkStepEventToolsAndOutputBytesPopulated(t *testing.T, _ Result, events []StepEvent, _ *scriptedStepper) {
	if len(events) == 0 {
		t.Fatal("events is empty, want at least 1")
	}
	first := events[0]
	wantTools := []string{"alpha", "beta"}
	if len(first.Tools) != len(wantTools) {
		t.Fatalf("Tools = %v, want %v", first.Tools, wantTools)
	}
	for i, name := range wantTools {
		if first.Tools[i] != name {
			t.Fatalf("Tools[%d] = %q, want %q", i, first.Tools[i], name)
		}
	}
	if want := len("aaaaa") + len("bbb"); first.OutputBytes != want {
		t.Fatalf("OutputBytes = %d, want %d", first.OutputBytes, want)
	}
}

func TestRun(t *testing.T) {
	cases := []runCase{
		{
			name: "grep_read_submit",
			tools: []Tool{
				&fakeTool{name: "grep", output: "widget.go:1: match"},
				&fakeTool{name: "read_file", output: "1\tpackage main"},
			},
			setup:     setupGrepReadSubmit,
			wantStop:  StopSubmitted,
			wantSteps: 3,
			check:     checkGrepReadSubmit,
		},
		{
			name:      "unknown_tool_then_submit",
			setup:     setupUnknownToolThenSubmit,
			wantStop:  StopSubmitted,
			wantSteps: 2,
			check:     checkUnknownToolThenSubmit,
		},
		{
			name:      "invalid_submit_json_then_valid",
			setup:     setupInvalidSubmitJSONThenValid,
			wantStop:  StopSubmitted,
			wantSteps: 2,
			check:     checkInvalidSubmitJSONThenValid,
		},
		{
			// MaxSteps: 1 makes the only step the last step, which forces
			// submit_review; an invalid submit on a forced step must not
			// get a second chance.
			name:      "forced_invalid_submit_stops_no_submit",
			limits:    Limits{MaxSteps: 1},
			setup:     setupForcedInvalidSubmitStopsNoSubmit,
			wantStop:  StopNoSubmit,
			wantSteps: 1,
			check:     checkForcedInvalidSubmitStopsNoSubmit,
		},
		{
			name:      "text_only_twice_stops_no_submit",
			setup:     setupTextOnlyTwiceStopsNoSubmit,
			wantStop:  StopNoSubmit,
			wantSteps: 2,
			check:     checkTextOnlyTwiceStopsNoSubmit,
		},
		{
			// A tool-call turn between the two text-only turns must not
			// reset the once-per-Run nudge: a second text-only turn still
			// ends the Run rather than sending a second nudge.
			name:      "nudge_is_once_per_run_not_reset_by_tool_turn",
			tools:     []Tool{&fakeTool{name: "noop", output: "ok"}},
			setup:     setupNudgeOncePerRunNotResetByToolTurn,
			wantStop:  StopNoSubmit,
			wantSteps: 3,
		},
		{
			name:      "budget_forces_tool_choice",
			tools:     []Tool{&fakeTool{name: "noop", output: "ok"}},
			limits:    Limits{MaxTokens: 100},
			setup:     setupBudgetForcesToolChoice,
			wantStop:  StopSubmitted,
			wantSteps: 2,
			check:     checkBudgetForcesToolChoice,
		},
		{
			name:      "budget_exhausted_stops_immediately",
			tools:     []Tool{&fakeTool{name: "noop", output: "ok"}},
			limits:    Limits{MaxTokens: 50},
			setup:     setupBudgetExhaustedStopsImmediately,
			wantStop:  StopBudget,
			wantSteps: 1,
			check:     checkBudgetExhaustedStopsImmediately,
		},
		{
			name:      "max_steps_reached_without_submit",
			tools:     []Tool{&fakeTool{name: "noop", output: "ok"}},
			limits:    Limits{MaxSteps: 2},
			setup:     setupMaxStepsReachedWithoutSubmit,
			wantStop:  StopMaxSteps,
			wantSteps: 2,
			check:     checkMaxStepsReachedWithoutSubmit,
		},
		{
			name:      "context_canceled_before_first_step",
			setup:     setupContextCanceledBeforeFirstStep,
			wantStop:  StopCanceled,
			wantSteps: 0,
			check:     checkContextCanceledBeforeFirstStep,
		},
		{
			name:      "stepper_error",
			setup:     setupStepperError,
			wantStop:  StopError,
			wantSteps: 0,
			check:     checkStepperError,
		},
		{
			// A Stepper error correlated with the Run's own ctx cancellation
			// must end as StopCanceled, not StopError: the Job asked for
			// this, it wasn't a genuine Stepper failure.
			name:      "stepper_error_while_ctx_canceled_is_stop_canceled",
			setup:     setupStepperErrorWhileCtxCanceled,
			wantStop:  StopCanceled,
			wantSteps: 0,
			check:     checkCanceledRunHasNoErr,
		},
		{
			// A job deadline arriving as context.DeadlineExceeded inside
			// Step must also end as StopCanceled, not StopError.
			name:      "stepper_error_on_ctx_deadline_exceeded_is_stop_canceled",
			setup:     setupStepperErrorOnCtxDeadlineExceeded,
			wantStop:  StopCanceled,
			wantSteps: 0,
			check:     checkCanceledRunHasNoErr,
		},
		{
			name:      "usage_summed_across_steps",
			tools:     []Tool{&fakeTool{name: "noop", output: "ok"}},
			setup:     setupUsageSummedAcrossSteps,
			wantStop:  StopSubmitted,
			wantSteps: 2,
			check:     checkUsageSummedAcrossSteps,
		},
		{
			name:      "tool_output_truncated",
			tools:     []Tool{&fakeTool{name: "big", output: "abcdefghij"}},
			limits:    Limits{MaxToolOutputBytes: 5},
			setup:     setupToolOutputTruncated,
			wantStop:  StopSubmitted,
			wantSteps: 2,
			check:     checkToolOutputTruncated,
		},
		{
			name:      "submit_ends_multi_call_turn_early",
			tools:     []Tool{&fakeTool{name: "noop", output: "ok"}, &fakeTool{name: "noop2", output: "ok"}},
			setup:     setupSubmitEndsMultiCallTurnEarly,
			wantStop:  StopSubmitted,
			wantSteps: 1,
			check:     checkSubmitEndsMultiCallTurnEarly,
		},
		{
			// Several non-submit tool calls in one turn must each get a
			// ToolResult carrying that call's own CallID and output, not a
			// mixed-up or missing one.
			name: "multiple_tool_calls_get_matching_call_ids",
			tools: []Tool{
				&fakeTool{name: "alpha", output: "alpha-out"},
				&fakeTool{name: "beta", output: "beta-out"},
				&fakeTool{name: "gamma", output: "gamma-out"},
			},
			setup:     setupMultipleToolCallsGetMatchingCallIDs,
			wantStop:  StopSubmitted,
			wantSteps: 2,
			check:     checkMultipleToolCallsGetMatchingCallIDs,
		},
		{
			// A step's StepEvent must list every tool called during it and
			// the total (post-truncation) bytes of tool output it produced.
			name: "step_event_tools_and_output_bytes_populated",
			tools: []Tool{
				&fakeTool{name: "alpha", output: "aaaaa"},
				&fakeTool{name: "beta", output: "bbb"},
			},
			setup:     setupStepEventToolsAndOutputBytesPopulated,
			wantStop:  StopSubmitted,
			wantSteps: 2,
			check:     checkStepEventToolsAndOutputBytesPopulated,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			stepper, ctx, scripted := tt.setup(t)
			if ctx == nil {
				ctx = t.Context()
			}
			var events []StepEvent
			run := Run{
				Stepper: stepper,
				Tools:   tt.tools,
				Submit:  testSubmitDef,
				Limits:  tt.limits,
				OnStep:  func(e StepEvent) { events = append(events, e) },
			}

			result := run.Do(ctx)

			if result.Stop != tt.wantStop {
				t.Fatalf("Stop = %v, want %v", result.Stop, tt.wantStop)
			}
			if result.Steps != tt.wantSteps {
				t.Fatalf("Steps = %d, want %d", result.Steps, tt.wantSteps)
			}
			if tt.check != nil {
				tt.check(t, result, events, scripted)
			}
		})
	}
}

func TestStopReasonValid(t *testing.T) {
	tests := []struct {
		reason StopReason
		want   bool
	}{
		{StopSubmitted, true},
		{StopMaxSteps, true},
		{StopBudget, true},
		{StopNoSubmit, true},
		{StopCanceled, true},
		{StopError, true},
		{StopReason(""), false},
		{StopReason("bogus"), false},
	}
	for _, tt := range tests {
		t.Run(string(tt.reason), func(t *testing.T) {
			if got := tt.reason.Valid(); got != tt.want {
				t.Errorf("StopReason(%q).Valid() = %v, want %v", tt.reason, got, tt.want)
			}
		})
	}
}

func TestLimitsWithDefaults(t *testing.T) {
	t.Run("all zero", func(t *testing.T) {
		got := Limits{}.WithDefaults()
		want := Limits{MaxSteps: 60, MaxToolOutputBytes: 32 << 10, MaxTokens: 4_000_000, MaxOutputTokensPerStep: 8192}
		if got != want {
			t.Fatalf("WithDefaults() = %+v, want %+v", got, want)
		}
	})

	t.Run("set fields untouched", func(t *testing.T) {
		got := Limits{MaxSteps: 5, MaxTokens: 10}.WithDefaults()
		if got.MaxSteps != 5 || got.MaxTokens != 10 {
			t.Fatalf("WithDefaults() = %+v, want set fields preserved", got)
		}
		if got.MaxToolOutputBytes != 32<<10 || got.MaxOutputTokensPerStep != 8192 {
			t.Fatalf("WithDefaults() = %+v, want zero fields filled", got)
		}
	})
}
