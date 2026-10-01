package review

import "fmt"

// Scope is what a review covered; the reviews table's CHECK lists the same
// set.
type Scope string

// Review scopes.
const (
	ScopeFull        Scope = "full"
	ScopeIncremental Scope = "incremental"
)

// DecideScope says whether a review can build on the last completed one:
// only when there is one, the head is not the one it reviewed, the runner
// fetched its head, and fewer than maxDeltaFiles files changed since. A
// full review says why it is one.
func DecideScope(hasPrior, sameHead, priorFetched bool, deltaFiles, maxDeltaFiles int) (Scope, string) {
	switch {
	case !hasPrior:
		return ScopeFull, "no completed review to build on"
	case sameHead:
		// Nothing changed since, so an incremental review could only
		// repeat the last one; a re-run of the same head is asked for a
		// fresh look.
		return ScopeFull, "re-run at the reviewed head"
	case !priorFetched:
		return ScopeFull, "prior head unreachable"
	case deltaFiles >= maxDeltaFiles:
		return ScopeFull, fmt.Sprintf("%d files changed since last review", deltaFiles)
	}
	return ScopeIncremental, ""
}
