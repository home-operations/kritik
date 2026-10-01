package review

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/home-operations/kritika/internal/textcut"
)

// Message is one comment in a thread, as the follow-up prompt shows it.
type Message struct {
	Author string
	Body   string
	When   time.Time
}

// FollowUpSystem is the reviewer's standing instructions when answering a
// thread rather than reviewing a diff.
const FollowUpSystem = `You are kritika, a code reviewer for pull requests, now answering a question in a pull request thread. You see
the diff, the context kritika gathered for its review, the findings it posted, and the thread. You cannot run code,
open other files, or change anything; say so when a request needs that.

Answer the last message directly and concisely in plain markdown without headings. Refer to lines of the diff by
path and line when it helps. If you were wrong in a finding, say so plainly. If the question cannot be answered
from what you see, say what is missing.`

// FollowUpSystemPrompt is FollowUpSystem with the rules and the
// repository's instructions appended, as SystemPrompt appends them to a
// review's.
func FollowUpSystemPrompt(rules []Rule, instructions []string) string {
	return withInstructions(FollowUpSystem, rules, "", instructions)
}

var followUpSchema = jsonSchema{
	Type: schemaObject,
	Properties: map[string]*jsonSchema{
		"reply": {Type: schemaString, Description: "The reply to post, in markdown without headings."},
	},
	Required: []string{"reply"},
}.mustMarshal()

// FollowUpSchema is the answer shape: one reply.
func FollowUpSchema() json.RawMessage { return slices.Clone(followUpSchema) }

// ParseFollowUp decodes the model's answer.
func ParseFollowUp(raw string) (string, error) {
	var out struct {
		Reply string `json:"reply"`
	}
	if err := json.NewDecoder(strings.NewReader(strings.TrimSpace(raw))).Decode(&out); err != nil {
		return "", fmt.Errorf("review: model output is not the expected JSON: %w", err)
	}
	out.Reply = strings.TrimSpace(out.Reply)
	if out.Reply == "" {
		return "", errors.New("review: model returned an empty reply")
	}
	return out.Reply, nil
}

// maxMessageChars bounds one thread message in the prompt.
const maxMessageChars = 2000

// BuildFollowUp renders the follow-up user message: the review input as
// Build renders it, then the findings kritika posted, then the thread with
// the message to answer last. The thread is never cut; the diff and
// context give way to it, since the question is what matters.
func BuildFollowUp(in Input, findings []Finding, thread []Message) string {
	var tail strings.Builder
	if len(findings) > 0 {
		fmt.Fprintf(&tail, "\n\nFindings kritika posted on this pull request (%d):\n", len(findings))
		for _, f := range findings {
			tail.WriteString(findingLine(f))
		}
	}
	tail.WriteString("\n\nThread, oldest first:\n")
	for i, m := range thread {
		body := strings.TrimSpace(m.Body)
		if len(body) > maxMessageChars {
			body = textcut.Prefix(body, maxMessageChars) + " …"
		}
		fmt.Fprintf(&tail, "\n--- %s", m.Author)
		if !m.When.IsZero() {
			fmt.Fprintf(&tail, " (%s)", m.When.UTC().Format("2006-01-02 15:04"))
		}
		if i == len(thread)-1 {
			tail.WriteString(" [answer this]")
		}
		tail.WriteString(" ---\n" + body + "\n")
	}
	if len(thread) > 0 {
		fmt.Fprintf(&tail, "\nReply to the last message from %s.\n", thread[len(thread)-1].Author)
	}

	budget := cmp.Or(in.BudgetTokens, DefaultBudgetTokens)
	in.BudgetTokens = max(budget-tail.Len()/charsPerToken, 2_000)
	msg, _, _ := Build(in)
	return msg + tail.String()
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 200 {
		return textcut.Prefix(s, 200) + " …"
	}
	return s
}

// FollowUpBody renders the reply as posted.
func FollowUpBody(reply, model string) string {
	return reply + fmt.Sprintf("\n\n<sub>kritika follow-up with %s.</sub>\n", model)
}

// LimitBody is posted once when a thread hits its follow-up rate limit.
const LimitBody = "kritika has answered the limit of follow-ups for this pull request in the past hour and will pick up again later.\n"

// DismissedBody is the reply to a dismissal of the finding a comment
// replies to.
const DismissedBody = "Dismissed: later reviews of this pull request will not raise this finding again.\n"

// DismissHintBody is the reply to a dismissal made anywhere but as a reply
// in one of kritika's finding threads, where there is no finding to
// dismiss.
const DismissHintBody = "`dismiss` works as a reply in one of kritika's finding threads: it dismisses that finding, " +
	"with the rest of the comment as the reason.\n"

// DismissUnknownBody is the reply to a dismissal of a finding kritika has
// no record of.
const DismissUnknownBody = "kritika has no record of this finding, so there is nothing to dismiss.\n"

// ReviewQueuedBody is the reply to a request for a review of headSHA:
// queued, or already queued or running.
func ReviewQueuedBody(headSHA string, already bool) string {
	if already {
		return fmt.Sprintf("A review of `%s` is already queued or running.\n", ShortSHA(headSHA))
	}
	return fmt.Sprintf("Reviewing `%s`; the summary lands on this pull request when it is done.\n", ShortSHA(headSHA))
}
