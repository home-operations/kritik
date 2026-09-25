// Package review turns a context pack into a prompt, a model answer into
// findings, and findings into the comments kritik posts. It knows nothing
// about forges or models beyond their interfaces.
package review

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Severity of a finding, in the order the summary lists them.
type Severity string

// Severities a finding may carry.
const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
	SeverityInfo    Severity = "info"
)

// Finding is one thing the reviewer wants a human to look at, anchored to a
// line on the head side of the diff.
type Finding struct {
	Path     string   `json:"path"`
	Line     int      `json:"line"`
	Severity Severity `json:"severity"`
	Title    string   `json:"title"`
	Body     string   `json:"body"`
}

// Result is the whole answer.
type Result struct {
	// Summary is a short paragraph for the sticky comment.
	Summary  string    `json:"summary"`
	Findings []Finding `json:"findings"`
}

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
}

func (s jsonSchema) mustMarshal() json.RawMessage {
	b, err := json.Marshal(s)
	if err != nil {
		panic(fmt.Sprintf("review: schema does not marshal: %v", err))
	}
	return b
}

// findingsSchema is kept minimal on purpose: every extra field is something
// a model can get wrong.
var findingsSchema = jsonSchema{
	Type: schemaObject,
	Properties: map[string]*jsonSchema{
		"summary": {
			Type:        schemaString,
			Description: "Two to four sentences: what the change does and the overall assessment. No markdown headings.",
		},
		"findings": {
			Type: "array",
			Items: &jsonSchema{
				Type: schemaObject,
				Properties: map[string]*jsonSchema{
					"path":     {Type: schemaString, Description: "Path of the changed file, exactly as it appears in the diff header."},
					"line":     {Type: "integer", Description: "Line number in the new version of the file (a + or context line inside a hunk)."},
					"severity": {Type: schemaString, Enum: []string{string(SeverityError), string(SeverityWarning), string(SeverityInfo)}},
					"title":    {Type: schemaString, Description: "One line, under 80 characters."},
					"body":     {Type: schemaString, Description: "Why it matters and what to do instead. Markdown allowed, no headings."},
				},
				Required: []string{"path", "line", "severity", "title", "body"},
			},
		},
	},
	Required: []string{"summary", "findings"},
}.mustMarshal()

// Schema is the JSON Schema of the answer the model must produce.
func Schema() json.RawMessage { return slices.Clone(findingsSchema) }

// Parse decodes the model's JSON and drops findings that cannot be anchored:
// a path the diff does not touch, or a line the diff does not add or keep.
// Dropped findings are returned separately so they can be logged, never
// silently lost. anchors maps a path to the head-side lines the diff covers.
func Parse(raw string, anchors map[string]map[int]bool) (Result, []Finding, error) {
	var res Result
	dec := json.NewDecoder(strings.NewReader(strings.TrimSpace(raw)))
	if err := dec.Decode(&res); err != nil {
		return Result{}, nil, fmt.Errorf("review: model output is not the expected JSON: %w", err)
	}
	res.Summary = strings.TrimSpace(res.Summary)
	kept := res.Findings[:0]
	var dropped []Finding
	for _, f := range res.Findings {
		f.Path = strings.TrimSpace(f.Path)
		f.Title = strings.TrimSpace(f.Title)
		f.Body = strings.TrimSpace(f.Body)
		switch f.Severity {
		case SeverityError, SeverityWarning, SeverityInfo:
		default:
			f.Severity = SeverityInfo
		}
		if f.Title == "" || f.Body == "" || f.Path == "" || f.Line <= 0 || !anchors[f.Path][f.Line] {
			dropped = append(dropped, f)
			continue
		}
		kept = append(kept, f)
	}
	res.Findings = kept
	sort.SliceStable(res.Findings, func(i, j int) bool {
		if res.Findings[i].Path != res.Findings[j].Path {
			return res.Findings[i].Path < res.Findings[j].Path
		}
		return res.Findings[i].Line < res.Findings[j].Line
	})
	return res, dropped, nil
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
