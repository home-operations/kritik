package configfile

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The configuration file may set the instance's defaults (ADR-0015): model
// providers, the review and fallback models, mode, thoroughness, forks and
// settle every account and repository inherits, and the embedder. The
// instance spec overrides each: a provider by name, a default by key, and
// the embedder whole.

// fileDefaults is the part of the defaults the file sets.
type fileDefaults struct {
	Models ModelsSpec     `yaml:"models,omitempty"`
	Mode   ReviewMode     `yaml:"mode,omitempty"`
	Forks  *bool          `yaml:"forks,omitempty"`
	Settle *time.Duration `yaml:"settle,omitempty"`
	Review fileReview     `yaml:"review,omitempty"`
}

// fileReview is the part of the review block the file sets.
type fileReview struct {
	Thoroughness *string `yaml:"thoroughness,omitempty"`
}

// defaults is d as the defaults it sets.
func (d fileDefaults) defaults() Defaults {
	var out Defaults
	out.Models, out.Mode, out.Forks, out.Settle = d.Models, d.Mode, d.Forks, d.Settle
	out.Review.Thoroughness = d.Review.Thoroughness
	return out
}

// Environment variable prefixes of the file's instance defaults.
const (
	providerEnvPrefix  = "KRITIK_PROVIDERS_"
	defaultsEnvPrefix  = "KRITIK_DEFAULTS_"
	embeddingEnvPrefix = "KRITIK_EMBEDDING_"
)

// DefaultEnvProvider names the environment's provider when
// KRITIK_PROVIDERS_NAME is unset.
const DefaultEnvProvider = "openrouter"

// overlayProviderEnv declares the one provider the environment may: it
// replaces the file's provider of its name whole, or joins them. Its type
// defaults to its name when that is a provider type. It returns the
// provider's name, "" when no KRITIK_PROVIDERS_* variable is set; a
// variable that names no key, or a key set both directly and by _FILE, is
// an error.
func overlayProviderEnv(providers *map[string]Provider, environ []string) (string, error) {
	name, p := DefaultEnvProvider, Provider{}
	setBy := map[string]string{}
	for _, kv := range environ {
		env, value, _ := strings.Cut(kv, "=")
		key, ok := strings.CutPrefix(env, providerEnvPrefix)
		if !ok {
			continue
		}
		setting := strings.TrimSuffix(key, "_FILE")
		if prev, dup := setBy[setting]; dup {
			return "", fmt.Errorf("configfile: environment variables %s and %s set the same provider setting; set one", prev, env)
		}
		setBy[setting] = env
		switch key {
		case "NAME":
			name = value
		case "TYPE":
			p.Type = ProviderType(value)
		case "BASE_URL":
			p.BaseURL = value
		case "API_KEY":
			p.APIKey = SecretRef{Env: env}
		case "API_KEY_FILE":
			p.APIKey = SecretRef{File: value}
		default:
			return "", fmt.Errorf("configfile: environment variable %s names no provider setting", env)
		}
	}
	if len(setBy) == 0 {
		return "", nil
	}
	if p.Type == "" && ProviderType(name).Valid() {
		p.Type = ProviderType(name)
	}
	if *providers == nil {
		*providers = map[string]Provider{}
	}
	(*providers)[name] = p
	return name, nil
}

