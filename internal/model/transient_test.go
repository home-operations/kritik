package model

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/openai/openai-go/v3"
)

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestTransient(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"a 502 from the provider", &openai.Error{StatusCode: http.StatusBadGateway}, true},
		{"a 503 from the provider, wrapped", fmt.Errorf("model: x: %w", &openai.Error{StatusCode: http.StatusServiceUnavailable}), true},
		{"a 429 from the provider", &anthropic.Error{StatusCode: http.StatusTooManyRequests}, true},
		{"a 408 from the provider", &anthropic.Error{StatusCode: http.StatusRequestTimeout}, true},
		{"a 400 refusing the request", &openai.Error{StatusCode: http.StatusBadRequest}, false},
		{"a 401 for a bad key", &anthropic.Error{StatusCode: http.StatusUnauthorized}, false},
		{"a 404 for an unknown model", &openai.Error{StatusCode: http.StatusNotFound}, false},
		{"the run's budget", fmt.Errorf("%w: spent", ErrBudget), false},
		{"a dial failure", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, true},
		{"a timeout", timeoutErr{}, true},
		{"a cut connection", io.ErrUnexpectedEOF, true},
		{"the request's deadline", context.DeadlineExceeded, true},
		{"anything else", errors.New("response has no choices"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Transient(tt.err); got != tt.want {
				t.Fatalf("Transient(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
