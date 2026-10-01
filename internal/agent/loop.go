package agent

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/home-operations/kritik/internal/model"
	"github.com/home-operations/kritik/internal/textcut"
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

// DefaultLimits are the fleet defaults. The configuration's agent defaults
// take theirs from here.
var DefaultLimits = Limits{MaxSteps: 60, MaxToolOutputBytes: 32 << 10, MaxTokens: 4_000_000, MaxOutputTokensPerStep: 16384}

// WithDefaults fills every zero-valued field of l from DefaultLimits,
// leaving any field the caller already set untouched.
func (l Limits) WithDefaults() Limits {
	l.MaxSteps = cmp.Or(l.MaxSteps, DefaultLimits.MaxSteps)
	l.MaxToolOutputBytes = cmp.Or(l.MaxToolOutputBytes, DefaultLimits.MaxToolOutputBytes)
	l.MaxTokens = cmp.Or(l.MaxTokens, DefaultLimits.MaxTokens)
	l.MaxOutputTokensPerStep = cmp.Or(l.MaxOutputTokensPerStep, DefaultLimits.MaxOutputTokensPerStep)
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
	// StopTruncated is a submit_review cut off at the step's output cap.
	StopTruncated StopReason = "truncated"
	StopCanceled  StopReason = "canceled"
	StopError     StopReason = "error"
)

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
	// Model is the model that answered the last step, empty before one
	// has.
	Model string
	// Err says why the Run stopped where the reason alone does not: Do
	// sets it to the Stepper's error for StopError, and a caller that
	// bounds ctx may set it to explain a StopCanceled.
	Err string
}

// Run is a bounded, read-only tool loop over a git commit's tree: on each
// step the Stepper may call one of Tools or Submit, until it submits, a
// limit is reached, or ctx ends.
type Run struct {
	Stepper model.Stepper
	Model   string
	System  string
	User    string
	Tools   []Tool
	// Submit is the submit_review tool; its schema is the review contract.
	// The loop never runs it: a call to Submit ends the Run.
	Submit model.ToolDef
	// Validate, if set, checks a Submit input against the contract beyond
	// its being JSON. A rejected input goes back to the model as the
	// tool's error, so it can correct it; on a forced step it ends the
	// Run as no submit, like invalid JSON.
	Validate func(input json.RawMessage) error
	Limits   Limits
	// OnStep, if set, is called after each step completes.
	OnStep func(StepEvent)
}

// nudgeText is appended once, as a user message, after the first turn with
// no tool call, before a second such turn ends the Run.
const nudgeText = "call submit_review"

// submitNowText ends the last user message of a step the model must submit
// on. It is told rather than forced through tool_choice, which the newest
// models reject.
const submitNowText = "Call submit_review now with the summary and findings you have, and call nothing else."

// noResponseText replaces an empty Text on an appended assistant message, so
// the conversation never carries a message with neither text nor tool calls.
const noResponseText = "(no response)"

// checkSubmit says why input is not an acceptable Submit input.
func (r Run) checkSubmit(input json.RawMessage) error {
	var scratch any
	if err := json.Unmarshal(input, &scratch); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	if r.Validate != nil {
		return r.Validate(input)
	}
	return nil
}

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
		if forced {
			last := &messages[len(messages)-1]
			if last.Text != "" {
				last.Text += "\n\n"
			}
			last.Text += submitNowText
		}

		req := model.StepRequest{
			Model:     r.Model,
			System:    r.System,
			Messages:  messages,
			Tools:     toolDefs,
			MaxTokens: limits.MaxOutputTokensPerStep,
		}

		start := time.Now()
		resp, err := r.Stepper.Step(ctx, req)
		if err != nil {
			if ctx.Err() != nil {
				result.Stop = StopCanceled
				return result
			}
			// The gateway's count is the one the caps see: its refusal ends
			// the run as the loop's own budget check would.
			result.Stop = StopError
			if errors.Is(err, model.ErrBudget) {
				result.Stop = StopBudget
			}
			result.Err = err.Error()
			return result
		}
		result.Steps++
		result.Usage = result.Usage.Add(resp.Usage)
		result.Model = resp.Model
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
			text := cmp.Or(resp.Text, noResponseText)
			messages = append(messages, model.Message{Role: model.RoleAssistant, Text: text})
			messages = append(messages, model.Message{Role: model.RoleUser, Text: nudgeText})
			continue
		}

		var toolResults []model.ToolResult
		var submitted json.RawMessage
		var submitFailed, truncated bool

		for _, call := range resp.ToolCalls {
			event.Tools = append(event.Tools, call.Name)
			result.ToolCalls[call.Name]++

			if call.Name == r.Submit.Name {
				if err := r.checkSubmit(call.Input); err != nil {
					// A submission the output cap cut off is not sent back
					// as an error: the model would re-emit it whole and be
					// cut off again, step after step.
					if resp.Stop == model.StopMaxTokens {
						truncated = true
						break
					}
					if forced {
						submitFailed = true
						break
					}
					toolResults = append(toolResults, model.ToolResult{
						CallID: call.ID, IsError: true,
						Content: textcut.Truncate(fmt.Sprintf("agent: submit_review: %s", err), limits.MaxToolOutputBytes),
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
					Content: textcut.Truncate(fmt.Sprintf("agent: unknown tool %q", call.Name), limits.MaxToolOutputBytes),
				})
				continue
			}
			out, err := tool.Run(ctx, call.Input)
			if err != nil {
				toolResults = append(toolResults, model.ToolResult{
					CallID: call.ID, IsError: true,
					Content: textcut.Truncate(err.Error(), limits.MaxToolOutputBytes),
				})
				continue
			}
			out = textcut.Truncate(out, limits.MaxToolOutputBytes)
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
		if truncated {
			result.Stop = StopTruncated
			result.Err = fmt.Sprintf("submit_review was cut off at the %d output tokens a step may produce", limits.MaxOutputTokensPerStep)
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
