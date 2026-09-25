package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/home-operations/kritik/internal/model"
)

// Limits bounds a Run: how many steps it may take, how much of a tool's
// output it keeps, and the token and per-step output budgets that force an
// early submit_review.
type Limits struct {
	MaxSteps               int
	MaxToolOutputBytes     int
	MaxTokens              int64 // total prompt+output budget
	MaxOutputTokensPerStep int64
}

// WithDefaults fills every zero-valued field of l with the fleet default,
// leaving any field the caller already set untouched.
func (l Limits) WithDefaults() Limits {
	if l.MaxSteps == 0 {
		l.MaxSteps = 60
	}
	if l.MaxToolOutputBytes == 0 {
		l.MaxToolOutputBytes = 32 << 10
	}
	if l.MaxTokens == 0 {
		l.MaxTokens = 4_000_000
	}
	if l.MaxOutputTokensPerStep == 0 {
		l.MaxOutputTokensPerStep = 8192
	}
	return l
}

// StopReason is why a Run ended.
type StopReason string

// Stop reasons a Run can end with.
const (
	StopSubmitted StopReason = "submitted"
	StopMaxSteps  StopReason = "max_steps"
	StopBudget    StopReason = "budget"
	StopNoSubmit  StopReason = "no_submit"
	StopCanceled  StopReason = "canceled"
	StopError     StopReason = "error"
)

// Valid reports whether s is a stop reason a Run can end with.
func (s StopReason) Valid() bool {
	switch s {
	case StopSubmitted, StopMaxSteps, StopBudget, StopNoSubmit, StopCanceled, StopError:
		return true
	}
	return false
}

// Tool is one function the loop offers the model.
type Tool interface {
	Def() model.ToolDef
	Run(ctx context.Context, input json.RawMessage) (string, error)
}

// StepEvent reports one completed step, for a timeline or a heartbeat.
type StepEvent struct {
	Index       int
	Tools       []string
	Duration    time.Duration
	OutputBytes int
	Usage       model.Usage
}

// Result is how a Run ended.
type Result struct {
	Stop StopReason
	// Submitted is the submit_review input, set iff Stop == StopSubmitted.
	Submitted json.RawMessage
	Steps     int
	ToolCalls map[string]int
	Usage     model.Usage
	CostUSD   float64
	// Err is the Stepper's error message, set iff Stop == StopError.
	Err string
}

// Run is a bounded, read-only tool loop over a git commit's tree: on each
// step the Stepper may call one of Tools or Submit, until it submits, a
// limit is reached, or ctx ends.
type Run struct {
	Stepper   model.Stepper
	Model     string
	Fallbacks []string
	System    string
	User      string
	Tools     []Tool
	// Submit is the submit_review tool; its schema is the review contract.
	// The loop never runs it: a call to Submit ends the Run.
	Submit model.ToolDef
	Limits Limits
	// OnStep, if set, is called after each step completes.
	OnStep func(StepEvent)
}

// nudgeText is appended once, as a user message, after the first turn with
// no tool call, before a second such turn ends the Run.
const nudgeText = "call submit_review"

// noResponseText replaces an empty Text on an appended assistant message, so
// the conversation never carries a message with neither text nor tool calls.
const noResponseText = "(no response)"

