package review

import (
	"fmt"
	"strings"

	"github.com/home-operations/kritik/internal/contextpack"
)

// Input is everything the prompt is built from.
type Input struct {
	Repository string
	Number     int
	Title      string
	Author     string
	BaseRef    string
	Changed    []string
	Diff       string
	// Context is the runner's context pack, in stage order. It is spent
	// after the diff, so a huge diff crowds it out rather than the reverse.
	Context []contextpack.Chunk
	// BudgetTokens bounds the whole user message. Tokens are approximated
	// at four characters each, rounded conservatively; the budget is a
	// ceiling, not a target.
	BudgetTokens int
}

// charsPerToken is the conservative approximation used for budgeting.
const charsPerToken = 4

// System is the reviewer's standing instructions. It is deliberately short:
// the diff carries the specifics, and a long persona costs tokens on every
// review without changing the answer much.
const System = `You are kritik, a code reviewer for pull requests. You see the diff of the change and nothing else
about the repository, so say so when something cannot be judged from the diff alone rather than guessing.

Report only things a maintainer would act on: bugs, behaviour changes the description does not mention, security
and data-loss risks, breaking changes, missing error handling, and mistakes in configuration or infrastructure
files. Do not comment on style, formatting, naming, or anything a linter enforces. Do not praise. Do not restate
the diff.

After the diff you may get a context section: whole declarations from the PR head that the diff touches, the
definitions of identifiers used on changed lines, and callers of changed declarations. Use it to judge the change;
never report findings on context lines, only on lines the diff itself shows.

Each finding must point at one line in the new version of a changed file. Prefer few, precise findings over many
vague ones. If nothing is worth flagging, return an empty findings list and say so in the summary.`

// Build renders the user message within the budget. When the diff does not
// fit, it is cut at a file boundary and the message says which files were
// left out, so the model never sees a truncated hunk as if it were whole.
// Context chunks follow in stage order until the budget is spent; the
// number left out is returned with the omitted diff files.
func Build(in Input) (msg string, omitted []string, contextOmitted int) {
	var b strings.Builder
	fmt.Fprintf(&b, "Repository: %s\nPull request #%d: %s\nAuthor: %s\nBase branch: %s\nChanged files (%d):\n",
		in.Repository, in.Number, in.Title, in.Author, in.BaseRef, len(in.Changed))
	for _, p := range in.Changed {
		fmt.Fprintf(&b, "- %s\n", p)
	}
	b.WriteString("\nDiff (unified, base to head):\n\n")

	budget := in.BudgetTokens * charsPerToken
	if budget <= 0 {
		budget = 24_000 * charsPerToken
	}
	room := budget - b.Len() - 512 // headroom for the omission note
	diff, omitted := fitDiff(in.Diff, room)
	b.WriteString(diff)
	if len(omitted) > 0 {
		fmt.Fprintf(&b, "\n\n[%d file(s) omitted to fit the context budget: %s]\n", len(omitted), strings.Join(omitted, ", "))
	}
	contextOmitted = writeContext(&b, in.Context, budget)
	return b.String(), omitted, contextOmitted
}

// writeContext appends chunks while they fit under budget (in characters,
// counting what is already in b) and returns how many did not fit.
func writeContext(b *strings.Builder, chunks []contextpack.Chunk, budget int) int {
	if len(chunks) == 0 {
		return 0
	}
	const header = "\n\nContext (not part of the diff; do not report findings on these lines):\n"
	written := 0
	for i, c := range chunks {
		var section strings.Builder
		if written == 0 {
			section.WriteString(header)
		}
		fmt.Fprintf(&section, "\n### %s: %s lines %d-%d", c.Stage, c.Path, c.StartLine, c.EndLine)
		if c.Symbol != "" {
			fmt.Fprintf(&section, " (%s %s", c.Kind, c.Symbol)
			if c.Scope != "" {
				fmt.Fprintf(&section, " in %s", c.Scope)
			}
			section.WriteString(")")
		}
		if c.Ref != "" && c.Stage != contextpack.StageOverlay {
			fmt.Fprintf(&section, " for %s", c.Ref)
		}
		fmt.Fprintf(&section, "\n```%s\n%s\n```\n", c.Language, c.Text)
		if b.Len()+section.Len() > budget {
			return len(chunks) - i
		}
		b.WriteString(section.String())
		written++
	}
	return 0
}

// fitDiff keeps whole file sections of a unified diff until the next one
// would overflow room, and reports the paths it left out.
func fitDiff(diff string, room int) (string, []string) {
	if len(diff) <= room {
		return diff, nil
	}
	sections := splitFiles(diff)
	var b strings.Builder
	var omitted []string
	for _, s := range sections {
		if b.Len()+len(s.text) > room {
			omitted = append(omitted, s.path)
			continue
		}
		b.WriteString(s.text)
	}
	return b.String(), omitted
}

type fileSection struct {
	path string
	text string
}

// splitFiles cuts a unified diff at "diff --git" boundaries.
func splitFiles(diff string) []fileSection {
	var out []fileSection
	var cur *fileSection
	for l := range strings.SplitSeq(diff, "\n") {
		if strings.HasPrefix(l, "diff --git ") {
			if cur != nil {
				out = append(out, *cur)
			}
			cur = &fileSection{path: pathFromHeader(l)}
		}
		if cur == nil {
			cur = &fileSection{path: "?"}
		}
		cur.text += l + "\n"
	}
	if cur != nil {
		out = append(out, *cur)
	}
	return out
}

func pathFromHeader(l string) string {
	// "diff --git a/x/y b/x/y"
	if i := strings.LastIndex(l, " b/"); i >= 0 {
		return l[i+3:]
	}
	return strings.TrimPrefix(l, "diff --git ")
}
