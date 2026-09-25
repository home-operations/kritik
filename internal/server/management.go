// Package server holds kritik's HTTP listeners. The management listener
// (health and metrics) runs in every role; the hook listener runs only in the
// roles that ingest forge webhooks.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// shutdownTimeout bounds how long an in-flight request may hold up process
// exit once the root context is cancelled.
const shutdownTimeout = 10 * time.Second

// Management serves /healthz, /readyz and /metrics. Readiness starts false and
// is flipped by the role once its own dependencies are up, so a pod is not
// routed to before it can do work.
type Management struct {
	addr     string
	ready    atomic.Bool
	registry *prometheus.Registry
	logger   *slog.Logger
}

// NewManagement builds a management listener bound to addr with its own
// Prometheus registry, so tests can construct several without colliding on
// the default global registry.
func NewManagement(addr string, logger *slog.Logger) *Management {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return &Management{addr: addr, registry: reg, logger: logger}
}

// Registry exposes the registry so roles can register their own collectors.
func (m *Management) Registry() *prometheus.Registry { return m.registry }

// SetReady flips the readiness probe.
func (m *Management) SetReady(ready bool) { m.ready.Store(ready) }

// Handler returns the management mux, exported so tests can drive it without
// binding a port.
func (m *Management) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !m.ready.Load() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.Handle("GET /metrics", promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{}))
	return mux
}

// Run serves until ctx is cancelled, then drains within shutdownTimeout.
func (m *Management) Run(ctx context.Context) error {
	return Serve(ctx, m.addr, m.Handler(), m.logger.With("listener", "management"))
}

// Serve runs h on addr until ctx is cancelled, then drains within
// shutdownTimeout.
func Serve(ctx context.Context, addr string, h http.Handler, logger *slog.Logger) error {
	return ServeDrain(ctx, addr, h, shutdownTimeout, logger)
}

// ServeDrain is Serve with its own drain. Requests still in flight when it
// runs out are cut: a stopping process is not failing.
func ServeDrain(ctx context.Context, addr string, h http.Handler, drain time.Duration, logger *slog.Logger) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", addr)
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("server: %w", err)
	case <-ctx.Done():
	}
	dctx, cancel := context.WithTimeout(context.Background(), drain)
	defer cancel()
	if err := srv.Shutdown(dctx); err != nil {
		logger.Warn("requests cut at shutdown", "drain", drain, "error", err)
		return srv.Close()
	}
	return nil
}
