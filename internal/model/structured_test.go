package model

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

// fakeStepper answers every Step with resp and records the request.
type fakeStepper struct {
	resp StepResponse
	err  error
	got  StepRequest
}

func (f *fakeStepper) Step(_ context.Context, req StepRequest) (StepResponse, error) {
	f.got = req
	return f.resp, f.err
}

func TestStructuredComplete(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"}}}`)
	req := CompletionRequest{
		System: "sys", User: "review this", Model: "acme/large", Fallbacks: []string{"acme/small"},
		Schema: schema, SchemaName: "findings", MaxTokens: 4096,
	}
	usage := Usage{Input: 100, CacheRead: 400, CacheWrite: 50, Output: 30}
	tests := []struct {
		name    string
		resp    StepResponse
		err     error
		want    CompletionResponse
		wantErr string
	}{
		{
			name: "forced tool input is the raw answer",
			resp: StepResponse{
				ToolCalls: []ToolCall{{ID: "c1", Name: "findings", Input: json.RawMessage(` {"summary":"ok"} `)}},
				Stop:      StopToolUse, Usage: usage, CostUSD: 0.01, Model: "acme/large", Upstream: "Acme",
			},
			want: CompletionResponse{
				Raw: `{"summary":"ok"}`, Model: "acme/large", Upstream: "Acme",
				InputTokens: 550, CachedTokens: 400, OutputTokens: 30, CostUSD: 0.01,
			},
		},
		{
			name:    "text only is an error",
			resp:    StepResponse{Text: "I think it is fine.", Stop: StopEndTurn, Model: "acme/large"},
			wantErr: "did not call findings",
		},
		{
			name:    "a call to another tool is an error",
			resp:    StepResponse{ToolCalls: []ToolCall{{ID: "c1", Name: "other", Input: json.RawMessage(`{}`)}}},
			wantErr: "did not call findings",
		},
		{
			name:    "stepper error is returned",
			err:     errors.New("boom"),
			wantErr: "boom",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeStepper{resp: tt.resp, err: tt.err}
			got, err := Structured{Stepper: f}.Complete(t.Context(), req)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want it to mention %q", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatalf("Complete: %v", err)
			} else if got != tt.want {
				t.Fatalf("resp = %+v, want %+v", got, tt.want)
			}

			s := f.got
			if s.Model != "acme/large" || !slices.Equal(s.Fallbacks, []string{"acme/small"}) || s.MaxTokens != 4096 || s.System != "sys" {
				t.Fatalf("step request = %+v; model, fallbacks, max tokens and system must pass through", s)
			}
			if len(s.Tools) != 1 || s.Tools[0].Name != "findings" || string(s.Tools[0].InputSchema) != string(schema) {
				t.Fatalf("tools = %+v; want the one schema tool", s.Tools)
			}
			if s.ToolChoice != (ToolChoice{Mode: ToolChoiceTool, Name: "findings"}) {
				t.Fatalf("tool choice = %+v", s.ToolChoice)
			}
			if len(s.Messages) != 1 || s.Messages[0].Role != RoleUser || s.Messages[0].Text != "review this" {
				t.Fatalf("messages = %+v; want the single user message", s.Messages)
			}
		})
	}
}
