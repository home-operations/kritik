package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

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

var testSubmitDef = model.ToolDef{
	Name:        "submit_review",
	Description: "submit the review",
	InputSchema: json.RawMessage(`{"type":"object"}`),
}

const validSubmitInput = `{"verdict":"approve"}`

func toolCall(id, name, input string) model.ToolCall {
	return model.ToolCall{ID: id, Name: name, Input: json.RawMessage(input)}
}

func TestRun_GrepReadSubmit(t *testing.T) {
	grep := &fakeTool{name: "grep", output: "widget.go:1: match"}
	read := &fakeTool{name: "read_file", output: "1\tpackage main"}
	stepper := &scriptedStepper{steps: []model.StepResponse{
		{ToolCalls: []model.ToolCall{toolCall("1", "grep", `{"pattern":"x"}`)}},
		{ToolCalls: []model.ToolCall{toolCall("2", "read_file", `{"path":"widget.go"}`)}},
		{ToolCalls: []model.ToolCall{toolCall("3", "submit_review", validSubmitInput)}},
	}}
	run := Run{Stepper: stepper, Tools: []Tool{grep, read}, Submit: testSubmitDef}

	result := run.Do(t.Context())

	if result.Stop != StopSubmitted {
		t.Fatalf("Stop = %v, want %v", result.Stop, StopSubmitted)
	}
	if string(result.Submitted) != validSubmitInput {
		t.Fatalf("Submitted = %s, want %s", result.Submitted, validSubmitInput)
	}
	if result.Steps != 3 {
		t.Fatalf("Steps = %d, want 3", result.Steps)
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

func TestRun_UnknownToolThenSubmit(t *testing.T) {
	stepper := &scriptedStepper{steps: []model.StepResponse{
		{ToolCalls: []model.ToolCall{toolCall("1", "does_not_exist", `{}`)}},
		{ToolCalls: []model.ToolCall{toolCall("2", "submit_review", validSubmitInput)}},
	}}
	run := Run{Stepper: stepper, Submit: testSubmitDef}

	result := run.Do(t.Context())

	if result.Stop != StopSubmitted {
		t.Fatalf("Stop = %v, want %v", result.Stop, StopSubmitted)
	}
	if result.ToolCalls["does_not_exist"] != 1 {
		t.Fatalf("ToolCalls = %v, want does_not_exist:1", result.ToolCalls)
	}
	// The unknown-tool step's error result must have reached the model as
	// the next turn's input, not silently vanished.
	last := stepper.calls[len(stepper.calls)-1]
	lastMsg := last.Messages[len(last.Messages)-1]
	if len(lastMsg.ToolResults) != 1 || !lastMsg.ToolResults[0].IsError {
		t.Fatalf("expected an error ToolResult for the unknown tool, got %+v", lastMsg.ToolResults)
	}
}

func TestRun_InvalidSubmitJSONThenValid(t *testing.T) {
	stepper := &scriptedStepper{steps: []model.StepResponse{
		{ToolCalls: []model.ToolCall{toolCall("1", "submit_review", `{not json`)}},
		{ToolCalls: []model.ToolCall{toolCall("2", "submit_review", validSubmitInput)}},
	}}
	run := Run{Stepper: stepper, Submit: testSubmitDef}

	result := run.Do(t.Context())

	if result.Stop != StopSubmitted {
		t.Fatalf("Stop = %v, want %v", result.Stop, StopSubmitted)
	}
	if string(result.Submitted) != validSubmitInput {
		t.Fatalf("Submitted = %s, want %s", result.Submitted, validSubmitInput)
	}
	if result.Steps != 2 {
		t.Fatalf("Steps = %d, want 2", result.Steps)
	}
}

func TestRun_ForcedInvalidSubmitStopsNoSubmit(t *testing.T) {
	// MaxSteps: 1 makes the only step the last step, which forces
	// submit_review; an invalid submit on a forced step must not get a
	// second chance.
	stepper := &scriptedStepper{steps: []model.StepResponse{
		{ToolCalls: []model.ToolCall{toolCall("1", "submit_review", `{not json`)}},
	}}
	run := Run{Stepper: stepper, Submit: testSubmitDef, Limits: Limits{MaxSteps: 1}}

	result := run.Do(t.Context())

	if result.Stop != StopNoSubmit {
		t.Fatalf("Stop = %v, want %v", result.Stop, StopNoSubmit)
	}
	if result.Submitted != nil {
		t.Fatalf("Submitted = %s, want nil", result.Submitted)
	}
	if len(stepper.calls) != 1 {
		t.Fatalf("expected exactly 1 call to the stepper, got %d", len(stepper.calls))
	}
}

func TestRun_TextOnlyTwiceStopsNoSubmit(t *testing.T) {
	var events []StepEvent
	stepper := &scriptedStepper{steps: []model.StepResponse{
		{Text: "thinking..."},
		{Text: "still thinking..."},
	}}
	run := Run{Stepper: stepper, Submit: testSubmitDef, OnStep: func(e StepEvent) { events = append(events, e) }}

	result := run.Do(t.Context())

	if result.Stop != StopNoSubmit {
		t.Fatalf("Stop = %v, want %v", result.Stop, StopNoSubmit)
	}
	if result.Steps != 2 {
		t.Fatalf("Steps = %d, want 2", result.Steps)
	}
	if len(events) != 2 || events[0].Index != 0 || events[1].Index != 1 {
		t.Fatalf("events = %+v", events)
	}
	// The nudge must have been sent as a user message after the first
	// text-only turn, or the second turn is not really giving the model a
	// chance to submit.
	last := stepper.calls[len(stepper.calls)-1]
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

func TestRun_BudgetForcesToolChoice(t *testing.T) {
	noop := &fakeTool{name: "noop", output: "ok"}
	stepper := &scriptedStepper{steps: []model.StepResponse{
		{ToolCalls: []model.ToolCall{toolCall("1", "noop", `{}`)}, Usage: model.Usage{Input: 80, Output: 10}},
		{ToolCalls: []model.ToolCall{toolCall("2", "submit_review", validSubmitInput)}},
	}}
	run := Run{Stepper: stepper, Tools: []Tool{noop}, Submit: testSubmitDef, Limits: Limits{MaxTokens: 100}}

	result := run.Do(t.Context())

	if result.Stop != StopSubmitted {
		t.Fatalf("Stop = %v, want %v", result.Stop, StopSubmitted)
	}
	if len(stepper.calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(stepper.calls))
	}
	forcedReq := stepper.calls[1]
	if forcedReq.ToolChoice.Mode != model.ToolChoiceTool || forcedReq.ToolChoice.Name != "submit_review" {
		t.Fatalf("ToolChoice = %+v, want a forced submit_review", forcedReq.ToolChoice)
	}
	// The first request must NOT have been forced: the budget only crosses
	// the 90%% threshold after step 0's usage lands.
	if stepper.calls[0].ToolChoice.Mode == model.ToolChoiceTool {
		t.Fatalf("first request was forced, want unforced: %+v", stepper.calls[0].ToolChoice)
	}
}

func TestRun_BudgetExhaustedStopsImmediately(t *testing.T) {
	noop := &fakeTool{name: "noop", output: "ok"}
	stepper := &scriptedStepper{steps: []model.StepResponse{
		{ToolCalls: []model.ToolCall{toolCall("1", "noop", `{}`)}, Usage: model.Usage{Input: 60}},
		{ToolCalls: []model.ToolCall{toolCall("2", "submit_review", validSubmitInput)}},
	}}
	run := Run{Stepper: stepper, Tools: []Tool{noop}, Submit: testSubmitDef, Limits: Limits{MaxTokens: 50}}

	result := run.Do(t.Context())

	if result.Stop != StopBudget {
		t.Fatalf("Stop = %v, want %v", result.Stop, StopBudget)
	}
	if result.Steps != 1 {
		t.Fatalf("Steps = %d, want 1", result.Steps)
	}
	if len(stepper.calls) != 1 {
		t.Fatalf("expected the loop to stop before a second call, got %d calls", len(stepper.calls))
	}
}

func TestRun_MaxStepsReachedWithoutSubmit(t *testing.T) {
	noop := &fakeTool{name: "noop", output: "ok"}
	stepper := &scriptedStepper{steps: []model.StepResponse{
		{ToolCalls: []model.ToolCall{toolCall("1", "noop", `{}`)}},
		{ToolCalls: []model.ToolCall{toolCall("2", "noop", `{}`)}},
	}}
	run := Run{Stepper: stepper, Tools: []Tool{noop}, Submit: testSubmitDef, Limits: Limits{MaxSteps: 2}}

	result := run.Do(t.Context())

	if result.Stop != StopMaxSteps {
		t.Fatalf("Stop = %v, want %v", result.Stop, StopMaxSteps)
	}
	if result.Steps != 2 {
		t.Fatalf("Steps = %d, want 2", result.Steps)
	}
	// The final step must still have been forced to offer submit_review,
	// even though the model chose not to take it.
	last := stepper.calls[len(stepper.calls)-1]
	if last.ToolChoice.Mode != model.ToolChoiceTool || last.ToolChoice.Name != "submit_review" {
		t.Fatalf("final ToolChoice = %+v, want a forced submit_review", last.ToolChoice)
	}
}

func TestRun_ContextCanceledBeforeFirstStep(t *testing.T) {
	stepper := &scriptedStepper{}
	run := Run{Stepper: stepper, Submit: testSubmitDef}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result := run.Do(ctx)

	if result.Stop != StopCanceled {
		t.Fatalf("Stop = %v, want %v", result.Stop, StopCanceled)
	}
	if result.Steps != 0 {
		t.Fatalf("Steps = %d, want 0", result.Steps)
	}
	if len(stepper.calls) != 0 {
		t.Fatalf("stepper was called %d times, want 0", len(stepper.calls))
	}
}

func TestRun_StepperError(t *testing.T) {
	run := Run{Stepper: &scriptedStepper{}, Submit: testSubmitDef}

	result := run.Do(t.Context())

	if result.Stop != StopError {
		t.Fatalf("Stop = %v, want %v", result.Stop, StopError)
	}
	if result.Err == "" {
		t.Fatal("Err is empty, want the stepper's error message")
	}
}

func TestRun_UsageSummedAcrossSteps(t *testing.T) {
	noop := &fakeTool{name: "noop", output: "ok"}
	stepper := &scriptedStepper{steps: []model.StepResponse{
		{ToolCalls: []model.ToolCall{toolCall("1", "noop", `{}`)}, Usage: model.Usage{Input: 100, Output: 10}, CostUSD: 0.01},
		{ToolCalls: []model.ToolCall{toolCall("2", "submit_review", validSubmitInput)}, Usage: model.Usage{Input: 50, Output: 5}, CostUSD: 0.02},
	}}
	run := Run{Stepper: stepper, Tools: []Tool{noop}, Submit: testSubmitDef}

	result := run.Do(t.Context())

	want := model.Usage{Input: 150, Output: 15}
	if result.Usage != want {
		t.Fatalf("Usage = %+v, want %+v", result.Usage, want)
	}
	if result.CostUSD != 0.03 {
		t.Fatalf("CostUSD = %v, want 0.03", result.CostUSD)
	}
}

func TestRun_ToolOutputTruncated(t *testing.T) {
	big := &fakeTool{name: "big", output: "abcdefghij"}
	stepper := &scriptedStepper{steps: []model.StepResponse{
		{ToolCalls: []model.ToolCall{toolCall("1", "big", `{}`)}},
		{ToolCalls: []model.ToolCall{toolCall("2", "submit_review", validSubmitInput)}},
	}}
	run := Run{Stepper: stepper, Tools: []Tool{big}, Submit: testSubmitDef, Limits: Limits{MaxToolOutputBytes: 5}}

	result := run.Do(t.Context())

	if result.Stop != StopSubmitted {
		t.Fatalf("Stop = %v, want %v", result.Stop, StopSubmitted)
	}
	last := stepper.calls[len(stepper.calls)-1]
	lastMsg := last.Messages[len(last.Messages)-1]
	if len(lastMsg.ToolResults) != 1 {
		t.Fatalf("ToolResults = %+v, want exactly 1", lastMsg.ToolResults)
	}
	content := lastMsg.ToolResults[0].Content
	if !strings.HasPrefix(content, "abcde") || !strings.Contains(content, "[truncated 5 bytes]") {
		t.Fatalf("content = %q, want a 5-byte prefix plus a truncation marker", content)
	}
}

func TestRun_SubmitEndsMultiCallTurnEarly(t *testing.T) {
	noop := &fakeTool{name: "noop", output: "ok"}
	noop2 := &fakeTool{name: "noop2", output: "ok"}
	stepper := &scriptedStepper{steps: []model.StepResponse{
		{ToolCalls: []model.ToolCall{
			toolCall("1", "noop", `{}`),
			toolCall("2", "submit_review", validSubmitInput),
			toolCall("3", "noop2", `{}`),
		}},
	}}
	run := Run{Stepper: stepper, Tools: []Tool{noop, noop2}, Submit: testSubmitDef}

	result := run.Do(t.Context())

	if result.Stop != StopSubmitted {
		t.Fatalf("Stop = %v, want %v", result.Stop, StopSubmitted)
	}
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
		if got := tt.reason.Valid(); got != tt.want {
			t.Errorf("StopReason(%q).Valid() = %v, want %v", tt.reason, got, tt.want)
		}
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
