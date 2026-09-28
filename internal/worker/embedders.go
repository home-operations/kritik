package worker

import (
	"sync"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/model"
)

// Embedders resolves the running configuration's embedder, building it on
// first use and again whenever its settings change. A nil *Embedders
// resolves none.
type Embedders struct {
	Build func(e configfile.Embedding) model.Embedder

	mu       sync.Mutex
	spec     configfile.Embedding
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
	if e.embedder == nil || !sameEmbedding(e.spec, spec) {
		e.spec, e.embedder = spec, e.Build(spec)
	}
	return e.embedder, &spec
}

func sameEmbedding(a, b configfile.Embedding) bool {
	return a.APIKeyValue().Value() == b.APIKeyValue().Value() && a.BaseURL == b.BaseURL && a.Model == b.Model &&
		a.Dims == b.Dims && a.MaxBatch == b.MaxBatch && a.MaxBatchChars == b.MaxBatchChars && a.MaxItemChars == b.MaxItemChars
}

// BuildEmbedder is the production Embedders.Build: an OpenAI-compatible
// embedder.
func BuildEmbedder(e configfile.Embedding) model.Embedder {
	out := model.NewOpenAIEmbedder(e.BaseURL, e.APIKeyValue().Value(), e.Model, e.Dims)
	out.MaxBatch, out.MaxBatchChars, out.MaxItemChars = e.Bounds()
	return out
}
