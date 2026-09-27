package tasks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"slices"
	"unicode/utf8"

	"github.com/bmatcuk/doublestar/v4"
)

// Answer bounds. Every string and list the model returns is bounded, in
// the schema it is given and again when its answer is parsed.
const (
	maxSummaryLength = 2000
	maxCommentLength = 16000
	// DefaultMaxLength bounds a string field that declares no maxLength.
	DefaultMaxLength = 1000
	maxFieldLength   = 10000
	// DefaultMaxItems bounds an array field that declares no maxItems.
	DefaultMaxItems   = 20
	maxFieldItems     = 100
	maxInline         = 50
	maxInlineBody     = 8000
	maxInlinePath     = 1024
	maxProposedValues = 100
	maxValueLength    = 255
)

// Inline is a comment on lines of a pull request's diff.
type Inline struct {
	Path    string `json:"path"`
	Line    int    `json:"line"`
	EndLine int    `json:"end_line,omitempty"`
	Body    string `json:"body"`
}

// LabelChanges are labels to add and remove.
type LabelChanges struct {
	Add    []string `json:"add"`
	Remove []string `json:"remove"`
}

// Answer is a model's validated answer.
type Answer struct {
	Summary string         `json:"summary"`
	Comment string         `json:"comment,omitempty"`
	Fields  map[string]any `json:"fields"`
	Labels  LabelChanges   `json:"labels"`
	// State is "close", "reopen" or "" for no change.
	State     string   `json:"state,omitempty"`
	Assignees []string `json:"assignees,omitempty"`
	Reviewers []string `json:"reviewers,omitempty"`
	Inline    []Inline `json:"inline,omitempty"`
}

// State changes.
const (
	StateClose  = "close"
	StateReopen = "reopen"
	stateNone   = "none"
)

// vars is the answer as a guard sees it: every key present.
func (a *Answer) vars() map[string]any {
	inline := make([]any, len(a.Inline))
	for i, c := range a.Inline {
		inline[i] = map[string]any{keyPath: c.Path, keyLine: c.Line, keyEndLine: c.EndLine, keyBody: c.Body}
	}
	fields := a.Fields
	if fields == nil {
		fields = map[string]any{}
	}
	return map[string]any{
		keySummary: a.Summary, keyComment: a.Comment, keyFields: fields,
		keyLabels: map[string]any{keyAdd: orEmpty(a.Labels.Add), keyRemove: orEmpty(a.Labels.Remove)},
		keyState:  a.State, keyAssignees: orEmpty(a.Assignees), keyReviewers: orEmpty(a.Reviewers), keyInline: inline,
	}
}

// answerKeys are the top-level keys an answer to p may carry.
func (p *Prepared) answerKeys() []string {
	keys := []string{keySummary, keyFields}
	a := p.Task.Actions
	if a.Labels != nil && (len(a.Labels.Propose.Add) > 0 || len(a.Labels.Propose.Remove) > 0) {
		keys = append(keys, keyLabels)
	}
	if a.State != nil && (a.State.Propose.Close || a.State.Propose.Reopen) {
		keys = append(keys, keyState)
	}
	if a.Assign != nil && len(a.Assign.Propose.Users) > 0 {
		keys = append(keys, keyAssignees)
	}
	if a.Reviewers != nil && len(a.Reviewers.Propose.Users) > 0 {
		keys = append(keys, keyReviewers)
	}
	if a.InlineComments != nil && a.InlineComments.Propose {
		keys = append(keys, keyInline)
	}
	if a.Comment != nil && a.Comment.PostMode() != CommentNone {
		keys = append(keys, keyComment)
	}
	return keys
}

// ErrAnswer is an answer that does not satisfy the task's schema.
var ErrAnswer = errors.New("tasks: invalid answer")

func answerErr(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrAnswer, fmt.Sprintf(format, args...))
}

