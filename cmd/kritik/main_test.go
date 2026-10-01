package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/home-operations/kritik/internal/config"
	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/configfile/configfiletest"
	"github.com/home-operations/kritik/internal/server"
)

func parseAccount(t *testing.T, slug string) *configfile.File {
	t.Helper()
	t.Setenv("TEST_MAIN_TOKEN", "tok")
	return configfiletest.Load(t, `
apps:
  - name: `+slug+`-bot
    accounts: [`+slug+`]
    clientId: Iv1.test
    privateKey: { env: TEST_MAIN_TOKEN }
    webhookSecret: { env: TEST_MAIN_TOKEN }
`)
}

// recordApplied is an onApplied that reports applied and returns err.
func recordApplied(ch chan<- string, err error) func(context.Context) error {
	return func(context.Context) error {
		ch <- "applied"
		return err
	}
}

// applyStage is the kritik_config_error value on reg, -1 when absent.
func applyStage(reg *prometheus.Registry) float64 {
	families, _ := reg.Gather()
	for _, mf := range families {
		if mf.GetName() == "kritik_config_error" && len(mf.GetMetric()) == 1 {
			return mf.GetMetric()[0].GetGauge().GetValue()
		}
	}
	return -1
}

// TestApplyConfig: a refusal for the configuration's content is logged and
// raised on the gauge without ending leadership, a database error ends
// it, and the onApplied error is returned as is.
func TestApplyConfig(t *testing.T) {
	for _, tt := range []struct {
		name      string
		apply     error
		onApplied error
		wantErr   string
		wantGauge float64
		applied   bool
	}{
		{name: "applied", wantGauge: 0, applied: true},
		{name: "refused for its content", apply: fmt.Errorf("store: account refused: %w", &pgconn.PgError{Code: "23514"}), wantGauge: 1},
		{name: "database error", apply: errors.New("connection reset"), wantErr: "connection reset", wantGauge: 0},
		{name: "onApplied error", onApplied: errors.New("enqueue failed"), wantErr: "enqueue failed", wantGauge: 0, applied: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reg := prometheus.NewRegistry()
			gauge := server.NewConfigErrorGauge(reg)
			applied := make(chan string, 1)
			err := applyConfig(t.Context(), parseAccount(t, "good"), func(context.Context, *configfile.File) error { return tt.apply },
				recordApplied(applied, tt.onApplied), gauge, slog.New(slog.DiscardHandler))
			if (err == nil) != (tt.wantErr == "") || err != nil && err.Error() != tt.wantErr {
				t.Fatalf("applyConfig = %v, want %q", err, tt.wantErr)
			}
			if (len(applied) != 0) != tt.applied {
				t.Fatalf("onApplied called: %v, want %v", len(applied) != 0, tt.applied)
			}
			if got := applyStage(reg); got != tt.wantGauge {
				t.Fatalf("config error gauge = %v, want %v", got, tt.wantGauge)
			}
		})
	}
}

// TestLoadConfigRefusesNoSignIn: serve refuses a configuration that leaves
// the dashboard no way to sign in.
func TestLoadConfigRefusesNoSignIn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kritik.yaml")
	if err := os.WriteFile(path, []byte("egress: { allowHosts: [first.example] }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); !errors.Is(err, errNoSignIn) {
		t.Fatalf("loadConfig = %v, want errNoSignIn", err)
	}
	t.Setenv("TEST_ADMIN_PASSWORD", "pw")
	if err := os.WriteFile(path, []byte("auth: { admin: { password: { env: TEST_ADMIN_PASSWORD } } }\negress: { allowHosts: [first.example] }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := loadConfig(path)
	if err != nil || len(f.Egress.AllowHosts) != 1 {
		t.Fatalf("loadConfig = %+v, %v", f, err)
	}
}

func TestStoreOptionsOwnerDSN(t *testing.T) {
	cfg := &config.Config{DatabaseURL: "postgres://app", DatabaseOwnerURL: "postgres://owner"}
	for _, tt := range []struct {
		command config.Command
		owner   string
	}{
		{config.CommandServe, "postgres://owner"},
		{config.CommandRun, ""},
	} {
		t.Run(string(tt.command), func(t *testing.T) {
			opts := storeOptions(tt.command, cfg, slog.New(slog.DiscardHandler))
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

func (s *fakeRetentionStore) SweepDiffs(_ context.Context, olderThan time.Duration) (int64, error) {
	s.calls <- sweepCall{name: "diffs", swept: olderThan}
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
	current := configfile.NewCurrent(parseAccount(t, "acme"))
	st := &fakeRetentionStore{calls: make(chan sweepCall, 16)}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		retentionSweep(ctx, st, current, 5*time.Millisecond, slog.New(slog.DiscardHandler))
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
	if want := current.Get().TranscriptRetention(); first.swept != want {
		t.Fatalf("olderThan = %s, want %s", first.swept, want)
	}
	second := next()
	if second.name != "diffs" {
		t.Fatalf("second call = %q, want diffs", second.name)
	}
	if want := current.Get().DiffRetention(); second.swept != want {
		t.Fatalf("olderThan = %s, want %s", second.swept, want)
	}
	if got := next(); got.name != "sessions" {
		t.Fatalf("third call = %q, want sessions", got.name)
	}
	fourth := next()
	if fourth.name != "disabledIndexes" {
		t.Fatalf("fourth call = %q, want disabledIndexes", fourth.name)
	}
	if want := current.Get().DisabledIndexGrace(); fourth.swept != want {
		t.Fatalf("grace = %s, want %s", fourth.swept, want)
	}
	// A second pass proves the loop actually re-runs after the interval.
	if got := next(); got.name != "modelCalls" {
		t.Fatalf("fifth call = %q, want modelCalls", got.name)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("retentionSweep did not return once ctx ended")
	}
}

func TestRetentionSweepLogsErrorsWithoutStopping(t *testing.T) {
	current := configfile.NewCurrent(parseAccount(t, "acme"))
	st := &fakeRetentionStore{calls: make(chan sweepCall, 16), fail: true}
	logs := &warnCounter{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		retentionSweep(ctx, st, current, 5*time.Millisecond, slog.New(logs))
		close(done)
	}()

	// Every sweep fails on every pass; wait for two full passes (8 calls)
	// to prove a failure doesn't stop the loop, and one call more, which
	// the eighth call's warning is logged before.
	for range 9 {
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
	if n := logs.n.Load(); n < 8 {
		t.Fatalf("logged %d warnings for two failed passes, want >= 8", n)
	}
}

// TestLingering: the context outlives its parent by the delay, so a
// listener keeps accepting connections while traffic moves off the pod.
func TestLingering(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	lctx := lingering(ctx, 100*time.Millisecond)
	cancel()
	select {
	case <-lctx.Done():
		t.Fatal("ended with its parent")
	case <-time.After(30 * time.Millisecond):
	}
	select {
	case <-lctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("did not end after the delay")
	}
}
