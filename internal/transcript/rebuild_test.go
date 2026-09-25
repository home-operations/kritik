package transcript

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/home-operations/kritik/internal/model"
)

// store records reqs as consecutive agent steps of run and decodes the
// rows the way the store reads them back.
func store(t *testing.T, run string, reqs ...model.StepRequest) []StoredRow {
	t.Helper()
	var prev State
	rows := make([]StoredRow, 0, len(reqs))
	for i, req := range reqs {
		r := delta(prev, req)
		r.Response = Response{Text: "step", Stop: model.StopToolUse}
		e := r.Encode()
		prev = e.State
		rows = append(rows, decoded(t, e, StoredRow{Kind: KindAgentStep, Step: i, RunnerRunID: run}))
	}
	return rows
}

func decoded(t *testing.T, e Encoded, row StoredRow) StoredRow {
	t.Helper()
	var err error
	row.System, row.MessagesFrom, row.Truncated = e.System, e.MessagesFrom, e.Truncated
	if row.Tools, err = DecodeTools(e.Tools); err != nil {
		t.Fatal(err)
	}
	if row.Messages, err = DecodeMessages(e.Messages); err != nil {
		t.Fatal(err)
	}
	if row.Response, err = DecodeResponse(e.Response); err != nil {
		t.Fatal(err)
	}
	return row
}

// full is the conversation a run's turns add up to.
func full(turns []Turn) []Message {
	var msgs []Message
	for _, turn := range turns {
		if turn.Reset {
			msgs = nil
		}
		msgs = append(msgs[:turn.MessagesFrom], turn.Messages...)
	}
	return msgs
}

func texts(msgs []Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.Text)
	}
	return out
}

func TestRebuild(t *testing.T) {
	a := stepReq("sys", user("review"))
	b := stepReq("sys", user("review"), user("two"))
	c := stepReq("sys", user("review"), user("two"), user("three"))
	grepTool := []model.ToolDef{{Name: "grep", Description: "search", InputSchema: json.RawMessage(`{"type":"object"}`)}}

	single := delta(State{}, model.StepRequest{System: "review sys", Messages: []model.Message{user("the diff")},
		Tools: []model.ToolDef{{Name: "findings", InputSchema: json.RawMessage(`{"type":"object"}`)}}})
	single.Response = response(model.StepResponse{ToolCalls: []model.ToolCall{{ID: "f", Name: "findings", Input: json.RawMessage(`{"findings":[]}`)}}})
	singleRow := decoded(t, single.Encode(), StoredRow{Kind: KindReview, Usage: model.Usage{Input: 10, Output: 5}, CostUSD: 0.01,
		Duration: time.Second, Model: "m"})

	truncated := delta(State{}, stepReq("sys", result(string(make([]byte, ToolResultCap+1)))))
	truncatedRow := decoded(t, truncated.Encode(), StoredRow{Kind: KindAgentStep, RunnerRunID: "r"})

	tests := []struct {
		name    string
		rows    []StoredRow
		system  string
		tools   []model.ToolDef
		want    []string
		resets  []bool
		changed []bool
	}{
		{name: "consecutive steps add up to the last request", rows: store(t, "r", a, b, c), system: "sys", tools: grepTool,
			want: []string{"review", "two", "three"}, resets: []bool{false, false, false}, changed: []bool{false, false, false}},
		{name: "a reset replaces the conversation", rows: store(t, "r", a, b, stepReq("sys", user("compacted"), user("four"))),
			system: "sys", tools: grepTool, want: []string{"compacted", "four"}, resets: []bool{false, false, true},
			changed: []bool{false, false, false}},
		{name: "a system change mid-run is on its turn", rows: store(t, "r", a, stepReq("sys2", user("review"), user("two"))),
			system: "sys", tools: grepTool, want: []string{"review", "two"}, resets: []bool{false, false}, changed: []bool{false, true}},
		{name: "a new run is not a reset", rows: append(store(t, "r1", a, b), store(t, "r2", a)...), system: "sys", tools: grepTool,
			want: []string{"review"}, resets: []bool{false, false, false}, changed: []bool{false, false, false}},
		{name: "a single-shot call is one turn", rows: []StoredRow{singleRow}, system: "review sys",
			tools: []model.ToolDef{{Name: "findings", InputSchema: json.RawMessage(`{"type":"object"}`)}},
			want:  []string{"the diff"}, resets: []bool{false}, changed: []bool{false}},
		{name: "a truncated row says so", rows: []StoredRow{truncatedRow}, system: "sys", tools: grepTool,
			want: []string{""}, resets: []bool{false}, changed: []bool{false}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conv := Rebuild(tt.rows)
			if conv.System != tt.system || !reflect.DeepEqual(conv.Tools, tt.tools) {
				t.Fatalf("system %q tools %+v", conv.System, conv.Tools)
			}
			if len(conv.Turns) != len(tt.rows) {
				t.Fatalf("%d turns for %d rows", len(conv.Turns), len(tt.rows))
			}
			last := conv.Turns
			if tt.name == "a new run is not a reset" {
				last = conv.Turns[2:]
			}
			if got := texts(full(last)); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("conversation = %q, want %q", got, tt.want)
			}
			for i, turn := range conv.Turns {
				if turn.Reset != tt.resets[i] || (turn.System != nil) != tt.changed[i] {
					t.Fatalf("turn %d: reset %v, system changed %v", i, turn.Reset, turn.System != nil)
				}
				if turn.Truncated != tt.rows[i].Truncated || turn.Kind != tt.rows[i].Kind {
					t.Fatalf("turn %d = %+v", i, turn)
				}
			}
		})
	}

	turn := Rebuild([]StoredRow{singleRow}).Turns[0]
	if turn.Usage.Input != 10 || turn.CostUSD != 0.01 || turn.Duration != time.Second || turn.Model != "m" ||
		string(turn.Response.ToolCalls[0].Input) != `{"findings":[]}` {
		t.Fatalf("single-shot turn = %+v", turn)
	}
	if !truncatedRow.Truncated || Rebuild([]StoredRow{truncatedRow}).Turns[0].Messages[0].ToolResults[0].TruncatedBytes != 1 {
		t.Fatal("the cut tool result lost its marker")
	}
}

func TestDecodeToolsNullIsUnchanged(t *testing.T) {
	tools, err := DecodeTools(nil)
	if err != nil || tools != nil {
		t.Fatalf("tools = %v, %v", tools, err)
	}
	tools, err = DecodeTools(json.RawMessage(`[]`))
	if err != nil || tools == nil || len(*tools) != 0 {
		t.Fatalf("tools = %v, %v", tools, err)
	}
	if _, err := DecodeMessages(json.RawMessage(`{`)); err == nil {
		t.Fatal("bad messages decoded")
	}
}
