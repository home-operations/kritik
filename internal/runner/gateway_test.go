package runner

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWaitForGateway(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	poll := gatewayPoll
	gatewayPoll = 50 * time.Millisecond
	t.Cleanup(func() { gatewayPoll = poll })

	t.Run("no proxy is nothing to wait for", func(t *testing.T) {
		t.Setenv("HTTPS_PROXY", "")
		t.Setenv("https_proxy", "")
		if err := waitForGateway(t.Context(), logger); err != nil {
			t.Fatalf("waitForGateway = %v", err)
		}
	})

	t.Run("a gateway that answers, even with a 404", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		defer srv.Close()
		t.Setenv("HTTPS_PROXY", srv.URL)
		if err := waitForGateway(t.Context(), logger); err != nil {
			t.Fatalf("waitForGateway = %v", err)
		}
	})

	t.Run("a gateway that comes up while waiting", func(t *testing.T) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := l.Addr().String()
		// Nothing listens until the server starts a moment later.
		_ = l.Close()
		t.Setenv("HTTPS_PROXY", "http://"+addr)
		srv := &http.Server{Addr: addr, Handler: http.NotFoundHandler()}
		defer func() { _ = srv.Close() }()
		go func() {
			time.Sleep(gatewayPoll + gatewayPoll/2)
			ln, err := net.Listen("tcp", addr)
			if err != nil {
				return
			}
			_ = srv.Serve(ln)
		}()
		start := time.Now()
		if err := waitForGateway(t.Context(), logger); err != nil {
			t.Fatalf("waitForGateway = %v", err)
		}
		if time.Since(start) < gatewayPoll {
			t.Fatalf("answered after %s, before the gateway was up", time.Since(start))
		}
	})

	t.Run("a gateway that never answers", func(t *testing.T) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := l.Addr().String()
		_ = l.Close()
		t.Setenv("HTTPS_PROXY", "http://"+addr)
		ctx, cancel := context.WithTimeout(t.Context(), 3*gatewayPoll)
		defer cancel()
		err = waitForGateway(ctx, logger)
		if err == nil || !strings.Contains(err.Error(), "did not answer within") {
			t.Fatalf("waitForGateway = %v, want the gateway's absence", err)
		}
	})
}
