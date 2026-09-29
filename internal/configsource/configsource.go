// Package configsource keeps configfile.Current holding the configuration
// file merged with the instance spec the dashboard keeps in Postgres
// (ADR-0014 §2.2): it re-merges when the file changes, when the database
// announces a spec write, and on a slow fingerprint poll in case an
// announcement was missed. A merge that fails leaves the last good snapshot
// live.
package configsource

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/sealbox"
	"github.com/home-operations/kritik/internal/server"
	"github.com/home-operations/kritik/internal/store"
)

var _ configfile.Opener = (*sealbox.Keyring)(nil)

// ErrNoDashboardKey is a stored instance spec with no key to open its
// sealed credentials.
var ErrNoDashboardKey = errors.New("configsource: an instance spec is stored but KRITIK_DASHBOARD_KEY is not set")

// DefaultPoll is how often Run checks the dashboard fingerprint when Poll
// is unset. Notifications carry changes promptly; the poll only bounds how
// long a missed one goes unnoticed.
const DefaultPoll = 30 * time.Second

// Dashboard is the store surface a Source reads; *store.Store implements it.
type Dashboard interface {
	InstanceSpec(ctx context.Context) (configfile.InstanceSpec, error)
	InstanceSpecFingerprint(ctx context.Context) (string, error)
	Listen(ctx context.Context, handlers store.ListenHandlers)
}

// Source produces the merged configuration. Set the exported fields, call
// Load once, then Run.
type Source struct {
	Store Dashboard
	// Keyring opens the spec's sealed credentials; nil when no key is
	// configured, which is fine until a spec is stored.
	Keyring *sealbox.Keyring
	// Current receives every merged snapshot. Load creates it when nil.
	Current *configfile.Current
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// Poll defaults to DefaultPoll.
	Poll time.Duration
	// Errors, when set, is raised at the merge stage while LastError is
	// not nil, the file's latest content does not parse, or the running
	// configuration leaves a file connection out.
	Errors *server.ConfigErrorGauge

	mu sync.Mutex
	// file is the latest file that parsed, whether or not it has merged.
	file *configfile.File
	// applied is what Current was last set from.
	appliedFile *configfile.File
	appliedSpec configfile.InstanceSpec
	lastErr     error
	loggedErr   string
	// fileErr is why the file's latest content failed to parse, nil once
	// content that parses replaces it; it keeps the merge gauge raised
	// even while the last good file still merges.
	fileErr error
	// skipped are the file connections the running configuration leaves
	// out.
	skipped []configfile.SkippedConnection
}

// Load reads the file at path, none when path is "", and the instance spec,
// merges them and seeds Current. Any failure is returned: at startup there
// is no last good snapshot to keep.
func (s *Source) Load(ctx context.Context, path string) (*configfile.File, error) {
	file, err := configfile.Load(path)
	if err != nil {
		return nil, err
	}
	spec, err := s.Store.InstanceSpec(ctx)
	if err != nil {
		return nil, fmt.Errorf("configsource: %w", err)
	}
	merged, err := s.merge(file, spec)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.file, s.appliedFile, s.appliedSpec = file, file, spec
	s.mu.Unlock()
	if s.Current == nil {
		s.Current = configfile.NewCurrent(merged)
	} else {
		s.Current.Set(merged)
	}
	s.noteSkipped(merged.Skipped())
	s.succeed()
	return merged, nil
}

// Run keeps Current fresh until ctx ends: the file at path, if any, is
// re-read every interval, a kritik_config notification or a reconnect of
// the listener re-reads the spec, and so does a change in the spec's
// fingerprint, checked every Poll. Call Load first.
func (s *Source) Run(ctx context.Context, path string, interval time.Duration) {
	trigger := make(chan struct{}, 1)
	kick := func() {
		select {
		case trigger <- struct{}{}:
		default:
		}
	}
	var wg sync.WaitGroup
	if path != "" {
		wg.Go(func() {
			configfile.Watch(ctx, path, interval, s.logger(), func(f *configfile.File) {
				s.mu.Lock()
				s.file, s.fileErr = f, nil
				s.mu.Unlock()
				kick()
			}, func(err error) {
				s.mu.Lock()
				s.fileErr = err
				s.mu.Unlock()
				s.Errors.Set(server.ConfigErrorMerge, true)
			})
		})
	}
	wg.Go(func() {
		s.Store.Listen(ctx, store.ListenHandlers{OnConfig: func(string) { kick() }, OnReconnect: kick})
	})
	wg.Go(func() { s.poll(ctx, kick) })
	wg.Go(func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-trigger:
				s.refresh(ctx)
			}
		}
	})
	wg.Wait()
}

