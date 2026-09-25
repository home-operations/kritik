// Package model is kritik's view of a language model and an embedder. A
// Stepper performs one model turn over typed messages and tools; Completer,
// a single forced tool call returning structured JSON, is built on it.
// Adapters speak to the vendors' official SDKs.
package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// ProviderType selects the adapter a provider uses.
type ProviderType string

// Provider types kritik implements. OpenRouter is the OpenAI adapter at
// OpenRouter's URL with its server-side fallback and reported cost.
const (
	ProviderOpenRouter ProviderType = "openrouter"
	ProviderOpenAI     ProviderType = "openai"
	ProviderAnthropic  ProviderType = "anthropic"
)

// Valid reports whether p is a provider type kritik implements.
func (p ProviderType) Valid() bool {
	switch p {
	case ProviderOpenRouter, ProviderOpenAI, ProviderAnthropic:
		return true
	}
	return false
}

// Role is who authored a message. The system prompt is not a message; it
// travels in StepRequest.System.
type Role string

// Message roles.
const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Valid reports whether r is a message role.
func (r Role) Valid() bool { return r == RoleUser || r == RoleAssistant }

// ToolCall is the model asking for a tool to run. Input is the arguments as
// the model wrote them, which need not match the tool's schema.
type ToolCall struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// ToolResult answers the ToolCall whose ID is CallID.
type ToolResult struct {
	CallID  string
	Content string
	IsError bool
}

// Message is one turn of a conversation. A user message carries Text and/or
// ToolResults; an assistant message Text and/or ToolCalls.
type Message struct {
	Role        Role
	Text        string
	ToolCalls   []ToolCall
	ToolResults []ToolResult
}

// ToolDef describes a tool the model may call. InputSchema is a JSON Schema
// object.
type ToolDef struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// ToolChoiceMode says whether and which tool the model must call.
type ToolChoiceMode string

// Tool choice modes. The zero value behaves as ToolChoiceAuto.
const (
	ToolChoiceAuto     ToolChoiceMode = "auto"
	ToolChoiceRequired ToolChoiceMode = "required"
	ToolChoiceTool     ToolChoiceMode = "tool"
)

// Valid reports whether m is a tool choice mode.
func (m ToolChoiceMode) Valid() bool {
	switch m {
	case ToolChoiceAuto, ToolChoiceRequired, ToolChoiceTool:
		return true
	}
	return false
}

// ToolChoice constrains tool use in a step. Name is set iff Mode is
// ToolChoiceTool.
type ToolChoice struct {
	Mode ToolChoiceMode
	Name string
}

// StopReason is why the model ended its turn.
type StopReason string

// Stop reasons, normalised across providers.
const (
	StopEndTurn   StopReason = "end_turn"
	StopToolUse   StopReason = "tool_use"
	StopMaxTokens StopReason = "max_tokens"
	StopOther     StopReason = "other"
)

// Valid reports whether s is a normalised stop reason.
func (s StopReason) Valid() bool {
	switch s {
	case StopEndTurn, StopToolUse, StopMaxTokens, StopOther:
		return true
	}
	return false
}

// Usage is the tokens one or more steps spent. Input is the uncached part of
// the prompt; CacheRead and CacheWrite are the cached parts, which providers
// bill differently.
type Usage struct {
	Input      int64
	CacheRead  int64
	CacheWrite int64
	Output     int64
}

// Add returns the sum of u and v.
func (u Usage) Add(v Usage) Usage {
	return Usage{
		Input: u.Input + v.Input, CacheRead: u.CacheRead + v.CacheRead,
		CacheWrite: u.CacheWrite + v.CacheWrite, Output: u.Output + v.Output,
	}
}

// Prompt is the whole prompt, cached parts included.
func (u Usage) Prompt() int64 { return u.Input + u.CacheRead + u.CacheWrite }

// StepRequest is one model turn.
type StepRequest struct {
	// Model is the primary model id in the provider's namespace.
	Model string
	// Fallbacks are tried, in order, if Model fails: by OpenRouter itself,
	// by the adapter for every other provider.
	Fallbacks []string
	System    string
	Messages  []Message
	Tools     []ToolDef
	// ToolChoice is ignored when Tools is empty.
	ToolChoice ToolChoice
	// MaxTokens bounds the answer; zero means the adapter's default.
	MaxTokens int64
}

