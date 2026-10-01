package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/home-operations/kritik/internal/agent"
	"github.com/home-operations/kritik/internal/contextpack"
	"github.com/home-operations/kritik/internal/model"
	"github.com/home-operations/kritik/internal/textcut"
)

// similarTimeout bounds one similar-code request to the gateway, which
// may wait for the account's embedding slot.
const similarTimeout = time.Minute

// maxSimilarBody bounds the gateway's answer: ten chunks.
const maxSimilarBody = 1 << 20

// searchCalls is how many times one run may call search_code.
const searchCalls = 10

// similarCode asks the gateway for the chunks of the repository's index
// nearest each query, outside the excluded paths.
func similarCode(ctx context.Context, gatewayURL, token string, req contextpack.SimilarRequest) (contextpack.SimilarResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, similarTimeout)
	defer cancel()
	body, err := json.Marshal(req)
	if err != nil {
		return contextpack.SimilarResponse{}, fmt.Errorf("runner: similar code: %w", err)
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(gatewayURL, "/")+"/v1/similar", bytes.NewReader(body))
	if err != nil {
		return contextpack.SimilarResponse{}, fmt.Errorf("runner: similar code: %w", err)
	}
	hreq.Header.Set("Authorization", "Bearer "+token)
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(hreq)
	if err != nil {
		return contextpack.SimilarResponse{}, fmt.Errorf("runner: similar code: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxSimilarBody))
	if err != nil {
		return contextpack.SimilarResponse{}, fmt.Errorf("runner: similar code: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return contextpack.SimilarResponse{}, fmt.Errorf("runner: similar code: %s: %s", resp.Status, bytes.TrimSpace(answer))
	}
	var out contextpack.SimilarResponse
	if err := json.Unmarshal(answer, &out); err != nil {
		return contextpack.SimilarResponse{}, fmt.Errorf("runner: similar code: %w", err)
	}
	return out, nil
}

// hunkQueries is what stage 4 embeds: the head-side text of the diff's
// first hunks, each led by its path and cut to the gateway's bound.
func hunkQueries(diff string) []string {
	hunks := contextpack.Hunks(diff)
	if len(hunks) > contextpack.SimilarQueries {
		hunks = hunks[:contextpack.SimilarQueries]
	}
	out := make([]string, len(hunks))
	for i, h := range hunks {
		out[i] = textcut.Prefix(h.Path+"\n"+h.Text, contextpack.SimilarQueryChars)
	}
	return out
}

var searchSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"query": {"type": "string", "description": "What to look for: a description of the code, or a snippet like it."}
	},
	"required": ["query"],
	"additionalProperties": false
}`)

// searchTool is search_code: the agent's own similar-code search, over
// the same gateway route as stage 4, bounded to searchCalls per run.
type searchTool struct {
	gatewayURL, token string
	exclude           []string
	maxBytes          int
	calls             int
}

func (s *searchTool) Def() model.ToolDef {
	return model.ToolDef{
		Name:        "search_code",
		Description: "Find the chunks of the repository most similar to a description or a snippet, outside the changed files.",
		InputSchema: searchSchema,
	}
}

func (s *searchTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	var req struct {
		Query string `json:"query"`
	}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &req); err != nil {
			return "", fmt.Errorf("agent: search_code: %w", err)
		}
	}
	if strings.TrimSpace(req.Query) == "" {
		return "", errors.New("agent: search_code: query is empty")
	}
	if s.calls >= searchCalls {
		return "", fmt.Errorf("agent: search_code: the limit of %d searches per review is reached", searchCalls)
	}
	s.calls++
	out, err := similarCode(ctx, s.gatewayURL, s.token, contextpack.SimilarRequest{
		Queries: []string{textcut.Prefix(req.Query, contextpack.SimilarQueryChars)}, Exclude: s.exclude,
	})
	if err != nil {
		return "", fmt.Errorf("agent: search_code: %w", err)
	}
	if len(out.Chunks) == 0 {
		return "No similar code found.", nil
	}
	var b strings.Builder
	for i, c := range out.Chunks {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "%s:%d-%d", c.Path, c.StartLine, c.EndLine)
		if c.Symbol != "" {
			fmt.Fprintf(&b, " (%s %s)", c.Kind, c.Symbol)
		}
		fmt.Fprintf(&b, " %s\n%s", c.Ref, c.Text)
	}
	return textcut.Truncate(b.String(), s.maxBytes), nil
}

var _ agent.Tool = (*searchTool)(nil)
