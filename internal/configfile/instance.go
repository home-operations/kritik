package configfile

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The environment may set the instance's defaults key by key (ADR-0015
// §2): one model provider, the review and fallback models, mode,
// feedback, forks and settle every account and repository inherits, and
// the embedder. Each wins over the file's.

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
func overlayDefaultsEnv(d *Defaults, environ []string, from map[string]bool) error {
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
		case "FEEDBACK":
			d.Review.Feedback, path = &value, keyFeedback
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
		case "MODEL":
			(*e).Ref = ModelRef(value)
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

// FileLayer is what the configuration file and its environment set of the
// instance's defaults: its providers, default models, other defaults and
// embedder, each with where it comes from.
type FileLayer struct {
	Providers map[string]FileProvider
	Review    FileValue
	Fallback  FileValue
	// Defaults are the other defaults it sets: mode, feedback, forks
	// and settle, in that order, by their policy keys.
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
type FileProvider struct {
	Type    ProviderType
	BaseURL string
	Source  Source
}

// FileValue is a default the file or the environment sets; the zero
// FileValue is none.
type FileValue struct {
	Value  string
	Source Source
}

// FileEmbedding is the embedder the file or the environment sets.
type FileEmbedding struct {
	Model  string
	Dims   int
	Source Source
}

// FileLayer returns f's instance defaults, each with where it comes from.
func (f *File) FileLayer() FileLayer {
	source := func(key string) Source {
		if f.envKeys[key] {
			return SourceEnv
		}
		return SourceFile
	}
	var out FileLayer
	for name, p := range f.Providers {
		if out.Providers == nil {
			out.Providers = map[string]FileProvider{}
		}
		src := SourceFile
		if name == f.envProvider {
			src = SourceEnv
		}
		out.Providers[name] = FileProvider{Type: p.Type, BaseURL: p.BaseURL, Source: src}
	}
	if r := f.Defaults.Models.Review; r != nil {
		out.Review = FileValue{Value: string(*r), Source: source("models.review")}
	}
	if r := f.Defaults.Models.Fallback; r != nil {
		out.Fallback = FileValue{Value: string(*r), Source: source("models.fallback")}
	}
	d := f.Defaults
	for _, x := range []struct {
		key, value string
		set        bool
	}{
		{keyMode, string(d.Mode), d.Mode != ""},
		{keyFeedback, deref(d.Review.Feedback), d.Review.Feedback != nil},
		{keyForks, strconv.FormatBool(d.Forks != nil && *d.Forks), d.Forks != nil},
		{keySettle, durationValue(d.Settle), d.Settle != nil},
	} {
		if x.set {
			out.Defaults = append(out.Defaults, FileDefault{Key: x.key, Value: x.value, Source: source(x.key)})
		}
	}
	if e := f.Embedding; e != nil {
		out.Embedding = &FileEmbedding{Model: string(e.Ref), Dims: e.Dims, Source: source("embedding")}
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
