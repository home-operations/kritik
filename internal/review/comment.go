package review

import (
	"fmt"
	"strings"
)

// Marker is the hidden HTML comment that identifies kritik's sticky comment
// on a pull request. It is matched together with the comment's author, never
// alone, so a PR author cannot plant one.
func Marker(number int) string {
	return fmt.Sprintf("<!-- kritik:pr-%d -->", number)
}

// FindingMarker is the hidden HTML comment that identifies the inline
// comment posted for a finding, by its Fingerprint, so a review that posts
// again after a crash finds the forge already has it. Like Marker, it is
// matched together with the comment's author.
func FindingMarker(fingerprint string) string {
	return "<!-- kritik:finding:" + fingerprint + " -->"
}

// MarkedFinding returns the fingerprint a FindingMarker in body names, and
// whether it holds one.
func MarkedFinding(body string) (string, bool) {
	return marked(body, "<!-- kritik:finding:")
}

// FollowUpMarker is the hidden HTML comment that identifies the reply to
// the comment commentID, so a follow-up job that runs again after a crash
// finds the reply already posted.
func FollowUpMarker(commentID int64) string {
	return fmt.Sprintf("<!-- kritik:followup:%d -->", commentID)
}

// MarkedFollowUp returns the comment id a FollowUpMarker in body names, and
// whether it holds one.
func MarkedFollowUp(body string) (string, bool) {
	return marked(body, "<!-- kritik:followup:")
}

func marked(body, prefix string) (string, bool) {
	_, rest, ok := strings.Cut(body, prefix)
	if !ok {
		return "", false
	}
	id, _, ok := strings.Cut(rest, " -->")
	if !ok || id == "" || strings.ContainsAny(id, " \n") {
		return "", false
	}
	return id, true
}