// overlayDefaultsEnv sets the file's defaults from KRITIK_DEFAULTS_*,
// recording each key it sets in from.
func overlayDefaultsEnv(d *fileDefaults, environ []string, from map[string]bool) error {
	for _, kv := range environ {
		env, value, _ := strings.Cut(kv, "=")
		key, ok := strings.CutPrefix(env, defaultsEnvPrefix)
		if !ok {
			continue
		}
		var path string
		switch key {
		case "MODELS_REVIEW":
			ref := ModelRef(value)
			d.Models.Review, path = &ref, "models.review"
		case "MODELS_FALLBACK":
			ref := ModelRef(value)
			d.Models.Fallback, path = &ref, "models.fallback"
		case "MODE":
			d.Mode, path = ReviewMode(value), keyMode
		case "REVIEW_THOROUGHNESS":
			d.Review.Thoroughness, path = &value, keyThoroughness
		case "FORKS":
			b, err := strconv.ParseBool(value)
			if err != nil {
				return fmt.Errorf("configfile: environment variable %s must be true or false, got %q", env, value)
			}
			d.Forks, path = &b, keyForks
		case "SETTLE":
			settle, err := time.ParseDuration(value)
			if err != nil {
				return fmt.Errorf("configfile: environment variable %s: %w", env, err)
			}
			d.Settle, path = &settle, keySettle
		default:
			return fmt.Errorf("configfile: environment variable %s names no defaults setting", env)
		}
		from[path] = true
	}
	return nil
}

// overlayEmbeddingEnv sets the file's embedder key by key from
// KRITIK_EMBEDDING_*, starting one when the file has none, and records in
// from that the environment set it.
func overlayEmbeddingEnv(e **Embedding, environ []string, from map[string]bool) error {
	setBy := map[string]string{}
	for _, kv := range environ {
		env, value, _ := strings.Cut(kv, "=")
		key, ok := strings.CutPrefix(env, embeddingEnvPrefix)
		if !ok {
			continue
		}
		setting := strings.TrimSuffix(key, "_FILE")
		if prev, dup := setBy[setting]; dup {
			return fmt.Errorf("configfile: environment variables %s and %s set the same embedding setting; set one", prev, env)
		}
		setBy[setting] = env
		if *e == nil {
			*e = &Embedding{}
		}
		switch key {
		case "BASE_URL":
			(*e).BaseURL = value
		case "API_KEY":
			(*e).APIKey = SecretRef{Env: env}
		case "API_KEY_FILE":
			(*e).APIKey = SecretRef{File: value}
		case "MODEL":
			(*e).Model = value
		case "DIMS":
			dims, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("configfile: environment variable %s must be a whole number, got %q", env, value)
			}
			(*e).Dims = dims
		default:
			return fmt.Errorf("configfile: environment variable %s names no embedding setting", env)
		}
		from["embedding"] = true
	}
	return nil
}

// resolveInstanceDefaults reads the file's provider and embedder keys,
// which are env or file references like every other secret the file holds.
func (f *File) resolveInstanceDefaults() error {
	for _, name := range slices.Sorted(maps.Keys(f.Providers)) {
		p := f.Providers[name]
		v, err := p.APIKey.resolve(fileRefs)
		if err != nil {
			return fmt.Errorf("configfile: providers.%s.apiKey: %w", name, err)
		}
		p.apiKey = v
		f.Providers[name] = p
	}
	if e := f.Embedding; e != nil {
		v, err := e.APIKey.resolve(fileRefs)
		if err != nil {
			return fmt.Errorf("configfile: embedding.apiKey: %w", err)
		}
		e.apiKey = v
	}
	return nil
}

// underSpec is the file's instance defaults with the spec s laid over them:
// its providers by name, its defaults by key, and its embedder whole.
func (f *File) underSpec(s *Spec) (map[string]Provider, Defaults, *Embedding) {
	providers := maps.Clone(f.Providers)
	if len(s.Providers) > 0 && providers == nil {
		providers = map[string]Provider{}
	}
	maps.Copy(providers, s.Providers)
	defaults := s.Defaults
	if defaults.Models.Review == nil {
		defaults.Models.Review = f.Defaults.Models.Review
	}
	if defaults.Models.Fallback == nil {
		defaults.Models.Fallback = f.Defaults.Models.Fallback
	}
	if defaults.Mode == "" {
		defaults.Mode = f.Defaults.Mode
	}
	if defaults.Forks == nil {
		defaults.Forks = f.Defaults.Forks
	}
	if defaults.Settle == nil {
		defaults.Settle = f.Defaults.Settle
	}
	if defaults.Review.Thoroughness == nil {
		defaults.Review.Thoroughness = f.Defaults.Review.Thoroughness
	}
	embedding := s.Embedding
	if embedding == nil {
		embedding = f.Embedding
	}
	return providers, defaults, embedding
}

