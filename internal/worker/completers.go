package worker

import (
	"fmt"
	"sync"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/model"
)

// Completers resolves a configured provider to a model.Completer, building
// each on first use and again whenever its configuration changes.
type Completers struct {
	Build func(p configfile.Provider) (model.Completer, error)

	mu      sync.Mutex
	entries map[string]completerEntry
}

type completerEntry struct {
	spec      configfile.Provider
	completer model.Completer
}

// For returns the completer for the named provider in f.
func (c *Completers) For(f *configfile.File, name string) (model.Completer, error) {
	spec, ok := f.Providers[name]
	if !ok {
		return nil, fmt.Errorf("worker: provider %q is not in the configuration", name)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[name]; ok && sameProvider(e.spec, spec) {
		return e.completer, nil
	}
	completer, err := c.Build(spec)
	if err != nil {
		return nil, err
	}
	if c.entries == nil {
		c.entries = map[string]completerEntry{}
	}
	c.entries[name] = completerEntry{spec: spec, completer: completer}
	return completer, nil
}

func sameProvider(a, b configfile.Provider) bool {
	return a.Type == b.Type && a.BaseURL == b.BaseURL && a.APIKeyValue().Value() == b.APIKeyValue().Value()
}

// BuildCompleter constructs the adapter a provider's type selects.
func BuildCompleter(p configfile.Provider) (model.Completer, error) {
	switch p.Type {
	case configfile.ProviderOpenRouter:
		return model.NewOpenRouter(p.APIKeyValue().Value(), nil)
	case configfile.ProviderOpenAI:
		return model.NewOpenAICompatible(string(p.Type), p.BaseURL, p.APIKeyValue().Value())
	default:
		return nil, fmt.Errorf("worker: provider type %q has no adapter", p.Type)
	}
}
