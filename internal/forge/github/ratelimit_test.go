package github

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRateLimitWait(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		status  int
		headers map[string]string
		want    time.Duration
		limited bool
	}{
		{name: "secondary limit", status: 403, headers: map[string]string{"Retry-After": "30"}, want: 30 * time.Second, limited: true},
		{name: "secondary limit as 429", status: 429, headers: map[string]string{"Retry-After": "5"}, want: 5 * time.Second, limited: true},
		{name: "a zero Retry-After still waits a second", status: 403, headers: map[string]string{"Retry-After": "0"}, want: time.Second, limited: true},
		{name: "primary limit", status: 403, headers: map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": strconv.FormatInt(now.Add(40*time.Second).Unix(), 10)},
			want: 41 * time.Second, limited: true},
		{name: "a reset in the past still waits a second", status: 403, headers: map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": strconv.FormatInt(now.Add(-time.Hour).Unix(), 10)},
			want: time.Second, limited: true},
		{name: "a 403 with quota left is not a limit", status: 403, headers: map[string]string{"X-RateLimit-Remaining": "12"}},
		{name: "a plain 403 is not a limit", status: 403},
		{name: "a 404 is not a limit", status: 404, headers: map[string]string{"Retry-After": "30"}},
		{name: "an unparsable Retry-After is not a limit", status: 403, headers: map[string]string{"Retry-After": "soon"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &http.Response{StatusCode: tt.status, Header: http.Header{}}
			for k, v := range tt.headers {
				resp.Header.Set(k, v)
			}
			got, limited := rateLimitWait(resp, now)
			if got != tt.want || limited != tt.limited {
				t.Errorf("rateLimitWait() = %s, %v; want %s, %v", got, limited, tt.want, tt.limited)
			}
		})
	}
}

// limitedServer refuses the first n requests with status and headers, then
// answers 200 with the request body echoed, recording every body it saw.
type limitedServer struct {
	mu      sync.Mutex
	refuse  int
	status  int
	headers map[string]string
	bodies  []string
}

func (s *limitedServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bodies = append(s.bodies, string(b))
	if len(s.bodies) <= s.refuse {
		for k, v := range s.headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(s.status)
		_, _ = io.WriteString(w, `{"message":"You have exceeded a secondary rate limit"}`)
		return
	}
	_, _ = w.Write(b)
}

func TestRateLimitTransport(t *testing.T) {
	tests := []struct {
		name       string
		server     *limitedServer
		body       string
		noGetBody  bool
		cancel     bool
		wantStatus int
		wantCalls  int
		wantWait   time.Duration
		wantWaited bool
		wantNoObs  bool
		wantErr    bool
	}{
		{
			name: "a secondary limit is waited out and the body sent again", body: `{"body":"lgtm"}`,
			server:     &limitedServer{refuse: 1, status: 403, headers: map[string]string{"Retry-After": "2"}},
			wantStatus: 200, wantCalls: 2, wantWait: 2 * time.Second, wantWaited: true,
		},
		{
			name:       "a primary limit resetting soon is waited out",
			server:     &limitedServer{refuse: 1, status: 429, headers: map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": strconv.FormatInt(time.Now().Add(3*time.Second).Unix(), 10)}},
			wantStatus: 200, wantCalls: 2, wantWait: 4 * time.Second, wantWaited: true,
		},
		{
			name:       "a second refusal is handed back",
			server:     &limitedServer{refuse: 2, status: 403, headers: map[string]string{"Retry-After": "1"}},
			wantStatus: 403, wantCalls: 2, wantWait: time.Second, wantWaited: true,
		},
		{
			name:       "a wait past the cap is not taken",
			server:     &limitedServer{refuse: 1, status: 403, headers: map[string]string{"Retry-After": "120"}},
			wantStatus: 403, wantCalls: 1, wantWait: 120 * time.Second,
		},
		{
			name: "a body that cannot be read again is not resent", body: "stream", noGetBody: true,
			server:     &limitedServer{refuse: 1, status: 403, headers: map[string]string{"Retry-After": "1"}},
			wantStatus: 403, wantCalls: 1, wantWait: time.Second,
		},
		{
			name:       "a response that is not a limit passes through",
			server:     &limitedServer{refuse: 1, status: 403},
			wantStatus: 403, wantCalls: 1, wantNoObs: true,
		},
		{
			name:   "a request cancelled during the wait fails",
			server: &limitedServer{refuse: 1, status: 403, headers: map[string]string{"Retry-After": "1"}},
			cancel: true, wantCalls: 1, wantWait: time.Second, wantWaited: true, wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.server)
			defer srv.Close()
			var observed []time.Duration
			var waited []bool
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			tr := &rateLimitTransport{
				base: http.DefaultTransport, maxWait: rateLimitMaxWait,
				observe: func(wait time.Duration, w bool) {
					observed = append(observed, wait)
					waited = append(waited, w)
				},
				sleep: func(ctx context.Context, _ time.Duration) error {
					if tt.cancel {
						cancel()
					}
					return ctx.Err()
				},
			}
			var body io.Reader
			if tt.body != "" {
				body = strings.NewReader(tt.body)
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, body)
			if err != nil {
				t.Fatal(err)
			}
			if tt.noGetBody {
				req.GetBody = nil
			}
			resp, err := tr.RoundTrip(req)
			if (err != nil) != tt.wantErr {
				t.Fatalf("RoundTrip() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil {
				defer func() { _ = resp.Body.Close() }()
				if resp.StatusCode != tt.wantStatus {
					t.Errorf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
				}
				if got, _ := io.ReadAll(resp.Body); tt.wantStatus == 200 && string(got) != tt.body {
					t.Errorf("body = %q, want %q echoed from the resent request", got, tt.body)
				}
			}
			tt.server.mu.Lock()
			calls := len(tt.server.bodies)
			bodies := tt.server.bodies
			tt.server.mu.Unlock()
			if calls != tt.wantCalls {
				t.Errorf("requests = %d, want %d", calls, tt.wantCalls)
			}
			for _, b := range bodies {
				if b != tt.body {
					t.Errorf("the server saw body %q, want %q every time", b, tt.body)
				}
			}
			switch {
			case tt.wantNoObs && len(observed) != 0:
				t.Errorf("observed %v, want nothing", observed)
			case !tt.wantNoObs && (len(observed) == 0 || observed[0] < tt.wantWait-time.Second || observed[0] > tt.wantWait || waited[0] != tt.wantWaited):
				t.Errorf("observed %v %v, want about %s, waited=%v", observed, waited, tt.wantWait, tt.wantWaited)
			}
			if tt.wantErr && !errors.Is(err, context.Canceled) {
				t.Errorf("error = %v, want context.Canceled", err)
			}
		})
	}
}