// Do runs the loop to completion.
func (r Run) Do(ctx context.Context) Result {
	limits := r.Limits.WithDefaults()

	toolDefs := make([]model.ToolDef, 0, len(r.Tools)+1)
	toolsByName := make(map[string]Tool, len(r.Tools))
	for _, t := range r.Tools {
		d := t.Def()
		toolDefs = append(toolDefs, d)
		toolsByName[d.Name] = t
	}
	toolDefs = append(toolDefs, r.Submit)

	messages := []model.Message{{Role: model.RoleUser, Text: r.User}}

	result := Result{ToolCalls: map[string]int{}}
	nudged := false

	for step := 0; ; step++ {
		if err := ctx.Err(); err != nil {
			result.Stop = StopCanceled
			return result
		}

		total := result.Usage.Prompt() + result.Usage.Output
		if total >= limits.MaxTokens {
			result.Stop = StopBudget
			return result
		}

		lastStep := step == limits.MaxSteps-1
		forced := lastStep || total*10 >= limits.MaxTokens*9

		req := model.StepRequest{
			Model:     r.Model,
			Fallbacks: r.Fallbacks,
			System:    r.System,
			Messages:  messages,
			Tools:     toolDefs,
			MaxTokens: limits.MaxOutputTokensPerStep,
		}
		if forced {
			req.ToolChoice = model.ToolChoice{Mode: model.ToolChoiceTool, Name: r.Submit.Name}
		}

		start := time.Now()
		resp, err := r.Stepper.Step(ctx, req)
		if err != nil {
			if ctx.Err() != nil {
				result.Stop = StopCanceled
				return result
			}
			result.Stop = StopError
			result.Err = err.Error()
			return result
		}
		result.Steps++
		result.Usage = result.Usage.Add(resp.Usage)
		result.CostUSD += resp.CostUSD

		event := StepEvent{Index: step, Usage: resp.Usage}

		if len(resp.ToolCalls) == 0 {
			event.Duration = time.Since(start)
			r.reportStep(event)

			if lastStep {
				result.Stop = StopMaxSteps
				return result
			}
			if nudged {
				result.Stop = StopNoSubmit
				return result
			}
			nudged = true
			text := resp.Text
			if text == "" {
				text = noResponseText
			}
			messages = append(messages, model.Message{Role: model.RoleAssistant, Text: text})
			messages = append(messages, model.Message{Role: model.RoleUser, Text: nudgeText})
			continue
		}

		var toolResults []model.ToolResult
		var submitted json.RawMessage
		var submitFailed bool

		for _, call := range resp.ToolCalls {
			event.Tools = append(event.Tools, call.Name)
			result.ToolCalls[call.Name]++

			if call.Name == r.Submit.Name {
				var scratch any
				if err := json.Unmarshal(call.Input, &scratch); err != nil {
					if forced {
						submitFailed = true
						break
					}
					toolResults = append(toolResults, model.ToolResult{
						CallID: call.ID, IsError: true,
						Content: truncate(fmt.Sprintf("agent: submit_review: invalid JSON: %s", err), limits.MaxToolOutputBytes),
					})
					continue
				}
				submitted = call.Input
				break
			}

			tool, ok := toolsByName[call.Name]
			if !ok {
				toolResults = append(toolResults, model.ToolResult{
					CallID: call.ID, IsError: true,
					Content: truncate(fmt.Sprintf("agent: unknown tool %q", call.Name), limits.MaxToolOutputBytes),
				})
				continue
			}
			out, err := tool.Run(ctx, call.Input)
			if err != nil {
				toolResults = append(toolResults, model.ToolResult{
					CallID: call.ID, IsError: true,
					Content: truncate(err.Error(), limits.MaxToolOutputBytes),
				})
				continue
			}
			out = truncate(out, limits.MaxToolOutputBytes)
			event.OutputBytes += len(out)
			toolResults = append(toolResults, model.ToolResult{CallID: call.ID, Content: out})
		}

		event.Duration = time.Since(start)
		r.reportStep(event)

		if submitted != nil {
			result.Stop = StopSubmitted
			result.Submitted = submitted
			return result
		}
		if submitFailed {
			result.Stop = StopNoSubmit
			return result
		}
		if lastStep {
			result.Stop = StopMaxSteps
			return result
		}

		messages = append(messages, model.Message{Role: model.RoleAssistant, Text: resp.Text, ToolCalls: resp.ToolCalls})
		messages = append(messages, model.Message{Role: model.RoleUser, ToolResults: toolResults})
	}
}

func (r Run) reportStep(e StepEvent) {
	if r.OnStep != nil {
		r.OnStep(e)
	}
}
