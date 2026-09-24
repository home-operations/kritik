package review

import (
	"fmt"
	"strings"
)

// Marker is the hidden HTML comment that identifies kritik's sticky comment
// on a pull request. It is matched together with the comment's author, never
// alone, so a PR author cannot plant one.
func Marker(number int) string {
	return fmt.Sprintf("<!-- kritik:pr-%d -->", number)
}

// StickyBody renders the summary comment. Inline findings are posted as
// review comments; the sticky comment lists them so a reader who lands on
// the conversation tab sees everything in one place.
func StickyBody(number int, res Result, model string, omitted []string, dropped int) string {
	var b strings.Builder
	b.WriteString(Marker(number))
	b.WriteString("\n### kritik review\n\n")
	b.WriteString(res.Summary)
	b.WriteString("\n")
	if len(res.Findings) == 0 {
		b.WriteString("\nNothing worth flagging in this diff.\n")
	} else {
		fmt.Fprintf(&b, "\n**%d finding(s)**\n\n", len(res.Findings))
		for _, f := range res.Findings {
			fmt.Fprintf(&b, "- %s `%s:%d` %s\n", badge(f.Severity), f.Path, f.Line, f.Title)
		}
	}
	var notes []string
	if len(omitted) > 0 {
		notes = append(notes, fmt.Sprintf("%d file(s) were omitted from the diff to fit the context budget", len(omitted)))
	}
	if dropped > 0 {
		notes = append(notes, fmt.Sprintf("%d finding(s) could not be anchored to a changed line and were dropped", dropped))
	}
	if len(notes) > 0 {
		b.WriteString("\n_" + strings.Join(notes, "; ") + "._\n")
	}
	fmt.Fprintf(&b, "\n<sub>Reviewed by kritik with %s. Reviews never block a merge.</sub>\n", model)
	return b.String()
}

// InlineBody renders one finding as an inline review comment.
func InlineBody(f Finding) string {
	return fmt.Sprintf("%s **%s**\n\n%s", badge(f.Severity), f.Title, f.Body)
}

func badge(s Severity) string {
	switch s {
	case SeverityError:
		return "🔴"
	case SeverityWarning:
		return "🟡"
	default:
		return "🔵"
	}
}
