package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// This file is the OpenAI chat completions wire format as kritik's model
// gateway serves it (ADR-0004): the part of it the OpenAI adapter sends and
// reads, decoded into a StepRequest and encoded from a StepResponse, so a
// runner's adapter can talk to the gateway and the gateway can answer
// through any provider's adapter.

// ErrBudget is a step the gateway refused because the run's token budget
// or the tenant's monthly cap is spent.
var ErrBudget = errors.New("model: token budget exhausted")

// BudgetCode is the error code of the gateway's budget refusal.
const BudgetCode = "budget_exhausted"

// toolErrorPrefix marks a failed tool result on the wire, where chat
// completions has no error flag.
const toolErrorPrefix = "Error: "

type chatRequest struct {
	Model               string          `json:"model"`
	Messages            []chatMessage   `json:"messages"`
	Tools               []chatTool      `json:"tools"`
	ToolChoice          json.RawMessage `json:"tool_choice"`
	MaxTokens           int64           `json:"max_tokens"`
	MaxCompletionTokens int64           `json:"max_completion_tokens"`
}

type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []chatToolCall  `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
}

type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// DecodeChatRequest reads a chat completions request into a StepRequest.
// System messages join into System; tool messages and the user text after
// them become one user message, as the OpenAI adapter splits them.
func DecodeChatRequest(body []byte) (StepRequest, error) {
	var in chatRequest
	if err := json.Unmarshal(body, &in); err != nil {
		return StepRequest{}, fmt.Errorf("model: chat request: %w", err)
	}
	req := StepRequest{Model: in.Model, MaxTokens: max(in.MaxCompletionTokens, in.MaxTokens)}
	var system []string
	for i, m := range in.Messages {
		text, err := chatText(m.Content)
		if err != nil {
			return StepRequest{}, fmt.Errorf("model: chat request: messages[%d]: %w", i, err)
		}
		last := len(req.Messages) - 1
		// A user message can take tool results, and then its text, only
		// while it has no text of its own.
		open := last >= 0 && req.Messages[last].Role == RoleUser && len(req.Messages[last].ToolResults) > 0 && req.Messages[last].Text == ""
		switch m.Role {
		case "system", "developer":
			system = append(system, text)
		case "user":
			if open {
				req.Messages[last].Text = text
				continue
			}
			req.Messages = append(req.Messages, Message{Role: RoleUser, Text: text})
		case "tool":
			content, isError := strings.CutPrefix(text, toolErrorPrefix)
			result := ToolResult{CallID: m.ToolCallID, Content: content, IsError: isError}
			if open {
				req.Messages[last].ToolResults = append(req.Messages[last].ToolResults, result)
				continue
			}
			req.Messages = append(req.Messages, Message{Role: RoleUser, ToolResults: []ToolResult{result}})
		case "assistant":
			msg := Message{Role: RoleAssistant, Text: text}
			for _, c := range m.ToolCalls {
				msg.ToolCalls = append(msg.ToolCalls, ToolCall{ID: c.ID, Name: c.Function.Name, Input: json.RawMessage(c.Function.Arguments)})
			}
			req.Messages = append(req.Messages, msg)
		default:
			return StepRequest{}, fmt.Errorf("model: chat request: messages[%d] has role %q", i, m.Role)
		}
	}
	req.System = strings.Join(system, "\n\n")
	for _, t := range in.Tools {
		req.Tools = append(req.Tools, ToolDef{Name: t.Function.Name, Description: t.Function.Description, InputSchema: t.Function.Parameters})
	}
	choice, err := chatToolChoice(in.ToolChoice)
	if err != nil {
		return StepRequest{}, err
	}
	req.ToolChoice = choice
	return req, nil
}

// chatText is a message's content: a string, null, or text parts.
func chatText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", errors.New("content is neither text nor text parts")
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type != "text" {
			return "", fmt.Errorf("content part of type %q is not supported", p.Type)
		}
		b.WriteString(p.Text)
	}
	return b.String(), nil
}

func chatToolChoice(raw json.RawMessage) (ToolChoice, error) {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ToolChoice{}, nil
	}
	var mode string
	if err := json.Unmarshal(raw, &mode); err == nil {
		switch mode {
		case string(ToolChoiceAuto), string(ToolChoiceRequired):
			return ToolChoice{Mode: ToolChoiceMode(mode)}, nil
		}
		return ToolChoice{}, fmt.Errorf("model: chat request: tool_choice %q is not supported", mode)
	}
	var named struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &named); err != nil || named.Function.Name == "" {
		return ToolChoice{}, errors.New("model: chat request: tool_choice names no function")
	}
	return ToolChoice{Mode: ToolChoiceTool, Name: named.Function.Name}, nil
}

// chatFinish is StopReason as the finish_reason openAIStop reads back.
var chatFinish = map[StopReason]string{StopEndTurn: "stop", StopToolUse: "tool_calls", StopMaxTokens: "length", StopOther: "other"}

// EncodeChatResponse writes resp as a chat completion. Usage carries the
// whole prompt with its cached parts broken out, and the cost and serving
// provider the way OpenRouter reports them, which the OpenAI adapter reads.
func EncodeChatResponse(id string, resp StepResponse) ([]byte, error) {
	type function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}
	type toolCall struct {
		ID       string   `json:"id"`
		Type     string   `json:"type"`
		Function function `json:"function"`
	}
	message := struct {
		Role      string     `json:"role"`
		Content   *string    `json:"content"`
		ToolCalls []toolCall `json:"tool_calls,omitempty"`
	}{Role: "assistant"}
	if resp.Text != "" || len(resp.ToolCalls) == 0 {
		message.Content = &resp.Text
	}
	for _, c := range resp.ToolCalls {
		fn := function{Name: c.Name, Arguments: string(c.Input)}
		message.ToolCalls = append(message.ToolCalls, toolCall{ID: c.ID, Type: "function", Function: fn})
	}
	finish, ok := chatFinish[resp.Stop]
	if !ok {
		finish = chatFinish[StopOther]
	}
	u := resp.Usage
	out := map[string]any{
		"id": id, "object": "chat.completion", "created": time.Now().Unix(), "model": resp.Model,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}},
		"usage": map[string]any{
			"prompt_tokens": u.Prompt(), "completion_tokens": u.Output, "total_tokens": u.Prompt() + u.Output,
			"prompt_tokens_details": map[string]any{"cached_tokens": u.CacheRead, "cache_write_tokens": u.CacheWrite},
			"cost":                  resp.CostUSD,
		},
	}
	if resp.Upstream != "" {
		out["provider"] = resp.Upstream
	}
	return json.Marshal(out)
}

// EncodeChatError writes an error body in the shape the OpenAI SDKs parse.
func EncodeChatError(code, message string) []byte {
	// A map of strings always encodes.
	b, _ := json.Marshal(map[string]any{"error": map[string]string{"code": code, "type": code, "message": message}})
	return b
}
