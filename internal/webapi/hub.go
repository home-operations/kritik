package webapi

import (
	"encoding/json"
	"errors"
	"fmt"
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
	// writeTimeout bounds each write to a stream, so a client that stops
	// reading is dropped rather than holding its goroutine forever.
	writeTimeout = 30 * time.Second
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
	// done is closed once the store listener has stopped: no event will
	// arrive again, so every stream ends.
	done      chan struct{}
	closeOnce sync.Once
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
		clients: map[*client]struct{}{}, done: make(chan struct{}),
	}
}

// close ends every stream, open or yet to open.
func (h *hub) close() { h.closeOnce.Do(func() { close(h.done) }) }

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
	// A server-wide write timeout would cut a stream that never finishes;
	// instead each write gets its own deadline.
	extend := func() bool {
		err := rc.SetWriteDeadline(time.Now().Add(writeTimeout))
		return err == nil || errors.Is(err, http.ErrNotSupported)
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

	frame := []byte(": connected\n\n")
	for {
		if !extend() {
			return
		}
		if _, err := w.Write(frame); err != nil {
			return
		}
		if err := rc.Flush(); err != nil {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-h.done:
			return
		case <-c.resync:
			c.drain()
			frame = []byte("event: resync\ndata: {}\n\n")
		case e := <-c.events:
			var err error
			if frame, err = eventFrame(e); err != nil {
				h.logger.ErrorContext(r.Context(), "webapi: encode event", "error", err)
				return
			}
		case <-ticker.C:
			frame = []byte(": heartbeat\n\n")
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

func eventFrame(e Event) ([]byte, error) {
	data, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	return fmt.Appendf(nil, "event: %s\ndata: %s\n\n", e.Kind, data), nil
}
