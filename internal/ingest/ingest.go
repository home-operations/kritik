// Package ingest is the only surface a forge reaches. It looks the
// connection up by hook path, verifies the signature with that
// connection's secret, parses the payload into a forge-neutral event, and
// hands it to a Dispatcher. It never does work itself.
package ingest

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/metrics"
	"github.com/home-operations/kritik/internal/webhook"
)

// Request is a verified, parsed webhook with the configuration it applies to.
type Request struct {
	File       *configfile.File
	Tenant     *configfile.Tenant
	Connection *configfile.Connection
	Event      webhook.Event
}

// Outcome is what the dispatcher did with a request, for the response and
// the log line.
type Outcome struct {
	// Status is enqueued, skipped or ignored.
	Status string
	// Reason explains a skip or ignore: filter, fork, disabled, duplicate,
	// not-default-branch, no-mention, action.
	Reason string
	Job    string
}

// Outcome statuses.
const (
	Enqueued = "enqueued"
	Skipped  = "skipped"
	Ignored  = "ignored"
)

// Dispatcher turns a request into rows and jobs. The store-backed
// implementation is Service; tests use a fake.
type Dispatcher interface {
	Dispatch(ctx context.Context, req Request) (Outcome, error)
}

// DeliveryRecorder notes that a connection's webhook delivered a request
// kritik verified. The store-backed implementation is Service.
type DeliveryRecorder interface {
	RecordDelivery(ctx context.Context, tenantID, connectionID string) error
}

// Handler serves POST /hooks/{connection}.
type Handler struct {
	current *configfile.Current
	disp    Dispatcher
	logger  *slog.Logger
	// Metrics may be nil.
	Metrics *metrics.Metrics
	// Deliveries may be nil.
	Deliveries DeliveryRecorder
}

// NewHandler builds the hook handler over the current configuration.
func NewHandler(current *configfile.Current, disp Dispatcher, logger *slog.Logger) *Handler {
	return &Handler{current: current, disp: disp, logger: logger}
}

// ServeHTTP verifies, parses and dispatches. Status codes: 404 for an
// unknown connection, 401 for a bad signature, 400 for an unparsable
// payload, 413 for an oversized one, 204 for a ping, 202 for anything
// accepted (enqueued, skipped or ignored: the forge only needs to know the
// delivery landed), 500 when the dispatcher failed and the forge should
// redeliver.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("connection")
	file := h.current.Get()
	in, tenant, ok := file.Connection(name)
	if !ok {
		// The name is the caller's, not ours: labelling by it would let any
		// request mint a new series.
		h.Metrics.Webhook("", "unknown_connection")
		http.Error(w, "unknown connection", http.StatusNotFound)
		return
	}
	logger := h.logger.With("connection", name, "tenant", tenant.Slug)

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, webhook.MaxBody))
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			h.Metrics.Webhook(name, "too_large")
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
			return
		}
		h.Metrics.Webhook(name, "unreadable")
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	if err := webhook.Verify(in.Forge, in.WebhookSecretValue().Value(), r.Header, body); err != nil {
		logger.Warn("webhook rejected", "error", err, "remote", r.RemoteAddr)
		h.Metrics.Webhook(name, "unauthorized")
		http.Error(w, "signature verification failed", http.StatusUnauthorized)
		return
	}
	// Any verified delivery shows the forge's webhook is set up, whatever
	// becomes of the event.
	if h.Deliveries != nil {
		if err := h.Deliveries.RecordDelivery(r.Context(), tenant.ID(), in.ID()); err != nil {
			logger.Warn("webhook delivery not recorded", "error", err)
		}
	}
	ev, err := webhook.Parse(in.Forge, r.Header, body)
	if err != nil {
		logger.Warn("webhook unparsable", "error", err)
		h.Metrics.Webhook(name, "unparsable")
		http.Error(w, "unparsable payload", http.StatusBadRequest)
		return
	}
	logger = logger.With("delivery", ev.Delivery, "kind", ev.Kind, "action", ev.Action)

	switch {
	case ev.Kind == webhook.KindPing:
		h.Metrics.Webhook(name, "ping")
		w.WriteHeader(http.StatusNoContent)
		return
	case ev.Kind == webhook.KindIgnored:
		logger.Debug("webhook ignored")
		h.Metrics.Webhook(name, Ignored)
		w.WriteHeader(http.StatusAccepted)
		return
	case ev.Account != "" && !in.Serves(ev.Account):
		// A public App can be installed by anyone; only the declared
		// accounts are served. Accepted, so the forge does not retry.
		logger.Warn("webhook for an undeclared account ignored", "account", ev.Account)
		h.Metrics.Webhook(name, "undeclared_account")
		w.WriteHeader(http.StatusAccepted)
		return
	}

	out, err := h.disp.Dispatch(r.Context(), Request{File: file, Tenant: tenant, Connection: in, Event: ev})
	if err != nil {
		logger.Error("webhook dispatch failed", "error", err)
		h.Metrics.Webhook(name, "error")
		http.Error(w, "dispatch failed", http.StatusInternalServerError)
		return
	}
	logger.Info("webhook "+out.Status, "reason", out.Reason, "job", out.Job)
	h.Metrics.Webhook(name, out.Status)
	w.WriteHeader(http.StatusAccepted)
}
