package worker

import (
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
			if _, err := c.Stepper(f, "p"); err != nil {
				t.Fatal(err)
			}
			// A second lookup is served from the cache.
			if _, err := c.Stepper(f, "p"); err != nil {
				t.Fatal(err)
			}
			if builds != tt.wantBuilds {
				t.Fatalf("builds = %d, want %d", builds, tt.wantBuilds)
			}
		})
	}
	if _, err := c.Stepper(&configfile.File{}, "p"); err == nil {
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
