package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

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
		recordApplied(current, make(chan string, 1), errors.New("enqueue failed")), gauge, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil || err.Error() != "enqueue failed" {
		t.Fatalf("applyLoop = %v, want the onApplied error", err)
	}
}

func TestApplyLoop(t *testing.T) {
	good, refused, broken, fixed := parseTenant(t, "good"), parseTenant(t, "refused"), parseTenant(t, "broken"), parseTenant(t, "fixed")
	current := configfile.NewCurrent(good)
	reg := prometheus.NewRegistry()
	gauge := server.NewConfigErrorGauge(reg)
	applyGauge := func() float64 {
		t.Helper()
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
		done <- applyLoop(t.Context(), current, apply, onApplied, gauge, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
