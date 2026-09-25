// Package webapi is the dashboard's JSON API and static UI server: the
// read endpoints over one tenant's reviews, repositories, usage and queue,
// and the server-sent event stream that keeps the UI live. The web role
// runs it; internal/auth decides who a request acts as.
package webapi

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/home-operations/kritik/internal/auth"
	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/store"
)

// Config wires a Server.
type Config struct {
	Store   *store.Store
	Current *configfile.Current
	Auth    *auth.Handler
	// UI is the built dashboard, served under the base path; nil serves
	// no UI.
	UI fs.FS
	// WebURL is the dashboard's external URL; its path is the base path
	// every route is served under.
	WebURL *url.URL
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// Now defaults to time.Now.
	Now func() time.Time
}

// Server serves the dashboard.
type Server struct {
	store    *store.Store
	current  *configfile.Current
	auth     *auth.Handler
	ui       fs.FS
	basePath string
	logger   *slog.Logger
	now      func() time.Time
	hub      *hub
}

// New builds a Server from cfg.
func New(cfg Config) *Server {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	var base string
	if cfg.WebURL != nil {
		base = strings.TrimRight(cfg.WebURL.Path, "/")
	}
	return &Server{
		store: cfg.Store, current: cfg.Current, auth: cfg.Auth, ui: cfg.UI, basePath: base,
		logger: cfg.Logger, now: cfg.Now, hub: newHub(cfg.Current, cfg.Logger),
	}
}

// Run feeds the event stream from the store's notifications until ctx
// ends. The web process runs exactly one, however many streams it serves.
// When it returns every open stream ends too, so a browser reconnects to
// a replica that is still listening instead of waiting on one shutting
// down.
func (s *Server) Run(ctx context.Context) error {
	defer s.hub.close()
	s.store.Listen(ctx, store.ListenHandlers{OnEvent: s.hub.publish, OnReconnect: s.hub.resyncAll})
	return nil
}

// Handler is the whole dashboard: API, sign-in routes and UI.
func (s *Server) Handler() http.Handler {
	var h = s.routes()
	if p := s.basePath; p != "" {
		outer := http.NewServeMux()
		// The bare prefix redirects to its trailing-slash form so the UI's
		// relative asset URLs resolve against the base path, not its parent.
		outer.HandleFunc("GET "+p, func(w http.ResponseWriter, r *http.Request) {
			target := p + "/"
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, target, http.StatusMovedPermanently)
		})
		outer.Handle(p+"/", http.StripPrefix(p, h))
		h = outer
	}
	return s.accessLog(s.recoverer(securityHeaders(s.auth.Authenticate(h))))
}

// routes splits the API, the sign-in routes and the UI by prefix by hand:
// a "/api/" pattern for every method would conflict with the UI's "GET /".
func (s *Server) routes() http.Handler {
	api := http.NewServeMux()
	s.registerAPI(api)
	api.HandleFunc("/", s.notFound)
	// Mutations are held to the dashboard's own origin; every API route
	// needs a principal.
	apiHandler := s.auth.SameOrigin(s.auth.RequirePrincipal(api))

	signIn := http.NewServeMux()
	s.auth.Register(signIn)
	signIn.HandleFunc("/", s.notFound)

	ui := s.uiHandler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/"):
			w.Header().Set("Cache-Control", "no-store")
			apiHandler.ServeHTTP(w, r)
		case strings.HasPrefix(r.URL.Path, "/auth/"):
			w.Header().Set("Cache-Control", "no-store")
			signIn.ServeHTTP(w, r)
		default:
			ui.ServeHTTP(w, r)
		}
	})
}

// registerAPI mounts every /api route. Each group of routes lives in its
// own routes_*.go file as a method that registers on mux.
func (s *Server) registerAPI(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/events", s.hub.serve)
	s.registerReads(mux)
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, s.logger, errNotFound("route"))
}

// uiHandler serves the embedded UI. Fingerprinted files under /assets/
// are immutable; everything else revalidates so a redeploy is picked up at
// once.
func (s *Server) uiHandler() http.Handler {
	if s.ui == nil {
		return http.NotFoundHandler()
	}
	files := http.FileServerFS(s.ui)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}

// contentSecurityPolicy allows only the dashboard's own scripts, styles
// and connections; img-src also admits https for forge avatars.
const contentSecurityPolicy = "default-src 'self'; img-src 'self' data: https:; style-src 'self' 'unsafe-inline'; " +
	"script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// statusRecorder remembers the status a handler wrote. Unwrap lets
// http.ResponseController reach the connection's Flush for the event
// stream.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// accessLog logs each request at debug, and one that failed server-side
// at warn.
func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w}
		start := s.now()
		next.ServeHTTP(rec, r)
		level := slog.LevelDebug
		if rec.status >= http.StatusInternalServerError {
			level = slog.LevelWarn
		}
		s.logger.Log(r.Context(), level, "http", "method", r.Method, "path", r.URL.Path,
			"status", rec.status, "duration", s.now().Sub(start))
	})
}

// recoverer turns a handler panic into a logged 500 rather than a dropped
// connection. http.ErrAbortHandler is the handler asking for exactly that
// drop, so it is re-raised.
func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w}
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if v == http.ErrAbortHandler {
				panic(v)
			}
			s.logger.ErrorContext(r.Context(), "webapi: handler panicked", "method", r.Method, "path", r.URL.Path, "panic", v)
			if rec.status == 0 {
				writeJSON(rec, http.StatusInternalServerError, ErrorBody{Code: CodeInternal, Message: "internal error"})
			}
		}()
		next.ServeHTTP(rec, r)
	})
}
