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
	review := []string{"properties", "review", "properties"}
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
