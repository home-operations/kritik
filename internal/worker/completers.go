package worker

import (
	"fmt"
	"sync"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/model"
)

// Completers resolves a configured provider to its model adapter, built on
// first use and kept for the life of the process, as the configuration is
// (ADR-0022 §2.1). The follow-up worker wraps its steppers in a
// model.Structured; the gateway calls them directly.
type Completers struct {
	Build func(p configfile.Provider) (model.Stepper, error)

	mu       sync.Mutex
	steppers map[string]model.Stepper
}

// Stepper returns the adapter for the named provider of account t in f: the
// account's own when it declares one by that name, else the file's.
func (c *Completers) Stepper(f *configfile.File, t *configfile.Account, name string) (model.Stepper, error) {
	spec, ok := f.Provider(t, name)
	if !ok {
		return nil, fmt.Errorf("worker: provider %q is not in the configuration", name)
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
		return nil, fmt.Errorf("worker: %w", err)
	}
	return s, nil
}
