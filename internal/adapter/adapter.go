// Package adapter resolves the configuration's providers to model adapters,
// built once for the life of the process, as the configuration is
// (ADR-0022 §2.1), and records what they are asked for the transcript
// view. The workers and the gateway share it.
package adapter

import (
	"fmt"
	"sync"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/model"
)

// Steppers resolves a configured provider to its model adapter, built on
// first use and kept.
type Steppers struct {
	Build func(p configfile.Provider) (model.Stepper, error)

	mu       sync.Mutex
	steppers map[string]model.Stepper
}

// Stepper returns the adapter for the named provider of account t in f: the
// account's own when it declares one by that name, else the file's.
func (c *Steppers) Stepper(f *configfile.File, t *configfile.Account, name string) (model.Stepper, error) {
	spec, ok := f.Provider(t, name)
	if !ok {
		return nil, fmt.Errorf("adapter: provider %q is not in the configuration", name)
	}
	// Two accounts may each name a provider of their own alike.
	key := name
	if t != nil {
		if _, own := t.Providers[name]; own {
			key = t.Key() + "\x00" + name
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.steppers[key]; ok {
		return s, nil
	}
	stepper, err := c.Build(spec)
	if err != nil {
		return nil, err
	}
	if c.steppers == nil {
		c.steppers = map[string]model.Stepper{}
	}
	c.steppers[key] = stepper
	return stepper, nil
}

// BuildStepper constructs the adapter a provider's type selects.
func BuildStepper(p configfile.Provider) (model.Stepper, error) {
	s, err := model.NewStepper(p.Type, p.BaseURL, p.APIKeyValue().Value(), p.Pricing, nil)
	if err != nil {
		return nil, fmt.Errorf("adapter: %w", err)
	}
	return s, nil
}

// Embedders resolves the configuration's embedder, built on first use and
// kept. A nil *Embedders resolves none.
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

// Outcome names how a call ended for the metrics: ok or error.
func Outcome(err error) string {
	if err != nil {
		return "error"
	}
	return "ok"
}

// ServedRef is the model that answered a call made to ref, as a model
// reference, for metrics: OpenRouter's server-side fallback may answer with
// a model other than the one asked for, and served names it, "" for none.
func ServedRef(ref configfile.ModelRef, served string) string {
	if served == "" {
		return string(ref)
	}
	return ref.Provider() + "/" + served
}