// ParseAnswer decodes raw and validates it against the task's answer
// schema: its keys, types, enums, bounds, lengths, patterns and list
// sizes. Whether a proposed label, user or state change may be applied is
// Plan's to judge, against the repository.
func (p *Prepared) ParseAnswer(raw []byte) (Answer, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return Answer{}, answerErr("decode: %v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Answer{}, answerErr("trailing data after the answer")
	}
	if m == nil {
		return Answer{}, answerErr("the answer must be an object")
	}
	allowed := p.answerKeys()
	for k := range m {
		if !slices.Contains(allowed, k) {
			return Answer{}, answerErr("unexpected key %q", k)
		}
	}
	var a Answer
	var err error
	if a.Summary, err = str(m[keySummary], keySummary, maxSummaryLength, true); err != nil {
		return Answer{}, err
	}
	if a.Fields, err = p.parseFields(m[keyFields]); err != nil {
		return Answer{}, err
	}
	if v, ok := m[keyLabels]; ok {
		if a.Labels, err = parseLabels(v); err != nil {
			return Answer{}, err
		}
	}
	if v, ok := m[keyState]; ok {
		s, err := str(v, keyState, 16, false)
		if err != nil {
			return Answer{}, err
		}
		switch s {
		case "", stateNone:
		case StateClose, StateReopen:
			a.State = s
		default:
			return Answer{}, answerErr("state %q must be none, close or reopen", s)
		}
	}
	if a.Assignees, err = strList(m[keyAssignees], keyAssignees); err != nil {
		return Answer{}, err
	}
	if a.Reviewers, err = strList(m[keyReviewers], keyReviewers); err != nil {
		return Answer{}, err
	}
	if a.Inline, err = parseInline(m[keyInline]); err != nil {
		return Answer{}, err
	}
	if v, ok := m[keyComment]; ok {
		if a.Comment, err = str(v, keyComment, maxCommentLength, false); err != nil {
			return Answer{}, err
		}
	}
	return a, nil
}

// str is v as a string of at most max runes; required refuses a missing
// value.
func str(v any, what string, maxLen int, required bool) (string, error) {
	if v == nil {
		if required {
			return "", answerErr("%s is required", what)
		}
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", answerErr("%s must be a string", what)
	}
	if utf8.RuneCountInString(s) > maxLen {
		return "", answerErr("%s is longer than %d characters", what, maxLen)
	}
	return s, nil
}

// strList is v as a list of short strings, nil when v is.
func strList(v any, what string) ([]string, error) {
	if v == nil {
		return nil, nil
	}
	l, ok := v.([]any)
	if !ok {
		return nil, answerErr("%s must be a list", what)
	}
	if len(l) > maxProposedValues {
		return nil, answerErr("%s has more than %d items", what, maxProposedValues)
	}
	out := make([]string, len(l))
	for i, x := range l {
		s, err := str(x, fmt.Sprintf("%s[%d]", what, i), maxValueLength, true)
		if err != nil {
			return nil, err
		}
		out[i] = s
	}
	return out, nil
}

func parseLabels(v any) (LabelChanges, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return LabelChanges{}, answerErr("labels must be an object")
	}
	for k := range m {
		if k != keyAdd && k != keyRemove {
			return LabelChanges{}, answerErr("unexpected key labels.%s", k)
		}
	}
	add, err := strList(m[keyAdd], "labels.add")
	if err != nil {
		return LabelChanges{}, err
	}
	remove, err := strList(m[keyRemove], "labels.remove")
	return LabelChanges{Add: add, Remove: remove}, err
}

func parseInline(v any) ([]Inline, error) {
	if v == nil {
		return nil, nil
	}
	l, ok := v.([]any)
	if !ok {
		return nil, answerErr("inline must be a list")
	}
	if len(l) > maxInline {
		return nil, answerErr("inline has more than %d comments", maxInline)
	}
	out := make([]Inline, len(l))
	for i, x := range l {
		m, ok := x.(map[string]any)
		if !ok {
			return nil, answerErr("inline[%d] must be an object", i)
		}
		for k := range m {
			if !slices.Contains([]string{keyPath, keyLine, keyEndLine, keyBody}, k) {
				return nil, answerErr("unexpected key inline[%d].%s", i, k)
			}
		}
		var c Inline
		var err error
		if c.Path, err = str(m[keyPath], fmt.Sprintf("inline[%d].path", i), maxInlinePath, true); err != nil {
			return nil, err
		}
		if c.Body, err = str(m[keyBody], fmt.Sprintf("inline[%d].body", i), maxInlineBody, true); err != nil {
			return nil, err
		}
		line, err := integer(m[keyLine], fmt.Sprintf("inline[%d].line", i))
		if err != nil {
			return nil, err
		}
		c.Line = int(line)
		if e, ok := m[keyEndLine]; ok && e != nil {
			end, err := integer(e, fmt.Sprintf("inline[%d].end_line", i))
			if err != nil {
				return nil, err
			}
			c.EndLine = int(end)
		}
		if c.Path == "" || c.Body == "" || c.Line < 1 || (c.EndLine != 0 && c.EndLine < c.Line) || checkRefPath(c.Path) != nil {
			return nil, answerErr("inline[%d] needs a repository path, a line of at least 1, an end_line not before it and a body", i)
		}
		out[i] = c
	}
	return out, nil
}

func integer(v any, what string) (int64, error) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, answerErr("%s must be an integer", what)
	}
	i, err := n.Int64()
	if err != nil || i < math.MinInt32 || i > math.MaxInt32 {
		return 0, answerErr("%s must be an integer", what)
	}
	return i, nil
}