// FileLayer is what the configuration file and its environment set of the
// instance's defaults, which the spec overrides: its providers, default
// models, other defaults and embedder, each with where it comes from.
type FileLayer struct {
	Providers map[string]FileProvider
	Review    FileValue
	Fallback  FileValue
	// Defaults are the other defaults it sets: mode, review.thoroughness,
	// forks and settle, in that order, by their policy keys.
	Defaults  []FileDefault
	Embedding *FileEmbedding
}

// FileDefault is one of the defaults the file or the environment sets
// other than the models, by its policy key.
type FileDefault struct {
	Key string
	FileValue
}

// FileProvider is one provider the file or the environment declares.
// Overridden is whether the spec declares one by its name, which runs
// instead.
type FileProvider struct {
	Type       ProviderType
	BaseURL    string
	Source     Source
	Overridden bool
}

// FileValue is a default model the file or the environment sets; the
// zero FileValue is none. Overridden is whether the spec sets it instead.
type FileValue struct {
	Value      string
	Source     Source
	Overridden bool
}

// FileEmbedding is the embedder the file or the environment sets.
// Overridden is whether the spec sets one, which runs instead.
type FileEmbedding struct {
	BaseURL, Model string
	Dims           int
	Source         Source
	Overridden     bool
}

// FileLayer returns the file's instance defaults of f, a parsed or a
// merged File, each marked overridden where f's spec sets its own.
func (f *File) FileLayer() FileLayer {
	base := f
	if f.base != nil {
		base = f.base
	}
	source := func(key string) Source {
		if base.envKeys[key] {
			return SourceEnv
		}
		return SourceFile
	}
	var out FileLayer
	for name, p := range base.Providers {
		if out.Providers == nil {
			out.Providers = map[string]FileProvider{}
		}
		src := SourceFile
		if name == base.envProvider {
			src = SourceEnv
		}
		out.Providers[name] = FileProvider{Type: p.Type, BaseURL: p.BaseURL, Source: src, Overridden: f.specProviders[name]}
	}
	if r := base.Defaults.Models.Review; r != nil {
		out.Review = FileValue{Value: string(*r), Source: source("models.review"), Overridden: f.specDefaults.Models.Review != nil}
	}
	if r := base.Defaults.Models.Fallback; r != nil {
		out.Fallback = FileValue{Value: string(*r), Source: source("models.fallback"), Overridden: f.specDefaults.Models.Fallback != nil}
	}
	d, spec := base.Defaults, f.specDefaults
	for _, x := range []struct {
		key, value string
		set, over  bool
	}{
		{keyMode, string(d.Mode), d.Mode != "", spec.Mode != ""},
		{keyThoroughness, deref(d.Review.Thoroughness), d.Review.Thoroughness != nil, spec.Review.Thoroughness != nil},
		{keyForks, strconv.FormatBool(d.Forks != nil && *d.Forks), d.Forks != nil, spec.Forks != nil},
		{keySettle, durationValue(d.Settle), d.Settle != nil, spec.Settle != nil},
	} {
		if x.set {
			v := FileValue{Value: x.value, Source: source(x.key), Overridden: x.over}
			out.Defaults = append(out.Defaults, FileDefault{Key: x.key, FileValue: v})
		}
	}
	if e := base.Embedding; e != nil {
		out.Embedding = &FileEmbedding{
			BaseURL: e.BaseURL, Model: e.Model, Dims: e.Dims, Source: source("embedding"), Overridden: f.Embedding != e,
		}
	}
	return out
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func durationValue(d *time.Duration) string {
	if d == nil {
		return ""
	}
	return d.String()
}
