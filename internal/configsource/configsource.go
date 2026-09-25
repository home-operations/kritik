// Package configsource keeps configfile.Current holding the configuration
// file merged with the dashboard-managed tenants (ADR-0009 §2.5): it
// re-merges when the file changes, when the database announces a dashboard
// write, and on a slow fingerprint poll in case an announcement was missed.
// A merge that fails leaves the last good snapshot live.
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

	"golang.org/x/sync/errgroup"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/sealbox"
	"github.com/home-operations/kritik/internal/server"
	"github.com/home-operations/kritik/internal/store"
)

var _ configfile.Opener = (*sealbox.Keyring)(nil)

// ErrNoDashboardKey is dashboard tenants present with no key to open their
// sealed credentials.
var ErrNoDashboardKey = errors.New("configsource: dashboard tenants exist but KRITIK_DASHBOARD_KEY is not set")

// DefaultPoll is how often Run checks the dashboard fingerprint when Poll
// is unset. Notifications carry changes promptly; the poll only bounds how
// long a missed one goes unnoticed.
const DefaultPoll = 30 * time.Second

// Dashboard is the store surface a Source reads; *store.Store implements it.
type Dashboard interface {
	DashboardTenants(ctx context.Context) ([]configfile.DashboardTenant, error)
	DashboardFingerprint(ctx context.Context) (string, error)
	Listen(ctx context.Context, handlers store.ListenHandlers)
}

// Source produces the merged configuration. Set the exported fields, call
// Load once, then Run.
type Source struct {
	Store Dashboard
	// Keyring opens dashboard tenants' sealed credentials; nil when no key
	// is configured, which is fine until a dashboard tenant exists.
	Keyring *sealbox.Keyring
	// Current receives every merged snapshot. Load creates it when nil.
	Current *configfile.Current
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// Poll defaults to DefaultPoll.
	Poll time.Duration
	// Errors, when set, is raised at the merge stage while LastError is
	// not nil.
	Errors *server.ConfigErrorGauge

	mu sync.Mutex
	// file is the latest file that parsed, whether or not it has merged.
	file *configfile.File
	// applied is what Current was last set from.
	appliedFile *configfile.File
	appliedRows []configfile.DashboardTenant
	lastErr     error
	loggedErr   string
}

// Load reads the file at path and the dashboard tenants, merges them and
// seeds Current. Any failure is returned: at startup there is no last good
// snapshot to keep.
func (s *Source) Load(ctx context.Context, path string) (*configfile.File, error) {
	file, err := configfile.Load(path)
	if err != nil {
		return nil, err
	}
	rows, err := s.Store.DashboardTenants(ctx)
	if err != nil {
		return nil, fmt.Errorf("configsource: %w", err)
	}
	merged, err := s.merge(file, rows)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.file, s.appliedFile, s.appliedRows = file, file, rows
	s.mu.Unlock()
	if s.Current == nil {
		s.Current = configfile.NewCurrent(merged)
	} else {
		s.Current.Set(merged)
	}
	return merged, nil
}

// Run keeps Current fresh until ctx ends: the file at path is re-read every
// interval, a kritik_config notification or a reconnect of the listener
// re-reads the dashboard, and so does a change in the dashboard
// fingerprint, checked every Poll. Call Load first.
func (s *Source) Run(ctx context.Context, path string, interval time.Duration) error {
	trigger := make(chan struct{}, 1)
	kick := func() {
		select {
		case trigger <- struct{}{}:
		default:
		}
	}
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		configfile.Watch(ctx, path, interval, s.logger(), func(f *configfile.File) {
			s.mu.Lock()
			s.file = f
			s.mu.Unlock()
			kick()
		})
		return nil
	})
	g.Go(func() error {
		s.Store.Listen(ctx, store.ListenHandlers{OnConfig: func(string) { kick() }, OnReconnect: kick})
		return nil
	})
	g.Go(func() error {
		s.poll(ctx, kick)
		return nil
	})
	g.Go(func() error {
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-trigger:
				s.refresh(ctx)
			}
		}
	})
	return g.Wait()
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
		fp, err := s.Store.DashboardFingerprint(ctx)
		if err != nil {
			if ctx.Err() == nil && err.Error() != logged {
				logged = err.Error()
				s.logger().Warn("configsource: dashboard fingerprint check failed", "error", err)
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

// refresh merges the latest file with the latest dashboard tenants into
// Current, unless neither has changed since Current was last set.
func (s *Source) refresh(ctx context.Context) {
	rows, err := s.Store.DashboardTenants(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.fail(fmt.Errorf("configsource: %w", err))
		}
		return
	}
	s.mu.Lock()
	file := s.file
	unchanged := file == s.appliedFile && slices.EqualFunc(rows, s.appliedRows, sameRow)
	s.mu.Unlock()
	if unchanged {
		s.succeed()
		return
	}
	merged, err := s.merge(file, rows)
	if err != nil {
		s.fail(err)
		return
	}
	s.mu.Lock()
	s.appliedFile, s.appliedRows = file, rows
	s.mu.Unlock()
	s.Current.Set(merged)
	s.succeed()
	s.logger().Info("configuration reloaded", "hash", merged.Hash(), "tenants", len(merged.Tenants), "dashboard_tenants", len(rows))
}

// sameRow compares specs as well as revisions: a tenant deleted and created
// again starts over at revision 1.
func sameRow(a, b configfile.DashboardTenant) bool {
	return a.Slug == b.Slug && a.Revision == b.Revision && bytes.Equal(a.Spec, b.Spec)
}

func (s *Source) merge(file *configfile.File, rows []configfile.DashboardTenant) (*configfile.File, error) {
	var open configfile.Opener
	if s.Keyring != nil {
		open = s.Keyring
	} else if len(rows) > 0 {
		return nil, ErrNoDashboardKey
	}
	merged, err := configfile.Merge(file, rows, open)
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
	s.mu.Unlock()
	s.Errors.Set(server.ConfigErrorMerge, false)
}

func (s *Source) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}
