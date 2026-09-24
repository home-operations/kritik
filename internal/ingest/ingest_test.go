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
	"github.com/home-operations/kritik/internal/webhook"
)

const configYAML = `
tenants:
  - slug: onedr0p
    installations:
      - name: bot-ross
        forge: github
        account: onedr0p
        app:
          clientId: Iv1.x
          privateKey: { env: TEST_PEM }
          webhookSecret: { env: TEST_SECRET }
`

type fakeDispatcher struct {
	got []Request
	out Outcome
	err error
}

func (f *fakeDispatcher) Dispatch(_ context.Context, req Request) (Outcome, error) {
	f.got = append(f.got, req)
	return f.out, f.err
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
	f, err := configfile.Parse([]byte(configYAML))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("POST /hooks/{installation}", NewHandler(configfile.NewCurrent(f), disp, slog.New(slog.NewTextHandler(io.Discard, nil))))
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
	}{
		{"unknown installation", "/hooks/nope", "pull_request", "s3cret", prBody, Outcome{}, nil, http.StatusNotFound, false},
		{"bad signature", "/hooks/bot-ross", "pull_request", "wrong", prBody, Outcome{}, nil, http.StatusUnauthorized, false},
		{"missing signature", "/hooks/bot-ross", "pull_request", "", prBody, Outcome{}, nil, http.StatusUnauthorized, false},
		{"unparsable", "/hooks/bot-ross", "pull_request", "s3cret", "{nope", Outcome{}, nil, http.StatusBadRequest, false},
		{"ping", "/hooks/bot-ross", "ping", "s3cret", `{"zen":"x"}`, Outcome{}, nil, http.StatusNoContent, false},
		{"unknown event ignored", "/hooks/bot-ross", "workflow_run", "s3cret", `{}`, Outcome{}, nil, http.StatusAccepted, false},
		{"undeclared account ignored", "/hooks/bot-ross", "pull_request", "s3cret",
			strings.ReplaceAll(prBody, `"owner":{"login":"onedr0p"}`, `"owner":{"login":"stranger"}`), Outcome{}, nil, http.StatusAccepted, false},
		{"dispatched", "/hooks/bot-ross", "pull_request", "s3cret", prBody, Outcome{Status: Enqueued}, nil, http.StatusAccepted, true},
		{"dispatcher error", "/hooks/bot-ross", "pull_request", "s3cret", prBody, Outcome{}, errors.New("db down"), http.StatusInternalServerError, true},
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
			if tt.dispatched {
				req := disp.got[0]
				if req.Tenant.Slug != "onedr0p" || req.Installation.Name != "bot-ross" || req.Event.Kind != webhook.KindPullRequest {
					t.Fatalf("request = %+v", req)
				}
			}
		})
	}
}

func TestHandlerRejectsOversizedBody(t *testing.T) {
	srv := setup(t, &fakeDispatcher{})
	body := `{"pad":"` + strings.Repeat("x", maxBody) + `"}`
	if resp := post(t, srv, "/hooks/bot-ross", "push", "s3cret", body); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}