// StepResponse is the model's turn and what it cost.
type StepResponse struct {
	Text      string
	ToolCalls []ToolCall
	Stop      StopReason
	Usage     Usage
	// CostUSD is the provider's reported cost, else the cost Pricing gives,
	// else zero.
	CostUSD float64
	// Model is the model kritik asked for that answered; OpenRouter's
	// server-side fallback does not always say which one did.
	Model string
	// Upstream is the provider that served the request, when known.
	Upstream string
}

// Stepper performs one model turn.
type Stepper interface {
	Step(ctx context.Context, req StepRequest) (StepResponse, error)
}

// CompletionRequest is one structured-output call.
type CompletionRequest struct {
	System string
	User   string
	// Model is the primary model id in the provider's namespace.
	Model string
	// Fallbacks are tried, in order, if Model fails.
	Fallbacks []string
	// Schema is the JSON Schema the answer must satisfy; the response
	// carries the raw JSON.
	Schema     json.RawMessage
	SchemaName string
	MaxTokens  int64
}

// CompletionResponse is the answer plus what it cost.
type CompletionResponse struct {
	// Raw is the JSON the model produced.
	Raw string
	// Model is the model that was asked for; providers that fall back do
	// not always say which one answered.
	Model string
	// Upstream is the provider that served the request, when known.
	Upstream string
	// InputTokens is the whole prompt, cached part included; CachedTokens
	// is the part the provider served from its prompt cache, which it
	// bills at a discount.
	InputTokens  int64
	CachedTokens int64
	OutputTokens int64
	// CostUSD is the reported or computed cost, zero when neither is known.
	CostUSD float64
}

// Completer produces structured answers.
type Completer interface {
	Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error)
}

// Embedder turns texts into vectors and reports the tokens it spent.
type Embedder interface {
	Embed(ctx context.Context, inputs []string) (vectors [][]float32, tokens int64, err error)
}

// NewStepper builds the adapter for a provider. An empty baseURL means the
// provider's default endpoint; client may be nil.
func NewStepper(t ProviderType, baseURL, apiKey string, pricing Pricing, client *http.Client) (Stepper, error) {
	switch t {
	case ProviderOpenRouter:
		if baseURL == "" {
			baseURL = OpenRouterBaseURL
		}
		return NewOpenAI(OpenAIConfig{BaseURL: baseURL, APIKey: apiKey, HTTPClient: client, OpenRouter: true, Pricing: pricing})
	case ProviderOpenAI:
		return NewOpenAI(OpenAIConfig{BaseURL: baseURL, APIKey: apiKey, HTTPClient: client, Pricing: pricing})
	case ProviderAnthropic:
		return NewAnthropic(AnthropicConfig{BaseURL: baseURL, APIKey: apiKey, HTTPClient: client, Pricing: pricing})
	default:
		return nil, fmt.Errorf("model: provider type %q has no adapter", t)
	}
}

// OpenRouterBaseURL is where an openrouter provider without a baseUrl goes.
const OpenRouterBaseURL = "https://openrouter.ai/api/v1"

// checkRequest rejects a request no provider could serve.
func checkRequest(req StepRequest) error {
	if req.Model == "" {
		return errors.New("model: request names no model")
	}
	for i, m := range req.Messages {
		if !m.Role.Valid() {
			return fmt.Errorf("model: messages[%d] has role %q", i, m.Role)
		}
	}
	switch c := req.ToolChoice; {
	case c.Mode == "":
	case !c.Mode.Valid():
		return fmt.Errorf("model: tool choice mode %q", c.Mode)
	case (c.Mode == ToolChoiceTool) != (c.Name != ""):
		return fmt.Errorf("model: tool choice %s must name a tool exactly when the mode is %s", c.Mode, ToolChoiceTool)
	}
	return nil
}

// eachModel calls step with Model, then each fallback in turn until one
// succeeds, for providers without server-side fallback. It stops early when
// ctx is done, since every further attempt would fail the same way.
func eachModel(ctx context.Context, req StepRequest, step func(modelID string) (StepResponse, error)) (StepResponse, error) {
	var errs []error
	for _, id := range append([]string{req.Model}, req.Fallbacks...) {
		resp, err := step(id)
		if err == nil {
			return resp, nil
		}
		errs = append(errs, fmt.Errorf("model: %s: %w", id, err))
		if ctx.Err() != nil {
			break
		}
	}
	return StepResponse{}, errors.Join(errs...)
}
