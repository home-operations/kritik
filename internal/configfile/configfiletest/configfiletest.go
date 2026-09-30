// Package configfiletest builds configurations for tests from one YAML
// document, as the configuration file holds it.
package configfiletest

import (
	"testing"

	"github.com/home-operations/kritik/internal/configfile"
)

// Load parses doc as the configuration file, failing the test on any
// error.
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
	return configfile.Parse([]byte(doc))
}
