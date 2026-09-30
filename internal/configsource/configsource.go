// Package configsource keeps configfile.Current holding the configuration
// file (ADR-0019): it re-reads the file on an interval, and content that
// does not load leaves the last good configuration running.
package configsource

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/server"
)

// ErrNoSignIn is a configuration that leaves the dashboard no way to sign
// in, refused where RequireSignIn is set.
var ErrNoSignIn = errors.New(
	"configsource: the dashboard has no way to sign in: set KRITIK_AUTH_ADMIN_PASSWORD, or auth in the configuration file")

// Source produces the running configuration. Set the exported fields, call
// Load once, then Run.
type Source struct {
	// Current receives every configuration that loads. Load creates it
	// when nil.
	Current *configfile.Current
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// RequireSignIn refuses a configuration with no way to sign in, at
	// startup and on every reload, for a process that serves the
	// dashboard: a reload that removed the last sign-in would lock every
	// admin out. Other roles may run without one.
	RequireSignIn bool
	// Errors, when set, is raised at the load stage while the file's
	// latest content is refused.
	Errors *server.ConfigErrorGauge

	mu      sync.Mutex
	lastErr error
}

// Load reads the file at path, none when path is "", and seeds Current.
// Any failure is returned: at startup there is no last good configuration
// to keep.
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
	s.Errors.Set(server.ConfigErrorLoad, false)
	return f, nil
}

// Run keeps Current fresh until ctx ends, re-reading the file at path every
// interval. With no file there is nothing to watch. Call Load first.
func (s *Source) Run(ctx context.Context, path string, interval time.Duration) {
	if path == "" {
		return
	}
	configfile.Watch(ctx, path, interval, s.logger(), func(f *configfile.File) {
		if err := s.check(f); err != nil {
			s.logger().Error("configsource: rejected, keeping the last good configuration", "error", err)
			s.reject(err)
			return
		}
		s.Current.Set(f)
		s.mu.Lock()
		s.lastErr = nil
		s.mu.Unlock()
		s.Errors.Set(server.ConfigErrorLoad, false)
		s.logger().Info("configuration reloaded", "hash", f.Hash(), "accounts", len(f.Accounts))
	}, s.reject)
}

// LastError is why the file's latest content was refused, nil while the
// running configuration is its latest content.
func (s *Source) LastError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
}

func (s *Source) reject(err error) {
	s.mu.Lock()
	s.lastErr = err
	s.mu.Unlock()
	s.Errors.Set(server.ConfigErrorLoad, true)
}

func (s *Source) check(f *configfile.File) error {
	if s.RequireSignIn && !f.Auth.Configured() {
		return ErrNoSignIn
	}
	return nil
}

func (s *Source) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}
