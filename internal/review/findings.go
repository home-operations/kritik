// Package review turns a context pack into a prompt, a model answer into
// findings, and findings into the comments kritik posts. It knows nothing
// about forges or models beyond their interfaces.
//
// # Templates
//
// Comments are rendered from Jinja2 templates (gonja) that a repository may
// replace. Templates run in a bounded subset of the language, and one that
// steps outside it falls back to kritik's default with a note:
//
//   - Statements: if/elif/else, for (with else, break, continue; not
//     recursive), set in the expression form {% set name = expression %},
//     raw and autoescape. Not available: block-form set, filter, call,
//     macro, with, block, do, trans, include, import, from and extends.
//   - Operators: comparisons, and, or, not, in, is, -, /, //, % and binary
//   - on numbers. Not available: + on strings or lists, unary +, ~, * and
//     **. Write parts side by side instead of concatenating them.
//   - Globals: range (at most 10,000 items), dict, cycler and joiner.
//     Filters: gonja's built-ins except random and format. Methods: string
//     methods and the read-only dict (keys, values, items, get, copy) and
//     list (copy) methods.
//   - Limits: output of 64 KiB, marker included; 20,000 loop iterations
//     per render, charged when a loop starts; values kept by set or
//     returned by a filter or method of at most 64 KiB; a filter or method
//     call whose input or estimated allocation exceeds 256 KiB is refused,
//     widths are at most 65,536, slice and batch counts at most 20,000, and
//     indent, tojson's indent and expandtabs at most 16; literals of at most
//     256 items; nesting of at most 64; a template source of at most
//     64 KiB; two seconds per render. Identifiers starting with __kritik_
//     are reserved.
//
// Trim-blocks and lstrip-blocks are on. The context keys are documented on
// RenderData; see templates/ for the defaults.
package review

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Severity of a finding, in the order the summary lists them.
type Severity string

// Severities a finding may carry.
const (
	SeverityBlocking  Severity = "blocking"
	SeverityImportant Severity = "important"
	SeverityNit       Severity = "nit"
)

var severities = []Severity{SeverityBlocking, SeverityImportant, SeverityNit}

// Valid reports whether s is one of the severities.
func (s Severity) Valid() bool { return slices.Contains(severities, s) }

// Rank orders severities most serious first; an invalid one sorts last.
func (s Severity) Rank() int {
	if i := slices.Index(severities, s); i >= 0 {
		return i
	}
	return len(severities)
}

// Summary is the review's overall judgement for the sticky comment.
type Summary struct {
	Take   string   `json:"take"`
	Praise []string `json:"praise"`
}

// maxPraise bounds Summary.Praise; the schema says so and Parse enforces it.
const maxPraise = 3

// Finding is one thing the reviewer wants a human to look at, anchored to a
// line on the head side of the diff.
type Finding struct {
	Path         string   `json:"path"`
	Line         int      `json:"line"`
	Severity     Severity `json:"severity"`
	Title        string   `json:"title"`
	Explanation  string   `json:"explanation"`
	SuggestedFix string   `json:"suggested_fix,omitempty"`
}

// Result is the whole answer.
type Result struct {
	Summary  Summary   `json:"summary"`
	Findings []Finding `json:"findings"`
}

// Counts is the number of findings at each severity.
type Counts struct {
	Blocking, Important, Nit int
}

// Counts tallies the findings by severity.
func (r Result) Counts() Counts {
	var c Counts
	for _, f := range r.Findings {
		switch f.Severity {
		case SeverityBlocking:
			c.Blocking++
		case SeverityImportant:
			c.Important++
		case SeverityNit:
			c.Nit++
		}
	}
	return c
}

// DropReason says why Parse discarded a finding.
type DropReason string

// Reasons a finding is dropped.
const (
	DropUnanchored  DropReason = "unanchored"
	DropIncomplete  DropReason = "incomplete"
	DropNoFix       DropReason = "no_suggested_fix"
	DropBadSeverity DropReason = "bad_severity"
)

// Valid reports whether r is one of the drop reasons.
func (r DropReason) Valid() bool {
	switch r {
	case DropUnanchored, DropIncomplete, DropNoFix, DropBadSeverity:
		return true
	}
	return false
}

