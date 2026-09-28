// Package configfiletest builds configurations for tests from one YAML
// document holding both layers: the configuration file's keys (auth and
// connections) and the instance spec's (everything else).
package configfiletest

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/home-operations/kritik/internal/configfile"
)

// sealedPrefix marks a value Opener opens: the plain value follows it.
const sealedPrefix = "test:"

// Opener opens the sealed values Split writes.
var Opener configfile.Opener = opener{}

type opener struct{}

func (opener) Open(sealed string) ([]byte, error) {
	plain, ok := strings.CutPrefix(sealed, sealedPrefix)
	if !ok {
		return nil, errors.New("configfiletest: not sealed by the test opener")
	}
	return []byte(plain), nil
}

// Seal is value sealed for Opener.
func Seal(value string) string { return sealedPrefix + value }

// Load parses doc's file keys and merges its spec keys over them, failing
// the test on any error.
func Load(t testing.TB, doc string) *configfile.File {
	t.Helper()
	f, err := Parse(t, doc)
	if err != nil {
		t.Fatalf("configfiletest: %v", err)
	}
	return f
}

// Parse is Load returning the error.
func Parse(t testing.TB, doc string) (*configfile.File, error) {
	t.Helper()
	file, spec := Split(t, doc)
	f, err := configfile.Parse(file)
	if err != nil {
		return nil, err
	}
	return configfile.Merge(f, configfile.InstanceSpec{Spec: spec, Revision: 1}, Opener)
}

// Split is doc's configuration file and its instance spec, as the store
// would hold it: each env or file secret reference in the spec is resolved
// now and sealed for Opener.
func Split(t testing.TB, doc string) (file []byte, spec json.RawMessage) {
	t.Helper()
	var root map[string]any
	if err := yaml.Unmarshal([]byte(doc), &root); err != nil {
		t.Fatalf("configfiletest: %v", err)
	}
	fileKeys := map[string]any{}
	for _, k := range []string{"auth", "connections"} {
		if v, ok := root[k]; ok {
			fileKeys[k] = v
			delete(root, k)
		}
	}
	if len(fileKeys) > 0 {
		var err error
		if file, err = yaml.Marshal(fileKeys); err != nil {
			t.Fatalf("configfiletest: %v", err)
		}
	}
	if root == nil {
		root = map[string]any{}
	}
	sealRefs(t, root)
	spec, err := json.Marshal(root)
	if err != nil {
		t.Fatalf("configfiletest: %v", err)
	}
	return file, spec
}

// sealRefs replaces every {env: X} and {file: P} under v with the value
// sealed for Opener.
func sealRefs(t testing.TB, v any) {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			if m, ok := child.(map[string]any); ok && len(m) == 1 {
				if name, ok := m["env"].(string); ok {
					x[k] = map[string]any{"sealed": Seal(strings.TrimRight(os.Getenv(name), "\r\n"))}
					continue
				}
				if path, ok := m["file"].(string); ok {
					raw, err := os.ReadFile(path)
					if err != nil {
						t.Fatalf("configfiletest: %v", err)
					}
					x[k] = map[string]any{"sealed": Seal(strings.TrimRight(string(raw), "\r\n"))}
					continue
				}
			}
			sealRefs(t, child)
		}
	case []any:
		for _, child := range x {
			sealRefs(t, child)
		}
	}
}
