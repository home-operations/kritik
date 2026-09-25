package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

// Attribution headers OpenRouter shows in its dashboard; harmless elsewhere.
var attribution = map[string]string{
	"HTTP-Referer": "https://github.com/home-operations/kritik",
	"X-Title":      "kritik",
}

// OpenAIConfig configures an OpenAI chat-completions adapter.
type OpenAIConfig struct {
	// BaseURL is the API root, ".../v1"; empty means the SDK's default.
	BaseURL string
	APIKey  string
	// HTTPClient may be nil.
	HTTPClient *http.Client
	// OpenRouter enables OpenRouter's extensions: server-side fallback
	// through the models list, and usage accounting.
	OpenRouter bool
	Pricing    Pricing
}

// OpenAI is a Stepper over the chat completions API of OpenAI or any server
// compatible with it.
type OpenAI struct {
	client     openai.Client
	openRouter bool
	pricing    Pricing
}

// NewOpenAI builds the adapter.
func NewOpenAI(cfg OpenAIConfig) (*OpenAI, error) {
	if cfg.APIKey == "" {
		return nil, errors.New("model: openai: an API key is required")
	}
	opts := []option.RequestOption{option.WithAPIKey(cfg.APIKey)}
	if cfg.BaseURL != "" {
		if err := checkBaseURL(cfg.BaseURL); err != nil {
			return nil, fmt.Errorf("model: openai: %w", err)
		}
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	if cfg.HTTPClient != nil {
		opts = append(opts, option.WithHTTPClient(cfg.HTTPClient))
	}
	for k, v := range attribution {
		opts = append(opts, option.WithHeader(k, v))
	}
	return &OpenAI{client: openai.NewClient(opts...), openRouter: cfg.OpenRouter, pricing: cfg.Pricing}, nil
}

func checkBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("base URL: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("base URL %q must be absolute", raw)
	}
	return nil
}

// Step implements Stepper.
func (o *OpenAI) Step(ctx context.Context, req StepRequest) (StepResponse, error) {
	if err := checkRequest(req); err != nil {
		return StepResponse{}, err
	}
	params, err := o.params(req)
	if err != nil {
		return StepResponse{}, err
	}
	if o.openRouter {
		// OpenRouter walks the models list itself, primary first.
		var opts []option.RequestOption
		opts = append(opts, option.WithJSONSet("usage", map[string]any{"include": true}))
		if len(req.Fallbacks) > 0 {
			opts = append(opts, option.WithJSONSet("models", append([]string{req.Model}, req.Fallbacks...)))
		}
		resp, err := o.step(ctx, params, req.Model, opts...)
		if err != nil {
			return StepResponse{}, fmt.Errorf("model: %s: %w", req.Model, err)
		}
		return resp, nil
	}
	return eachModel(ctx, req, func(id string) (StepResponse, error) { return o.step(ctx, params, id) })
}

func (o *OpenAI) step(
	ctx context.Context, params openai.ChatCompletionNewParams, modelID string, opts ...option.RequestOption,
) (StepResponse, error) {
	params.Model = modelID
	cc, err := o.client.Chat.Completions.New(ctx, params, opts...)
	if err != nil {
		return StepResponse{}, err
	}
	if len(cc.Choices) == 0 {
		return StepResponse{}, errors.New("response has no choices")
	}
	msg := cc.Choices[0].Message
	out := StepResponse{Text: msg.Content, Stop: openAIStop(cc.Choices[0].FinishReason), Model: modelID}
	if o.openRouter && cc.Model != "" {
		// OpenRouter's server-side fallback may answer with another model
		// in the list; its response says which. Another provider's model
		// field names a dated snapshot of the one asked for, which is not
		// what the operator configured, so it is not used.
		out.Model = cc.Model
	}
	for _, tc := range msg.ToolCalls {
		if tc.Type != "function" {
			continue
		}
		args := tc.Function.Arguments
		if args == "" {
			args = "{}"
		}
		out.ToolCalls = append(out.ToolCalls, ToolCall{ID: tc.ID, Name: tc.Function.Name, Input: json.RawMessage(args)})
	}

	u := cc.Usage
	cached, written := u.PromptTokensDetails.CachedTokens, u.PromptTokensDetails.CacheWriteTokens
	out.Usage = Usage{Input: max(u.PromptTokens-cached-written, 0), CacheRead: cached, CacheWrite: written, Output: u.CompletionTokens}

	// Cost and the serving provider are OpenRouter's additions to the
	// response, which the SDK's types do not carry.
	var extra struct {
		Provider string `json:"provider"`
		Usage    struct {
			Cost *float64 `json:"cost"`
		} `json:"usage"`
	}
	if raw := cc.RawJSON(); raw != "" {
		if err := json.Unmarshal([]byte(raw), &extra); err != nil {
			return StepResponse{}, fmt.Errorf("decode response: %w", err)
		}
	}
	out.Upstream = extra.Provider
	if extra.Usage.Cost != nil {
		out.CostUSD = *extra.Usage.Cost
	} else {
		out.CostUSD = o.pricing.cost(modelID, out.Usage)
	}
	return out, nil
}

