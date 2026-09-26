package transcript

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/home-operations/kritik/internal/model"
)

// StoredRow is a model_calls row as read back. System and Tools are nil
// where the row left them unchanged.
type StoredRow struct {
	ID                string
	Kind              Kind
	Step              int
	ReviewID          string
	RunnerRunID       string
	FollowupCommentID int64
	Model             string
	Upstream          string
	System            *string
	Tools             *[]model.ToolDef
	MessagesFrom      int
	Messages          []Message
	Response          Response
	Usage             model.Usage
	CostUSD           float64
	Duration          time.Duration
	Error             string
	Truncated         bool
	CreatedAt         time.Time
}

// Conversation is a set of model calls as the transcript view shows them:
// the system prompt and tools the first call carried, then one turn per
// call.
type Conversation struct {
	System string
	Tools  []model.ToolDef
	Turns  []Turn
}

// Turn is one model call: the request messages new in it, from index
// MessagesFrom of its request, and what the model answered. System and
// Tools are set when the call changed them from the turn before; Reset
// says an agent step's request did not extend what its run had recorded,
// so Messages is the whole request.
type Turn struct {
	ID                string
	Kind              Kind
	Step              int
	RunnerRunID       string
	FollowupCommentID int64
	Model             string
	Upstream          string
	System            *string
	Tools             *[]model.ToolDef
	Reset             bool
	MessagesFrom      int
	Messages          []Message
	Response          Response
	Usage             model.Usage
	CostUSD           float64
	Duration          time.Duration
	Error             string
	Truncated         bool
	CreatedAt         time.Time
}

// Rebuild turns rows, in the order they were recorded, into a
// conversation.
func Rebuild(rows []StoredRow) Conversation {
	var c Conversation
	var system *string
	var tools *[]model.ToolDef
	runs := map[string]bool{}
	for _, r := range rows {
		t := Turn{
			ID: r.ID, Kind: r.Kind, Step: r.Step, RunnerRunID: r.RunnerRunID, FollowupCommentID: r.FollowupCommentID,
			Model: r.Model, Upstream: r.Upstream, MessagesFrom: r.MessagesFrom, Messages: r.Messages, Response: r.Response,
			Usage: r.Usage, CostUSD: r.CostUSD, Duration: r.Duration, Error: r.Error, Truncated: r.Truncated, CreatedAt: r.CreatedAt,
		}
		if r.System != nil {
			switch {
			case system == nil:
				c.System = *r.System
			case *r.System != *system:
				t.System = r.System
			}
			system = r.System
		}
		if r.Tools != nil {
			switch {
			case tools == nil:
				c.Tools = *r.Tools
			case !slices.EqualFunc(*r.Tools, *tools, sameTool):
				t.Tools = r.Tools
			}
			tools = r.Tools
		}
		if r.Kind == KindAgentStep {
			t.Reset = r.MessagesFrom == 0 && runs[r.RunnerRunID]
			runs[r.RunnerRunID] = true
		}
		c.Turns = append(c.Turns, t)
	}
	return c
}

func sameTool(a, b model.ToolDef) bool {
	return a.Name == b.Name && a.Description == b.Description && bytes.Equal(a.InputSchema, b.InputSchema)
}

// DecodeMessages decodes a row's messages column.
func DecodeMessages(raw []byte) ([]Message, error) {
	var msgs []Message
	if err := json.Unmarshal(raw, &msgs); err != nil {
		return nil, fmt.Errorf("transcript: decode messages: %w", err)
	}
	return msgs, nil
}

// DecodeResponse decodes a row's response column.
func DecodeResponse(raw []byte) (Response, error) {
	var r Response
	if err := json.Unmarshal(raw, &r); err != nil {
		return Response{}, fmt.Errorf("transcript: decode response: %w", err)
	}
	return r, nil
}

// DecodeTools decodes a row's tools column; nil (SQL NULL) is nil, the
// tools left unchanged.
func DecodeTools(raw []byte) (*[]model.ToolDef, error) {
	if raw == nil {
		return nil, nil
	}
	var tools []Tool
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, fmt.Errorf("transcript: decode tools: %w", err)
	}
	defs := make([]model.ToolDef, len(tools))
	for i, t := range tools {
		defs[i] = model.ToolDef{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema}
	}
	return &defs, nil
}