// Dropped is a finding Parse discarded, with the reason.
type Dropped struct {
	Finding Finding
	Reason  DropReason
}

// ParseOptions tune what Parse accepts.
type ParseOptions struct {
	// RequireSuggestedFix drops findings that carry no suggested fix.
	RequireSuggestedFix bool
}

// Field names of the contract, shared by its JSON Schema and the template
// context, so a template sees the names the model was asked for.
const (
	keySummary      = "summary"
	keyTake         = "take"
	keyPraise       = "praise"
	keyFindings     = "findings"
	keyPath         = "path"
	keyLine         = "line"
	keySeverity     = "severity"
	keyTitle        = "title"
	keyExplanation  = "explanation"
	keySuggestedFix = "suggested_fix"
)

// JSON Schema types the answer shapes use more than once.
const schemaObject, schemaString = "object", "string"

// jsonSchema is the subset of JSON Schema kritik's answer shapes use.
type jsonSchema struct {
	Type        string                 `json:"type"`
	Description string                 `json:"description,omitempty"`
	Enum        []string               `json:"enum,omitempty"`
	Properties  map[string]*jsonSchema `json:"properties,omitempty"`
	Items       *jsonSchema            `json:"items,omitempty"`
	Required    []string               `json:"required,omitempty"`
	MaxItems    int                    `json:"maxItems,omitempty"`
}

func (s jsonSchema) mustMarshal() json.RawMessage {
	b, err := json.Marshal(s)
	if err != nil {
		panic(fmt.Sprintf("review: schema does not marshal: %v", err))
	}
	return b
}

// contractSchema is kept minimal on purpose: every extra field is something
// a model can get wrong.
func contractSchema(requireFix bool) json.RawMessage {
	required := []string{keyPath, keyLine, keySeverity, keyTitle, keyExplanation}
	fix := "A concrete fix: replacement code or a precise instruction. Markdown allowed, no headings."
	if requireFix {
		required = append(required, keySuggestedFix)
	} else {
		fix += " Omit it when there is no concrete fix."
	}
	enum := make([]string, len(severities))
	for i, s := range severities {
		enum[i] = string(s)
	}
	return jsonSchema{
		Type: schemaObject,
		Properties: map[string]*jsonSchema{
			keySummary: {
				Type: schemaObject,
				Properties: map[string]*jsonSchema{
					keyTake: {
						Type:        schemaString,
						Description: "Two to four sentences: what the change does and the overall assessment. No markdown headings.",
					},
					keyPraise: {
						Type:        "array",
						Description: "Up to three specific things the change does well; empty when nothing stands out.",
						Items:       &jsonSchema{Type: schemaString},
						MaxItems:    maxPraise,
					},
				},
				Required: []string{keyTake, keyPraise},
			},
			keyFindings: {
				Type: "array",
				Items: &jsonSchema{
					Type: schemaObject,
					Properties: map[string]*jsonSchema{
						keyPath: {Type: schemaString, Description: "Path of the changed file, exactly as it appears in the diff header."},
						keyLine: {Type: "integer", Description: "Line number in the new version of the file (a + or context line inside a hunk)."},
						keySeverity: {Type: schemaString, Enum: enum,
							Description: "blocking: must be fixed before merging. important: should be fixed. nit: optional polish."},
						keyTitle:        {Type: schemaString, Description: "One line, under 80 characters."},
						keyExplanation:  {Type: schemaString, Description: "Why it matters. Markdown allowed, no headings."},
						keySuggestedFix: {Type: schemaString, Description: fix},
					},
					Required: required,
				},
			},
		},
		Required: []string{keySummary, keyFindings},
	}.mustMarshal()
}

var (
	findingsSchema       = contractSchema(false)
	findingsSchemaStrict = contractSchema(true)
)

// Schema is the JSON Schema of the answer the model must produce.
func Schema() json.RawMessage { return slices.Clone(findingsSchema) }

// SchemaStrict is Schema with suggested_fix required on every finding.
func SchemaStrict() json.RawMessage { return slices.Clone(findingsSchemaStrict) }

