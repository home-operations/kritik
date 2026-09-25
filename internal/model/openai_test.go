package model

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// capture is one request a fake provider received.
type capture struct {
	path   string
	header http.Header
	body   map[string]any
}

// fakeProvider answers every request with status and body and records what
// it was sent, so an adapter's wire behaviour is checked without a network.
func fakeProvider(t *testing.T, status int, body string) (*httptest.Server, *capture) {
	t.Helper()
	got := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path, got.header = r.URL.Path, r.Header.Clone()
		raw, _ := io.ReadAll(r.Body)
		got.body = nil
		_ = json.Unmarshal(raw, &got.body)
		w.Header().Set("Content-Type", "application/json")
		// Keep the SDKs from retrying the error cases.
		w.Header().Set("x-should-retry", "false")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func chatCompletion(message, finish, usage, extra string) string {
	return `{"id":"x","object":"chat.completion","created":1,"model":"acme/large",` + extra +
		`"choices":[{"index":0,"message":` + message + `,"finish_reason":"` + finish + `"}],"usage":` + usage + `}`
}

const plainUsage = `{"prompt_tokens":120,"completion_tokens":30,"total_tokens":150}`

func newTestOpenAI(t *testing.T, srv *httptest.Server, openRouter bool, pricing Pricing) *OpenAI {
	t.Helper()
	c, err := NewOpenAI(OpenAIConfig{BaseURL: srv.URL + "/gw/v1", APIKey: "test-key", OpenRouter: openRouter, Pricing: pricing})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// field walks a decoded JSON body by keys and indexes.
func field(v any, path ...any) any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, _ := v.(map[string]any)
			v = m[k]
		case int:
			a, _ := v.([]any)
			if k >= len(a) {
				return nil
			}
			v = a[k]
		}
	}
	return v
}

func TestOpenAIStepResponses(t *testing.T) {
	tests := []struct {
		name       string
		openRouter bool
		pricing    Pricing
		body       string
		wantText   string
		wantCalls  []ToolCall
		wantStop   StopReason
		wantUsage  Usage
		wantCost   float64
		wantUp     string
	}{
		{
			name:      "plain text",
			body:      chatCompletion(`{"role":"assistant","content":"hello"}`, "stop", plainUsage, ""),
			wantText:  "hello",
			wantStop:  StopEndTurn,
			wantUsage: Usage{Input: 120, Output: 30},
		},
		{
			name: "tool call with raw JSON input",
			body: chatCompletion(`{"role":"assistant","content":null,"tool_calls":[
			  {"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"main.go\"}"}}]}`, "tool_calls", plainUsage, ""),
			wantCalls: []ToolCall{{ID: "call_1", Name: "read_file", Input: json.RawMessage(`{"path":"main.go"}`)}},
			wantStop:  StopToolUse,
			wantUsage: Usage{Input: 120, Output: 30},
		},
		{
			name:      "length is max tokens",
			body:      chatCompletion(`{"role":"assistant","content":"trunc"}`, "length", plainUsage, ""),
			wantText:  "trunc",
			wantStop:  StopMaxTokens,
			wantUsage: Usage{Input: 120, Output: 30},
		},
		{
			name: "cached tokens split out of the prompt",
			body: chatCompletion(`{"role":"assistant","content":"ok"}`, "stop",
				`{"prompt_tokens":700,"completion_tokens":59,"total_tokens":759,"prompt_tokens_details":{"cached_tokens":500}}`, ""),
			wantText:  "ok",
			wantStop:  StopEndTurn,
			wantUsage: Usage{Input: 200, CacheRead: 500, Output: 59},
		},
		{
			name:       "openrouter reports cost and upstream",
			openRouter: true,
			pricing:    Pricing{"acme/large": {Input: 1000}},
			body: chatCompletion(`{"role":"assistant","content":"ok"}`, "stop",
				`{"prompt_tokens":700,"completion_tokens":59,"total_tokens":759,"cost":0.0123,"prompt_tokens_details":{"cached_tokens":500}}`,
				`"provider":"Anthropic",`),
			wantText:  "ok",
			wantStop:  StopEndTurn,
			wantUsage: Usage{Input: 200, CacheRead: 500, Output: 59},
			wantCost:  0.0123,
			wantUp:    "Anthropic",
		},
		{
			name:      "cost computed from pricing when none is reported",
			pricing:   Pricing{"acme/large": {Input: 2, Output: 10}},
			body:      chatCompletion(`{"role":"assistant","content":"ok"}`, "stop", `{"prompt_tokens":1000000,"completion_tokens":100000,"total_tokens":1100000}`, ""),
			wantText:  "ok",
			wantStop:  StopEndTurn,
			wantUsage: Usage{Input: 1_000_000, Output: 100_000},
			wantCost:  3,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, got := fakeProvider(t, http.StatusOK, tt.body)
			c := newTestOpenAI(t, srv, tt.openRouter, tt.pricing)
			resp, err := c.Step(t.Context(), StepRequest{
				Model: "acme/large", System: "sys", Messages: []Message{{Role: RoleUser, Text: "hi"}}, MaxTokens: 500,
			})
			if err != nil {
				t.Fatalf("Step: %v", err)
			}
			if resp.Text != tt.wantText || resp.Stop != tt.wantStop || resp.Usage != tt.wantUsage || resp.Upstream != tt.wantUp {
				t.Fatalf("resp = %+v", resp)
			}
			if math.Abs(resp.CostUSD-tt.wantCost) > 1e-9 {
				t.Fatalf("cost = %v, want %v", resp.CostUSD, tt.wantCost)
			}
			if resp.Model != "acme/large" {
				t.Fatalf("model = %q", resp.Model)
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
			if got.path != "/gw/v1/chat/completions" {
				t.Fatalf("path = %q; the base URL prefix must be kept", got.path)
			}
			if got.header.Get("Authorization") != "Bearer test-key" {
				t.Fatalf("authorization = %q", got.header.Get("Authorization"))
			}
			if field(got.body, "model") != "acme/large" || field(got.body, "messages", 0, "role") != "system" ||
				field(got.body, "messages", 1, "content") != "hi" {
				t.Fatalf("request = %v", got.body)
			}
		})
	}
}

