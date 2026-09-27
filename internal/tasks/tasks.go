// Package tasks defines kritik's event-driven tasks: a trigger on a forge
// event, a CEL guard, the context a run gathers, templated prompts, the
// custom fields the model answers with and the actions its answer may lead
// to. It parses and validates a task definition, clips it to the
// operator's bounds, matches it against an event, renders its prompts,
// builds the answer schema and turns a model's answer into a validated plan
// of forge writes.
//
// The model's answer is untrusted: Plan is the only gate between it and
// the forge, so everything the answer proposes is checked against what the
// task declares and what the repository has. The package does no I/O; the
// caller gathers context, calls the model and applies the plan.
package tasks

import (
	"fmt"
	"time"

	"go.yaml.in/yaml/v3"
)

// Subject is the issue or pull request an event is about.
type Subject struct {
	Kind      string // "issue" | "pull"
	Number    int
	Title     string
	Body      string
	State     string
	Author    string
	URL       string
	Labels    []string
	Assignees []string
	Draft     bool
}

// Subject kinds.
const (
	SubjectIssue = "issue"
	SubjectPull  = "pull"
)

// Repo is the repository an event comes from.
type Repo struct {
	Owner         string
	Name          string
	DefaultBranch string
}

// Input is an event as a task sees it.
type Input struct {
	// Forge is "github", "forgejo" or "gitea".
	Forge string
	// Event is the normalized event, EventIssue, EventPullRequest or
	// EventComment, or "" when the delivery has none.
	Event string
	// RawEvent is the forge's own event name, from its event header.
	RawEvent string
	Action   string
	Sender   string
	// Subject is nil for an event about no issue or pull request.
	Subject *Subject
	// Raw is the decoded payload.
	Raw  map[string]any
	Repo Repo
}

// Normalized events, and the trigger key of a forge-specific one.
const (
	EventIssue       = "issue"
	EventPullRequest = "pull_request"
	EventComment     = "comment"
	EventRaw         = "raw"
)

// Mode is how a task runs.
type Mode string

// Modes. An unset mode is agentic.
const (
	ModeAgentic Mode = "agentic"
	ModeSingle  Mode = "single"
)

// Task is one task definition.
type Task struct {
	// Name identifies the task in markers, run ids and notes.
	Name string    `yaml:"name"`
	On   []Trigger `yaml:"on"`
	// If is a CEL guard over event, subject and raw; empty always runs.
	If           string  `yaml:"if,omitempty"`
	Mode         Mode    `yaml:"mode,omitempty"`
	Models       Models  `yaml:"models,omitempty"`
	Agent        Agent   `yaml:"agent,omitempty"`
	Context      Context `yaml:"context,omitempty"`
	System       string  `yaml:"system,omitempty"`
	Prompt       string  `yaml:"prompt,omitempty"`
	PromptInline string  `yaml:"promptInline,omitempty"`
	Fields       Fields  `yaml:"fields,omitempty"`
	Actions      Actions `yaml:"actions,omitempty"`
}

// RunMode is the task's mode, agentic when it sets none.
func (t *Task) RunMode() Mode {
	if t.Mode == "" {
		return ModeAgentic
	}
	return t.Mode
}

// Models are the review and fallback models a task chooses, each a
// "<provider>/<model>" reference; the caller checks them against its
// bounds.
type Models struct {
	Review   string `yaml:"review,omitempty"`
	Fallback string `yaml:"fallback,omitempty"`
}

// Agent is an agentic task's limits, tools and commands; the caller
// bounds each by the operator's.
type Agent struct {
	MaxSteps           *int           `yaml:"maxSteps,omitempty"`
	MaxToolOutputBytes *int           `yaml:"maxToolOutputBytes,omitempty"`
	MaxTokens          *int64         `yaml:"maxTokens,omitempty"`
	Timeout            *time.Duration `yaml:"timeout,omitempty"`
	Tools              []string       `yaml:"tools,omitempty"`
	Commands           []string       `yaml:"commands,omitempty"`
}

// Trigger is one event a task runs on. Actions empty is any action.
type Trigger struct {
	// Event is EventIssue, EventPullRequest, EventComment or EventRaw.
	Event string
	// RawEvent is the glob a raw trigger matches the forge's event name
	// with.
	RawEvent string
	// Actions are globs over the event's action.
	Actions []string
}

