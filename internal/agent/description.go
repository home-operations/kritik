package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/home-operations/kritika/internal/model"
	"github.com/home-operations/kritika/internal/textcut"
)

var readDescriptionSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"issue": {"type": "integer", "description": "Number of a linked issue the prompt lists, to read its body instead of the description."}
	},
	"additionalProperties": false
}`)

type readDescriptionTool struct {
	body     string
	issues   map[int]string
	maxBytes int
}

// ReadDescriptionTool returns the pull request description whole, or the
// body of one of the linked issues, by number, where the prompt shows
// each only up to its share of the budget. Its output is capped at
// maxBytes.
func ReadDescriptionTool(body string, issues map[int]string, maxBytes int) Tool {
	return &readDescriptionTool{body: body, issues: issues, maxBytes: maxBytes}
}

func (dt *readDescriptionTool) Def() model.ToolDef {
	return model.ToolDef{
		Name:        "read_description",
		Description: "Read the whole pull request description, or a linked issue's body by number, where the prompt shows it cut.",
		InputSchema: readDescriptionSchema,
	}
}

func (dt *readDescriptionTool) Run(_ context.Context, input json.RawMessage) (string, error) {
	var req struct {
		Issue int `json:"issue"`
	}
	if err := decodeInput(input, &req); err != nil {
		return "", fmt.Errorf("agent: read_description: %w", err)
	}
	text := dt.body
	if req.Issue != 0 {
		var ok bool
		if text, ok = dt.issues[req.Issue]; !ok {
			return "", fmt.Errorf("agent: read_description: issue #%d is not one the prompt lists", req.Issue)
		}
	}
	if text = strings.TrimSpace(text); text == "" {
		return "(empty)", nil
	}
	return textcut.Truncate(text, dt.maxBytes), nil
}
