package configfile

import "testing"

// load parses doc as the configuration file.
func load(t *testing.T, doc string) (*File, error) {
	t.Helper()
	return Parse([]byte(doc))
}

func mustLoad(t *testing.T, doc string) *File {
	t.Helper()
	f, err := load(t, doc)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return f
}