// Parse decodes the model's JSON and drops findings kritik cannot post: an
// unknown severity, a missing field, a missing fix when opts require one,
// or a line the diff does not add or keep. Dropped findings are returned
// with the reason so they can be logged and counted, never silently lost.
// anchors maps a path to the head-side lines the diff covers. Kept findings
// are ordered most severe first, then by path and line.
func Parse(raw string, anchors map[string]map[int]bool, opts ParseOptions) (Result, []Dropped, error) {
	var res Result
	dec := json.NewDecoder(strings.NewReader(strings.TrimSpace(raw)))
	if err := dec.Decode(&res); err != nil {
		return Result{}, nil, fmt.Errorf("review: model output is not the expected JSON: %w", err)
	}
	res.Summary.Take = strings.TrimSpace(res.Summary.Take)
	praise := make([]string, 0, maxPraise)
	for _, p := range res.Summary.Praise {
		if p = strings.TrimSpace(p); p != "" && len(praise) < maxPraise {
			praise = append(praise, p)
		}
	}
	res.Summary.Praise = praise
	kept := make([]Finding, 0, len(res.Findings))
	var dropped []Dropped
	for _, f := range res.Findings {
		f.Path = strings.TrimSpace(f.Path)
		f.Title = strings.TrimSpace(f.Title)
		f.Explanation = strings.TrimSpace(f.Explanation)
		f.SuggestedFix = strings.TrimSpace(f.SuggestedFix)
		var reason DropReason
		switch {
		case !f.Severity.Valid():
			reason = DropBadSeverity
		case f.Path == "" || f.Line <= 0 || f.Title == "" || f.Explanation == "":
			reason = DropIncomplete
		case opts.RequireSuggestedFix && f.SuggestedFix == "":
			reason = DropNoFix
		case !anchors[f.Path][f.Line]:
			reason = DropUnanchored
		}
		if reason != "" {
			dropped = append(dropped, Dropped{Finding: f, Reason: reason})
			continue
		}
		kept = append(kept, f)
	}
	slices.SortStableFunc(kept, func(a, b Finding) int {
		return cmp.Or(
			cmp.Compare(a.Severity.Rank(), b.Severity.Rank()),
			strings.Compare(a.Path, b.Path),
			cmp.Compare(a.Line, b.Line),
		)
	})
	res.Findings = kept
	return res, dropped, nil
}

// Fingerprint identifies a finding across reviews of the same pull request:
// the path and the title, ignoring case and whitespace, so a finding that
// moves by a few lines or is reworded in case only is recognised as the
// same one.
func Fingerprint(f Finding) string {
	title := strings.ToLower(strings.Join(strings.Fields(f.Title), " "))
	sum := sha256.Sum256([]byte(f.Path + "\x00" + title))
	return hex.EncodeToString(sum[:])
}

// Anchors reads a unified diff and returns, per head-side path, the set of
// new-file line numbers the diff shows (added and context lines). A finding
// may only be attached to one of these, which is also the set of lines a
// forge accepts an inline comment on.
func Anchors(diff string) map[string]map[int]bool {
	out := map[string]map[int]bool{}
	var path string
	var line int
	inHunk := false
	for l := range strings.SplitSeq(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "+++ "):
			path = strings.TrimPrefix(l, "+++ ")
			path = strings.TrimPrefix(path, "b/")
			if path == "/dev/null" {
				path = ""
			}
			inHunk = false
		case strings.HasPrefix(l, "@@"):
			// @@ -a,b +c,d @@ : c is the first new-file line of the hunk.
			line = hunkStart(l)
			inHunk = line > 0
		case !inHunk || path == "":
		case strings.HasPrefix(l, "+"), strings.HasPrefix(l, " "):
			if out[path] == nil {
				out[path] = map[int]bool{}
			}
			out[path][line] = true
			line++
		case strings.HasPrefix(l, "-"):
			// removed lines do not advance the new-file counter
		case strings.HasPrefix(l, "\\"):
			// "\ No newline at end of file"
		default:
			inHunk = false
		}
	}
	return out
}

func hunkStart(header string) int {
	// header looks like "@@ -12,7 +12,8 @@ optional text"
	plus := strings.Index(header, "+")
	if plus < 0 {
		return 0
	}
	rest := header[plus+1:]
	end := strings.IndexAny(rest, ", @")
	if end < 0 {
		end = len(rest)
	}
	n := 0
	for _, c := range rest[:end] {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}
