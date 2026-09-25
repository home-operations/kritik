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

// Valid reports whether s is a known scope.
func (s Scope) Valid() bool { return s == ScopeFull || s == ScopeIncremental }

func (s Scope) String() string { return string(s) }

// DecideScope says whether a review can build on the last completed one:
// only when there is one, the runner fetched its head, and fewer than
// maxDeltaFiles files changed since. A full review says why it is one.
func DecideScope(hasPrior, priorFetched bool, deltaFiles, maxDeltaFiles int) (Scope, string) {
	switch {
	case !hasPrior:
		return ScopeFull, "no completed review to build on"
	case !priorFetched:
		return ScopeFull, "prior head unreachable"
	case deltaFiles >= maxDeltaFiles:
		return ScopeFull, fmt.Sprintf("%d files changed since last review", deltaFiles)
	}
	return ScopeIncremental, ""
}
