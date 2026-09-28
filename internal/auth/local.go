package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/store"
)

// Password guessing is held to maxFailedSignIns wrong answers per client
// address in failedSignInWindow. Behind a proxy every client shares its
// address, so the limit is then the whole instance's: guessing slows for
// everyone, which is the side to err on.
const (
	maxFailedSignIns   = 10
	failedSignInWindow = 15 * time.Minute
	// maxTrackedClients bounds the limiter's memory; past it, expired
	// windows are dropped.
	maxTrackedClients = 10_000
)

// attempts counts failed local sign-ins per client address.
type attempts struct {
	now func() time.Time

	mu     sync.Mutex
	failed map[string]window
}

type window struct {
	start time.Time
	n     int
}

func newAttempts(now func() time.Time) *attempts {
	return &attempts{now: now, failed: map[string]window{}}
}

// allowed reports whether client may try a password now.
func (a *attempts) allowed(client string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	w, ok := a.failed[client]
	return !ok || a.now().Sub(w.start) >= failedSignInWindow || w.n < maxFailedSignIns
}

// fail records a wrong password from client.
func (a *attempts) fail(client string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	if len(a.failed) >= maxTrackedClients {
		for k, w := range a.failed {
			if now.Sub(w.start) >= failedSignInWindow {
				delete(a.failed, k)
			}
		}
	}
	w := a.failed[client]
	if now.Sub(w.start) >= failedSignInWindow {
		w = window{start: now}
	}
	w.n++
	a.failed[client] = w
}

// clientAddr is the address a request came from, without its port.
func clientAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// maxLocalBody bounds a password sign-in's request body.
const maxLocalBody = 4 << 10

// localSignIn signs the local admin in with the password the file or the
// environment sets, and starts an admin session. It answers in JSON, since
// the dashboard posts it from the sign-in page.
func (h *Handler) localSignIn(w http.ResponseWriter, r *http.Request) {
	file := h.current.Get()
	user, password, ok := file.Auth.AdminUser()
	if !ok {
		writeJSON(w, http.StatusNotFound, errorBody{Code: codeUnknownSignIn})
		return
	}
	client := clientAddr(r)
	if !h.attempts.allowed(client) {
		writeJSON(w, http.StatusTooManyRequests, errorBody{Code: codeTooManyAttempts})
		return
	}
	var body struct {
		User     string `json:"user"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxLocalBody)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Code: codeInvalidRequest})
		return
	}
	// Both compared whatever the first says, so the time taken tells nothing
	// about which was wrong.
	userOK, passwordOK := equalSecret(body.User, user), equalSecret(body.Password, password.Value())
	if !userOK || !passwordOK {
		h.attempts.fail(client)
		h.logger.WarnContext(r.Context(), "auth: local sign-in refused", "client", client)
		writeJSON(w, http.StatusUnauthorized, errorBody{Code: codeInvalidCredentials})
		return
	}
	key, _ := GrantKey(file.Auth, string(configfile.SignInLocal))
	id := Identity{Provider: string(configfile.SignInLocal), Origin: localOrigin, Subject: user, Login: user, DisplayName: user}
	token, expires, err := h.replaceSession(r, id, store.SessionGrant{Role: RoleAdmin, Key: key}, file.Auth.SessionTTLOrDefault())
	if err != nil {
		h.logger.ErrorContext(r.Context(), "auth: local sign-in", "error", err)
		writeJSON(w, http.StatusInternalServerError, errorBody{Code: codeInternal})
		return
	}
	http.SetCookie(w, sessionCookie(h.webURL, token, expires))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// equalSecret compares two strings in time that depends on neither.
func equalSecret(a, b string) bool {
	ha, hb := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}
