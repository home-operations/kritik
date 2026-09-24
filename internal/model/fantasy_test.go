package model

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"charm.land/fantasy/schema"
)

// fakeOpenAI answers /chat/completions with a fixed JSON object and records
// the request, so the adapter's wire behaviour is checked without a network.
func fakeOpenAI(t *testing.T, answer string) (*httptest.Server, *map[string]any) {
	t.Helper()
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-key" || r.Header.Get("X-Title") != "kritik" {
			t.Errorf("headers = %v", r.Header)
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.Header().Set("Content-Type", "application/json")
		// Fantasy asks OpenAI-compatible servers for objects through a forced
		// tool call named after the schema, so that is what the fake returns.
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","created":1,"model":"test/model",
		  "choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[
		    {"id":"call_1","type":"function","function":{"name":"review","arguments":` + quote(answer) + `}}]},"finish_reason":"tool_calls"}],
		  "usage":{"prompt_tokens":120,"completion_tokens":30,"total_tokens":150}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestFantasyCompleterStructuredOutput(t *testing.T) {
	answer := `{"summary":"ok","findings":[]}`
	srv, got := fakeOpenAI(t, answer)
	c, err := NewOpenAICompatible("test", srv.URL+"/v1", "test-key")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Complete(t.Context(), CompletionRequest{
		System: "sys", User: "user", Model: "test/model", SchemaName: "review", MaxTokens: 500,
		Schema: schema.Schema{Type: "object", Properties: map[string]*schema.Schema{"summary": {Type: "string"}, "findings": {Type: "array", Items: &schema.Schema{Type: "object"}}}, Required: []string{"summary", "findings"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Raw != answer || resp.Model != "test/model" || resp.InputTokens != 120 || resp.OutputTokens != 30 {
		t.Fatalf("resp = %+v", resp)
	}
	if (*got)["model"] != "test/model" {
		t.Fatalf("request model = %v", (*got)["model"])
	}
	msgs := (*got)["messages"].([]any)
	if len(msgs) != 2 || msgs[0].(map[string]any)["role"] != "system" || !strings.Contains(msgs[1].(map[string]any)["content"].(string), "user") {
		t.Fatalf("messages = %v", msgs)
	}
	tools, _ := (*got)["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["function"].(map[string]any)["name"] != "review" || (*got)["tool_choice"] == nil {
		t.Fatalf("tools = %v tool_choice = %v; want one forced tool named after the schema", tools, (*got)["tool_choice"])
	}
	if (*got)["max_tokens"] != float64(500) && (*got)["max_completion_tokens"] != float64(500) {
		t.Fatalf("max tokens not sent: %v", *got)
	}
	// The second call must reuse the cached language model.
	if _, err := c.Complete(t.Context(), CompletionRequest{System: "s", User: "u", Model: "test/model", SchemaName: "review", Schema: schema.Schema{Type: "object"}}); err != nil {
		t.Fatal(err)
	}
	if len(c.models) != 1 {
		t.Fatalf("cached models = %d", len(c.models))
	}
}

// rewriteTo sends every request to the test server, whatever host the
// provider dialled.
type rewriteTo struct{ target *url.URL }

func (r rewriteTo) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme, req.URL.Host = r.target.Scheme, r.target.Host
	return http.DefaultTransport.RoundTrip(req)
}

func TestOpenRouterCompleterReadsCostAndFallbacks(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","created":1,"model":"openai/gpt-6-sol","provider":"OpenAI",
		  "choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[
		    {"id":"call_1","type":"function","function":{"name":"review","arguments":"{\"summary\":\"ok\"}"}}]},"finish_reason":"tool_calls"}],
		  "usage":{"prompt_tokens":700,"completion_tokens":59,"total_tokens":759,"cost":0.0019,"prompt_tokens_details":{"cached_tokens":500}}}`))
	}))
	defer srv.Close()
	target, _ := url.Parse(srv.URL)
	c, err := NewOpenRouter("test-key", &http.Client{Transport: rewriteTo{target: target}})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Complete(t.Context(), CompletionRequest{
		System: "s", User: "u", Model: "openai/gpt-6-sol", Fallbacks: []string{"anthropic/claude-sonnet-5"},
		SchemaName: "review", Schema: schema.Schema{Type: "object", Properties: map[string]*schema.Schema{"summary": {Type: "string"}}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Upstream != "OpenAI" || resp.CostUSD != 0.0019 || resp.InputTokens != 700 || resp.CachedTokens != 500 || resp.OutputTokens != 59 {
		t.Fatalf("resp = %+v; input must include the cached part", resp)
	}
	models, _ := got["models"].([]any)
	if len(models) != 2 || models[0] != "openai/gpt-6-sol" || models[1] != "anthropic/claude-sonnet-5" {
		t.Fatalf("models = %v; want the primary then the fallback", got["models"])
	}
	if usage, _ := got["usage"].(map[string]any); usage["include"] != true {
		t.Fatalf("usage = %v; want usage accounting requested", got["usage"])
	}
}

func TestFantasyCompleterSurfacesErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"rate limited"}}`, http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c, _ := NewOpenAICompatible("test", srv.URL+"/v1", "k")
	_, err := c.Complete(t.Context(), CompletionRequest{System: "s", User: "u", Model: "m", SchemaName: "r", Schema: schema.Schema{Type: "object"}})
	if err == nil || !strings.Contains(err.Error(), "model: test/m") {
		t.Fatalf("err = %v", err)
	}
}

func TestOpenAIEmbedderBatchesAndChecksDims(t *testing.T) {
	var batches [][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input      []string `json:"input"`
			Dimensions int      `json:"dimensions"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		batches = append(batches, req.Input)
		data := make([]string, 0, len(req.Input))
		for i := range req.Input {
			data = append(data, `{"object":"embedding","index":`+strconvInt(i)+`,"embedding":[0.1,0.2,0.3]}`)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","model":"emb","data":[` + strings.Join(data, ",") + `],"usage":{"prompt_tokens":1,"total_tokens":1}}`))
	}))
	defer srv.Close()
	e := NewOpenAIEmbedder(srv.URL, "k", "emb", 3)
	e.MaxBatch = 2
	e.MaxItemChars = 5
	vecs, tokens, err := e.Embed(t.Context(), []string{"a", "b", "c", "dddddddddd"})
	if err != nil {
		t.Fatal(err)
	}
	if tokens != 2 {
		t.Fatalf("tokens = %d, want the two batches' usage summed", tokens)
	}
	if len(vecs) != 4 || len(vecs[0]) != 3 || len(batches) != 2 || len(batches[0]) != 2 || batches[1][1] != "ddddd" {
		t.Fatalf("vecs=%d batches=%v", len(vecs), batches)
	}
	wrong := NewOpenAIEmbedder(srv.URL, "k", "emb", 8)
	if _, _, err := wrong.Embed(t.Context(), []string{"x"}); err == nil {
		t.Fatal("a dimension mismatch must be an error")
	}
}

func strconvInt(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}
