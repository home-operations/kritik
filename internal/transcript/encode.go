package transcript

import (
	"encoding/json"
	"unicode/utf8"
)

// Size caps (ADR-0009 §2.8).
const (
	// ToolResultCap bounds one tool result's content.
	ToolResultCap = 64 << 10
	// RowCap bounds one row's encoded messages; a row over it keeps its
	// messages' structure with Placeholder for every text, content and
	// input.
	RowCap = 1 << 20
	// RunCap bounds what a run records: once its rows exceed it, later rows
	// keep only the response and usage.
	RunCap = 16 << 20
)

// Placeholder stands in for content a row over RowCap dropped.
const Placeholder = "[omitted: the row exceeded its size cap]"

// Encoded is a row as the model_calls columns take it. System and Tools
// are nil when unchanged. State is the run's state after this row.
type Encoded struct {
	MessagesFrom int
	Messages     json.RawMessage
	Response     json.RawMessage
	System       *string
	Tools        json.RawMessage
	Truncated    bool
	State        State
}

// Mask returns r with fn applied to its system prompt, tool descriptions
// and schemas, and every text, tool input and tool result, request and
// response alike. It must run before Encode: a cut made first could leave
// part of a secret that fn no longer recognises.
func (r Row) Mask(fn func(string) string) Row {
	out := r
	if r.System != nil {
		s := fn(*r.System)
		out.System = &s
	}
	if r.Tools != nil {
		tools := make([]Tool, len(*r.Tools))
		for i, t := range *r.Tools {
			tools[i] = Tool{Name: t.Name, Description: fn(t.Description), InputSchema: maskRaw(t.InputSchema, fn)}
		}
		out.Tools = &tools
	}
	out.Messages = make([]Message, len(r.Messages))
	for i, m := range r.Messages {
		out.Messages[i] = Message{Role: m.Role, Text: fn(m.Text), ToolCalls: maskCalls(m.ToolCalls, fn)}
		for _, res := range m.ToolResults {
			res.Content = fn(res.Content)
			out.Messages[i].ToolResults = append(out.Messages[i].ToolResults, res)
		}
	}
	out.Response = Response{Text: fn(r.Response.Text), ToolCalls: maskCalls(r.Response.ToolCalls, fn), Stop: r.Response.Stop}
	return out
}

func maskCalls(calls []ToolCall, fn func(string) string) []ToolCall {
	out := make([]ToolCall, 0, len(calls))
	for _, c := range calls {
		out = append(out, ToolCall{ID: c.ID, Name: c.Name, Input: maskRaw(c.Input, fn)})
	}
	return out
}

func maskRaw(raw json.RawMessage, fn func(string) string) json.RawMessage {
	if raw == nil {
		return nil
	}
	return validJSON([]byte(fn(string(raw))))
}

// Encode applies the caps and encodes r. Call Mask first.
func (r Row) Encode() Encoded {
	e := Encoded{MessagesFrom: r.MessagesFrom, System: r.System, State: r.next}
	msgs := r.Messages
	if r.prevBytes > RunCap {
		msgs, e.System, e.Truncated = nil, nil, true
	} else if r.Tools != nil {
		e.Tools, _ = json.Marshal(*r.Tools) // cannot fail: every RawMessage is valid JSON
	}
	msgs, cut := cutResults(msgs)
	e.Truncated = e.Truncated || cut
	e.Messages = marshalMessages(msgs)
	if len(e.Messages) > RowCap {
		e.Messages, e.Truncated = marshalMessages(placeholders(msgs)), true
	}
	e.Response, _ = json.Marshal(r.Response) // cannot fail: every RawMessage is valid JSON
	size := len(e.Messages) + len(e.Response) + len(e.Tools)
	if e.System != nil {
		size += len(*e.System)
	}
	e.State.Bytes = r.prevBytes + int64(size)
	return e
}

func marshalMessages(msgs []Message) json.RawMessage {
	if len(msgs) == 0 {
		return json.RawMessage("[]")
	}
	b, _ := json.Marshal(msgs) // cannot fail: every RawMessage is valid JSON
	return b
}

// cutResults cuts every tool result over ToolResultCap, on a rune
// boundary, and reports whether it cut any.
func cutResults(msgs []Message) ([]Message, bool) {
	cut := false
	out := make([]Message, len(msgs))
	for i, m := range msgs {
		out[i] = m
		if len(m.ToolResults) == 0 {
			continue
		}
		out[i].ToolResults = make([]ToolResult, len(m.ToolResults))
		for j, res := range m.ToolResults {
			if len(res.Content) > ToolResultCap {
				n := ToolResultCap
				for n > 0 && !utf8.RuneStart(res.Content[n]) {
					n--
				}
				res.TruncatedBytes += len(res.Content) - n
				res.Content = res.Content[:n]
				cut = true
			}
			out[i].ToolResults[j] = res
		}
	}
	return out, cut
}

// placeholders keeps msgs' roles, tool call ids and names, and replaces
// every text, input and content with Placeholder.
func placeholders(msgs []Message) []Message {
	input, _ := json.Marshal(Placeholder) // cannot fail for a string
	out := make([]Message, len(msgs))
	for i, m := range msgs {
		out[i] = Message{Role: m.Role}
		if m.Text != "" {
			out[i].Text = Placeholder
		}
		for _, c := range m.ToolCalls {
			out[i].ToolCalls = append(out[i].ToolCalls, ToolCall{ID: c.ID, Name: c.Name, Input: input})
		}
		for _, res := range m.ToolResults {
			out[i].ToolResults = append(out[i].ToolResults, ToolResult{CallID: res.CallID, Content: Placeholder, IsError: res.IsError,
				TruncatedBytes: res.TruncatedBytes})
		}
	}
	return out
}
