package repoconfig

import (
	"encoding/json"
	"maps"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/tasks"
)

// TestSchemaMatchesFile keeps the published JSON Schema's keys in step
// with the types .kritik.yaml decodes into, object by object.
func TestSchemaMatchesFile(t *testing.T) {
	raw, err := os.ReadFile("../../docs/kritik.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	review := []string{"properties", "review", "properties"}
	task := []string{"$defs", "task"}
	taskContext := []string{"$defs", "task", "properties", "context"}
	actions := []string{"$defs", "task", "properties", "actions"}
	tests := []struct {
		name string
		path []string
		want []string
	}{
		{"the file", nil, yamlKeys[File]()},
		{"models", []string{"properties", "models"}, yamlKeys[Models]()},
		{"agent", []string{"properties", "agent"}, yamlKeys[Agent]()},
		{"skip", []string{"properties", "skip"}, yamlKeys[Skip]()},
		{"review", []string{"properties", "review"}, yamlKeys[Review]()},
		{"review.templates", append(review, "templates"), yamlKeys[Templates]()},
		{"review.context", append(review, "context", "items"), yamlKeys[configfile.ContextFile]()},
		{"a scoped instruction", append(review, "instructions", "items", "oneOf", "1"), []string{"path", "paths"}},
		{"a task", task, yamlKeys[tasks.Task]()},
		{"a raw trigger", []string{"$defs", "trigger", "oneOf", "3", "properties", "raw"}, []string{"event", "actions"}},
		{"task models", append(task, "properties", "models"), yamlKeys[tasks.Models]()},
		{"task agent", append(task, "properties", "agent"), yamlKeys[tasks.Agent]()},
		{"task context", taskContext, yamlKeys[tasks.Context]()},
		{"task context thread", append(taskContext, "properties", "thread"), yamlKeys[tasks.Thread]()},
		{"task context command", append(taskContext, "properties", "commands", "items"), yamlKeys[tasks.Command]()},
		{"a query", []string{"$defs", "query"}, yamlKeys[tasks.Query]()},
		{"a field", []string{"$defs", "field"}, yamlKeys[tasks.Field]()},
		{"task actions", actions, yamlKeys[tasks.Actions]()},
		{"the comment action", append(actions, "properties", "comment"), yamlKeys[tasks.CommentSpec]()},
		{"the labels action", append(actions, "properties", "labels"), yamlKeys[tasks.LabelsSpec]()},
		{"labels to propose", append(actions, "properties", "labels", "properties", "propose"), yamlKeys[tasks.LabelSet]()},
		{"a label rule", append(actions, "properties", "labels", "properties", "rules", "items"), yamlKeys[tasks.LabelRule]()},
		{"the state action", append(actions, "properties", "state"), yamlKeys[tasks.StateSpec]()},
		{"states to propose", append(actions, "properties", "state", "properties", "propose"), yamlKeys[tasks.StateSet]()},
		{"a state rule", append(actions, "properties", "state", "properties", "rules", "items"), yamlKeys[tasks.StateRule]()},
		{"a users action", []string{"$defs", "users"}, yamlKeys[tasks.UsersSpec]()},
		{"users to propose", []string{"$defs", "users", "properties", "propose"}, yamlKeys[tasks.UserSet]()},
		{"a users rule", []string{"$defs", "users", "properties", "rules", "items"}, yamlKeys[tasks.UserRule]()},
		{"the inline comments action", append(actions, "properties", "inlineComments"), yamlKeys[tasks.InlineSpec]()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := any(schema)
			for _, k := range tt.path {
				switch n := node.(type) {
				case map[string]any:
					node = n[k]
				case []any:
					i, _ := strconv.Atoi(k)
					node = n[i]
				}
			}
			props, _ := node.(map[string]any)["properties"].(map[string]any)
			got := slices.Sorted(maps.Keys(props))
			slices.Sort(tt.want)
			if !slices.Equal(got, tt.want) {
				t.Fatalf("schema keys %v, want the decoder's %v", got, tt.want)
			}
		})
	}
}

// yamlKeys lists the keys T decodes from.
func yamlKeys[T any]() []string {
	var out []string
	for f := range reflect.TypeFor[T]().Fields() {
		if name, _, _ := strings.Cut(f.Tag.Get("yaml"), ","); name != "" && name != "-" {
			out = append(out, name)
		}
	}
	return out
}

// TestFileFollowsPolicies checks the keys .kritik.yaml takes are the
// settings the policy table gives the repository a rule for.
func TestFileFollowsPolicies(t *testing.T) {
	var ruled []string
	for _, p := range configfile.Policies {
		if p.Repository == "" {
			continue
		}
		ruled = append(ruled, p.Key)
		if _, ok := configfile.SpecValue(&File{}, p.Key); !ok {
			t.Errorf("the table gives %s a repository rule, but the file has no such key", p.Key)
		}
	}
	for _, key := range leafKeys(reflect.TypeFor[File](), "") {
		if !slices.ContainsFunc(ruled, func(r string) bool { return key == r || strings.HasPrefix(key, r+".") }) {
			t.Errorf("the file takes %s, but the table gives the repository no rule for it", key)
		}
	}
}

// leafKeys lists the dotted keys of the settings t decodes, not looking
// into lists.
func leafKeys(t reflect.Type, prefix string) []string {
	var out []string
	for f := range t.Fields() {
		name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if name == "" || name == "-" {
			continue
		}
		if f.Type.Kind() == reflect.Struct {
			out = append(out, leafKeys(f.Type, prefix+name+".")...)
			continue
		}
		out = append(out, prefix+name)
	}
	return out
}