// UnmarshalYAML takes a mapping with exactly one key: issue, pull_request
// or comment with a list of actions, or raw with an event and actions.
func (tr *Trigger) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode || len(n.Content) != 2 {
		return fmt.Errorf("line %d: a trigger is a mapping with exactly one of issue, pull_request, comment or raw", n.Line)
	}
	key, val := n.Content[0], n.Content[1]
	switch key.Value {
	case EventIssue, EventPullRequest, EventComment:
		var actions []string
		if err := val.Decode(&actions); err != nil {
			return err
		}
		*tr = Trigger{Event: key.Value, Actions: actions}
	case EventRaw:
		if err := knownKeys(val, "raw trigger", "event", "actions"); err != nil {
			return err
		}
		var raw struct {
			Event   string   `yaml:"event"`
			Actions []string `yaml:"actions"`
		}
		if err := val.Decode(&raw); err != nil {
			return err
		}
		*tr = Trigger{Event: EventRaw, RawEvent: raw.Event, Actions: raw.Actions}
	default:
		return fmt.Errorf("line %d: unknown trigger %q; want issue, pull_request, comment or raw", key.Line, key.Value)
	}
	return nil
}

// MarshalYAML is the inverse of UnmarshalYAML.
func (tr Trigger) MarshalYAML() (any, error) {
	actions := tr.Actions
	if actions == nil {
		actions = []string{}
	}
	if tr.Event == EventRaw {
		type raw struct {
			Event   string   `yaml:"event"`
			Actions []string `yaml:"actions,omitempty"`
		}
		return map[string]raw{EventRaw: {Event: tr.RawEvent, Actions: tr.Actions}}, nil
	}
	return map[string][]string{tr.Event: actions}, nil
}

// Context names the sources a run gathers; each source's result is a
// template variable under .Context.
type Context struct {
	Thread   *Thread       `yaml:"thread,omitempty"`
	Files    []ContextFile `yaml:"files,omitempty"`
	Search   []Query       `yaml:"search,omitempty"`
	Related  []Query       `yaml:"related,omitempty"`
	Commands []Command     `yaml:"commands,omitempty"`
}

// Thread gathers the subject's last Comments comments.
type Thread struct {
	Comments int `yaml:"comments"`
}

// ContextFile is a file, or up to Max files matching Glob, read from the
// default branch.
type ContextFile struct {
	Path string `yaml:"path,omitempty"`
	Glob string `yaml:"glob,omitempty"`
	Max  int    `yaml:"max,omitempty"`
}

// Query is a named search of the repository index (search) or of the
// forge's issues and pull requests (related); Query is a template over the
// event, and K bounds the results.
type Query struct {
	Name  string `yaml:"name"`
	Query string `yaml:"query"`
	K     int    `yaml:"k,omitempty"`
}

// Command is a named command an agentic run executes, one of the
// operator's allowed commands.
type Command struct {
	Name string `yaml:"name"`
	Run  string `yaml:"run"`
}

// Field is a custom answer field, a restricted JSON Schema: a string with
// an enum, maxLength or pattern; a number or integer with a minimum and
// maximum; a boolean; an array of one of those with maxItems; or an
// object of those, one level deep.
type Field struct {
	Type        string   `yaml:"type"`
	Description string   `yaml:"description,omitempty"`
	Enum        []string `yaml:"enum,omitempty"`
	MaxLength   *int     `yaml:"maxLength,omitempty"`
	Pattern     string   `yaml:"pattern,omitempty"`
	Minimum     *float64 `yaml:"minimum,omitempty"`
	Maximum     *float64 `yaml:"maximum,omitempty"`
	Items       *Field   `yaml:"items,omitempty"`
	MaxItems    *int     `yaml:"maxItems,omitempty"`
	Properties  Fields   `yaml:"properties,omitempty"`
}

// fieldKeys are the keys a field takes.
var fieldKeys = []string{"type", "description", "enum", "maxLength", "pattern", "minimum", "maximum", "items", "maxItems", "properties"}

// UnmarshalYAML refuses unknown keys, which a decoder reached through
// Fields would otherwise accept.
func (f *Field) UnmarshalYAML(n *yaml.Node) error {
	if err := knownKeys(n, "field", fieldKeys...); err != nil {
		return err
	}
	type plain Field
	return n.Decode((*plain)(f))
}

// NamedField is one field and its name.
type NamedField struct {
	Name  string
	Field Field
}

// Fields are custom answer fields in the order they are declared.
type Fields []NamedField

// Get returns the named field.
func (fs Fields) Get(name string) (Field, bool) {
	for _, f := range fs {
		if f.Name == name {
			return f.Field, true
		}
	}
	return Field{}, false
}

// UnmarshalYAML decodes a mapping of name to field, keeping its order.
func (fs *Fields) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: fields must be a mapping of name to field", n.Line)
	}
	out := make(Fields, 0, len(n.Content)/2)
	for i := 0; i < len(n.Content); i += 2 {
		var f Field
		if err := n.Content[i+1].Decode(&f); err != nil {
			return err
		}
		out = append(out, NamedField{Name: n.Content[i].Value, Field: f})
	}
	*fs = out
	return nil
}

// MarshalYAML encodes the fields as a mapping in their order.
func (fs Fields) MarshalYAML() (any, error) {
	n := &yaml.Node{Kind: yaml.MappingNode}
	for _, f := range fs {
		var v yaml.Node
		if err := v.Encode(f.Field); err != nil {
			return nil, err
		}
		n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: f.Name}, &v)
	}
	return n, nil
}

