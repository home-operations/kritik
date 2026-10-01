package gateway

import (
	"fmt"
	"testing"

	"github.com/home-operations/kritika/internal/contextpack"
	"github.com/home-operations/kritika/internal/store"
)

func TestKeepSimilar(t *testing.T) {
	hit := func(path string, line int, sim float64) store.SimilarHit {
		return store.SimilarHit{Chunk: contextpack.Chunk{Path: path, StartLine: line}, Similarity: sim}
	}
	hits := []store.SimilarHit{
		hit("a.go", 1, 0.65), hit("b.go", 1, 0.91), hit("a.go", 1, 0.65), hit("c.go", 10, 0.70), hit("d.go", 1, 0.49),
	}
	tests := []struct {
		name  string
		floor float64
		want  []string // "path:line:ref", most similar first
	}{
		{name: "the default floor", floor: 0.5, want: []string{"b.go:1:similarity 0.91", "c.go:10:similarity 0.70", "a.go:1:similarity 0.65"}},
		{name: "a floor the weak matches miss", floor: 0.7, want: []string{"b.go:1:similarity 0.91", "c.go:10:similarity 0.70"}},
		{name: "a floor nothing clears", floor: 0.95, want: []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := keepSimilar(hits, tt.floor)
			if len(got) != len(tt.want) {
				t.Fatalf("kept %d chunks, want %d: %+v", len(got), len(tt.want), got)
			}
			for i, c := range got {
				if key := fmt.Sprintf("%s:%d:%s", c.Path, c.StartLine, c.Ref); key != tt.want[i] {
					t.Errorf("chunk %d = %q, want %q", i, key, tt.want[i])
				}
			}
		})
	}
	many := make([]store.SimilarHit, 0, similarMax+5)
	for i := range similarMax + 5 {
		many = append(many, hit("m.go", i+1, 0.9))
	}
	if got := keepSimilar(many, 0.5); len(got) != similarMax {
		t.Fatalf("kept %d chunks, want at most %d", len(got), similarMax)
	}
}
