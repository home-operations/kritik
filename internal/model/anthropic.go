package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// defaultAnthropicMaxTokens applies when a request sets none; the Messages
// API requires one.
const defaultAnthropicMaxTokens = 8192

// AnthropicConfig configures an Anthropic Messages adapter.
type AnthropicConfig struct {
	// BaseURL is the API root, without "/v1"; empty means the SDK's default.
	BaseURL string
	APIKey  string
	// HTTPClient may be nil.
	HTTPClient *http.Client
	Pricing    Pricing
}

// Anthropic is a Stepper over the Anthropic Messages API.
type Anthropic struct {
	client  anthropic.Client
	pricing Pricing
}

// NewAnthropic builds the adapter. It ignores the SDK's environment and
// profile credentials so that only the configured key is ever sent.
func NewAnthropic(cfg AnthropicConfig) (*Anthropic, error) {
	if cfg.APIKey == "" {
		return nil, errors.New("model: anthropic: an API key is required")
	}
	opts := []option.RequestOption{option.WithoutEnvironmentDefaults(), option.WithAPIKey(cfg.APIKey)}
	if cfg.BaseURL != "" {
		if err := checkBaseURL(cfg.BaseURL); err != nil {
			return nil, fmt.Errorf("model: anthropic: %w", err)
		}
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	if cfg.HTTPClient != nil {
		opts = append(opts, option.WithHTTPClient(cfg.HTTPClient))
	}
	return &Anthropic{client: anthropic.NewClient(opts...), pricing: cfg.Pricing}, nil
}

// Step implements Stepper.
func (a *Anthropic) Step(ctx context.Context, req StepRequest) (StepResponse, error) {
	if err := checkRequest(req); err != nil {
		return StepResponse{}, err
	}
	params, err := anthropicParams(req)
	if err != nil {
		return StepResponse{}, err
	}
	return eachModel(ctx, req, func(id string) (StepResponse, error) {
		params.Model = id
		msg, err := a.client.Messages.New(ctx, params)
		if err != nil {
			return StepResponse{}, err
		}
		out := StepResponse{Stop: anthropicStop(msg.StopReason), Model: id}
		for _, b := range msg.Content {
			switch b.Type {
			case "text":
				out.Text += b.Text
			case "tool_use":
				out.ToolCalls = append(out.ToolCalls, ToolCall{ID: b.ID, Name: b.Name, Input: b.Input})
			}
		}
		u := msg.Usage
		out.Usage = Usage{Input: u.InputTokens, CacheRead: u.CacheReadInputTokens, CacheWrite: u.CacheCreationInputTokens, Output: u.OutputTokens}
		out.CostUSD = a.pricing.cost(id, out.Usage)
		return out, nil
	})
}

func anthropicStop(s anthropic.StopReason) StopReason {
	switch s {
	case anthropic.StopReasonEndTurn:
		return StopEndTurn
	case anthropic.StopReasonToolUse:
		return StopToolUse
	case anthropic.StopReasonMaxTokens:
		return StopMaxTokens
	default:
		return StopOther
	}
}

// anthropicParams maps everything but the model, which each attempt sets.
// The system prompt and the last block of the last message are cache
// breakpoints: the first caches the instructions, the second everything so
// far, which the next step of a tool loop reads back.
func anthropicParams(req StepRequest) (anthropic.MessageNewParams, error) {
	p := anthropic.MessageNewParams{MaxTokens: req.MaxTokens}
	if p.MaxTokens <= 0 {
		p.MaxTokens = defaultAnthropicMaxTokens
	}
	if req.System != "" {
		p.System = []anthropic.TextBlockParam{{Text: req.System, CacheControl: anthropic.NewCacheControlEphemeralParam()}}
	}
	for _, m := range req.Messages {
		if blocks := anthropicBlocks(m); len(blocks) > 0 {
			p.Messages = append(p.Messages, anthropic.MessageParam{Role: anthropic.MessageParamRole(m.Role), Content: blocks})
		}
	}
	if n := len(p.Messages); n > 0 {
		blocks := p.Messages[n-1].Content
		if cc := blocks[len(blocks)-1].GetCacheControl(); cc != nil {
			*cc = anthropic.NewCacheControlEphemeralParam()
		}
	}
	if len(req.Tools) == 0 {
		return p, nil
	}
	for _, t := range req.Tools {
		schema, err := anthropicSchema(t.InputSchema)
		if err != nil {
			return p, fmt.Errorf("model: tool %s: input schema: %w", t.Name, err)
		}
		tool := anthropic.ToolParam{Name: t.Name, InputSchema: schema}
		if t.Description != "" {
			tool.Description = anthropic.String(t.Description)
		}
		p.Tools = append(p.Tools, anthropic.ToolUnionParam{OfTool: &tool})
	}
	switch req.ToolChoice.Mode {
	case ToolChoiceRequired:
		p.ToolChoice = anthropic.ToolChoiceUnionParam{OfAny: &anthropic.ToolChoiceAnyParam{}}
	case ToolChoiceTool:
		p.ToolChoice = anthropic.ToolChoiceParamOfTool(req.ToolChoice.Name)
	default:
		p.ToolChoice = anthropic.ToolChoiceUnionParam{OfAuto: &anthropic.ToolChoiceAutoParam{}}
	}
	return p, nil
}

// anthropicBlocks maps one message. Tool results lead a user message, as
// the API requires; empty text is dropped because the API rejects it.
func anthropicBlocks(m Message) []anthropic.ContentBlockParamUnion {
	blocks := make([]anthropic.ContentBlockParamUnion, 0, 1+len(m.ToolCalls)+len(m.ToolResults))
	for _, r := range m.ToolResults {
		res := anthropic.ToolResultBlockParam{ToolUseID: r.CallID, IsError: anthropic.Bool(r.IsError)}
		if r.Content != "" {
			res.Content = []anthropic.ToolResultBlockParamContentUnion{{OfText: &anthropic.TextBlockParam{Text: r.Content}}}
		}
		blocks = append(blocks, anthropic.ContentBlockParamUnion{OfToolResult: &res})
	}
	if m.Text != "" {
		blocks = append(blocks, anthropic.NewTextBlock(m.Text))
	}
	for _, c := range m.ToolCalls {
		input := c.Input
		if len(input) == 0 {
			input = json.RawMessage(`{}`)
		}
		blocks = append(blocks, anthropic.NewToolUseBlock(c.ID, input, c.Name))
	}
	return blocks
}

// anthropicSchema splits a JSON Schema object into the SDK's typed fields
// and passes every other keyword through unchanged.
func anthropicSchema(raw json.RawMessage) (anthropic.ToolInputSchemaParam, error) {
	var s anthropic.ToolInputSchemaParam
	if len(raw) == 0 {
		return s, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return s, err
	}
	s.Properties = m["properties"]
	if req, ok := m["required"].([]any); ok {
		for _, r := range req {
			name, ok := r.(string)
			if !ok {
				return s, fmt.Errorf("required holds %v, not a property name", r)
			}
			s.Required = append(s.Required, name)
		}
	}
	delete(m, "type")
	delete(m, "properties")
	delete(m, "required")
	if len(m) > 0 {
		s.ExtraFields = m
	}
	return s, nil
}
