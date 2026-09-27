package configfile

import (
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
)

// Provider is the model provider name refers to for tenant t: the tenant's
// own when it declares one by that name, else the file's. t may be nil.
func (f *File) Provider(t *Tenant, name string) (Provider, bool) {
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
	for id, price := range p.Pricing {
		if price.Input < 0 || price.Output < 0 || price.CacheRead < 0 || price.CacheWrite < 0 {
			return fmt.Errorf("configfile: %s.pricing.%s: prices must not be negative", where, id)
		}
	}
	return nil
}

// validateTenantProviders checks a tenant's own providers: names a model
// reference can carry, none the file's providers already use, each valid.
func (f *File) validateTenantProviders(where string, t *Tenant) error {
	for _, name := range slices.Sorted(maps.Keys(t.Providers)) {
		pwhere := where + ".providers." + name
		if !nameRe.MatchString(name) {
			return fmt.Errorf("configfile: %s: a provider name must be lowercase alphanumerics and hyphens, 1 to 63 characters", pwhere)
		}
		if _, ok := f.Providers[name]; ok {
			return fmt.Errorf("configfile: %s: the file declares a provider by that name", pwhere)
		}
		if err := t.Providers[name].validate(pwhere); err != nil {
			return err
		}
	}
	return nil
}

// checkDashboardProviderHosts rejects a dashboard tenant's provider whose
// baseUrl is not https on a host the operator allows. The worker calls a
// provider from inside the cluster with its key, so a tenant admin must not
// aim it anywhere else; with no baseUrl a provider uses its type's own
// endpoint.
func (f *File) checkDashboardProviderHosts(allowed []string) error {
	for ti := range f.Tenants {
		t := &f.Tenants[ti]
		if t.Origin() != OriginDashboard {
			continue
		}
		for _, name := range slices.Sorted(maps.Keys(t.Providers)) {
			raw := t.Providers[name].BaseURL
			if raw == "" {
				continue
			}
			u, err := url.Parse(raw)
			host := ""
			if err == nil {
				host = strings.ToLower(u.Hostname())
			}
			switch {
			case err != nil || u.Scheme != "https" || (u.Port() != "" && u.Port() != "443"):
				return &MergeError{Slug: t.Slug, Err: fmt.Errorf(
					"configfile: %s.providers.%s.baseUrl: a dashboard provider's endpoint must be https on port 443", t.where(ti), name)}
			case !slices.ContainsFunc(allowed, func(h string) bool { return strings.EqualFold(h, host) }):
				return &MergeError{Slug: t.Slug, Err: fmt.Errorf("configfile: %s.providers.%s.baseUrl: %q is not an allowed dashboard provider host",
					t.where(ti), name, host)}
			}
		}
	}
	return nil
}
