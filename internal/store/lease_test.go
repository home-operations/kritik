package store

import (
	"testing"
	"time"

	"github.com/home-operations/kritik/internal/configfile"
)

func TestBackoff(t *testing.T) {
	base, limit := 5*time.Second, 5*time.Minute
	for _, tt := range []struct {
		n    int
		want time.Duration
	}{{0, 5 * time.Second}, {1, 10 * time.Second}, {3, 40 * time.Second}, {6, 5 * time.Minute}, {40, 5 * time.Minute}} {
		seen := map[time.Duration]bool{}
		for range 200 {
			d := Backoff(tt.n, base, limit)
			if d < tt.want/2 || d > tt.want {
				t.Fatalf("Backoff(%d) = %s, want within [%s, %s]", tt.n, d, tt.want/2, tt.want)
			}
			seen[d] = true
		}
		// Jitter spreads waiters out rather than waking them together.
		if len(seen) < 10 {
			t.Fatalf("Backoff(%d) took only %d distinct values in 200 draws", tt.n, len(seen))
		}
	}
}

func TestCapReason(t *testing.T) {
	limits := configfile.Limits{ReviewsPerDay: 2, TokensPerMonth: 100}
	for _, tt := range []struct {
		usage MonthUsage
		want  string
	}{
		{MonthUsage{}, ""},
		{MonthUsage{ReviewsToday: 2}, "reviewsPerDay (2) reached"},
		{MonthUsage{Tokens: 100}, "tokensPerMonth (100) reached"},
		{MonthUsage{ReviewsToday: 5, Tokens: 500}, "reviewsPerDay (2) reached"},
	} {
		if got := CapReason(tt.usage, limits); got != tt.want {
			t.Errorf("CapReason(%+v) = %q, want %q", tt.usage, got, tt.want)
		}
	}
	if got := CapReason(MonthUsage{ReviewsToday: 9, Tokens: 9}, configfile.Limits{}); got != "" {
		t.Fatalf("no caps = %q", got)
	}
}
