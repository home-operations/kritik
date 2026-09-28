package configfile

import (
	"fmt"
	"maps"
	"net/url"
	"slices"
)

// Provider is the model provider name refers to for account t: the account's
// own when it declares one by that name, else the instance's. t may be nil.
func (f *File) Provider(t *Account, name string) (Provider, bool) {
	if t != nil {
		if p, ok := t.Providers[name]; ok {
			return p, true
		}
	}
	p, ok := f.Providers[name]
	return p, ok
}

// validate checks a provider's type, endpoint, key and prices.
func (p Provider) validate(where string) error {
	if !p.Type.Valid() {
		return fmt.Errorf("configfile: %s.type must be %s, %s or %s, got %q",
			where, ProviderOpenRouter, ProviderOpenAI, ProviderAnthropic, p.Type)
	}
	if p.BaseURL != "" {
		if u, err := url.Parse(p.BaseURL); err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("configfile: %s.baseUrl %q must be an absolute URL", where, p.BaseURL)
		}
	}
	if p.apiKey.Value() == "" {
		return fmt.Errorf("configfile: %s.apiKey resolved to an empty value", where)
	}
	for _, id := range slices.Sorted(maps.Keys(p.Pricing)) {
		if price := p.Pricing[id]; price.Input < 0 || price.Output < 0 || price.CacheRead < 0 || price.CacheWrite < 0 {
			return fmt.Errorf("configfile: %s.pricing.%s: prices must not be negative", where, id)
		}
	}
	return nil
}

// validateAccountProviders checks an account's own providers: names a model
// reference can carry, none the instance's providers already use, each valid.
func (f *File) validateAccountProviders(where string, t *Account) error {
	for _, name := range slices.Sorted(maps.Keys(t.Providers)) {
		pwhere := where + ".providers." + name
		if !nameRe.MatchString(name) {
			return fmt.Errorf("configfile: %s: a provider name must be lowercase alphanumerics and hyphens, 1 to 63 characters", pwhere)
		}
		if _, ok := f.Providers[name]; ok {
			return fmt.Errorf("configfile: %s: the instance declares a provider by that name", pwhere)
		}
		if err := t.Providers[name].validate(pwhere); err != nil {
			return err
		}
	}
	return nil
}
