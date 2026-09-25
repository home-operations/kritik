package webapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/home-operations/kritik/internal/auth"
	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/store"
)

// SSE tuning: how often an idle stream carries a comment so proxies and
// the browser keep it open, and how many events a slow client may fall
// behind before it is told to resync instead.
const (
	heartbeatInterval = 25 * time.Second
	clientBuffer      = 64
)

// hub fans the store's row events out to the server-sent event streams of
// every principal allowed to read the event's tenant.
type hub struct {
	current   *configfile.Current
	logger    *slog.Logger
	heartbeat time.Duration
	buffer    int

	mu      sync.Mutex
	clients map[*client]struct{}
}

// client is one open stream. resync holds at most one pending resync,
// which supersedes whatever events are still buffered.
type client struct {
	principal *auth.Principal
	events    chan Event
	resync    chan struct{}
}

func newHub(current *configfile.Current, logger *slog.Logger) *hub {
	return &hub{
		current: current, logger: logger, heartbeat: heartbeatInterval, buffer: clientBuffer,
		clients: map[*client]struct{}{},
	}
}

func (h *hub) subscribe(p *auth.Principal) *client {
	c := &client{principal: p, events: make(chan Event, h.buffer), resync: make(chan struct{}, 1)}
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	return c
}

func (h *hub) unsubscribe(c *client) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
}

// slug returns the slug of the tenant with id tenantID in the current
// file, false for a tenant the file no longer has.
func (h *hub) slug(tenantID string) (string, bool) {
	file := h.current.Get()
	for i := range file.Tenants {
		if file.Tenants[i].ID() == tenantID {
			return file.Tenants[i].Slug, true
		}
	}
	return "", false
}

// publish delivers e to every client that may read its tenant, without
// ever blocking: a client whose buffer is full is sent a resync instead.
// It runs on the store listener's single callback goroutine.
func (h *hub) publish(e store.Event) {
	slug, ok := h.slug(e.TenantID)
	if !ok {
		return
	}
	ev := Event{Kind: e.Kind, Tenant: slug, ID: e.ID, ReviewID: e.ReviewID}
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if !c.principal.CanRead(e.TenantID) {
			continue
		}
		select {
		case c.events <- ev:
		default:
			c.requestResync()
		}
	}
}

// resyncAll tells every client to refetch: events were lost while the
// store listener was disconnected.
func (h *hub) resyncAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		c.requestResync()
	}
}

func (c *client) requestResync() {
	select {
	case c.resync <- struct{}{}:
	default:
	}
}

// serve streams events to one client until the request ends.
func (h *hub) serve(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	// A server-wide write timeout would cut every stream; this one never
	// finishes writing, so it has none.
	if err := rc.SetWriteDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
		h.logger.WarnContext(r.Context(), "webapi: clear stream write deadline", "error", err)
	}
	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream")
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	c := h.subscribe(auth.PrincipalFrom(r.Context()))
	defer h.unsubscribe(c)
	ticker := time.NewTicker(h.heartbeat)
	defer ticker.Stop()

	if _, err := io.WriteString(w, ": connected\n\n"); err != nil {
		return
	}
	for {
		if err := rc.Flush(); err != nil {
			return
		}
		var err error
		select {
		case <-r.Context().Done():
			return
		case <-c.resync:
			c.drain()
			_, err = io.WriteString(w, "event: resync\ndata: {}\n\n")
		case e := <-c.events:
			err = writeEvent(w, e)
		case <-ticker.C:
			_, err = io.WriteString(w, ": heartbeat\n\n")
		}
		if err != nil {
			return
		}
	}
}

// drain discards buffered events a resync makes redundant.
func (c *client) drain() {
	for {
		select {
		case <-c.events:
		default:
			return
		}
	}
}

func writeEvent(w io.Writer, e Event) error {
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Kind, data)
	return err
}
