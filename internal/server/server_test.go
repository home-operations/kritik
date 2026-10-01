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
	m := NewManagement(":0", slog.New(slog.DiscardHandler))
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

func TestPublicRouting(t *testing.T) {
	hooks := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("hook " + r.PathValue("connection")))
	})
	web := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("web")) })
	srv := httptest.NewServer(Public("/kritika/", hooks, web))
	defer srv.Close()

	for _, tt := range []struct {
		name   string
		method string
		path   string
		want   string
	}{
		{"post to a connection", http.MethodPost, "/hooks/sticky-gecko", "hook sticky-gecko"},
		{"post under the dashboard's base path", http.MethodPost, "/kritika/hooks/sticky-gecko", "hook sticky-gecko"},
		{"a get is the dashboard's", http.MethodGet, "/hooks/sticky-gecko", "web"},
		{"the bare hooks path is the dashboard's", http.MethodPost, "/hooks", "web"},
		{"another base path is the dashboard's", http.MethodPost, "/other/hooks/sticky-gecko", "web"},
		{"the dashboard", http.MethodGet, "/kritika/", "web"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest(tt.method, srv.URL+tt.path, nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if string(body) != tt.want {
				t.Fatalf("%s %s = %q, want %q", tt.method, tt.path, body, tt.want)
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
		done <- Serve(ctx, addr, http.NotFoundHandler(), shutdownTimeout, slog.New(slog.DiscardHandler))
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

func TestServeDrainCutsWhatOutlastsIt(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	started, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	slow := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, addr, slow, 50*time.Millisecond, slog.New(slog.DiscardHandler))
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.Dial("tcp", addr)
		if err == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("listener never came up")
		}
		time.Sleep(10 * time.Millisecond)
	}
	go func() { _, _ = http.Get("http://" + addr + "/") }()
	<-started

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v when the drain ran out", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return when the drain ran out")
	}
}