func openAIStop(finish string) StopReason {
	switch finish {
	case "stop":
		return StopEndTurn
	case "tool_calls", "function_call":
		return StopToolUse
	case "length":
		return StopMaxTokens
	default:
		return StopOther
	}
}

// params maps everything but the model, which each attempt sets.
func (o *OpenAI) params(req StepRequest) (openai.ChatCompletionNewParams, error) {
	var p openai.ChatCompletionNewParams
	if req.System != "" {
		p.Messages = append(p.Messages, openai.SystemMessage(req.System))
	}
	for _, m := range req.Messages {
		p.Messages = append(p.Messages, openAIMessages(m)...)
	}
	if req.MaxTokens > 0 {
		// OpenAI's own API has deprecated max_tokens; OpenRouter documents
		// only max_tokens.
		if o.openRouter {
			p.MaxTokens = openai.Int(req.MaxTokens)
		} else {
			p.MaxCompletionTokens = openai.Int(req.MaxTokens)
		}
	}
	if len(req.Tools) == 0 {
		return p, nil
	}
	for _, t := range req.Tools {
		fn := shared.FunctionDefinitionParam{Name: t.Name}
		if t.Description != "" {
			fn.Description = openai.String(t.Description)
		}
		if len(t.InputSchema) > 0 {
			if err := json.Unmarshal(t.InputSchema, &fn.Parameters); err != nil {
				return p, fmt.Errorf("model: tool %s: input schema: %w", t.Name, err)
			}
		}
		p.Tools = append(p.Tools, openai.ChatCompletionFunctionTool(fn))
	}
	switch req.ToolChoice.Mode {
	case ToolChoiceRequired:
		p.ToolChoice.OfAuto = openai.String(string(ToolChoiceRequired))
	case ToolChoiceTool:
		p.ToolChoice = openai.ToolChoiceOptionFunctionToolChoice(openai.ChatCompletionNamedToolChoiceFunctionParam{Name: req.ToolChoice.Name})
	default:
		p.ToolChoice.OfAuto = openai.String(string(ToolChoiceAuto))
	}
	return p, nil
}

// openAIMessages maps one message. Tool results become one tool message
// each, ahead of any text, since they must follow the assistant's calls.
func openAIMessages(m Message) []openai.ChatCompletionMessageParamUnion {
	if m.Role == RoleAssistant {
		var a openai.ChatCompletionAssistantMessageParam
		if m.Text != "" {
			a.Content.OfString = openai.String(m.Text)
		}
		for _, c := range m.ToolCalls {
			args := string(c.Input)
			if args == "" {
				args = "{}"
			}
			a.ToolCalls = append(a.ToolCalls, openai.ChatCompletionMessageToolCallUnionParam{
				OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
					ID: c.ID, Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{Name: c.Name, Arguments: args},
				},
			})
		}
		return []openai.ChatCompletionMessageParamUnion{{OfAssistant: &a}}
	}
	out := make([]openai.ChatCompletionMessageParamUnion, 0, len(m.ToolResults)+1)
	for _, r := range m.ToolResults {
		content := r.Content
		if r.IsError {
			// Chat completions has no error flag on a tool message.
			content = "Error: " + content
		}
		out = append(out, openai.ToolMessage(content, r.CallID))
	}
	if m.Text != "" || len(m.ToolResults) == 0 {
		out = append(out, openai.UserMessage(m.Text))
	}
	return out
}
