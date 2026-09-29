package ingest

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/configfile/configfiletest"
	"github.com/home-operations/kritik/internal/webhook"
)

const configYAML = `
connections:
  - name: bot-ross
    forge: github
    accounts: [onedr0p, home-operations]
    app:
      clientId: Iv1.x
      privateKey: { env: TEST_PEM }
      webhookSecret: { env: TEST_SECRET }
`

type fakeDispatcher struct {
	got       []Request
	out       Outcome
	err       error
	delivered []string
	recordErr error
}

func (f *fakeDispatcher) Dispatch(_ context.Context, req Request) (Outcome, error) {
	f.got = append(f.got, req)
	return f.out, f.err
}

func (f *fakeDispatcher) RecordDelivery(_ context.Context, connectionID string) error {
	f.delivered = append(f.delivered, connectionID)
	return f.recordErr
}

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

const prBody = `{"action":"opened","pull_request":{"number":1,"user":{"login":"x"},
  "head":{"ref":"f","sha":"1","repo":{"full_name":"onedr0p/home-ops"}},
  "base":{"ref":"main","sha":"2","repo":{"full_name":"onedr0p/home-ops"}}},
  "repository":{"full_name":"onedr0p/home-ops","default_branch":"main","owner":{"login":"onedr0p"}}}`

func setup(t *testing.T, disp Dispatcher) *httptest.Server {
	t.Helper()
	t.Setenv("TEST_PEM", "pem")
	t.Setenv("TEST_SECRET", "s3cret")
	f := configfiletest.Load(t, configYAML)
	h := NewHandler(configfile.NewCurrent(f), disp, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if r, ok := disp.(DeliveryRecorder); ok {
		h.Deliveries = r
	}
	mux := http.NewServeMux()
	mux.Handle("POST /hooks/{connection}", h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func post(t *testing.T, srv *httptest.Server, path, event, secret, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", "d-1")
	if secret != "" {
		req.Header.Set("X-Hub-Signature-256", sign(secret, []byte(body)))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp
}

func TestHandler(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		event      string
		secret     string
		body       string
		out        Outcome
		err        error
		want       int
		dispatched bool
		// recorded: any delivery that passes the signature check counts.
		recorded bool
	}{
		{"unknown connection", "/hooks/nope", "pull_request", "s3cret", prBody, Outcome{}, nil, http.StatusNotFound, false, false},
		{"bad signature", "/hooks/bot-ross", "pull_request", "wrong", prBody, Outcome{}, nil, http.StatusUnauthorized, false, false},
		{"missing signature", "/hooks/bot-ross", "pull_request", "", prBody, Outcome{}, nil, http.StatusUnauthorized, false, false},
		{"unparsable", "/hooks/bot-ross", "pull_request", "s3cret", "{nope", Outcome{}, nil, http.StatusBadRequest, false, true},
		{"ping", "/hooks/bot-ross", "ping", "s3cret", `{"zen":"x"}`, Outcome{}, nil, http.StatusNoContent, false, true},
		{"unknown event ignored", "/hooks/bot-ross", "workflow_run", "s3cret", `{}`, Outcome{}, nil, http.StatusAccepted, false, true},
		{"undeclared account ignored", "/hooks/bot-ross", "pull_request", "s3cret",
			strings.ReplaceAll(prBody, `"owner":{"login":"onedr0p"}`, `"owner":{"login":"stranger"}`), Outcome{}, nil, http.StatusAccepted, false, true},
		{"dispatched", "/hooks/bot-ross", "pull_request", "s3cret", prBody, Outcome{Status: Enqueued}, nil, http.StatusAccepted, true, true},
		{"another declared account dispatched", "/hooks/bot-ross", "pull_request", "s3cret",
			strings.ReplaceAll(prBody, "onedr0p", "Home-Operations"), Outcome{Status: Enqueued}, nil, http.StatusAccepted, true, true},
		{"dispatcher error", "/hooks/bot-ross", "pull_request", "s3cret", prBody, Outcome{}, errors.New("db down"), http.StatusInternalServerError, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			disp := &fakeDispatcher{out: tt.out, err: tt.err}
			srv := setup(t, disp)
			resp := post(t, srv, tt.path, tt.event, tt.secret, tt.body)
			if resp.StatusCode != tt.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.want)
			}
			if (len(disp.got) > 0) != tt.dispatched {
				t.Fatalf("dispatched = %v, want %v", len(disp.got) > 0, tt.dispatched)
			}
			if (len(disp.delivered) > 0) != tt.recorded {
				t.Fatalf("recorded = %v, want %v", len(disp.delivered) > 0, tt.recorded)
			}
			if tt.dispatched {
				req := disp.got[0]
				if !strings.EqualFold(req.Account.Name, req.Event.Account) || req.Event.Kind != webhook.KindPullRequest {
					t.Fatalf("request = %+v", req)
				}
			}
		})
	}
}

func TestHandlerServesWhenTheDeliveryIsNotRecorded(t *testing.T) {
	disp := &fakeDispatcher{out: Outcome{Status: Enqueued}, recordErr: errors.New("db down")}
	srv := setup(t, disp)
	if resp := post(t, srv, "/hooks/bot-ross", "pull_request", "s3cret", prBody); resp.StatusCode != http.StatusAccepted || len(disp.got) != 1 {
		t.Fatalf("status = %d, dispatched %d times; want 202 and once", resp.StatusCode, len(disp.got))
	}
}

func TestHandlerRejectsOversizedBody(t *testing.T) {
	srv := setup(t, &fakeDispatcher{})
	body := `{"pad":"` + strings.Repeat("x", webhook.MaxBody) + `"}`
	if resp := post(t, srv, "/hooks/bot-ross", "push", "s3cret", body); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}