// knownKeys refuses a mapping key not in keys: Node.Decode drops the
// strictness the top-level decoder asked for.
func knownKeys(n *yaml.Node, what string, keys ...string) error {
	if n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i < len(n.Content); i += 2 {
		k := n.Content[i]
		found := false
		for _, want := range keys {
			found = found || k.Value == want
		}
		if !found {
			return fmt.Errorf("line %d: field %s not found in %s", k.Line, k.Value, what)
		}
	}
	return nil
}

// Actions are what a task's answer may lead to. Each block is off when
// unset.
type Actions struct {
	Comment        *CommentSpec `yaml:"comment,omitempty"`
	Labels         *LabelsSpec  `yaml:"labels,omitempty"`
	State          *StateSpec   `yaml:"state,omitempty"`
	Assign         *UsersSpec   `yaml:"assign,omitempty"`
	Reviewers      *UsersSpec   `yaml:"reviewers,omitempty"`
	InlineComments *InlineSpec  `yaml:"inlineComments,omitempty"`
}

// Comment modes. An unset mode is sticky.
const (
	CommentSticky = "sticky"
	CommentAppend = "append"
	CommentNone   = "none"
)

// CommentSpec is the report comment: sticky updates one comment per task
// and subject, append posts a new one each run, none posts nothing.
// Template names a repository file; unset uses the built-in report.
type CommentSpec struct {
	Mode     string `yaml:"mode,omitempty"`
	Template string `yaml:"template,omitempty"`
	If       string `yaml:"if,omitempty"`
}

// PostMode is the comment's mode, sticky when it sets none.
func (c *CommentSpec) PostMode() string {
	if c.Mode == "" {
		return CommentSticky
	}
	return c.Mode
}

// LabelsSpec lets the model add and remove labels matching the propose
// globs, and adds and removes the labels the rules' templates render.
type LabelsSpec struct {
	Propose LabelSet    `yaml:"propose,omitempty"`
	Rules   []LabelRule `yaml:"rules,omitempty"`
	If      string      `yaml:"if,omitempty"`
}

// LabelSet is globs of labels to add and to remove.
type LabelSet struct {
	Add    []string `yaml:"add,omitempty"`
	Remove []string `yaml:"remove,omitempty"`
}

// LabelRule adds and removes the labels its templates render when its If
// holds.
type LabelRule struct {
	Add    []string `yaml:"add,omitempty"`
	Remove []string `yaml:"remove,omitempty"`
	If     string   `yaml:"if,omitempty"`
}

// StateSpec lets the model close or reopen the subject, and closes or
// reopens it when a rule's If holds.
type StateSpec struct {
	Propose StateSet    `yaml:"propose,omitempty"`
	Rules   []StateRule `yaml:"rules,omitempty"`
	If      string      `yaml:"if,omitempty"`
}

// StateSet is the state changes the model may propose.
type StateSet struct {
	Close  bool `yaml:"close,omitempty"`
	Reopen bool `yaml:"reopen,omitempty"`
}

// StateRule closes or reopens the subject when If holds.
type StateRule struct {
	Close  bool   `yaml:"close,omitempty"`
	Reopen bool   `yaml:"reopen,omitempty"`
	If     string `yaml:"if,omitempty"`
}

// UsersSpec lets the model pick among the proposed users, and picks the
// users the rules' templates render, to assign or to request reviews from.
type UsersSpec struct {
	Propose UserSet    `yaml:"propose,omitempty"`
	Rules   []UserRule `yaml:"rules,omitempty"`
	If      string     `yaml:"if,omitempty"`
}

// UserSet is the logins the model may pick.
type UserSet struct {
	Users []string `yaml:"users,omitempty"`
}

// UserRule picks the users its templates render when If holds.
type UserRule struct {
	Users []string `yaml:"users,omitempty"`
	If    string   `yaml:"if,omitempty"`
}

// InlineSpec lets the model comment on lines of a pull request's diff.
type InlineSpec struct {
	Propose bool   `yaml:"propose,omitempty"`
	If      string `yaml:"if,omitempty"`
}

// Action kinds, as the operator's bounds name them.
const (
	ActionComment        = "comment"
	ActionLabels         = "labels"
	ActionState          = "state"
	ActionAssign         = "assign"
	ActionReviewers      = "reviewers"
	ActionInlineComments = "inlineComments"
)

// Context source kinds, as the operator's bounds name them.
const (
	ContextThread   = "thread"
	ContextFiles    = "files"
	ContextSearch   = "search"
	ContextRelated  = "related"
	ContextCommands = "commands"
)

// Marker is the hidden text a sticky comment carries, with the task name.
const Marker = "<!-- kritik:task:%s -->"

// StickyMarker is the marker of the named task's sticky comment.
func StickyMarker(name string) string { return fmt.Sprintf(Marker, name) }
