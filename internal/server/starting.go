package server

import (
	"net/http"
	"strconv"
	"sync/atomic"
	"time"
)

// Switch is the public listener's handler from the moment it binds: it
// answers with one handler until Set hands it another, so the listener
// can come up, and the replica be ready, before what it serves exists.
type Switch struct {
	h atomic.Pointer[http.Handler]
}

// NewSwitch returns a Switch serving h.
func NewSwitch(h http.Handler) *Switch {
	s := &Switch{}
	s.Set(h)
	return s
}

// Set makes h the handler of every request from now on.
func (s *Switch) Set(h http.Handler) { s.h.Store(&h) }

func (s *Switch) ServeHTTP(w http.ResponseWriter, r *http.Request) { (*s.h.Load()).ServeHTTP(w, r) }

// startingRetry is how long a caller of a starting replica is told to
// wait: the store's own retry interval, so a browser's refresh lands just
// after the next attempt.
const startingRetry = 5 * time.Second

// Starting is what the public listener serves until the database answers
// and the service is built on it: a 503 for everything, with a page a
// browser renders and a plain line for a forge's delivery, which it refuses
// rather than queues since nothing can hold it yet. The forge marks the
// delivery failed; the leader's backstop poll picks the pull request up
// once the service is up.
func Starting() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", strconv.Itoa(int(startingRetry/time.Second)))
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "kritika is starting: waiting for the database", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(startingPage))
		}
	})
}

// startingPage reloads itself every startingRetry, so the dashboard
// appears on its own once the service is up.
const startingPage = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="refresh" content="5">
<title>kritika is starting</title>
<style>
body { font: 16px/1.5 system-ui, sans-serif; color: #1f2328; background: #fff; margin: 0; }
body { display: grid; place-items: center; min-height: 100vh; }
@media (prefers-color-scheme: dark) { body { color: #e6edf3; background: #0d1117; } }
main { text-align: center; padding: 1rem; }
p { margin: 0.25rem 0; opacity: 0.8; }
</style>
</head>
<body>
<main>
<h1>kritika is starting</h1>
<p>Waiting for the database. This page reloads on its own.</p>
</main>
</body>
</html>
`
