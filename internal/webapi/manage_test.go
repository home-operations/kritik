package webapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/home-operations/kritik/internal/auth"
	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/sealbox"
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
	if m.Management || m.WebURL != "https://kritik.example" || m.SignIn == nil {
		t.Errorf("meta = %+v", m)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q", w.Header().Get("Cache-Control"))
	}
}

func TestManagementRefusals(t *testing.T) {
	ts := newTestServer(t, "https://kritik.example")
	operator := &auth.Principal{Operator: true}
	member := memberOf(t, ts.file, "alpha")
	const spec = `{"slug":"gamma","spec":{"slug":"gamma"}}`
	tests := []struct {
		name   string
		p      *auth.Principal
		req    *http.Request
		status int
		code   ErrorCode
	}{
		{"create needs an operator", member, mutate("POST", "/api/v1/accounts", spec), 403, CodeForbidden},
		{"create needs the sealing key", operator, mutate("POST", "/api/v1/accounts", spec), 503, CodeManagementDisabled},
		{"update of a file account", member, mutate("PUT", "/api/v1/accounts/alpha/config", `{"revision":1,"spec":{}}`), 403, CodeFileManaged},
		{"update of a file account by an operator", operator, mutate("PUT", "/api/v1/accounts/alpha/config", `{}`), 403, CodeFileManaged},
		{"update of an unreadable account", member, mutate("PUT", "/api/v1/accounts/beta/config", `{}`), 404, CodeNotFound},
		{"update of an unknown account", member, mutate("PUT", "/api/v1/accounts/gamma/config", `{}`), 404, CodeNotFound},
		{"update needs the sealing key", operator, mutate("PUT", "/api/v1/accounts/gamma/config", `{}`), 503, CodeManagementDisabled},
		{"delete needs an operator", member, mutate("DELETE", "/api/v1/accounts/gamma?revision=1", ""), 403, CodeForbidden},
		{"delete of a file account", operator, mutate("DELETE", "/api/v1/accounts/alpha?revision=1", ""), 403, CodeFileManaged},
		{"config of an unreadable account", member, httptest.NewRequest("GET", "/api/v1/accounts/beta/config", nil), 404, CodeNotFound},
		{"rerun as a member", member, mutate("POST", "/api/v1/accounts/alpha/pulls/o/r/1/rerun", ""), 403, CodeForbidden},
		{"cancel as a member", member, mutate("POST", "/api/v1/accounts/alpha/reviews/x/cancel", ""), 403, CodeForbidden},
		{"reindex as a member", member, mutate("POST", "/api/v1/accounts/alpha/repos/o/r/reindex", ""), 403, CodeForbidden},
		{"rerun without actions", operator, mutate("POST", "/api/v1/accounts/alpha/pulls/o/r/1/rerun", ""), 503, CodeActionsDisabled},
		{"account audit as a member", member, httptest.NewRequest("GET", "/api/v1/accounts/alpha/audit", nil), 403, CodeForbidden},
		{"admin audit as a member", member, httptest.NewRequest("GET", "/api/v1/operator/audit", nil), 403, CodeForbidden},
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

func TestCreateRefusesAFileSlug(t *testing.T) {
	ts := newTestServer(t, "https://kritik.example")
	key := make([]byte, 32)
	kr, err := sealbox.NewKeyring(key)
	if err != nil {
		t.Fatal(err)
	}
	ts.srv.keyring = kr
	w := ts.as(&auth.Principal{Operator: true}, mutate("POST", "/api/v1/accounts", `{"slug":"alpha","spec":{"slug":"alpha"}}`))
	if w.Code != http.StatusConflict || decodeError(t, w).Code != CodeSlugTaken {
		t.Fatalf("status = %d: %s, want 409 slug_taken", w.Code, w.Body)
	}
}

func TestManagementNeedsSameOrigin(t *testing.T) {
	ts := newTestServer(t, "https://kritik.example")
	operator := &auth.Principal{Operator: true}
	for _, route := range []struct{ method, path string }{
		{"POST", "/api/v1/accounts"}, {"PUT", "/api/v1/accounts/alpha/config"}, {"DELETE", "/api/v1/accounts/alpha"},
		{"POST", "/api/v1/accounts/alpha/pulls/o/r/1/rerun"}, {"POST", "/api/v1/accounts/alpha/reviews/x/cancel"},
		{"POST", "/api/v1/accounts/alpha/repos/o/r/reindex"},
	} {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			req := httptest.NewRequest(route.method, route.path, strings.NewReader("{}"))
			req.Header.Set("Origin", "https://kritik.example")
			if w := ts.as(operator, req); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "csrf") {
				t.Errorf("without X-Kritik: %d %s, want 403 csrf", w.Code, w.Body)
			}
		})
	}
}

func TestFileAccountConfigIsRedacted(t *testing.T) {
	ts := newTestServer(t, "https://kritik.example")
	w := ts.as(memberOf(t, ts.file, "alpha"), httptest.NewRequest("GET", "/api/v1/accounts/alpha/config", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	var c AccountConfig
	if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil {
		t.Fatal(err)
	}
	if c.ManagedBy != "file" || c.Editable || c.Revision != nil || len(c.Policy) != len(configfile.Policies) ||
		slices.ContainsFunc(c.Policy, func(p FieldPolicy) bool { return p.Editable }) {
		t.Errorf("config = %+v", c)
	}
	if body := w.Body.String(); strings.Contains(body, "KRITIK_TEST_TOKEN") || !strings.Contains(body, `"privateKey":{"set":true}`) {
		t.Errorf("spec is not redacted: %s", body)
	}
	if in := c.Inherited; in.Account.Mode != configfile.ReviewSingle || in.AccountSources["mode"] != configfile.SourceDefault ||
		in.Repository.Limits.Concurrency != configfile.DefaultConcurrency || in.RepositorySources["limits"] != configfile.SourceDefault {
		t.Errorf("inherited = %+v", in)
	}
}
