package auth

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAttempts(t *testing.T) {
	now := time.Now()
	a := newAttempts(func() time.Time { return now })
	for range maxFailedSignIns {
		if !a.allowed("1.2.3.4") {
			t.Fatal("refused before the limit")
		}
		a.fail("1.2.3.4")
	}
	if a.allowed("1.2.3.4") {
		t.Fatal("allowed past the limit")
	}
	if !a.allowed("5.6.7.8") {
		t.Fatal("another client was held to the first's limit")
	}
	now = now.Add(failedSignInWindow)
	if !a.allowed("1.2.3.4") {
		t.Fatal("still refused after the window")
	}
}

// TestAttemptsStayBounded: a flood of failures from distinct addresses,
// none of whose windows has expired, cannot grow the tracker past its cap;
// the oldest window gives way, and a client already tracked keeps its
// count.
func TestAttemptsStayBounded(t *testing.T) {
	now := time.Now()
	a := newAttempts(func() time.Time { return now })
	a.fail("first")
	for i := range maxTrackedClients + 100 {
		now = now.Add(time.Millisecond)
		a.fail(fmt.Sprintf("2001:db8::%x", i))
	}
	if len(a.failed) > maxTrackedClients {
		t.Fatalf("%d clients tracked, cap is %d", len(a.failed), maxTrackedClients)
	}
	// "first" and the 100 oldest flood addresses gave way; the next survives.
	if _, ok := a.failed["first"]; ok {
		t.Fatal("the oldest window was not the one dropped")
	}
	if _, ok := a.failed["2001:db8::64"]; !ok {
		t.Fatal("a window newer than the oldest was dropped")
	}
	last := fmt.Sprintf("2001:db8::%x", maxTrackedClients+99)
	a.fail(last)
	if got := a.failed[last].n; got != 2 || len(a.failed) > maxTrackedClients {
		t.Fatalf("count = %d with %d tracked; a tracked client failing again keeps its count and evicts nothing", got, len(a.failed))
	}
}

// TestLocalSignInRefuses covers every way the password form says no before
// it would touch the store.
func TestLocalSignInRefuses(t *testing.T) {
	h := testHandler(t, "https://kritik.example.com", testFile(t, "auth:\n"+adminPassword))
	mux := http.NewServeMux()
	h.Register(mux)
	post := func(body, from string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/auth/local", strings.NewReader(body))
		r.Header.Set("X-Kritik", "1")
		r.Header.Set("Origin", "https://kritik.example.com")
		r.RemoteAddr = from + ":1234"
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	if w := post(`{"user":"admin","password":"wrong"}`, "10.0.0.1"); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d", w.Code)
	} else {
		assertCode(t, w, "invalid_credentials")
	}
	if w := post(`{"user":"root","password":"s3cret"}`, "10.0.0.1"); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong user: %d", w.Code)
	}
	if w := post(`not json`, "10.0.0.1"); w.Code != http.StatusBadRequest {
		t.Fatalf("bad body: %d", w.Code)
	}
	for range maxFailedSignIns - 2 {
		post(`{"user":"admin","password":"wrong"}`, "10.0.0.1")
	}
	if w := post(`{"user":"admin","password":"s3cret"}`, "10.0.0.1"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("past the limit: %d", w.Code)
	} else {
		assertCode(t, w, "too_many_attempts")
	}

	r := httptest.NewRequest(http.MethodPost, "/auth/local", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin: %d, want 403", w.Code)
	}

	h.current.Set(testFile(t, "auth:\n  github:\n    clientId: c\n    clientSecret: { env: TEST_AUTH_SECRET }\n    roleMappingExpr: '\"admin\"'\n"))
	if w := post(`{"user":"admin","password":"s3cret"}`, "10.0.0.2"); w.Code != http.StatusNotFound {
		t.Fatalf("without a password: %d, want 404", w.Code)
	}
}
