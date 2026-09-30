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
	tests := []struct {
		name string
		path []string
		want []string
	}{
		{"the file", nil, yamlKeys[File]()},
		{"models", []string{"properties", "models"}, yamlKeys[Models]()},
		{"comments", []string{"properties", "comments"}, yamlKeys[Comments]()},
		{"context", []string{"properties", "context", "items"}, yamlKeys[configfile.ContextFile]()},
		{"rules", []string{"properties", "rules", "items"}, yamlKeys[configfile.Rule]()},
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
