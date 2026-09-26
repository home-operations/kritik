package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/home-operations/kritik/internal/config"
	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/server"
	"github.com/home-operations/kritik/internal/store"
)

func parseTenant(t *testing.T, slug string) *configfile.File {
	t.Helper()
	t.Setenv("TEST_MAIN_TOKEN", "tok")
	f, err := configfile.Parse([]byte(`
tenants:
  - slug: ` + slug + `
    installations:
      - name: ` + slug + `-bot
        forge: forgejo
        account: ` + slug + `
        token: { env: TEST_MAIN_TOKEN }
        webhookSecret: { env: TEST_MAIN_TOKEN }
`))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// recordApplied is an onApplied that reports the applied slug and returns err.
func recordApplied(current *configfile.Current, ch chan<- string, err error) func(context.Context) error {
	return func(context.Context) error {
		ch <- current.Get().Tenants[0].Slug
		return err
	}
}

func TestApplyLoopReturnsOnAppliedError(t *testing.T) {
	current := configfile.NewCurrent(parseTenant(t, "good"))
	gauge := server.NewConfigErrorGauge(prometheus.NewRegistry())
	err := applyLoop(t.Context(), current, func(context.Context, *configfile.File) error { return nil },
		recordApplied(current, make(chan string, 1), errors.New("enqueue failed")), time.Hour, gauge, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil || err.Error() != "enqueue failed" {
		t.Fatalf("applyLoop = %v, want the onApplied error", err)
	}
}

// applyStage is the kritik_config_error{stage="apply"} value on reg, -1 when
// absent.
func applyStage(reg *prometheus.Registry) float64 {
	families, _ := reg.Gather()
	for _, mf := range families {
		for _, m := range mf.GetMetric() {
			if m.GetLabel()[0].GetValue() == "apply" {
				return m.GetGauge().GetValue()
			}
		}
	}
	return -1
}

func TestApplyLoop(t *testing.T) {
	good, refused, broken, fixed := parseTenant(t, "good"), parseTenant(t, "refused"), parseTenant(t, "broken"), parseTenant(t, "fixed")
	current := configfile.NewCurrent(good)
	reg := prometheus.NewRegistry()
	gauge := server.NewConfigErrorGauge(reg)
	applyGauge := func() float64 { return applyStage(reg) }
	appliedCh := make(chan string, 10)
	attempts := make(chan string, 10)
	apply := func(_ context.Context, f *configfile.File) error {
		slug := f.Tenants[0].Slug
		attempts <- slug
		switch slug {
		case "refused":
			return fmt.Errorf("store: tenant refused: %w", store.ErrManagedBy)
		case "broken":
			return errors.New("connection reset")
		}
		return nil
	}
	onApplied := recordApplied(current, appliedCh, nil)
	done := make(chan error, 1)
	go func() {
		done <- applyLoop(t.Context(), current, apply, onApplied, time.Hour, gauge, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	next := func(t *testing.T, ch chan string) string {
		t.Helper()
		select {
		case v := <-ch:
			return v
		case <-time.After(5 * time.Second):
			t.Fatal("timed out")
			return ""
		}
	}

	if got := next(t, appliedCh); got != "good" {
		t.Fatalf("applied %s, want good", got)
	}
	next(t, attempts)

	current.Set(refused)
	if got := next(t, attempts); got != "refused" {
		t.Fatalf("attempted %s, want refused", got)
	}
	current.Set(refused)
	current.Set(fixed)
	if got := next(t, appliedCh); got != "fixed" {
		t.Fatalf("applied %s, want fixed", got)
	}
	// A repeat of the refused snapshot is not retried.
	if got := next(t, attempts); got != "fixed" {
		t.Fatalf("attempted %s after the refusal, want fixed", got)
	}
	if v := applyGauge(); v != 0 {
		t.Fatalf("apply gauge after recovery = %v", v)
	}

	current.Set(refused)
	next(t, attempts)
	for applyGauge() != 1 {
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-done:
		t.Fatalf("applyLoop returned %v on a content refusal", err)
	default:
	}

	current.Set(broken)
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "connection reset") {
			t.Fatalf("applyLoop = %v, want the database error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("applyLoop kept running after a database error")
	}
}

// errorCounter counts error records.
type errorCounter struct{ n atomic.Int32 }

func (h *errorCounter) Enabled(context.Context, slog.Level) bool { return true }
func (h *errorCounter) Handle(_ context.Context, r slog.Record) error {
	if r.Level >= slog.LevelError {
		h.n.Add(1)
	}
	return nil
}
func (h *errorCounter) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *errorCounter) WithGroup(string) slog.Handler      { return h }

func TestApplyLoopRetriesARefusal(t *testing.T) {
	current := configfile.NewCurrent(parseTenant(t, "racy"))
	reg := prometheus.NewRegistry()
	gauge := server.NewConfigErrorGauge(reg)
	logs := &errorCounter{}
	var attempts atomic.Int32
	apply := func(context.Context, *configfile.File) error {
		// The first three attempts lose a race; the fourth wins.
		if attempts.Add(1) <= 3 {
			return fmt.Errorf("store: tenant racy: %w", store.ErrManagedBy)
		}
		return nil
	}
	applied := make(chan string, 1)
	go func() {
		_ = applyLoop(t.Context(), current, apply, recordApplied(current, applied, nil), 5*time.Millisecond, gauge, slog.New(logs))
	}()
	select {
	case <-applied:
	case <-time.After(5 * time.Second):
		t.Fatalf("a refused snapshot was never retried (%d attempts)", attempts.Load())
	}
	if n := attempts.Load(); n != 4 {
		t.Fatalf("attempts = %d, want 4", n)
	}
	if n := logs.n.Load(); n != 1 {
		t.Fatalf("logged %d errors for one distinct refusal, want 1", n)
	}
	if v := applyStage(reg); v != 0 {
		t.Fatalf("apply gauge after a successful retry = %v, want 0", v)
	}
}

func TestStoreOptionsOwnerDSN(t *testing.T) {
	cfg := &config.Config{DatabaseURL: "postgres://app", DatabaseOwnerURL: "postgres://owner"}
	for _, tt := range []struct {
		role  config.Role
		owner string
	}{
		{config.RoleAll, "postgres://owner"},
		{config.RoleWorker, "postgres://owner"},
		{config.RoleIngest, ""},
		{config.RoleRunner, ""},
		{config.RoleWeb, ""},
	} {
		t.Run(string(tt.role), func(t *testing.T) {
			opts := storeOptions(tt.role, cfg, slog.New(slog.DiscardHandler))
			if opts.OwnerURL != tt.owner || opts.AppURL != "postgres://app" {
				t.Errorf("owner = %q, app = %q; want owner %q", opts.OwnerURL, opts.AppURL, tt.owner)
			}
		})
	}
}

// sweepCall is one call fakeRetentionStore received, carrying the argument
// it was given. Sending the value on the channel (rather than stashing it in
// a field the test reads separately) means the data only ever crosses
// goroutines through the channel op itself, so there's no shared state for
// a later, unsynchronized pass to race against.
type sweepCall struct {
	name    string
	swept   time.Duration
	sweptAt time.Time
}

// fakeRetentionStore signals every call on a channel, so a test can wait for
// a specific pass instead of sleeping.
type fakeRetentionStore struct {
	calls chan sweepCall
	fail  bool
}

func (s *fakeRetentionStore) SweepModelCalls(_ context.Context, olderThan time.Duration) (int64, error) {
	s.calls <- sweepCall{name: "modelCalls", swept: olderThan}
	if s.fail {
		return 0, errors.New("db down")
	}
	return 1, nil
}

func (s *fakeRetentionStore) SweepSessions(_ context.Context, now time.Time) (int64, error) {
	s.calls <- sweepCall{name: "sessions", sweptAt: now}
	if s.fail {
		return 0, errors.New("db down")
	}
	return 1, nil
}

func (s *fakeRetentionStore) SweepDisabledIndexes(_ context.Context, grace time.Duration) (int64, error) {
	s.calls <- sweepCall{name: "disabledIndexes", swept: grace}
	if s.fail {
		return 0, errors.New("db down")
	}
	return 1, nil
}

// warnCounter counts warn-level (or higher) records.
type warnCounter struct{ n atomic.Int32 }

func (h *warnCounter) Enabled(context.Context, slog.Level) bool { return true }
func (h *warnCounter) Handle(_ context.Context, r slog.Record) error {
	if r.Level >= slog.LevelWarn {
		h.n.Add(1)
	}
	return nil
}
func (h *warnCounter) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *warnCounter) WithGroup(string) slog.Handler      { return h }

func TestRetentionSweep(t *testing.T) {
	current := configfile.NewCurrent(parseTenant(t, "acme"))
	st := &fakeRetentionStore{calls: make(chan sweepCall, 16)}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		retentionSweep(ctx, st, current, 5*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
		close(done)
	}()

	next := func() sweepCall {
		select {
		case c := <-st.calls:
			return c
		case <-time.After(5 * time.Second):
			t.Fatal("retentionSweep never called the store")
			return sweepCall{}
		}
	}
	// The first pass runs immediately, without waiting for a tick.
	first := next()
	if first.name != "modelCalls" {
		t.Fatalf("first call = %q, want modelCalls", first.name)
	}
	if want := current.Get().Retention.TranscriptsOrDefault(); first.swept != want {
		t.Fatalf("olderThan = %s, want %s", first.swept, want)
	}
	if got := next(); got.name != "sessions" {
		t.Fatalf("second call = %q, want sessions", got.name)
	}
	third := next()
	if third.name != "disabledIndexes" {
		t.Fatalf("third call = %q, want disabledIndexes", third.name)
	}
	if want := current.Get().DisabledIndexGrace(); third.swept != want {
		t.Fatalf("grace = %s, want %s", third.swept, want)
	}
	// A second pass proves the loop actually re-runs after the interval.
	if got := next(); got.name != "modelCalls" {
		t.Fatalf("fourth call = %q, want modelCalls", got.name)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("retentionSweep did not return once ctx ended")
	}
}

func TestRetentionSweepLogsErrorsWithoutStopping(t *testing.T) {
	current := configfile.NewCurrent(parseTenant(t, "acme"))
	st := &fakeRetentionStore{calls: make(chan sweepCall, 16), fail: true}
	logs := &warnCounter{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		retentionSweep(ctx, st, current, 5*time.Millisecond, slog.New(logs))
		close(done)
	}()

	// Every sweep fails on every pass; wait for two full passes (6 calls)
	// to prove a failure doesn't stop the loop, and one call more, which
	// the sixth call's warning is logged before.
	for range 7 {
		select {
		case <-st.calls:
		case <-time.After(5 * time.Second):
			t.Fatal("retentionSweep stopped calling the store after an error")
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("retentionSweep did not return once ctx ended")
	}
	if n := logs.n.Load(); n < 6 {
		t.Fatalf("logged %d warnings for two failed passes, want >= 6", n)
	}
}
