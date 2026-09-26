package webapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/home-operations/kritik/internal/auth"
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
	admin := memberOf(t, ts.file, "alpha", auth.RoleAdmin)
	member := memberOf(t, ts.file, "alpha", auth.RoleMember)
	const spec = `{"slug":"gamma","spec":{"slug":"gamma"}}`
	tests := []struct {
		name   string
		p      *auth.Principal
		req    *http.Request
		status int
		code   ErrorCode
	}{
		{"create needs an operator", admin, mutate("POST", "/api/v1/tenants", spec), 403, CodeForbidden},
		{"create needs the sealing key", operator, mutate("POST", "/api/v1/tenants", spec), 503, CodeManagementDisabled},
		{"update of a file tenant", admin, mutate("PUT", "/api/v1/tenants/alpha/config", `{"revision":1,"spec":{}}`), 403, CodeFileManaged},
		{"update of a file tenant by an operator", operator, mutate("PUT", "/api/v1/tenants/alpha/config", `{}`), 403, CodeFileManaged},
		{"update of an unreadable tenant", admin, mutate("PUT", "/api/v1/tenants/beta/config", `{}`), 404, CodeNotFound},
		{"update of an unknown tenant", admin, mutate("PUT", "/api/v1/tenants/gamma/config", `{}`), 404, CodeNotFound},
		{"update needs the sealing key", operator, mutate("PUT", "/api/v1/tenants/gamma/config", `{}`), 503, CodeManagementDisabled},
		{"delete needs an operator", admin, mutate("DELETE", "/api/v1/tenants/gamma?revision=1", ""), 403, CodeForbidden},
		{"delete of a file tenant", operator, mutate("DELETE", "/api/v1/tenants/alpha?revision=1", ""), 403, CodeFileManaged},
		{"config of an unreadable tenant", member, httptest.NewRequest("GET", "/api/v1/tenants/beta/config", nil), 404, CodeNotFound},
		{"invite as a member", member, mutate("POST", "/api/v1/tenants/alpha/invites", `{"email":"a@b.c","role":"member"}`), 403, CodeForbidden},
		{"invite elsewhere", admin, mutate("POST", "/api/v1/tenants/beta/invites", `{"email":"a@b.c","role":"member"}`), 404, CodeNotFound},
		{"bad invite email", admin, mutate("POST", "/api/v1/tenants/alpha/invites", `{"email":"Ada <a@b.c>","role":"member"}`), 400, CodeBadRequest},
		{"bad invite role", admin, mutate("POST", "/api/v1/tenants/alpha/invites", `{"email":"a@b.c","role":"owner"}`), 400, CodeBadRequest},
		{"invite ttl too long", admin, mutate("POST", "/api/v1/tenants/alpha/invites", `{"email":"a@b.c","role":"member","ttlHours":721}`),
			400, CodeBadRequest},
		{"unknown invite field", admin, mutate("POST", "/api/v1/tenants/alpha/invites", `{"email":"a@b.c","role":"member","x":1}`),
			400, CodeBadRequest},
		{"member change as a member", member, mutate("PATCH", "/api/v1/tenants/alpha/members/x", `{"role":"admin"}`), 403, CodeForbidden},
		{"member removal as a member", member, mutate("DELETE", "/api/v1/tenants/alpha/members/x", ""), 403, CodeForbidden},
		{"invite removal of a bad id", admin, mutate("DELETE", "/api/v1/tenants/alpha/invites/x", ""), 404, CodeNotFound},
		{"rerun as a member", member, mutate("POST", "/api/v1/tenants/alpha/pulls/o/r/1/rerun", ""), 403, CodeForbidden},
		{"cancel as a member", member, mutate("POST", "/api/v1/tenants/alpha/reviews/x/cancel", ""), 403, CodeForbidden},
		{"reindex as a member", member, mutate("POST", "/api/v1/tenants/alpha/repos/o/r/reindex", ""), 403, CodeForbidden},
		{"rerun without actions", admin, mutate("POST", "/api/v1/tenants/alpha/pulls/o/r/1/rerun", ""), 503, CodeActionsDisabled},
		{"tenant audit as a member", member, httptest.NewRequest("GET", "/api/v1/tenants/alpha/audit", nil), 403, CodeForbidden},
		{"operator audit as an admin", admin, httptest.NewRequest("GET", "/api/v1/operator/audit", nil), 403, CodeForbidden},
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
	w := ts.as(&auth.Principal{Operator: true}, mutate("POST", "/api/v1/tenants", `{"slug":"alpha","spec":{"slug":"alpha"}}`))
	if w.Code != http.StatusConflict || decodeError(t, w).Code != CodeSlugTaken {
		t.Fatalf("status = %d: %s, want 409 slug_taken", w.Code, w.Body)
	}
}

func TestManagementNeedsSameOrigin(t *testing.T) {
	ts := newTestServer(t, "https://kritik.example")
	operator := &auth.Principal{Operator: true}
	for _, route := range []struct{ method, path string }{
		{"POST", "/api/v1/tenants"}, {"PUT", "/api/v1/tenants/alpha/config"}, {"DELETE", "/api/v1/tenants/alpha"},
		{"POST", "/api/v1/tenants/alpha/invites"}, {"DELETE", "/api/v1/tenants/alpha/invites/x"},
		{"PATCH", "/api/v1/tenants/alpha/members/x"}, {"DELETE", "/api/v1/tenants/alpha/members/x"},
		{"POST", "/api/v1/tenants/alpha/pulls/o/r/1/rerun"}, {"POST", "/api/v1/tenants/alpha/reviews/x/cancel"},
		{"POST", "/api/v1/tenants/alpha/repos/o/r/reindex"},
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

func TestFileTenantConfigIsRedacted(t *testing.T) {
	ts := newTestServer(t, "https://kritik.example")
	w := ts.as(memberOf(t, ts.file, "alpha", auth.RoleAdmin), httptest.NewRequest("GET", "/api/v1/tenants/alpha/config", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	var c TenantConfig
	if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil {
		t.Fatal(err)
	}
	if c.ManagedBy != "file" || c.Editable || c.Revision != nil || len(c.OperatorOnlyFields) != 7 {
		t.Errorf("config = %+v", c)
	}
	if body := w.Body.String(); strings.Contains(body, "KRITIK_TEST_TOKEN") || !strings.Contains(body, `"token":{"set":true}`) {
		t.Errorf("spec is not redacted: %s", body)
	}
}
