package worker

import (
	"testing"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/model"
)

func TestEmbeddersBuildOnce(t *testing.T) {
	builds := 0
	e := &Embedders{Build: func(configfile.Embedding) model.Embedder {
		builds++
		return &model.OpenAIEmbedder{}
	}}
	if got, spec := e.Embedder(&configfile.File{}); got != nil || spec != nil || builds != 0 {
		t.Fatalf("no embedder configured: %v, %v, %d builds", got, spec, builds)
	}
	f := &configfile.File{Embedding: &configfile.Embedding{BaseURL: "https://embed.example/v1", Model: "m", Dims: 8}}
	for range 3 {
		got, spec := e.Embedder(f)
		if got == nil || spec == nil || spec.Model != "m" {
			t.Fatalf("Embedder = %v, %+v", got, spec)
		}
	}
	if builds != 1 {
		t.Fatalf("builds = %d, want 1", builds)
	}
	var none *Embedders
	if got, _ := none.Embedder(f); got != nil {
		t.Fatal("a nil Embedders resolves no embedder")
	}
}
