package webapi

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/home-operations/kritik/internal/auth"
	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/store"
)

func testHub(t *testing.T) (*hub, *configfile.File) {
	t.Helper()
	f := testFile(t)
	return newHub(configfile.NewCurrent(f), slog.New(slog.NewTextHandler(io.Discard, nil))), f
}

func TestHubPublishFiltersByTenant(t *testing.T) {
	h, f := testHub(t)
	alpha, beta := tenantIDOf(t, f, "alpha"), tenantIDOf(t, f, "beta")
	a := h.subscribe(&auth.Principal{Memberships: map[string]auth.Role{alpha: auth.RoleMember}})
	b := h.subscribe(&auth.Principal{Memberships: map[string]auth.Role{beta: auth.RoleMember}})
	op := h.subscribe(&auth.Principal{Operator: true})
	rid := "r-1"

	h.publish(store.Event{TenantID: alpha, Kind: store.EventReview, ID: "e-1", ReviewID: &rid})
	h.publish(store.Event{TenantID: "not-in-the-file", Kind: store.EventReview, ID: "e-2"})

	want := Event{Kind: store.EventReview, Tenant: "alpha", ID: "e-1", ReviewID: &rid}
	for name, c := range map[string]*client{"member of alpha": a, "operator": op} {
		select {
		case got := <-c.events:
			if got.Kind != want.Kind || got.Tenant != want.Tenant || got.ID != want.ID || *got.ReviewID != rid {
				t.Errorf("%s got %+v, want %+v", name, got, want)
			}
		default:
			t.Errorf("%s got no event", name)
		}
		if len(c.events) != 0 {
			t.Errorf("%s got the event of a tenant not in the file", name)
		}
	}
	if len(b.events) != 0 {
		t.Errorf("member of beta got an alpha event")
	}

	h.unsubscribe(a)
	h.publish(store.Event{TenantID: alpha, Kind: store.EventReview, ID: "e-3"})
	if len(a.events) != 0 {
		t.Errorf("unsubscribed client still receives events")
	}
}

func TestHubOverflowAndReconnectResync(t *testing.T) {
	h, f := testHub(t)
	h.buffer = 2
	alpha := tenantIDOf(t, f, "alpha")
	slow := h.subscribe(&auth.Principal{Memberships: map[string]auth.Role{alpha: auth.RoleMember}})
	other := h.subscribe(&auth.Principal{})
	for range 5 {
		h.publish(store.Event{TenantID: alpha, Kind: store.EventRunnerRun, ID: "x"})
	}
	if len(slow.resync) != 1 {
		t.Errorf("a full client was not sent a resync")
	}
	if len(other.resync) != 0 {
		t.Errorf("a client that missed nothing was sent a resync")
	}
	h.resyncAll()
	if len(other.resync) != 1 || len(slow.resync) != 1 {
		t.Errorf("resyncAll: pending resyncs = %d, %d, want 1 each", len(other.resync), len(slow.resync))
	}
	slow.drain()
	if len(slow.events) != 0 {
		t.Errorf("drain left %d events", len(slow.events))
	}
}

// sseLine reads the stream's next non-empty line.
func sseLine(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("read stream: %v", err)
		}
		if line = strings.TrimRight(line, "\n"); line != "" {
			return line
		}
	}
}

func TestHubServe(t *testing.T) {
	h, f := testHub(t)
	h.heartbeat = 20 * time.Millisecond
	alpha := tenantIDOf(t, f, "alpha")
	p := &auth.Principal{Memberships: map[string]auth.Role{alpha: auth.RoleMember}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.serve(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(t.Context())
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" || resp.Header.Get("X-Accel-Buffering") != "no" {
		t.Fatalf("headers = %v", resp.Header)
	}
	br := bufio.NewReader(resp.Body)
	if got := sseLine(t, br); got != ": connected" {
		t.Fatalf("first frame = %q", got)
	}

	h.publish(store.Event{TenantID: alpha, Kind: store.EventIndexRun, ID: "ix-1"})
	var lines []string
	for len(lines) < 2 {
		l := sseLine(t, br)
		if l != ": heartbeat" {
			lines = append(lines, l)
		}
	}
	if lines[0] != "event: index_run" {
		t.Errorf("event line = %q", lines[0])
	}
	var ev Event
	if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[1], "data: ")), &ev); err != nil {
		t.Fatalf("data %q: %v", lines[1], err)
	}
	if ev.Tenant != "alpha" || ev.ID != "ix-1" || ev.Kind != store.EventIndexRun || ev.ReviewID != nil {
		t.Errorf("event = %+v", ev)
	}

	h.resyncAll()
	for {
		l := sseLine(t, br)
		if l == "event: resync" {
			break
		}
		if l != ": heartbeat" {
			t.Fatalf("unexpected line %q before resync", l)
		}
	}

	cancel()
	deadline := time.Now().Add(2 * time.Second)
	for {
		h.mu.Lock()
		n := len(h.clients)
		h.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("client was not removed after disconnect")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestHubCloseEndsStreams(t *testing.T) {
	h, _ := testHub(t)
	p := &auth.Principal{Operator: true}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.serve(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
	}))
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	br := bufio.NewReader(resp.Body)
	if got := sseLine(t, br); got != ": connected" {
		t.Fatalf("first frame = %q", got)
	}
	h.close()
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(br)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("stream ended with %v, want a clean end", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream still open after the hub closed")
	}
	h.close() // idempotent
}
