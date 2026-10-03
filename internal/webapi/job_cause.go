package webapi

import (
	"regexp"
	"strings"
)

// JobCause is why a job's last attempt failed, where kritika can tell from
// the error: what the dashboard says in place of the error's own text.
type JobCause string

// CauseForgeUnavailable is GitHub not answering a call, or answering it
// with a server error: nothing kritika or its admin can fix, and the job's
// next attempt may well pass.
const CauseForgeUnavailable JobCause = "forge_unavailable"

// forgeServerError is go-github's text for a 5xx response: the request,
// then the status.
var forgeServerError = regexp.MustCompile(`: 5\d\d `)

// forgeUnanswered are the texts of a call that got no response.
var forgeUnanswered = []string{
	"context deadline exceeded", "timeout", "connection refused", "connection reset", "no such host", "EOF",
}

// jobCause reads a cause from a job's last error. River keeps only the
// error's text, so the forge package's "github: " prefix is what says the
// error is GitHub's.
func jobCause(lastError string) JobCause {
	_, forge, ok := strings.Cut(lastError, "github: ")
	if !ok {
		return ""
	}
	if forgeServerError.MatchString(forge) {
		return CauseForgeUnavailable
	}
	for _, text := range forgeUnanswered {
		if strings.Contains(forge, text) {
			return CauseForgeUnavailable
		}
	}
	return ""
}