func TestOpenAIToolChoice(t *testing.T) {
	tests := []struct {
		name   string
		choice ToolChoice
		want   string
	}{
		{"zero value is auto", ToolChoice{}, `"auto"`},
		{"auto", ToolChoice{Mode: ToolChoiceAuto}, `"auto"`},
		{"required", ToolChoice{Mode: ToolChoiceRequired}, `"required"`},
		{"named tool", ToolChoice{Mode: ToolChoiceTool, Name: "submit_review"}, `{"function":{"name":"submit_review"},"type":"function"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, got := fakeProvider(t, http.StatusOK, chatCompletion(`{"role":"assistant","content":"ok"}`, "stop", plainUsage, ""))
			c := newTestOpenAI(t, srv, false, nil)
			_, err := c.Step(t.Context(), StepRequest{
				Model: "m", Messages: []Message{{Role: RoleUser, Text: "hi"}}, ToolChoice: tt.choice,
				Tools: []ToolDef{{Name: "submit_review", Description: "Submit.", InputSchema: json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"}}}`)}},
			})
			if err != nil {
				t.Fatalf("Step: %v", err)
			}
			choice, _ := json.Marshal(got.body["tool_choice"])
			if string(choice) != tt.want {
				t.Fatalf("tool_choice = %s, want %s", choice, tt.want)
			}
			if field(got.body, "tools", 0, "type") != "function" || field(got.body, "tools", 0, "function", "name") != "submit_review" ||
				field(got.body, "tools", 0, "function", "description") != "Submit." ||
				field(got.body, "tools", 0, "function", "parameters", "properties", "a", "type") != "string" {
				t.Fatalf("tools = %v", got.body["tools"])
			}
		})
	}
	t.Run("named tool without a name is rejected", func(t *testing.T) {
		srv, _ := fakeProvider(t, http.StatusOK, "{}")
		c := newTestOpenAI(t, srv, false, nil)
		_, err := c.Step(t.Context(), StepRequest{Model: "m", Messages: []Message{{Role: RoleUser, Text: "hi"}}, ToolChoice: ToolChoice{Mode: ToolChoiceTool}})
		if err == nil {
			t.Fatal("expected an error")
		}
	})
}

func TestOpenAIConversation(t *testing.T) {
	srv, got := fakeProvider(t, http.StatusOK, chatCompletion(`{"role":"assistant","content":"done"}`, "stop", plainUsage, ""))
	c := newTestOpenAI(t, srv, false, nil)
	_, err := c.Step(t.Context(), StepRequest{
		Model: "m",
		Messages: []Message{
			{Role: RoleUser, Text: "review"},
			{Role: RoleAssistant, Text: "looking", ToolCalls: []ToolCall{
				{ID: "c1", Name: "read_file", Input: json.RawMessage(`{"path":"a.go"}`)},
				{ID: "c2", Name: "grep", Input: json.RawMessage(`{"pattern":"x"}`)},
			}},
			{Role: RoleUser, ToolResults: []ToolResult{{CallID: "c1", Content: "package a"}, {CallID: "c2", Content: "no such pattern", IsError: true}}},
		},
	})
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	msgs, _ := got.body["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages = %v; want user, assistant and one tool message per result", msgs)
	}
	if field(msgs, 1, "role") != "assistant" || field(msgs, 1, "content") != "looking" ||
		field(msgs, 1, "tool_calls", 0, "id") != "c1" || field(msgs, 1, "tool_calls", 0, "type") != "function" ||
		field(msgs, 1, "tool_calls", 0, "function", "name") != "read_file" ||
		field(msgs, 1, "tool_calls", 0, "function", "arguments") != `{"path":"a.go"}` ||
		field(msgs, 1, "tool_calls", 1, "id") != "c2" {
		t.Fatalf("assistant message = %v", msgs[1])
	}
	for i, want := range []struct{ id, content string }{{"c1", "package a"}, {"c2", "no such pattern"}} {
		m := msgs[2+i]
		content, _ := field(m, "content").(string)
		if field(m, "role") != "tool" || field(m, "tool_call_id") != want.id || !strings.Contains(content, want.content) {
			t.Fatalf("tool message %d = %v", i, m)
		}
	}
	if content, _ := field(msgs, 3, "content").(string); !strings.HasPrefix(strings.ToLower(content), "error") {
		t.Fatalf("an error result must say so, got %q", content)
	}
}

