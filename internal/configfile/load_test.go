package configfile

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// testOpener opens a value sealed as "test:<plain>".
type testOpener struct{}

func (testOpener) Open(sealed string) ([]byte, error) {
	plain, ok := strings.CutPrefix(sealed, "test:")
	if !ok {
		return nil, errors.New("not sealed by the test opener")
	}
	return []byte(plain), nil
}

// load parses doc's configuration file keys, auth and connections, and
// merges the rest over them as the instance spec, each env or file secret
// reference in the spec sealed as the dashboard would store it.
func load(t *testing.T, doc string) (*File, error) {
	t.Helper()
	file, spec := split(t, doc)
	f, err := Parse(file)
	if err != nil {
		return nil, err
	}
	return Merge(f, InstanceSpec{Spec: spec, Revision: 1}, testOpener{})
}

func mustLoad(t *testing.T, doc string) *File {
	t.Helper()
	f, err := load(t, doc)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return f
}

// split is doc's configuration file and its spec, as load merges them.
func split(t *testing.T, doc string) ([]byte, json.RawMessage) {
	t.Helper()
	var root map[string]any
	if err := yaml.Unmarshal([]byte(doc), &root); err != nil {
		t.Fatalf("split: %v", err)
	}
	if root == nil {
		root = map[string]any{}
	}
	fileKeys := map[string]any{}
	for _, k := range []string{"auth", "connections"} {
		if v, ok := root[k]; ok {
			fileKeys[k], root[k] = v, nil
			delete(root, k)
		}
	}
	var file []byte
	if len(fileKeys) > 0 {
		var err error
		if file, err = yaml.Marshal(fileKeys); err != nil {
			t.Fatal(err)
		}
	}
	sealRefs(root)
	spec, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	return file, spec
}

func sealRefs(v any) {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			if m, ok := child.(map[string]any); ok && len(m) == 1 {
				if name, ok := m["env"].(string); ok {
					x[k] = map[string]any{"sealed": "test:" + os.Getenv(name)}
					continue
				}
				if path, ok := m["file"].(string); ok {
					raw, err := os.ReadFile(path)
					if err != nil {
						raw = nil
					}
					x[k] = map[string]any{"sealed": "test:" + strings.TrimRight(string(raw), "\r\n")}
					continue
				}
			}
			sealRefs(child)
		}
	case []any:
		for _, child := range x {
			sealRefs(child)
		}
	}
}
