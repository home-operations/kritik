package tasks

import (
	"encoding/json"
	"slices"
)

// Field types.
const (
	TypeString  = "string"
	TypeNumber  = "number"
	TypeInteger = "integer"
	TypeBoolean = "boolean"
	TypeArray   = "array"
	TypeObject  = "object"
)

// The answer's keys.
const (
	keySummary   = "summary"
	keyFields    = "fields"
	keyLabels    = "labels"
	keyState     = "state"
	keyAssignees = "assignees"
	keyReviewers = "reviewers"
	keyInline    = "inline"
	keyComment   = "comment"
	keyAdd       = "add"
	keyRemove    = "remove"
	keyPath      = "path"
	keyLine      = "line"
	keyEndLine   = "end_line"
	keyBody      = "body"
)

// schema is the subset of JSON Schema an answer schema uses.
type schema struct {
	Type                 string             `json:"type"`
	Description          string             `json:"description,omitempty"`
	Enum                 []string           `json:"enum,omitempty"`
	MaxLength            *int               `json:"maxLength,omitempty"`
	Pattern              string             `json:"pattern,omitempty"`
	Minimum              *float64           `json:"minimum,omitempty"`
	Maximum              *float64           `json:"maximum,omitempty"`
	Items                *schema            `json:"items,omitempty"`
	MaxItems             *int               `json:"maxItems,omitempty"`
	Properties           map[string]*schema `json:"properties,omitempty"`
	Required             []string           `json:"required,omitempty"`
	AdditionalProperties *bool              `json:"additionalProperties,omitempty"`
}

// object is an object schema that requires each property, in order, and
// allows no other.
func object(order []string, props map[string]*schema) *schema {
	return &schema{Type: TypeObject, AdditionalProperties: new(false), Required: order, Properties: props}
}

// AnswerSchema is the JSON Schema the model's answer must satisfy: a
// summary, the declared fields and the actions the model may propose,
// each constrained to what the task offers. Labels are offered only when
// they exist in repoLabels. Every property but an inline comment's
// end_line is required, and no other is allowed. The result encodes deterministically: object keys sort, and
// required lists keep declaration order.
func (p *Prepared) AnswerSchema(repoLabels []string) map[string]any {
	props := map[string]*schema{
		keySummary: {
			Type: TypeString, MaxLength: new(maxSummaryLength), Description: "A short, factual summary of what you found, for the task's report.",
		},
		keyFields: objectSchema(p.Task.Fields),
	}
	order := []string{keySummary, keyFields}
	add := func(key string, s *schema) {
		props[key] = s
		order = append(order, key)
	}
	a := p.Task.Actions
	if a.Labels != nil {
		labels := map[string]*schema{}
		var lo []string
		for _, c := range []struct {
			key   string
			globs []string
		}{{keyAdd, a.Labels.Propose.Add}, {keyRemove, a.Labels.Propose.Remove}} {
			var enum []string
			for _, l := range repoLabels {
				if matchAny(c.globs, l) && !slices.Contains(enum, l) {
					enum = append(enum, l)
				}
			}
			if len(enum) > 0 {
				labels[c.key] = enumList(enum, "Labels to "+c.key+", only from this list.")
				lo = append(lo, c.key)
			}
		}
		if len(lo) > 0 {
			add(keyLabels, object(lo, labels))
		}
	}
	if s := a.State; s != nil && (s.Propose.Close || s.Propose.Reopen) {
		enum := []string{stateNone}
		if s.Propose.Close {
			enum = append(enum, StateClose)
		}
		if s.Propose.Reopen {
			enum = append(enum, StateReopen)
		}
		add(keyState, &schema{Type: TypeString, Enum: enum, Description: "Whether to change the subject's state; none leaves it."})
	}
	if u := a.Assign; u != nil && len(u.Propose.Users) > 0 {
		add(keyAssignees, enumList(u.Propose.Users, "Users to assign, only from this list."))
	}
	if u := a.Reviewers; u != nil && len(u.Propose.Users) > 0 {
		add(keyReviewers, enumList(u.Propose.Users, "Users to request a review from, only from this list."))
	}
	if c := a.InlineComments; c != nil && c.Propose {
		item := object([]string{keyPath, keyLine, keyBody}, map[string]*schema{
			keyPath:    {Type: TypeString, MaxLength: new(maxInlinePath)},
			keyLine:    {Type: TypeInteger, Minimum: new(1.0)},
			keyEndLine: {Type: TypeInteger, Minimum: new(1.0)},
			keyBody:    {Type: TypeString, MaxLength: new(maxInlineBody)},
		})
		add(keyInline, &schema{
			Type: TypeArray, MaxItems: new(maxInline), Items: item, Description: "Comments on lines the pull request changes.",
		})
	}
	if c := a.Comment; c != nil && c.PostMode() != CommentNone {
		add(keyComment, &schema{
			Type: TypeString, MaxLength: new(maxCommentLength),
			Description: "Text for the task's report comment; empty when there is nothing to add to the summary.",
		})
	}
	return toMap(object(order, props))
}

// toMap is s as the generic map a request body carries.
func toMap(s *schema) map[string]any {
	raw, err := json.Marshal(s)
	if err != nil {
		panic("tasks: encode answer schema: " + err.Error()) // a schema of strings, numbers and maps always encodes
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		panic("tasks: decode answer schema: " + err.Error())
	}
	return m
}

func enumList(values []string, description string) *schema {
	return &schema{
		Type: TypeArray, MaxItems: new(len(values)), Description: description, Items: &schema{Type: TypeString, Enum: slices.Clone(values)},
	}
}

func objectSchema(fs Fields) *schema {
	props := make(map[string]*schema, len(fs))
	order := make([]string, 0, len(fs))
	for _, f := range fs {
		props[f.Name] = fieldSchema(f.Field)
		order = append(order, f.Name)
	}
	return object(order, props)
}

func fieldSchema(f Field) *schema {
	if f.Type == TypeObject {
		s := objectSchema(f.Properties)
		s.Description = f.Description
		return s
	}
	s := &schema{Type: f.Type, Description: f.Description, Minimum: f.Minimum, Maximum: f.Maximum}
	switch f.Type {
	case TypeString:
		s.MaxLength, s.Enum, s.Pattern = new(fieldMaxLength(f)), slices.Clone(f.Enum), f.Pattern
	case TypeArray:
		s.MaxItems = new(fieldMaxItems(f))
		if f.Items != nil {
			s.Items = fieldSchema(*f.Items)
		}
	}
	return s
}
