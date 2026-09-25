package server

import (
	"context"
	"log/slog"
	"net/http"
)

// Hooks is the only surface a forge can reach: /hooks/{installation}.
type Hooks struct {
	addr    string
	handler http.Handler
	logger  *slog.Logger
}

// NewHooks builds the hook listener bound to addr, routing every hook to h.
func NewHooks(addr string, h http.Handler, logger *slog.Logger) *Hooks {
	return &Hooks{addr: addr, handler: h, logger: logger}
}

// Handler returns the hook mux, exported so tests can drive it without
// binding a port. Only POST /hooks/{installation} exists; everything else
// is a 404 so the listener exposes nothing to probe.
func (h *Hooks) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /hooks/{installation}", h.handler)
	return mux
}

// Run serves until ctx is cancelled.
func (h *Hooks) Run(ctx context.Context) error {
	return Serve(ctx, h.addr, h.Handler(), h.logger.With("listener", "hooks"))
}
