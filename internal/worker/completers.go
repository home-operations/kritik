package worker

import (
	"fmt"
	"maps"
	"sync"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/model"
)

// Completers resolves a configured provider to its model adapter, building
// each on first use and again whenever its configuration changes. The
// review and follow-up workers wrap its steppers in a model.Structured,
// the gateway calls them directly.
type Completers struct {
	Build func(p configfile.Provider) (model.Stepper, error)

	mu      sync.Mutex
	entries map[string]stepperEntry
}

type stepperEntry struct {
	spec    configfile.Provider
	stepper model.Stepper
}

// Stepper returns the adapter for the named provider in f.
func (c *Completers) Stepper(f *configfile.File, name string) (model.Stepper, error) {
	spec, ok := f.Providers[name]
	if !ok {
		return nil, fmt.Errorf("worker: provider %q is not in the configuration", name)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[name]; ok && sameProvider(e.spec, spec) {
		return e.stepper, nil
	}
	stepper, err := c.Build(spec)
	if err != nil {
		return nil, err
	}
	if c.entries == nil {
		c.entries = map[string]stepperEntry{}
	}
	c.entries[name] = stepperEntry{spec: spec, stepper: stepper}
	return stepper, nil
}

func sameProvider(a, b configfile.Provider) bool {
	return a.Type == b.Type && a.BaseURL == b.BaseURL && a.APIKeyValue().Value() == b.APIKeyValue().Value() &&
		maps.Equal(a.Pricing, b.Pricing)
}

// BuildStepper constructs the adapter a provider's type selects.
func BuildStepper(p configfile.Provider) (model.Stepper, error) {
	s, err := model.NewStepper(p.Type, p.BaseURL, p.APIKeyValue().Value(), p.Pricing, nil)
	if err != nil {
		return nil, fmt.Errorf("worker: %w", err)
	}
	return s, nil
}
