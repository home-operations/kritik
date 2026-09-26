package worker

import (
	"testing"
	"time"
)

func TestBackoff(t *testing.T) {
	base, limit := 5*time.Second, 5*time.Minute
	for _, tt := range []struct {
		n    int
		want time.Duration
	}{{0, 5 * time.Second}, {1, 10 * time.Second}, {3, 40 * time.Second}, {6, 5 * time.Minute}, {40, 5 * time.Minute}} {
		seen := map[time.Duration]bool{}
		for range 200 {
			d := backoff(tt.n, base, limit)
			if d < tt.want/2 || d > tt.want {
				t.Fatalf("backoff(%d) = %s, want within [%s, %s]", tt.n, d, tt.want/2, tt.want)
			}
			seen[d] = true
		}
		// Jitter spreads waiters out rather than waking them together.
		if len(seen) < 10 {
			t.Fatalf("backoff(%d) took only %d distinct values in 200 draws", tt.n, len(seen))
		}
	}
}
