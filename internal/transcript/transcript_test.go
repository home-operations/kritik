package transcript

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/home-operations/kritik/internal/model"
)

func user(text string) model.Message { return model.Message{Role: model.RoleUser, Text: text} }

func call(name, input string) model.Message {
	return model.Message{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "c1", Name: name, Input: json.RawMessage(input)}}}
}

func result(content string) model.Message {
	return model.Message{Role: model.RoleUser, ToolResults: []model.ToolResult{{CallID: "c1", Content: content}}}
}

func stepReq(system string, msgs ...model.Message) model.StepRequest {
	return model.StepRequest{Model: "m", System: system, Messages: msgs,
		Tools: []model.ToolDef{{Name: "grep", Description: "search", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
}

// advance records req after prev the way the gateway does and returns the
// row and the state it leaves.
func advance(prev State, req model.StepRequest) (Row, Encoded) {
	r := Delta(prev, req)
	return r, r.Encode()
}

func TestDelta(t *testing.T) {
	first := stepReq("sys", user("review"))
	second := stepReq("sys", user("review"), call("grep", `{"pattern":"b"}`), result("main.go:3"))
	_, e1 := advance(State{}, first)

	tests := []struct {
		name       string
		prev       State
		req        model.StepRequest
		from, n    int
		reset      bool
		system     bool
		tools      bool
		messageEnd int
	}{
		{name: "first step stores everything", prev: State{}, req: first, from: 0, n: 1, system: true, tools: true, messageEnd: 1},
		{name: "next step stores only new messages", prev: e1.State, req: second, from: 1, n: 2, messageEnd: 3},
		{name: "a repeated request stores nothing new", prev: e1.State, req: first, from: 1, n: 0, messageEnd: 1},
		{name: "a changed system prompt is stored again", prev: e1.State, req: stepReq("sys2", user("review"), user("more")),
			from: 1, n: 1, system: true, messageEnd: 2},
		{name: "a changed tool list is stored again", prev: e1.State,
			req:  model.StepRequest{System: "sys", Messages: []model.Message{user("review")}},
			from: 1, n: 0, tools: true, messageEnd: 1},
		{name: "a rewritten prefix resets", prev: e1.State, req: stepReq("sys", user("other"), user("more")),
			from: 0, n: 2, reset: true, messageEnd: 2},
		{name: "a shorter request resets", prev: State{MessagesEnd: 5, MessagesSHA: e1.State.MessagesSHA,
			SystemSHA: e1.State.SystemSHA, ToolsSHA: e1.State.ToolsSHA}, req: first, from: 0, n: 1, reset: true, messageEnd: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := Delta(tt.prev, tt.req)
			if r.MessagesFrom != tt.from || len(r.Messages) != tt.n || r.Reset != tt.reset ||
				(r.System != nil) != tt.system || (r.Tools != nil) != tt.tools || r.next.MessagesEnd != tt.messageEnd {
				t.Fatalf("row = from %d, %d messages, reset %v, system %v, tools %v, end %d",
					r.MessagesFrom, len(r.Messages), r.Reset, r.System != nil, r.Tools != nil, r.next.MessagesEnd)
			}
		})
	}
}

func TestMessagesHashIsCanonical(t *testing.T) {
	a := Delta(State{}, stepReq("s", call("grep", `{"pattern": "b"}`)))
	b := Delta(State{}, stepReq("s", call("grep", `{"pattern":"b"}`)))
	if a.next.MessagesSHA != b.next.MessagesSHA {
		t.Fatal("whitespace in a tool call's input changed the hash")
	}
	c := Delta(State{}, stepReq("s", call("grep", `{"pattern":"c"}`)))
	if a.next.MessagesSHA == c.next.MessagesSHA {
		t.Fatal("a different input hashed the same")
	}
}

func TestInvalidToolInputIsKeptAsText(t *testing.T) {
	r := Delta(State{}, stepReq("s", call("grep", `{"pattern":`)))
	e := r.Encode()
	var msgs []Message
	if err := json.Unmarshal(e.Messages, &msgs); err != nil {
		t.Fatal(err)
	}
	var s string
	if err := json.Unmarshal(msgs[0].ToolCalls[0].Input, &s); err != nil || s != `{"pattern":` {
		t.Fatalf("input = %s, %v", msgs[0].ToolCalls[0].Input, err)
	}
}

func TestMask(t *testing.T) {
	req := stepReq("key sk-1 in system", user("text sk-1"), call("run", `{"arg":"sk-1"}`), result("out sk-1"))
	r := Delta(State{}, req)
	r.Response = NewResponse(model.StepResponse{Text: "said sk-1",
		ToolCalls: []model.ToolCall{{ID: "c2", Name: "x", Input: json.RawMessage(`{"k":"sk-1"}`)}}})
	e := r.Mask(func(s string) string { return strings.ReplaceAll(s, "sk-1", "***") }).Encode()
	all := string(e.Messages) + string(e.Response) + *e.System + string(e.Tools)
	if strings.Contains(all, "sk-1") || strings.Count(all, "***") != 6 {
		t.Fatalf("masked row: %s", all)
	}
	if strings.Contains(req.Messages[0].Text, "***") {
		t.Fatal("Mask changed the request it was given")
	}
}

func TestEncodeCaps(t *testing.T) {
	big := strings.Repeat("x", ToolResultCap+10)
	huge := strings.Repeat("y", RowCap)
	tests := []struct {
		name      string
		prevBytes int64
		req       model.StepRequest
		truncated bool
		check     func(t *testing.T, msgs []Message)
	}{
		{name: "small row is kept whole", req: stepReq("s", user("hi")), check: func(t *testing.T, msgs []Message) {
			if len(msgs) != 1 || msgs[0].Text != "hi" {
				t.Fatalf("messages = %+v", msgs)
			}
		}},
		{name: "a large tool result is cut", req: stepReq("s", result(big)), truncated: true,
			check: func(t *testing.T, msgs []Message) {
				r := msgs[0].ToolResults[0]
				if len(r.Content) != ToolResultCap || r.TruncatedBytes != 10 {
					t.Fatalf("content %d bytes, truncated %d", len(r.Content), r.TruncatedBytes)
				}
			}},
		{name: "a cut never splits a rune", req: stepReq("s", result(strings.Repeat("x", ToolResultCap-1)+"é")), truncated: true,
			check: func(t *testing.T, msgs []Message) {
				r := msgs[0].ToolResults[0]
				if len(r.Content) != ToolResultCap-1 || r.TruncatedBytes != 2 {
					t.Fatalf("content %d bytes, truncated %d", len(r.Content), r.TruncatedBytes)
				}
			}},
		{name: "a row over the cap keeps only the structure", req: stepReq("s", user(huge), call("grep", `{"p":1}`)),
			truncated: true, check: func(t *testing.T, msgs []Message) {
				if len(msgs) != 2 || msgs[0].Text != Placeholder || msgs[1].ToolCalls[0].Name != "grep" ||
					string(msgs[1].ToolCalls[0].Input) != `"`+Placeholder+`"` {
					t.Fatalf("messages = %+v", msgs)
				}
			}},
		{name: "a run over its cap keeps no messages", prevBytes: RunCap + 1, req: stepReq("s", user("hi")), truncated: true,
			check: func(t *testing.T, msgs []Message) {
				if len(msgs) != 0 {
					t.Fatalf("messages = %+v", msgs)
				}
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := Delta(State{Bytes: tt.prevBytes}, tt.req)
			r.Response = Response{Text: "answer", Stop: model.StopEndTurn}
			e := r.Encode()
			var msgs []Message
			if err := json.Unmarshal(e.Messages, &msgs); err != nil {
				t.Fatal(err)
			}
			if e.Truncated != tt.truncated {
				t.Fatalf("truncated = %v", e.Truncated)
			}
			if !strings.Contains(string(e.Response), "answer") {
				t.Fatalf("response = %s", e.Response)
			}
			if tt.prevBytes > RunCap && (e.System != nil || e.Tools != nil) {
				t.Fatal("a run over its cap still stored the system prompt or tools")
			}
			if want := tt.prevBytes + int64(len(e.Messages)+len(e.Response)+len(e.Tools)) + int64(len(deref(e.System))); e.State.Bytes != want {
				t.Fatalf("bytes = %d, want %d", e.State.Bytes, want)
			}
			tt.check(t, msgs)
		})
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func TestKindValid(t *testing.T) {
	for k, want := range map[Kind]bool{KindAgentStep: true, KindReview: true, KindFallback: true, KindFollowUp: true, "other": false, "": false} {
		if k.Valid() != want {
			t.Fatalf("%q.Valid() = %v", k, !want)
		}
	}
}
