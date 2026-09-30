// Package configsource loads the configuration file (ADR-0019) into
// configfile.Current. It is read once, at startup: a change takes a
// restart (ADR-0022 §2.1).
package configsource

import (
	"errors"

	"github.com/home-operations/kritik/internal/configfile"
)

// ErrNoSignIn is a configuration that leaves the dashboard no way to sign
// in, refused where RequireSignIn is set.
var ErrNoSignIn = errors.New(
	"configsource: the dashboard has no way to sign in: set KRITIK_AUTH_ADMIN_PASSWORD, or auth in the configuration file")

// Source produces the running configuration. Set the exported fields, then
// call Load.
type Source struct {
	// Current receives the configuration Load reads. Load creates it when
	// nil.
	Current *configfile.Current
	// RequireSignIn refuses a configuration with no way to sign in, for a
	// process that serves the dashboard. Other roles may run without one.
	RequireSignIn bool
}

// Load reads the file at path, none when path is "", and seeds Current.
// Any failure is returned, and fails startup.
func (s *Source) Load(path string) (*configfile.File, error) {
	f, err := configfile.Load(path)
	if err == nil {
		err = s.check(f)
	}
	if err != nil {
		return nil, err
	}
	if s.Current == nil {
		s.Current = configfile.NewCurrent(f)
	} else {
		s.Current.Set(f)
	}
	return f, nil
}

func (s *Source) check(f *configfile.File) error {
	if s.RequireSignIn && !f.Auth.Configured() {
		return ErrNoSignIn
	}
	return nil
}
