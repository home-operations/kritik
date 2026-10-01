package model

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Structured implements Completer on a Stepper by offering one tool named
// SchemaName whose input schema is the answer's and telling the model to
// answer by calling it; the call's input is the answer. The call is asked
// for in the prompt rather than forced through tool_choice, which the
// newest models reject.
type Structured struct {
	Stepper Stepper
	// OnStep, when set, is called after every Step Complete makes, with the
	// step's request, its response or error, and how long it took, so the
	// caller can record the call.
	OnStep func(req StepRequest, resp StepResponse, err error, d time.Duration)
}

// answerWith ends the user message with the call the answer is.
func answerWith(name string) string {
	return fmt.Sprintf("Answer by calling the %s tool, and call nothing else; its input is your answer.", name)
}

// Complete implements Completer.
func (s Structured) Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error) {
	step := StepRequest{
		Model: req.Model, Fallbacks: req.Fallbacks, System: req.System,
		Messages:  []Message{{Role: RoleUser, Text: req.User + "\n\n" + answerWith(req.SchemaName)}},
		Tools:     []ToolDef{{Name: req.SchemaName, InputSchema: req.Schema}},
		MaxTokens: req.MaxTokens,
	}
	start := time.Now()
	resp, err := s.Stepper.Step(ctx, step)
	if s.OnStep != nil {
		s.OnStep(step, resp, err, time.Since(start))
	}
	if err != nil {
		return CompletionResponse{}, err
	}
	for _, c := range resp.ToolCalls {
		if c.Name == req.SchemaName {
			return CompletionResponse{
				Raw: strings.TrimSpace(string(c.Input)), Model: resp.Model, Upstream: resp.Upstream,
				InputTokens: resp.Usage.Prompt(), CachedTokens: resp.Usage.CacheRead, OutputTokens: resp.Usage.Output,
				CostUSD: resp.CostUSD,
			}, nil
		}
	}
	return CompletionResponse{}, fmt.Errorf("model: %s did not call %s (stop %s)", resp.Model, req.SchemaName, resp.Stop)
}