// LastError is why the latest refresh failed, nil once one succeeds.
func (s *Source) LastError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
}

func (s *Source) poll(ctx context.Context, kick func()) {
	every := s.Poll
	if every <= 0 {
		every = DefaultPoll
	}
	t := time.NewTicker(every)
	defer t.Stop()
	var last, logged string
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		fp, err := s.Store.InstanceSpecFingerprint(ctx)
		if err != nil {
			if ctx.Err() == nil && err.Error() != logged {
				logged = err.Error()
				s.logger().Warn("configsource: instance spec fingerprint check failed", "error", err)
			}
			continue
		}
		logged = ""
		if fp != last {
			last = fp
			kick()
		}
	}
}

// refresh merges the latest file with the latest spec into Current, unless
// neither has changed since Current was last set.
func (s *Source) refresh(ctx context.Context) {
	spec, err := s.Store.InstanceSpec(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.fail(fmt.Errorf("configsource: %w", err))
		}
		return
	}
	s.mu.Lock()
	file := s.file
	unchanged := file == s.appliedFile && sameSpec(spec, s.appliedSpec)
	s.mu.Unlock()
	if unchanged {
		s.succeed()
		return
	}
	merged, err := s.merge(file, spec)
	if err != nil {
		s.fail(err)
		return
	}
	s.mu.Lock()
	s.appliedFile, s.appliedSpec = file, spec
	s.mu.Unlock()
	s.Current.Set(merged)
	s.noteSkipped(merged.Skipped())
	s.succeed()
	s.logger().Info("configuration reloaded", "hash", merged.Hash(), "accounts", len(merged.Accounts), "spec_revision", spec.Revision)
}

// sameSpec compares specs as well as revisions, which a restored database
// could repeat.
func sameSpec(a, b configfile.InstanceSpec) bool {
	return a.Revision == b.Revision && bytes.Equal(a.Spec, b.Spec)
}

func (s *Source) merge(file *configfile.File, spec configfile.InstanceSpec) (*configfile.File, error) {
	var open configfile.Opener
	if s.Keyring != nil {
		open = s.Keyring
	} else if spec.Revision > 0 {
		return nil, ErrNoDashboardKey
	}
	merged, err := configfile.Merge(file, spec, open)
	if err != nil {
		return nil, fmt.Errorf("configsource: %w", err)
	}
	return merged, nil
}

// fail records err and logs it unless it is the one already logged, so a
// collision that persists across triggers is reported once.
func (s *Source) fail(err error) {
	s.mu.Lock()
	s.lastErr = err
	repeat := err.Error() == s.loggedErr
	s.loggedErr = err.Error()
	s.mu.Unlock()
	s.Errors.Set(server.ConfigErrorMerge, true)
	if !repeat {
		s.logger().Error("configsource: rejected, keeping the last good configuration", "error", err)
	}
}

func (s *Source) succeed() {
	s.mu.Lock()
	s.lastErr, s.loggedErr = nil, ""
	failing := s.fileErr != nil || len(s.skipped) > 0
	s.mu.Unlock()
	s.Errors.Set(server.ConfigErrorMerge, failing)
}

// noteSkipped records the file connections the latest merge left out, and
// logs them unless the previous merge left out the same ones, so a clash
// that persists across refreshes is reported once.
func (s *Source) noteSkipped(skipped []configfile.SkippedConnection) {
	s.mu.Lock()
	repeat := slices.Equal(skipped, s.skipped)
	s.skipped = skipped
	s.mu.Unlock()
	if repeat {
		return
	}
	for _, c := range skipped {
		s.logger().Warn("configsource: file connection left out of the running configuration", "connection", c.Name, "reason", c.Reason)
	}
}

func (s *Source) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}