// parseFields validates the fields object: every declared field, and no
// other.
func (p *Prepared) parseFields(v any) (map[string]any, error) {
	decl := p.Task.Fields
	if v == nil {
		if len(decl) == 0 {
			return map[string]any{}, nil
		}
		return nil, answerErr("fields is required")
	}
	return parseObject(v, keyFields, decl)
}

func parseObject(v any, what string, decl Fields) (map[string]any, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, answerErr("%s must be an object", what)
	}
	for k := range m {
		if _, ok := decl.Get(k); !ok {
			return nil, answerErr("unexpected key %s.%s", what, k)
		}
	}
	out := make(map[string]any, len(decl))
	for _, f := range decl {
		x, ok := m[f.Name]
		if !ok {
			return nil, answerErr("%s.%s is required", what, f.Name)
		}
		val, err := parseValue(x, what+"."+f.Name, f.Field)
		if err != nil {
			return nil, err
		}
		out[f.Name] = val
	}
	return out, nil
}

// parseValue validates one field's value and returns it as a string,
// int64, float64, bool, []any or map[string]any.
func parseValue(v any, what string, f Field) (any, error) {
	switch f.Type {
	case TypeString:
		s, err := str(v, what, fieldMaxLength(f), true)
		if err != nil {
			return nil, err
		}
		if len(f.Enum) > 0 && !slices.Contains(f.Enum, s) {
			return nil, answerErr("%s %q is not one of the allowed values", what, s)
		}
		if f.Pattern != "" {
			re, err := regexp.Compile(f.Pattern)
			if err != nil || !re.MatchString(s) {
				return nil, answerErr("%s %q does not match its pattern", what, s)
			}
		}
		return s, nil
	case TypeInteger, TypeNumber:
		n, ok := v.(json.Number)
		if !ok {
			return nil, answerErr("%s must be a %s", what, f.Type)
		}
		x, err := n.Float64()
		if err != nil || math.IsInf(x, 0) || math.IsNaN(x) {
			return nil, answerErr("%s must be a %s", what, f.Type)
		}
		if (f.Minimum != nil && x < *f.Minimum) || (f.Maximum != nil && x > *f.Maximum) {
			return nil, answerErr("%s %v is out of range", what, x)
		}
		if f.Type == TypeNumber {
			return x, nil
		}
		i, err := n.Int64()
		if err != nil {
			return nil, answerErr("%s must be an integer", what)
		}
		return i, nil
	case TypeBoolean:
		b, ok := v.(bool)
		if !ok {
			return nil, answerErr("%s must be a boolean", what)
		}
		return b, nil
	case TypeArray:
		l, ok := v.([]any)
		if !ok {
			return nil, answerErr("%s must be a list", what)
		}
		if len(l) > fieldMaxItems(f) {
			return nil, answerErr("%s has more than %d items", what, fieldMaxItems(f))
		}
		out := make([]any, len(l))
		for i, x := range l {
			val, err := parseValue(x, fmt.Sprintf("%s[%d]", what, i), *f.Items)
			if err != nil {
				return nil, err
			}
			out[i] = val
		}
		return out, nil
	case TypeObject:
		return parseObject(v, what, f.Properties)
	}
	return nil, answerErr("%s has unknown type %q", what, f.Type)
}

func fieldMaxLength(f Field) int {
	if f.MaxLength != nil {
		return *f.MaxLength
	}
	return DefaultMaxLength
}

func fieldMaxItems(f Field) int {
	if f.MaxItems != nil {
		return *f.MaxItems
	}
	return DefaultMaxItems
}

// sampleAnswer is an answer with a value of the right type for every
// field, for smoke tests.
func (p *Prepared) sampleAnswer() Answer {
	return Answer{
		Summary: "Sample summary.", Comment: "Sample comment.", Fields: sampleObject(p.Task.Fields),
		Labels: LabelChanges{Add: []string{}, Remove: []string{}},
	}
}

func sampleObject(fs Fields) map[string]any {
	out := make(map[string]any, len(fs))
	for _, f := range fs {
		out[f.Name] = sampleValue(f.Field)
	}
	return out
}

func sampleValue(f Field) any {
	switch f.Type {
	case TypeString:
		if len(f.Enum) > 0 {
			return f.Enum[0]
		}
		return "sample"
	case TypeInteger:
		if f.Minimum != nil {
			return int64(math.Ceil(*f.Minimum))
		}
		return int64(0)
	case TypeNumber:
		if f.Minimum != nil {
			return *f.Minimum
		}
		return 0.0
	case TypeBoolean:
		return false
	case TypeArray:
		if f.Items == nil {
			return []any{}
		}
		return []any{sampleValue(*f.Items)}
	case TypeObject:
		return sampleObject(f.Properties)
	}
	return nil
}

// matchAny reports whether s matches one of the globs.
func matchAny(globs []string, s string) bool {
	for _, g := range globs {
		if ok, _ := doublestar.Match(g, s); ok {
			return true
		}
	}
	return false
}
