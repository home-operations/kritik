package gateway

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"

	"github.com/home-operations/kritika/internal/adapter"
	"github.com/home-operations/kritika/internal/configfile"
	"github.com/home-operations/kritika/internal/configfile/configfiletest"
	"github.com/home-operations/kritika/internal/model"
)

func TestFallback(t *testing.T) {
	t.Setenv("TEST_PROVIDER_KEY", "sk-provider")
	f, err := configfiletest.Parse(t, `providers:
  p:
    type: openai
    apiKey: { env: TEST_PROVIDER_KEY }
  q:
    type: anthropic
    apiKey: { env: TEST_PROVIDER_KEY }
    retries: 2
apps:
  acme-bot:
    accounts: [acme]
    clientId: Iv1.test
    privateKey: { env: TEST_PROVIDER_KEY }
    webhookSecret: { env: TEST_PROVIDER_KEY }
`)
	if err != nil {
		t.Fatal(err)
	}
	account := &f.Accounts[0]
	built := 0
	g := &Server{Logger: slog.Default(), Steppers: &adapter.Steppers{Build: func(configfile.Provider) (model.Stepper, error) {
		built++
		return &stepperFunc{}, nil
	}}}
	c := runCall{file: f, account: account, logger: slog.Default()}
	stepper, provider, ok := g.fallback(c, "q/small")
	if !ok || stepper == nil || provider.Type != configfile.ProviderAnthropic || provider.Retries != 2 || built != 1 {
		t.Fatalf("fallback = %v, %+v, %v after %d builds; want q's adapter and provider", stepper, provider, ok, built)
	}
	if _, _, ok := g.fallback(c, "nowhere/small"); ok {
		t.Fatal("fallback resolved a provider the configuration lacks")
	}
}

// stepperFunc answers each step from the next error in errs, then the
// answer; it records how many steps it took.
type stepperFunc struct {
	errs  []error
	steps int
}

func (s *stepperFunc) Step(context.Context, model.StepRequest) (model.StepResponse, error) {
	s.steps++
	if s.steps <= len(s.errs) {
		return model.StepResponse{}, s.errs[s.steps-1]
	}
	return model.StepResponse{Model: "m", Text: "ok"}, nil
}

func TestStepRetries(t *testing.T) {
	transient := &openai.Error{StatusCode: http.StatusBadGateway}
	final := &openai.Error{StatusCode: http.StatusBadRequest}
	tests := []struct {
		name     string
		errs     []error
		retries  int
		wantErr  bool
		attempts int
		waits    int
	}{
		{"an answer first time", nil, 3, false, 1, 0},
		{"a transient failure, then an answer", []error{transient}, 3, false, 2, 1},
		{"transient failures past the retries", []error{transient, transient, transient}, 2, true, 3, 2},
		{"no retries", []error{transient}, 0, true, 1, 0},
		{"a final failure is not tried again", []error{final}, 3, true, 1, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &stepperFunc{errs: tt.errs}
			var waits []time.Duration
			wait := func(_ context.Context, d time.Duration) bool { waits = append(waits, d); return true }
			failed := 0
			resp, attempts, err := step(t.Context(), s, model.StepRequest{Model: "m"}, tt.retries, wait, func(error) { failed++ })
			if (err != nil) != tt.wantErr || attempts != tt.attempts || s.steps != tt.attempts || len(waits) != tt.waits || failed != tt.waits {
				t.Fatalf("step = %+v, %d attempts, %v; %d waits, %d reported; want %d attempts, %d waits, err %v",
					resp, attempts, err, len(waits), failed, tt.attempts, tt.waits, tt.wantErr)
			}
			for i, d := range waits {
				if d < retryMin/2 || d > retryMax {
					t.Fatalf("wait %d = %s, outside the backoff", i, d)
				}
			}
			if !tt.wantErr && resp.Text != "ok" {
				t.Fatalf("resp = %+v", resp)
			}
		})
	}

	t.Run("a Retry-After longer than the backoff is waited out", func(t *testing.T) {
		limited := &openai.Error{StatusCode: http.StatusTooManyRequests, Response: &http.Response{Header: http.Header{"Retry-After": {"45"}}}}
		s := &stepperFunc{errs: []error{limited}}
		var waited time.Duration
		wait := func(_ context.Context, d time.Duration) bool { waited = d; return true }
		if _, attempts, err := step(t.Context(), s, model.StepRequest{Model: "m"}, 1, wait, func(error) {}); err != nil || attempts != 2 {
			t.Fatalf("step = %d attempts, %v; want an answer after two", attempts, err)
		}
		if waited != 45*time.Second {
			t.Fatalf("waited %s, want the provider's 45s", waited)
		}
	})

	t.Run("a wait the ctx cut ends with the failure", func(t *testing.T) {
		s := &stepperFunc{errs: []error{transient}}
		wait := func(context.Context, time.Duration) bool { return false }
		_, attempts, err := step(t.Context(), s, model.StepRequest{Model: "m"}, 3, wait, func(error) {})
		if !errors.Is(err, transient) || attempts != 1 {
			t.Fatalf("step = %d attempts, %v; want the transient failure after one", attempts, err)
		}
	})

	t.Run("sleep ends early when ctx does", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if sleep(ctx, time.Minute) {
			t.Fatal("sleep waited a cancelled ctx out")
		}
		if !sleep(t.Context(), time.Millisecond) {
			t.Fatal("sleep did not wait a millisecond out")
		}
	})
}

func TestRefuseRetries(t *testing.T) {
	for status, retry := range map[int]bool{
		http.StatusInternalServerError: true,
		http.StatusBadGateway:          false,
		http.StatusTooManyRequests:     false,
		http.StatusUnauthorized:        false,
		http.StatusBadRequest:          false,
	} {
		rec := httptest.NewRecorder()
		refuse(rec, status, "code", "message")
		if got := rec.Header().Get("X-Should-Retry") != "false"; got != retry || rec.Code != status {
			t.Fatalf("%d: retry = %v, want %v", status, got, retry)
		}
	}
}

func TestMaskProvider(t *testing.T) {
	t.Setenv("TEST_PROVIDER_KEY", "sk-provider")
	f, err := configfiletest.Parse(t, `providers:
  p:
    type: openai
    baseUrl: https://kritika:url-secret@llm.example/v1
    apiKey: { env: TEST_PROVIDER_KEY }
apps:
  acme-bot:
    accounts: [acme]
    clientId: Iv1.test
    privateKey: { env: TEST_PROVIDER_KEY }
    webhookSecret: { env: TEST_PROVIDER_KEY }
`)
	if err != nil {
		t.Fatal(err)
	}
	got := maskProvider(`POST "https://kritika:url-secret@llm.example/v1/chat/completions": 401 {"error":"bad key sk-provider"}`, f.Providers["p"])
	want := `POST "https://***@llm.example/v1/chat/completions": 401 {"error":"bad key ***"}`
	if got != want {
		t.Fatalf("masked = %s\nwant     %s", got, want)
	}
}
