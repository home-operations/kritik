package webapi

import "testing"

func TestJobCause(t *testing.T) {
	for _, tc := range []struct {
		name, lastError string
		want            JobCause
	}{
		{"no error", "", ""},
		{"forge call timed out", "github: find App installation for alpha/one: context deadline exceeded", CauseForgeUnavailable},
		{"forge call wrapped by its caller", "worker: review: github: list files: read tcp: i/o timeout", CauseForgeUnavailable},
		{"forge never sent headers", "github: mint installation token for 7: net/http: timeout awaiting response headers", CauseForgeUnavailable},
		{"forge refused the connection", "github: read App: dial tcp 192.0.2.1:443: connect: connection refused", CauseForgeUnavailable},
		{"forge closed the connection", "github: read App: unexpected EOF", CauseForgeUnavailable},
		{"forge server error", "github: find App installation for alpha/one: GET https://api.github.com/repos/alpha/one/installation: 503 Service Unavailable []", CauseForgeUnavailable},
		{"forge refused the call", "github: find App installation for alpha/one: GET https://api.github.com/repos/alpha/one/installation: 404 Not Found []", ""},
		{"a timeout that is not the forge's", "model: complete: context deadline exceeded", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := jobCause(tc.lastError); got != tc.want {
				t.Errorf("jobCause(%q) = %q, want %q", tc.lastError, got, tc.want)
			}
		})
	}
}