func TestOpenAIFallbacks(t *testing.T) {
	body := chatCompletion(`{"role":"assistant","content":"ok"}`, "stop", plainUsage, "")
	tests := []struct {
		name       string
		openRouter bool
		fallbacks  []string
		want       []any // nil means the models key must be absent
	}{
		{"openrouter with fallbacks", true, []string{"acme/small"}, []any{"acme/large", "acme/small"}},
		{"openrouter without fallbacks", true, nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, got := fakeProvider(t, http.StatusOK, body)
			c := newTestOpenAI(t, srv, tt.openRouter, nil)
			if _, err := c.Step(t.Context(), StepRequest{Model: "acme/large", Fallbacks: tt.fallbacks, Messages: []Message{{Role: RoleUser, Text: "hi"}}}); err != nil {
				t.Fatalf("Step: %v", err)
			}
			models, present := got.body["models"]
			if tt.want == nil {
				if present {
					t.Fatalf("models = %v; want none without fallbacks", models)
				}
				return
			}
			b1, _ := json.Marshal(models)
			b2, _ := json.Marshal(tt.want)
			if string(b1) != string(b2) {
				t.Fatalf("models = %s, want %s", b1, b2)
			}
		})
	}

	t.Run("plain openai walks fallbacks itself", func(t *testing.T) {
		var seen []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				Model string `json:"model"`
			}
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &req)
			seen = append(seen, req.Model)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("x-should-retry", "false")
			if req.Model == "acme/large" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"message":"no such model"}}`))
				return
			}
			_, _ = w.Write([]byte(body))
		}))
		t.Cleanup(srv.Close)
		c := newTestOpenAI(t, srv, false, nil)
		resp, err := c.Step(t.Context(), StepRequest{Model: "acme/large", Fallbacks: []string{"acme/small"}, Messages: []Message{{Role: RoleUser, Text: "hi"}}})
		if err != nil {
			t.Fatalf("Step: %v", err)
		}
		if resp.Model != "acme/small" || len(seen) != 2 || seen[1] != "acme/small" {
			t.Fatalf("model = %q, seen = %v; want the fallback to answer", resp.Model, seen)
		}
	})
}

func TestOpenAIAnsweringModel(t *testing.T) {
	answeredBy := func(model string) string {
		return strings.Replace(chatCompletion(`{"role":"assistant","content":"ok"}`, "stop", plainUsage, ""),
			`"model":"acme/large"`, `"model":"`+model+`"`, 1)
	}
	tests := []struct {
		name       string
		openRouter bool
		body       string
		want       string
	}{
		{name: "openrouter fell back server-side", openRouter: true, body: answeredBy("acme/small"), want: "acme/small"},
		{name: "openrouter without a model field", openRouter: true,
			body: strings.Replace(answeredBy("x"), `"model":"x",`, "", 1), want: "acme/large"},
		{name: "plain openai names a snapshot", body: answeredBy("acme/large-2026-01-01"), want: "acme/large"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := fakeProvider(t, http.StatusOK, tt.body)
			c := newTestOpenAI(t, srv, tt.openRouter, nil)
			resp, err := c.Step(t.Context(), StepRequest{Model: "acme/large", Fallbacks: []string{"acme/small"},
				Messages: []Message{{Role: RoleUser, Text: "hi"}}})
			if err != nil {
				t.Fatalf("Step: %v", err)
			}
			if resp.Model != tt.want {
				t.Fatalf("model = %q, want %q", resp.Model, tt.want)
			}
		})
	}
}

func TestOpenAIErrors(t *testing.T) {
	srv, _ := fakeProvider(t, http.StatusTooManyRequests, `{"error":{"message":"rate limited"}}`)
	c := newTestOpenAI(t, srv, false, nil)
	_, err := c.Step(t.Context(), StepRequest{Model: "acme/large", Messages: []Message{{Role: RoleUser, Text: "hi"}}})
	if err == nil || !strings.Contains(err.Error(), "model:") || !strings.Contains(err.Error(), "acme/large") {
		t.Fatalf("err = %v", err)
	}
}

func TestNewOpenAIRejects(t *testing.T) {
	tests := []struct {
		name string
		cfg  OpenAIConfig
	}{
		{"no key", OpenAIConfig{BaseURL: "https://gw.example.com/v1"}},
		{"relative base URL", OpenAIConfig{BaseURL: "gw.example.com/v1", APIKey: "k"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewOpenAI(tt.cfg); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}
