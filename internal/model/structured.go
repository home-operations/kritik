package model

import (
	"context"
	"fmt"
	"strings"
)

// Structured implements Completer on a Stepper by forcing a call to one tool
// named SchemaName whose input schema is the answer's; the call's input is
// the answer. Every provider supports forced tool calls, unlike response
// formats.
type Structured struct {
	Stepper Stepper
}

// Complete implements Completer.
func (s Structured) Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error) {
	resp, err := s.Stepper.Step(ctx, StepRequest{
		Model: req.Model, Fallbacks: req.Fallbacks, System: req.System,
		Messages:   []Message{{Role: RoleUser, Text: req.User}},
		Tools:      []ToolDef{{Name: req.SchemaName, InputSchema: req.Schema}},
		ToolChoice: ToolChoice{Mode: ToolChoiceTool, Name: req.SchemaName},
		MaxTokens:  req.MaxTokens,
	})
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
