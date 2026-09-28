package worker

import (
	"slices"
	"testing"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/model"
)

func TestCompletersRebuildOnChange(t *testing.T) {
	builds := 0
	c := &Completers{Build: func(configfile.Provider) (model.Stepper, error) {
		builds++
		return &model.OpenAI{}, nil
	}}
	base := configfile.Provider{Type: configfile.ProviderAnthropic, Pricing: model.Pricing{"acme-large": {Input: 3}}}
	tests := []struct {
		name       string
		provider   configfile.Provider
		wantBuilds int
	}{
		{"first use builds", base, 1},
		{"same configuration is cached", configfile.Provider{Type: base.Type, Pricing: model.Pricing{"acme-large": {Input: 3}}}, 1},
		{"new pricing rebuilds", configfile.Provider{Type: base.Type, Pricing: model.Pricing{"acme-large": {Input: 4}}}, 2},
		{"new base URL rebuilds", configfile.Provider{Type: base.Type, BaseURL: "https://gw.example.com/", Pricing: model.Pricing{"acme-large": {Input: 4}}}, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &configfile.File{Providers: map[string]configfile.Provider{"p": tt.provider}}
			if _, err := c.Stepper(f, nil, "p"); err != nil {
				t.Fatal(err)
			}
			// A second lookup is served from the cache.
			if _, err := c.Stepper(f, nil, "p"); err != nil {
				t.Fatal(err)
			}
			if builds != tt.wantBuilds {
				t.Fatalf("builds = %d, want %d", builds, tt.wantBuilds)
			}
		})
	}
	if _, err := c.Stepper(&configfile.File{}, nil, "p"); err == nil {
		t.Fatal("an undeclared provider must be an error")
	}
}

// TestCompletersAccountProviders: an account's own provider is its, even when
// another account's has the same name, and the file's providers stay
// shared.
func TestCompletersAccountProviders(t *testing.T) {
	var built []configfile.ProviderType
	c := &Completers{Build: func(p configfile.Provider) (model.Stepper, error) {
		built = append(built, p.Type)
		return &model.OpenAI{}, nil
	}}
	f := &configfile.File{Providers: map[string]configfile.Provider{"shared": {Type: configfile.ProviderOpenRouter}}}
	alpha := &configfile.Account{Slug: "alpha", Providers: map[string]configfile.Provider{"own": {Type: configfile.ProviderOpenAI}}}
	beta := &configfile.Account{Slug: "beta", Providers: map[string]configfile.Provider{"own": {Type: configfile.ProviderAnthropic}}}
	for _, call := range []struct {
		t    *configfile.Account
		name string
	}{{alpha, "own"}, {beta, "own"}, {alpha, "shared"}, {beta, "shared"}, {alpha, "own"}} {
		if _, err := c.Stepper(f, call.t, call.name); err != nil {
			t.Fatal(err)
		}
	}
	want := []configfile.ProviderType{configfile.ProviderOpenAI, configfile.ProviderAnthropic, configfile.ProviderOpenRouter}
	if !slices.Equal(built, want) {
		t.Fatalf("built %v, want %v: one per account's own provider and one shared", built, want)
	}
	if _, err := c.Stepper(f, beta, "missing"); err == nil {
		t.Fatal("an undeclared provider must be an error")
	}
}

func TestBuildStepper(t *testing.T) {
	for _, typ := range []configfile.ProviderType{configfile.ProviderOpenRouter, configfile.ProviderOpenAI, configfile.ProviderAnthropic} {
		t.Run(string(typ), func(t *testing.T) {
			if _, err := BuildStepper(configfile.Provider{Type: typ}); err == nil {
				t.Fatal("a provider without a key must not build")
			}
		})
	}
}
