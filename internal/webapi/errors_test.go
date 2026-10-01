package webapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/home-operations/kritika/internal/store"
)

func TestWriteError(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		canceled   bool
		wantStatus int
		wantCode   ErrorCode
		wantMsg    string
	}{
		{name: "api error as itself", err: errStatus(http.StatusConflict, CodeAlreadyQueued, "already queued"),
			wantStatus: http.StatusConflict, wantCode: CodeAlreadyQueued, wantMsg: "already queued"},
		{name: "wrapped api error", err: fmt.Errorf("handler: %w", errNotFound("review")),
			wantStatus: http.StatusNotFound, wantCode: CodeNotFound, wantMsg: "review not found"},
		{name: "store miss", err: fmt.Errorf("x: %w", store.ErrNotFound),
			wantStatus: http.StatusNotFound, wantCode: CodeNotFound, wantMsg: "not found"},
		{name: "bad filter", err: store.ErrFilter,
			wantStatus: http.StatusBadRequest, wantCode: CodeBadRequest, wantMsg: "invalid filter"},
		{name: "bad page limit", err: store.ErrPageLimit,
			wantStatus: http.StatusBadRequest, wantCode: CodeBadRequest, wantMsg: "invalid filter"},
		{name: "client went away", err: context.Canceled, canceled: true},
		{name: "unknown error", err: errors.New("pg: connection reset"),
			wantStatus: http.StatusInternalServerError, wantCode: CodeInternal, wantMsg: "internal error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/v1/reviews", nil)
			if tt.canceled {
				ctx, cancel := context.WithCancel(r.Context())
				cancel()
				r = r.WithContext(ctx)
			}
			w := httptest.NewRecorder()
			writeError(w, r, slog.New(slog.DiscardHandler), tt.err)
			if tt.canceled {
				if w.Code != http.StatusOK || w.Body.Len() != 0 || len(w.Header()) != 0 {
					t.Fatalf("wrote %d %q %v for a client that went away", w.Code, w.Body, w.Header())
				}
				return
			}
			var body ErrorBody
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("body %q: %v", w.Body, err)
			}
			if w.Code != tt.wantStatus || body.Code != tt.wantCode || body.Message != tt.wantMsg {
				t.Fatalf("got %d %+v, want %d %s %q", w.Code, body, tt.wantStatus, tt.wantCode, tt.wantMsg)
			}
			if w.Header().Get("Content-Type") != "application/json" || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("headers = %v", w.Header())
			}
		})
	}
}
