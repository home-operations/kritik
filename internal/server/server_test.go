package server

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestManagementHandler(t *testing.T) {
	m := NewManagement(":0", slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	get := func(t *testing.T, path string) *http.Response {
		t.Helper()
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}

	tests := []struct {
		name  string
		path  string
		ready bool
		want  int
	}{
		{name: "healthz", path: "/healthz", want: http.StatusOK},
		{name: "readyz before ready", path: "/readyz", want: http.StatusServiceUnavailable},
		{name: "readyz after ready", path: "/readyz", ready: true, want: http.StatusOK},
		{name: "metrics", path: "/metrics", want: http.StatusOK},
		{name: "unknown", path: "/nope", want: http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m.SetReady(tt.ready)
			if got := get(t, tt.path).StatusCode; got != tt.want {
				t.Fatalf("GET %s = %d, want %d", tt.path, got, tt.want)
			}
		})
	}

	t.Run("metrics body has go collector", func(t *testing.T) {
		body, err := io.ReadAll(get(t, "/metrics").Body)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), "go_goroutines") {
			t.Fatal("expected go_goroutines in /metrics output")
		}
	})
}

func TestHooksHandlerRouting(t *testing.T) {
	h := NewHooks(":0", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(r.PathValue("installation")))
	}), slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()

	tests := []struct {
		name   string
		method string
		path   string
		want   int
	}{
		{name: "post to an installation", method: http.MethodPost, path: "/hooks/sticky-gecko", want: http.StatusAccepted},
		{name: "get is not routable", method: http.MethodGet, path: "/hooks/sticky-gecko", want: http.StatusMethodNotAllowed},
		{name: "bare hooks path", method: http.MethodPost, path: "/hooks", want: http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest(tt.method, srv.URL+tt.path, nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != tt.want {
				t.Fatalf("%s %s = %d, want %d", tt.method, tt.path, resp.StatusCode, tt.want)
			}
		})
	}
}

func TestServeDrainsOnCancel(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, addr, http.NotFoundHandler(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := http.Get("http://" + addr + "/"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("listener never came up")
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v after cancel", err)
		}
	case <-time.After(shutdownTimeout + time.Second):
		t.Fatal("serve did not return after cancel")
	}
}
