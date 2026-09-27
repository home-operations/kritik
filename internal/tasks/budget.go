package tasks

import (
	"encoding/json"
	"fmt"
)

// Budget bounds what a run's context sources add to its prompt: one source
// at most PerSource bytes, all of them together Left. Notes say what it
// cut or left out.
type Budget struct {
	PerSource, Left int
	Notes           []string
}

// allowance is what one source may still take.
func (b *Budget) allowance() int { return max(min(b.PerSource, b.Left), 0) }

// Take is s cut to what one source may still take, charged against the
// budget.
func (b *Budget) Take(name, s string) string {
	n := b.allowance()
	if len(s) > n {
		s = cutUTF8(s, n)
		b.note(name, len(s))
	}
	b.Left -= len(s)
	return s
}

// note records that the source name was cut to n bytes.
func (b *Budget) note(name string, n int) {
	if n == 0 {
		b.Notes = append(b.Notes, fmt.Sprintf("context %s left out: the context budget is spent", name))
		return
	}
	b.Notes = append(b.Notes, fmt.Sprintf("context %s cut to %d bytes", name, n))
}

// TakeList is the leading items whose JSON encoding one source may still
// take, charged against the budget; it keeps no item it cannot encode.
func TakeList[T any](b *Budget, name string, items []T) []T {
	n := b.allowance()
	// Each item takes its encoding and the ',' or ']' after it.
	kept, used := []T{}, len("[")
	for _, it := range items {
		raw, err := json.Marshal(it)
		if err != nil {
			b.Notes = append(b.Notes,
				fmt.Sprintf("context %s kept %d of %d results: a result could not be encoded: %v", name, len(kept), len(items), err))
			break
		}
		if used+len(raw)+1 > n {
			b.Notes = append(b.Notes, fmt.Sprintf("context %s kept %d of %d results within the context budget", name, len(kept), len(items)))
			break
		}
		kept, used = append(kept, it), used+len(raw)+1
	}
	if len(kept) == 0 {
		used = len("[]")
	}
	b.Left -= used
	return kept
}
