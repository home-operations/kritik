package model

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func anthropicMessage(content, stop, usage string) string {
	return `{"id":"msg_1","type":"message","role":"assistant","model":"acme-large","content":` + content +
		`,"stop_reason":"` + stop + `","stop_sequence":null,"usage":` + usage + `}`
}

const anthropicUsage = `{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":400,"cache_creation_input_tokens":50}`

func newTestAnthropic(t *testing.T, srv *httptest.Server, pricing Pricing) *Anthropic {
	t.Helper()
	c, err := NewAnthropic(AnthropicConfig{BaseURL: srv.URL + "/gw/", APIKey: "test-key", Pricing: pricing})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestAnthropicStepResponses(t *testing.T) {
	tests := []struct {
		name      string
		pricing   Pricing
		body      string
		wantText  string
		wantCalls []ToolCall
		wantStop  StopReason
		wantUsage Usage
		wantCost  float64
	}{
		{
			name:      "text",
			body:      anthropicMessage(`[{"type":"text","text":"hello"}]`, "end_turn", anthropicUsage),
			wantText:  "hello",
			wantStop:  StopEndTurn,
			wantUsage: Usage{Input: 100, CacheRead: 400, CacheWrite: 50, Output: 20},
		},
		{
			name: "tool use becomes a tool call",
			body: anthropicMessage(`[{"type":"text","text":"reading"},{"type":"tool_use","id":"tu_1","name":"read_file","input":{"path":"a.go"}}]`,
				"tool_use", anthropicUsage),
			wantText:  "reading",
			wantCalls: []ToolCall{{ID: "tu_1", Name: "read_file", Input: json.RawMessage(`{"path":"a.go"}`)}},
			wantStop:  StopToolUse,
			wantUsage: Usage{Input: 100, CacheRead: 400, CacheWrite: 50, Output: 20},
		},
		{
			name:      "max tokens",
			body:      anthropicMessage(`[{"type":"text","text":"trunc"}]`, "max_tokens", anthropicUsage),
			wantText:  "trunc",
			wantStop:  StopMaxTokens,
			wantUsage: Usage{Input: 100, CacheRead: 400, CacheWrite: 50, Output: 20},
		},
		{
			name:      "cost computed from pricing",
			pricing:   Pricing{"acme-large": {Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75}},
			body:      anthropicMessage(`[{"type":"text","text":"ok"}]`, "end_turn", `{"input_tokens":1000000,"output_tokens":100000,"cache_read_input_tokens":500000,"cache_creation_input_tokens":200000}`),
			wantText:  "ok",
			wantStop:  StopEndTurn,
			wantUsage: Usage{Input: 1_000_000, CacheRead: 500_000, CacheWrite: 200_000, Output: 100_000},
			wantCost:  3 + 1.5 + 0.15 + 0.75,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, got := fakeProvider(t, http.StatusOK, tt.body)
			c := newTestAnthropic(t, srv, tt.pricing)
			resp, err := c.Step(t.Context(), StepRequest{
				Model: "acme-large", System: "sys", Messages: []Message{{Role: RoleUser, Text: "hi"}}, MaxTokens: 500,
			})
			if err != nil {
				t.Fatalf("Step: %v", err)
			}
			if resp.Text != tt.wantText || resp.Stop != tt.wantStop || resp.Usage != tt.wantUsage || resp.Model != "acme-large" {
				t.Fatalf("resp = %+v", resp)
			}
			if math.Abs(resp.CostUSD-tt.wantCost) > 1e-9 {
				t.Fatalf("cost = %v, want %v", resp.CostUSD, tt.wantCost)
			}
			if len(resp.ToolCalls) != len(tt.wantCalls) {
				t.Fatalf("tool calls = %+v", resp.ToolCalls)
			}
			for i, c := range tt.wantCalls {
				g := resp.ToolCalls[i]
				if g.ID != c.ID || g.Name != c.Name || string(g.Input) != string(c.Input) {
					t.Fatalf("tool call %d = %+v, want %+v", i, g, c)
				}
			}
			if got.path != "/gw/v1/messages" {
				t.Fatalf("path = %q; the base URL prefix must be kept", got.path)
			}
			if got.header.Get("X-Api-Key") != "test-key" {
				t.Fatalf("x-api-key = %q", got.header.Get("X-Api-Key"))
			}
			if field(got.body, "model") != "acme-large" || field(got.body, "max_tokens") != float64(500) {
				t.Fatalf("request = %v", got.body)
			}
		})
	}
}

