package review

import "fmt"

// Marker is the hidden HTML comment that identifies kritik's sticky comment
// on a pull request. It is matched together with the comment's author, never
// alone, so a PR author cannot plant one.
func Marker(number int) string {
	return fmt.Sprintf("<!-- kritik:pr-%d -->", number)
}
