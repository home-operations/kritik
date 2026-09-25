package model

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// fakeGateway decodes each request as the gateway does, records it, and
// answers with resp encoded as the gateway does.
func fakeGateway(t *testing.T, resp StepResponse) (*httptest.Server, *[]StepRequest) {
	t.Helper()
	var got []StepRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		req, err := DecodeChatRequest(body)
		if err != nil {
			t.Errorf("DecodeChatRequest: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		got = append(got, req)
		out, err := EncodeChatResponse("gw-1", resp)
		if err != nil {
			t.Errorf("EncodeChatResponse: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func TestChatRoundTrip(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)
	sent := StepRequest{
		Model:  "review",
		System: "You are kritik.",
		Messages: []Message{
			{Role: RoleUser, Text: "Review this diff."},
			{Role: RoleAssistant, Text: "Reading two files.", ToolCalls: []ToolCall{
				{ID: "c1", Name: "read_file", Input: json.RawMessage(`{"path":"a.go"}`)},
				{ID: "c2", Name: "run", Input: json.RawMessage(`{"command":"rg","args":["x"]}`)},
			}},
			{Role: RoleUser, ToolResults: []ToolResult{
				{CallID: "c1", Content: "1\tpackage a"},
				{CallID: "c2", Content: "not allowed", IsError: true},
			}},
			{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c3", Name: "list_files", Input: json.RawMessage(`{}`)}}},
			{Role: RoleUser, ToolResults: []ToolResult{{CallID: "c3", Content: "a.go"}}, Text: "call submit_review"},
		},
		Tools:     []ToolDef{{Name: "read_file", Description: "Read a file.", InputSchema: schema}, {Name: "submit_review", InputSchema: schema}},
		MaxTokens: 8192,
	}
	answer := StepResponse{
		Text:      "Submitting.",
		ToolCalls: []ToolCall{{ID: "s1", Name: "submit_review", Input: json.RawMessage(`{"summary":{"take":"ok"}}`)}},
		Stop:      StopToolUse,
		Usage:     Usage{Input: 900, CacheRead: 4000, CacheWrite: 100, Output: 55},
		CostUSD:   0.0125,
		Model:     "openai/gpt-6-sol",
		Upstream:  "OpenAI",
	}
	for _, choice := range []ToolChoice{{Mode: ToolChoiceAuto}, {Mode: ToolChoiceRequired}, {Mode: ToolChoiceTool, Name: "submit_review"}} {
		t.Run(string(choice.Mode), func(t *testing.T) {
			srv, got := fakeGateway(t, answer)
			c, err := NewOpenAI(OpenAIConfig{BaseURL: srv.URL + "/v1", APIKey: "krk_token", ReportsModel: true})
			if err != nil {
				t.Fatal(err)
			}
			req := sent
			req.ToolChoice = choice
			resp, err := c.Step(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(resp, answer) {
				t.Fatalf("response = %+v\nwant %+v", resp, answer)
			}
			if len(*got) != 1 {
				t.Fatalf("requests = %d", len(*got))
			}
			decoded := (*got)[0]
			// The adapter re-encodes schemas; compare them as JSON.
			for i := range decoded.Tools {
				if !jsonEqual(t, decoded.Tools[i].InputSchema, req.Tools[i].InputSchema) {
					t.Fatalf("tool %d schema = %s", i, decoded.Tools[i].InputSchema)
				}
				decoded.Tools[i].InputSchema = req.Tools[i].InputSchema
			}
			if !reflect.DeepEqual(decoded, req) {
				t.Fatalf("decoded request = %+v\nwant %+v", decoded, req)
			}
		})
	}
}

func jsonEqual(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatal(err)
	}
	return reflect.DeepEqual(x, y)
}

func TestChatBudgetRefusal(t *testing.T) {
	for _, tt := range []struct {
		name, code string
		budget     bool
	}{
		{"the gateway's budget refusal", BudgetCode, true},
		{"a provider's rate limit", "rate_limit_exceeded", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := fakeProvider(t, http.StatusTooManyRequests, string(EncodeChatError(tt.code, "run budget of 1000 tokens spent")))
			c := newTestOpenAI(t, srv, false, nil)
			_, err := c.Step(t.Context(), StepRequest{Model: "review", Messages: []Message{{Role: RoleUser, Text: "hi"}}})
			if err == nil || errors.Is(err, ErrBudget) != tt.budget {
				t.Fatalf("err = %v, budget = %v", err, errors.Is(err, ErrBudget))
			}
			if tt.budget && !strings.Contains(err.Error(), "run budget of 1000 tokens spent") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestDecodeChatRequest(t *testing.T) {
	t.Run("content parts and several system messages", func(t *testing.T) {
		req, err := DecodeChatRequest([]byte(`{"model":"review","messages":[
			{"role":"system","content":"one"},{"role":"developer","content":[{"type":"text","text":"two"}]},
			{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}],"max_tokens":10}`))
		if err != nil {
			t.Fatal(err)
		}
		want := StepRequest{Model: "review", System: "one\n\ntwo", Messages: []Message{{Role: RoleUser, Text: "ab"}}, MaxTokens: 10}
		if !reflect.DeepEqual(req, want) {
			t.Fatalf("req = %+v", req)
		}
	})
	for _, tt := range []struct{ name, body, want string }{
		{"not JSON", `{`, "chat request"},
		{"unknown role", `{"messages":[{"role":"function","content":"x"}]}`, `role "function"`},
		{"image content", `{"messages":[{"role":"user","content":[{"type":"image_url"}]}]}`, `"image_url"`},
		{"tool choice none", `{"messages":[],"tool_choice":"none"}`, `tool_choice "none"`},
		{"tool choice without a name", `{"messages":[],"tool_choice":{"type":"function"}}`, "names no function"},
	} {
		t.Run("rejects "+tt.name, func(t *testing.T) {
			if _, err := DecodeChatRequest([]byte(tt.body)); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to mention %s", err, tt.want)
			}
		})
	}
}
