// Package transcript records model calls for the dashboard's transcript
// view (ADR-0009 §2.8). An agentic run's request is the whole conversation
// so far, so each step is stored as a delta against what the run has
// already recorded; a single-shot call is a delta against nothing. Rows are
// masked and size-capped before they are encoded, and Rebuild turns stored
// rows back into one turn per call.
package transcript

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"hash"

	"github.com/home-operations/kritik/internal/model"
)

// Kind is what made a model call, as the model_calls table spells it.
type Kind string

// Model call kinds.
const (
	KindAgentStep Kind = "agent_step"
	KindReview    Kind = "review"
	KindFallback  Kind = "fallback"
	KindFollowUp  Kind = "followup"
)

// Valid reports whether k is a model call kind.
func (k Kind) Valid() bool {
	switch k {
	case KindAgentStep, KindReview, KindFallback, KindFollowUp:
		return true
	}
	return false
}

// ToolCall is model.ToolCall as stored. Input is valid JSON: the model's
// arguments when they parse, else those arguments as a JSON string.
type ToolCall struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input,omitempty"`
}

// ToolResult is model.ToolResult as stored. TruncatedBytes is how much of
// Content was cut to fit ToolResultCap.
type ToolResult struct {
	CallID         string `json:"callId"`
	Content        string `json:"content"`
	IsError        bool   `json:"isError,omitempty"`
	TruncatedBytes int    `json:"truncatedBytes,omitempty"`
}

// Message is model.Message as stored.
type Message struct {
	Role        model.Role   `json:"role"`
	Text        string       `json:"text,omitempty"`
	ToolCalls   []ToolCall   `json:"toolCalls,omitempty"`
	ToolResults []ToolResult `json:"toolResults,omitempty"`
}

// Response is the model's answer to one call: its text and its tool calls'
// input as returned, not the vendor's wire JSON.
type Response struct {
	Text      string           `json:"text,omitempty"`
	ToolCalls []ToolCall       `json:"toolCalls,omitempty"`
	Stop      model.StopReason `json:"stop,omitempty"`
}

// Tool is model.ToolDef as stored.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

// State is what a run has recorded so far, carried from one step's row to
// the next. MessagesSHA is the cumulative hash of the first MessagesEnd
// messages (see Delta); SystemSHA and ToolsSHA hash the system prompt and
// tool list last stored; a zero hash means nothing is recorded. Bytes is
// the encoded size of every row the run has stored.
type State struct {
	MessagesEnd int
	MessagesSHA [32]byte
	SystemSHA   [32]byte
	ToolsSHA    [32]byte
	Bytes       int64
}

// Row is one call ready to be masked and encoded. System and Tools are nil
// when unchanged from the run's previous row. Reset says the request did
// not extend what the run had recorded, so Messages is the whole request.
type Row struct {
	MessagesFrom int
	Messages     []Message
	Reset        bool
	System       *string
	Tools        *[]Tool
	Response     Response

	prevBytes int64
	next      State
}

// Delta is the row that records req after prev: the messages from
// prev.MessagesEnd on, and System and Tools only when their hash changed.
// When req is shorter than what was recorded, or its first MessagesEnd
// messages hash differently, the row holds every message from 0.
//
// The messages hash is SHA-256 over, for each message in order, its JSON
// encoding as a Message (encoding/json, tool input compacted) followed by
// a newline. It is computed before masking and caps, so a rotated secret
// or a cut tool result does not break the chain.
func Delta(prev State, req model.StepRequest) Row {
	msgs := make([]Message, len(req.Messages))
	h := sha256.New()
	var prefix [32]byte
	for i, m := range req.Messages {
		if i == prev.MessagesEnd {
			prefix = sum(h)
		}
		msgs[i] = fromMessage(m)
		writeMessage(h, msgs[i])
	}
	all := sum(h)
	if prev.MessagesEnd == len(msgs) {
		prefix = all
	}
	r := Row{prevBytes: prev.Bytes, next: State{MessagesEnd: len(msgs), MessagesSHA: all, Bytes: prev.Bytes}}
	if prev.MessagesEnd == 0 || (prev.MessagesEnd <= len(msgs) && prefix == prev.MessagesSHA) {
		r.MessagesFrom = prev.MessagesEnd
	} else {
		r.Reset = true
	}
	r.Messages = msgs[r.MessagesFrom:]

	r.next.SystemSHA = sha256.Sum256([]byte(req.System))
	if r.next.SystemSHA != prev.SystemSHA {
		system := req.System
		r.System = &system
	}
	tools := fromTools(req.Tools)
	encoded, _ := json.Marshal(tools) // cannot fail: every RawMessage in tools is valid JSON
	r.next.ToolsSHA = sha256.Sum256(encoded)
	if r.next.ToolsSHA != prev.ToolsSHA {
		r.Tools = &tools
	}
	return r
}

// NewResponse is resp as stored.
func NewResponse(resp model.StepResponse) Response {
	return Response{Text: resp.Text, ToolCalls: fromCalls(resp.ToolCalls), Stop: resp.Stop}
}

func sum(h hash.Hash) [32]byte {
	var s [32]byte
	h.Sum(s[:0])
	return s
}

func writeMessage(h hash.Hash, m Message) {
	b, _ := json.Marshal(m) // cannot fail: tool input is valid JSON by construction
	h.Write(b)
	h.Write([]byte{'\n'})
}

func fromMessage(m model.Message) Message {
	out := Message{Role: m.Role, Text: m.Text, ToolCalls: fromCalls(m.ToolCalls)}
	for _, r := range m.ToolResults {
		out.ToolResults = append(out.ToolResults, ToolResult{CallID: r.CallID, Content: r.Content, IsError: r.IsError})
	}
	return out
}

func fromCalls(calls []model.ToolCall) []ToolCall {
	out := make([]ToolCall, 0, len(calls))
	for _, c := range calls {
		out = append(out, ToolCall{ID: c.ID, Name: c.Name, Input: validJSON(c.Input)})
	}
	return out
}

func fromTools(defs []model.ToolDef) []Tool {
	out := make([]Tool, 0, len(defs))
	for _, d := range defs {
		out = append(out, Tool{Name: d.Name, Description: d.Description, InputSchema: validJSON(d.InputSchema)})
	}
	return out
}

// validJSON is raw compacted when it is JSON, else raw as a JSON string,
// so a model's malformed tool arguments are kept rather than lost.
func validJSON(raw []byte) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err == nil {
		return buf.Bytes()
	}
	s, _ := json.Marshal(string(raw)) // cannot fail for a string
	return s
}
