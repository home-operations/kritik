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

// SystemCap bounds a stored system prompt; one over it is cut, on a rune
// boundary, and ends in SystemTruncated.
const SystemCap = RowCap

// SystemTruncated ends a system prompt cut to SystemCap.
const SystemTruncated = "\n[truncated: the system prompt exceeded its size cap]"

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

func maskMessage(m Message, fn func(string) string) Message {
	out := Message{Role: m.Role, Text: fn(m.Text), ToolCalls: maskCalls(m.ToolCalls, fn)}
	for _, res := range m.ToolResults {
		res.Content = fn(res.Content)
		out.ToolResults = append(out.ToolResults, res)
	}
	return out
}

func maskTools(tools []Tool, fn func(string) string) []Tool {
	out := make([]Tool, len(tools))
	for i, t := range tools {
		out[i] = Tool{Name: t.Name, Description: fn(t.Description), InputSchema: maskRaw(t.InputSchema, fn)}
	}
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

// Encode applies the caps and encodes r.
func (r Row) Encode() Encoded {
	e := Encoded{MessagesFrom: r.MessagesFrom, System: r.System, State: r.next}
	msgs := r.Messages
	if r.prevBytes > RunCap {
		msgs, e.System, e.Truncated = nil, nil, true
	} else if r.Tools != nil {
		e.Tools, _ = json.Marshal(*r.Tools) // cannot fail: every RawMessage is valid JSON
	}
	if e.System != nil && len(*e.System) > SystemCap {
		system := cutString(*e.System, SystemCap) + SystemTruncated
		e.System, e.Truncated = &system, true
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
				kept := cutString(res.Content, ToolResultCap)
				res.TruncatedBytes += len(res.Content) - len(kept)
				res.Content, cut = kept, true
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

// cutString is s cut to at most n bytes on a rune boundary.
func cutString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
