package worker

import (
	"testing"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/model"
)

func TestEmbeddersRebuildOnChange(t *testing.T) {
	builds := 0
	e := &Embedders{Build: func(configfile.Embedding) model.Embedder {
		builds++
		return &model.OpenAIEmbedder{}
	}}
	if got, spec := e.Embedder(&configfile.File{}); got != nil || spec != nil || builds != 0 {
		t.Fatalf("no embedder configured: %v, %v, %d builds", got, spec, builds)
	}
	base := configfile.Embedding{BaseURL: "https://embed.example/v1", Model: "m", Dims: 8}
	for _, tt := range []struct {
		name       string
		embedding  configfile.Embedding
		wantBuilds int
	}{
		{"first use builds", base, 1},
		{"same configuration is cached", base, 1},
		{"a new model rebuilds", configfile.Embedding{BaseURL: base.BaseURL, Model: "n", Dims: 8}, 2},
		{"new bounds rebuild", configfile.Embedding{BaseURL: base.BaseURL, Model: "n", Dims: 8, MaxBatch: 8}, 3},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := &configfile.File{Embedding: &tt.embedding}
			got, spec := e.Embedder(f)
			if got == nil || spec == nil || spec.Model != tt.embedding.Model {
				t.Fatalf("Embedder = %v, %+v", got, spec)
			}
			if builds != tt.wantBuilds {
				t.Fatalf("builds = %d, want %d", builds, tt.wantBuilds)
			}
		})
	}
	var none *Embedders
	if got, _ := none.Embedder(&configfile.File{Embedding: &base}); got != nil {
		t.Fatal("a nil Embedders resolves no embedder")
	}
}
