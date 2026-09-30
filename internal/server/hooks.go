package server

import (
	"net/http"
	"strings"
)

// Public is the handler of the one listener a forge and a browser reach
// (ADR-0024 §2.2). POST /hooks/{connection}, and the same under basePath,
// the dashboard URL's path, goes to hooks, which checks each delivery's
// signature itself; everything else goes to web, the dashboard, which signs
// its own requests in.
func Public(basePath string, hooks, web http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /hooks/{connection}", hooks)
	if base := strings.TrimRight(basePath, "/"); base != "" {
		mux.Handle("POST "+base+"/hooks/{connection}", hooks)
	}
	mux.Handle("/", web)
	return mux
}
