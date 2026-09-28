package webapi

import (
	"net/http"
	"regexp"
	"strings"
)

var (
	yamlLineRe  = regexp.MustCompile(`^line \d+: `)
	yamlFieldRe = regexp.MustCompile(`field (\S+) not found`)
	yamlTypeRe  = regexp.MustCompile(` in type [\w.*\[\]]+`)
)

func trimConfigfile(msg string) string {
	for strings.HasPrefix(msg, "configfile: ") {
		msg = strings.TrimPrefix(msg, "configfile: ")
	}
	return msg
}

// splitPath is the spec path a configuration error names, up to the first
// colon or space, and the message; the path is "" when the message starts
// with no key of the spec.
func splitPath(msg string) (path, rest string) {
	path = msg
	if i := strings.IndexAny(path, ": "); i >= 0 {
		path = path[:i]
	}
	if !strings.ContainsAny(path, ".[") && !isSpecKey(path) {
		return "", msg
	}
	return path, msg
}

// isSpecKey reports whether key is a top-level key of the spec.
func isSpecKey(key string) bool {
	switch key {
	case "providers", "defaults", "polling", "indexing", "tools", "retention", "egress", "connections", "accounts":
		return true
	}
	return false
}

// decodeFailure is a spec that does not decode: the decoder's message
// without the line numbers of a document the client never wrote, and the
// offending field's name as the path when it has one.
func decodeFailure(err error) error {
	msg := trimConfigfile(err.Error())
	msg = strings.TrimPrefix(msg, "spec: ")
	msg = strings.Replace(msg, "yaml: unmarshal errors:", "", 1)
	var parts []string
	for line := range strings.SplitSeq(msg, "\n") {
		line = strings.TrimSpace(line)
		line = yamlLineRe.ReplaceAllString(line, "")
		if line != "" {
			parts = append(parts, yamlTypeRe.ReplaceAllString(line, ""))
		}
	}
	msg = strings.ReplaceAll(strings.Join(parts, "; "), ":; ", ": ")
	var path string
	if m := yamlFieldRe.FindStringSubmatch(msg); m != nil {
		path = m[1]
	}
	return errStatus(http.StatusUnprocessableEntity, CodeInvalidSpec, "spec: "+msg, pathDetails{Path: path})
}
