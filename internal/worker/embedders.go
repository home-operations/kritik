package worker

import (
	"sync"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/model"
)

// Embedders resolves the configuration's embedder, built on first use and
// kept for the life of the process, as the configuration is (ADR-0022
// §2.1). A nil *Embedders resolves none.
type Embedders struct {
	Build func(e configfile.Embedding) model.Embedder

	mu       sync.Mutex
	embedder model.Embedder
}

// Embedder returns f's embedder and its settings, or nil for both when f
// configures none.
func (e *Embedders) Embedder(f *configfile.File) (model.Embedder, *configfile.Embedding) {
	if e == nil || f.Embedding == nil {
		return nil, nil
	}
	spec := *f.Embedding
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.embedder == nil {
		e.embedder = e.Build(spec)
	}
	return e.embedder, &spec
}

// BuildEmbedder is the production Embedders.Build: an OpenAI-compatible
// embedder.
func BuildEmbedder(e configfile.Embedding) model.Embedder {
	out := model.NewOpenAIEmbedder(e.BaseURL, e.APIKeyValue().Value(), e.Model, e.Dims)
	out.MaxBatch, out.MaxBatchChars, out.MaxItemChars = e.Bounds()
	return out
}
