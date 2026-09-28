package server

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
)

// Hooks is the only surface a forge can reach: /hooks/{connection}.
type Hooks struct {
	addr     string
	basePath string
	handler  http.Handler
	logger   *slog.Logger
}

// NewHooks builds the hook listener bound to addr, routing every hook to h.
// basePath is the dashboard URL's path: the listener shares that URL, so a
// hook also arrives under it (ADR-0014 §2.1). "" for none.
func NewHooks(addr, basePath string, h http.Handler, logger *slog.Logger) *Hooks {
	return &Hooks{addr: addr, basePath: strings.TrimRight(basePath, "/"), handler: h, logger: logger}
}

// Handler returns the hook mux, exported so tests can drive it without
// binding a port. Only POST /hooks/{connection}, and the same under the
// base path, exist; everything else is a 404 so the listener exposes
// nothing to probe.
func (h *Hooks) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /hooks/{connection}", h.handler)
	if h.basePath != "" {
		mux.Handle("POST "+h.basePath+"/hooks/{connection}", h.handler)
	}
	return mux
}

// Run serves until ctx is cancelled.
func (h *Hooks) Run(ctx context.Context) error {
	return Serve(ctx, h.addr, h.Handler(), h.logger.With("listener", "hooks"))
}
