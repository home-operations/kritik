//go:build !linux

package runner

import "errors"

// hideEnviron is only implemented where runner pods run; elsewhere the run
// tool is never offered.
func hideEnviron() error {
	return errors.New("runner: hiding the environment from commands needs Linux")
}
