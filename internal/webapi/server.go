// Package webapi is the dashboard's JSON API and static UI server: reads
// over each account's reviews, repositories, usage and queue; the admin's
// configuration, setup wizard, GitHub App registration and connections;
// re-runs, cancels and reindexes with their audit log; and the server-sent
// event stream that keeps the UI live. The web role runs it; internal/auth
// decides who a request acts as.
package webapi

import (
	"cmp"
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/home-operations/kritik/internal/auth"
	"github.com/home-operations/kritik/internal/config"
	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/sealbox"
	"github.com/home-operations/kritik/internal/store"
)

// Config wires a Server.
type Config struct {
	Store   *store.Store
	Current *configfile.Current
	Auth    *auth.Handler
	// Keyring seals the instance spec's secrets; nil disables writing the
	// spec.
	Keyring *sealbox.Keyring
	// Actions queues re-runs, cancels and reindexes; nil disables them.
	Actions Actions
	// Version is the build's version, shown by /api/v1/meta.
	Version string
	// UI is the built dashboard, served under the base path; nil serves
	// no UI.
	UI fs.FS
	// WebURL is the dashboard's external URL, required; its path is the
	// base path every route is served under.
	WebURL *url.URL
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// Now defaults to time.Now.
	Now func() time.Time
	// Env is this process's environment as the configuration read it,
	// shown to admins; secrets show only whether they are set.
	Env []config.EnvVar
	// GitHubAPI is the GitHub API the App manifest flow converts its code
	// at, "" for api.github.com; tests point it at a server of their own.
	GitHubAPI string
}

// Server serves the dashboard.
type Server struct {
	store     *store.Store
	current   *configfile.Current
	auth      *auth.Handler
	keyring   *sealbox.Keyring
	actions   Actions
	version   string
	webURL    *url.URL
	ui        fs.FS
	basePath  string
	logger    *slog.Logger
	now       func() time.Time
	hub       *hub
	env       []config.EnvVar
	githubAPI string
}

// New builds a Server from cfg.
func New(cfg Config) *Server {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Server{
		store: cfg.Store, current: cfg.Current, auth: cfg.Auth, keyring: cfg.Keyring, actions: cfg.Actions, version: cfg.Version,
		webURL: cfg.WebURL, ui: cfg.UI, basePath: strings.TrimRight(cfg.WebURL.Path, "/"),
		logger: cfg.Logger, now: cfg.Now, hub: newHub(cfg.Current, cfg.Logger), env: cfg.Env, githubAPI: cfg.GitHubAPI,
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
	authed := s.auth.Authenticate(h)
	assets := s.basePath + "/assets/"
	// A fingerprinted asset is the same for everyone; resolving the session
	// cookie every one of them carries would only cost database round trips.
	return s.accessLog(s.recoverer(securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, assets) {
			h.ServeHTTP(w, r)
			return
		}
		authed.ServeHTTP(w, r)
	}))))
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
	// What the sign-in page needs is served before anyone signs in.
	public := http.NewServeMux()
	public.HandleFunc("GET "+metaPath, s.handler(s.getMeta))

	signIn := http.NewServeMux()
	s.auth.Register(signIn)
	signIn.HandleFunc("/", s.notFound)

	app := http.NewServeMux()
	s.registerAppPages(app)
	app.HandleFunc("/", s.notFound)

	ui := s.uiHandler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == metaPath:
			w.Header().Set("Cache-Control", "no-store")
			public.ServeHTTP(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/"):
			w.Header().Set("Cache-Control", "no-store")
			apiHandler.ServeHTTP(w, r)
		case strings.HasPrefix(r.URL.Path, "/auth/"):
			w.Header().Set("Cache-Control", "no-store")
			signIn.ServeHTTP(w, r)
		case strings.HasPrefix(r.URL.Path, "/app/"):
			w.Header().Set("Cache-Control", "no-store")
			app.ServeHTTP(w, r)
		default:
			ui.ServeHTTP(w, r)
		}
	})
}

// registerAPI mounts every /api route. Each group of routes registers on
// mux from the file that holds its handlers.
func (s *Server) registerAPI(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/events", s.hub.serve)
	s.registerReads(mux)
	s.registerManage(mux)
	s.registerApp(mux)
	s.registerConnections(mux)
	s.registerSetup(mux)
	s.registerActions(mux)
	s.registerAudit(mux)
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
		if isBareDir(s.ui, r.URL.Path) {
			http.NotFound(w, r)
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

// isBareDir reports whether urlPath names a directory of ui with no
// index.html, which http.FileServerFS would otherwise list.
func isBareDir(ui fs.FS, urlPath string) bool {
	name := strings.TrimPrefix(path.Clean("/"+urlPath), "/")
	name = cmp.Or(name, ".")
	st, err := fs.Stat(ui, name)
	if err != nil || !st.IsDir() {
		return false
	}
	_, err = fs.Stat(ui, path.Join(name, "index.html"))
	return err != nil
}

// contentSecurityPolicy allows only the dashboard's own scripts, styles
// and connections; img-src also admits https for forge avatars, and
// form-action GitHub, where the App manifest form posts.
const contentSecurityPolicy = "default-src 'self'; img-src 'self' data: https:; style-src 'self' 'unsafe-inline'; " +
	"script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self' https://github.com"

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
	r.status = cmp.Or(r.status, code)
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.status = cmp.Or(r.status, http.StatusOK)
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