func TestAnthropicRequest(t *testing.T) {
	srv, got := fakeProvider(t, http.StatusOK, anthropicMessage(`[{"type":"text","text":"ok"}]`, "end_turn", anthropicUsage))
	c := newTestAnthropic(t, srv, nil)
	_, err := c.Step(t.Context(), StepRequest{
		Model: "acme-large", System: "be terse",
		Messages: []Message{
			{Role: RoleUser, Text: "review"},
			{Role: RoleAssistant, Text: "looking", ToolCalls: []ToolCall{{ID: "tu_1", Name: "read_file", Input: json.RawMessage(`{"path":"a.go"}`)}}},
			{Role: RoleUser, ToolResults: []ToolResult{{CallID: "tu_1", Content: "package a"}}, Text: "keep going"},
		},
		Tools: []ToolDef{{Name: "read_file", Description: "Read a file.", InputSchema: json.RawMessage(
			`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`)}},
		ToolChoice: ToolChoice{Mode: ToolChoiceTool, Name: "read_file"},
	})
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	b := got.body

	t.Run("system is cached", func(t *testing.T) {
		if field(b, "system", 0, "text") != "be terse" || field(b, "system", 0, "cache_control", "type") != "ephemeral" {
			t.Fatalf("system = %v", b["system"])
		}
	})
	t.Run("only the last block of the last message is a breakpoint", func(t *testing.T) {
		msgs, _ := b["messages"].([]any)
		if len(msgs) != 3 {
			t.Fatalf("messages = %v", msgs)
		}
		for i, m := range msgs {
			blocks, _ := field(m, "content").([]any)
			for j, blk := range blocks {
				cc := field(blk, "cache_control", "type")
				last := i == len(msgs)-1 && j == len(blocks)-1
				if last != (cc == "ephemeral") {
					t.Fatalf("message %d block %d cache_control = %v", i, j, cc)
				}
			}
		}
	})
	t.Run("assistant tool use is replayed", func(t *testing.T) {
		if field(b, "messages", 1, "role") != "assistant" || field(b, "messages", 1, "content", 0, "text") != "looking" ||
			field(b, "messages", 1, "content", 1, "type") != "tool_use" || field(b, "messages", 1, "content", 1, "id") != "tu_1" ||
			field(b, "messages", 1, "content", 1, "input", "path") != "a.go" {
			t.Fatalf("assistant = %v", field(b, "messages", 1))
		}
	})
	t.Run("tool results come before text", func(t *testing.T) {
		if field(b, "messages", 2, "content", 0, "type") != "tool_result" || field(b, "messages", 2, "content", 0, "tool_use_id") != "tu_1" ||
			field(b, "messages", 2, "content", 0, "content", 0, "text") != "package a" ||
			field(b, "messages", 2, "content", 1, "text") != "keep going" {
			t.Fatalf("user = %v", field(b, "messages", 2))
		}
	})
	t.Run("tool choice names the tool", func(t *testing.T) {
		if field(b, "tool_choice", "type") != "tool" || field(b, "tool_choice", "name") != "read_file" {
			t.Fatalf("tool_choice = %v", b["tool_choice"])
		}
	})
	t.Run("tool schema is passed through", func(t *testing.T) {
		if field(b, "tools", 0, "name") != "read_file" || field(b, "tools", 0, "description") != "Read a file." ||
			field(b, "tools", 0, "input_schema", "type") != "object" ||
			field(b, "tools", 0, "input_schema", "properties", "path", "type") != "string" ||
			field(b, "tools", 0, "input_schema", "required", 0) != "path" ||
			field(b, "tools", 0, "input_schema", "additionalProperties") != false {
			t.Fatalf("tools = %v", b["tools"])
		}
	})
}

func TestAnthropicToolChoice(t *testing.T) {
	tests := []struct {
		name   string
		choice ToolChoice
		want   string
	}{
		{"zero value is auto", ToolChoice{}, "auto"},
		{"auto", ToolChoice{Mode: ToolChoiceAuto}, "auto"},
		{"required is any", ToolChoice{Mode: ToolChoiceRequired}, "any"},
		{"named", ToolChoice{Mode: ToolChoiceTool, Name: "x"}, "tool"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, got := fakeProvider(t, http.StatusOK, anthropicMessage(`[{"type":"text","text":"ok"}]`, "end_turn", anthropicUsage))
			c := newTestAnthropic(t, srv, nil)
			_, err := c.Step(t.Context(), StepRequest{
				Model: "m", Messages: []Message{{Role: RoleUser, Text: "hi"}}, ToolChoice: tt.choice,
				Tools: []ToolDef{{Name: "x", InputSchema: json.RawMessage(`{"type":"object"}`)}},
			})
			if err != nil {
				t.Fatalf("Step: %v", err)
			}
			if field(got.body, "tool_choice", "type") != tt.want {
				t.Fatalf("tool_choice = %v", got.body["tool_choice"])
			}
		})
	}
}

func TestAnthropicErrors(t *testing.T) {
	for _, status := range []int{529, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv, _ := fakeProvider(t, status, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
			c := newTestAnthropic(t, srv, nil)
			_, err := c.Step(t.Context(), StepRequest{Model: "acme-large", Messages: []Message{{Role: RoleUser, Text: "hi"}}})
			if err == nil || !strings.Contains(err.Error(), "model:") || !strings.Contains(err.Error(), "acme-large") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestNewStepper(t *testing.T) {
	tests := []struct {
		name    string
		typ     ProviderType
		baseURL string
		wantErr bool
	}{
		{"openrouter default url", ProviderOpenRouter, "", false},
		{"openai", ProviderOpenAI, "https://gw.example.com/v1", false},
		{"openai default url", ProviderOpenAI, "", false},
		{"anthropic", ProviderAnthropic, "", false},
		{"unknown", ProviderType("cohere"), "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := NewStepper(tt.typ, tt.baseURL, "k", nil, nil)
			if (err != nil) != tt.wantErr || (err == nil && s == nil) {
				t.Fatalf("NewStepper = %v, %v", s, err)
			}
		})
	}
}
