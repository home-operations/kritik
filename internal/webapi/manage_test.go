package webapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/home-operations/kritik/internal/auth"
)

// mutate builds a state-changing request that passes the same-origin
// check.
func mutate(method, path, body string) *http.Request {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	req.Header.Set("Origin", "https://kritik.example")
	req.Header.Set("X-Kritik", "1")
	return req
}

func TestMetaNeedsNoSession(t *testing.T) {
	ts := newTestServer(t, "https://kritik.example")
	w := ts.as(nil, httptest.NewRequest("GET", "/api/v1/meta", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
	var m Meta
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m.WebURL != "https://kritik.example" {
		t.Errorf("meta = %+v", m)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q", w.Header().Get("Cache-Control"))
	}
}

func TestManagementRefusals(t *testing.T) {
	ts := newTestServer(t, "https://kritik.example")
	admin := &auth.Principal{Admin: true}
	member := memberOf(t, ts.file, "alpha")
	tests := []struct {
		name   string
		p      *auth.Principal
		req    *http.Request
		status int
		code   ErrorCode
	}{
		{"the configuration is not served", admin, httptest.NewRequest("GET", "/api/v1/config", nil), 404, CodeNotFound},
		{"turning on as a member", member, mutate("PUT", "/api/v1/accounts/github/alpha/repos/o/r/turned-on", `{"on":true}`), 403, CodeForbidden},
		{"turning on in an unreadable account", member, mutate("PUT", "/api/v1/accounts/github/beta/repos/o/r/turned-on", `{"on":true}`), 404,
			CodeNotFound},
		{"rerun as a member", member, mutate("POST", "/api/v1/accounts/github/alpha/pulls/o/r/1/rerun", ""), 403, CodeForbidden},
		{"cancel as a member", member, mutate("POST", "/api/v1/accounts/github/alpha/reviews/x/cancel", ""), 403, CodeForbidden},
		{"reindex as a member", member, mutate("POST", "/api/v1/accounts/github/alpha/repos/o/r/reindex", ""), 403, CodeForbidden},
		{"rerun without actions", admin, mutate("POST", "/api/v1/accounts/github/alpha/pulls/o/r/1/rerun", ""), 503, CodeActionsDisabled},
		{"account audit as a member", member, httptest.NewRequest("GET", "/api/v1/accounts/github/alpha/audit", nil), 403, CodeForbidden},
		{"admin audit as a member", member, httptest.NewRequest("GET", "/api/v1/admin/audit", nil), 404, CodeNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := ts.as(tt.p, tt.req)
			if w.Code != tt.status {
				t.Fatalf("status = %d, want %d: %s", w.Code, tt.status, w.Body)
			}
			if e := decodeError(t, w); e.Code != tt.code {
				t.Errorf("code = %q, want %q", e.Code, tt.code)
			}
		})
	}
}

func TestManagementNeedsSameOrigin(t *testing.T) {
	ts := newTestServer(t, "https://kritik.example")
	admin := &auth.Principal{Admin: true}
	for _, route := range []struct{ method, path string }{
		{"PUT", "/api/v1/accounts/github/alpha/repos/o/r/turned-on"},
		{"POST", "/api/v1/accounts/github/alpha/pulls/o/r/1/rerun"}, {"POST", "/api/v1/accounts/github/alpha/reviews/x/cancel"},
		{"POST", "/api/v1/accounts/github/alpha/repos/o/r/reindex"},
	} {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			req := httptest.NewRequest(route.method, route.path, strings.NewReader("{}"))
			req.Header.Set("Origin", "https://kritik.example")
			if w := ts.as(admin, req); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "csrf") {
				t.Errorf("without X-Kritik: %d %s, want 403 csrf", w.Code, w.Body)
			}
		})
	}
}
