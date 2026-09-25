package configfile

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"go.yaml.in/yaml/v3"

	"github.com/home-operations/kritik/internal/prfilter"
)

// nameRe bounds installation and tenant names to what is safe in a URL path
// segment, a Kubernetes label value and a log line.
var nameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// Load reads, decodes, resolves and validates the file at name.
func Load(name string) (*File, error) {
	raw, err := os.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("configfile: %w", err)
	}
	return Parse(raw)
}

// Parse is Load for bytes already in hand.
func Parse(raw []byte) (*File, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var f File
	if err := dec.Decode(&f); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("configfile: file is empty")
		}
		return nil, fmt.Errorf("configfile: parse: %w", err)
	}
	if err := f.resolve(); err != nil {
		return nil, err
	}
	if err := f.validate(); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	f.hash = hex.EncodeToString(sum[:])
	return &f, nil
}

// resolve reads every secret reference into memory and compiles every filter.
func (f *File) resolve() error {
	for name, p := range f.Providers {
		v, err := p.APIKey.resolve()
		if err != nil {
			return fmt.Errorf("configfile: providers.%s.apiKey: %w", name, err)
		}
		p.apiKey = v
		f.Providers[name] = p
	}

	prg, err := compileFilter(f.Defaults.Filter)
	if err != nil {
		return fmt.Errorf("configfile: defaults.filter: %w", err)
	}
	f.defaultFilter = prg

	for ti := range f.Tenants {
		t := &f.Tenants[ti]
		if t.filter, err = compileFilter(t.Filter); err != nil {
			return fmt.Errorf("configfile: tenants[%d].filter: %w", ti, err)
		}
		for ii := range t.Installations {
			in := &t.Installations[ii]
			where := fmt.Sprintf("tenants[%d].installations[%d]", ti, ii)
			if in.App != nil {
				in.App.clientID = in.App.ClientID
				if !in.App.ClientIDFrom.empty() {
					v, err := in.App.ClientIDFrom.resolve()
					if err != nil {
						return fmt.Errorf("configfile: %s.app.clientIdFrom: %w", where, err)
					}
					in.App.clientID = v.Value()
				}
				if in.App.privateKey, err = in.App.PrivateKey.resolve(); err != nil {
					return fmt.Errorf("configfile: %s.app.privateKey: %w", where, err)
				}
				if in.App.webhookSecret, err = in.App.WebhookSecret.resolve(); err != nil {
					return fmt.Errorf("configfile: %s.app.webhookSecret: %w", where, err)
				}
			}
			if !in.Token.empty() {
				if in.token, err = in.Token.resolve(); err != nil {
					return fmt.Errorf("configfile: %s.token: %w", where, err)
				}
			}
			if !in.WebhookSecret.empty() {
				if in.webhookSecret, err = in.WebhookSecret.resolve(); err != nil {
					return fmt.Errorf("configfile: %s.webhookSecret: %w", where, err)
				}
			}
		}
		for ri := range t.Repositories {
			r := &t.Repositories[ri]
			if r.filter, err = compileFilter(r.Filter); err != nil {
				return fmt.Errorf("configfile: tenants[%d].repositories[%d].filter: %w", ti, ri, err)
			}
		}
	}
	return nil
}

// validate checks every invariant the rest of kritik relies on.
func (f *File) validate() error {
	if err := f.validateProviders(); err != nil {
		return err
	}
	if err := f.checkModels("defaults.models", f.Defaults.Models); err != nil {
		return err
	}
	return f.validateTenants()
}

func (f *File) validateProviders() error {
	for name, p := range f.Providers {
		if !p.Type.Valid() {
			return fmt.Errorf("configfile: providers.%s.type must be %s, %s or %s, got %q",
				name, ProviderOpenRouter, ProviderOpenAI, ProviderAnthropic, p.Type)
		}
		if p.BaseURL != "" {
			if u, err := url.Parse(p.BaseURL); err != nil || u.Scheme == "" || u.Host == "" {
				return fmt.Errorf("configfile: providers.%s.baseUrl %q must be an absolute URL", name, p.BaseURL)
			}
		}
		if p.apiKey.Value() == "" {
			return fmt.Errorf("configfile: providers.%s.apiKey resolved to an empty value", name)
		}
		for id, price := range p.Pricing {
			if price.Input < 0 || price.Output < 0 || price.CacheRead < 0 || price.CacheWrite < 0 {
				return fmt.Errorf("configfile: providers.%s.pricing.%s: prices must not be negative", name, id)
			}
		}
	}
	return nil
}

func (f *File) validateTenants() error {
	if err := checkLimits("defaults.limits", f.Defaults.Limits); err != nil {
		return err
	}
	if f.Retention.DisabledIndexGrace < 0 {
		return errors.New("configfile: retention.disabledIndexGrace must not be negative")
	}

	if len(f.Tenants) == 0 {
		return errors.New("configfile: tenants must list at least one tenant")
	}
	slugs := map[string]int{}
	installations := map[string]string{}
	for ti, t := range f.Tenants {
		where := fmt.Sprintf("tenants[%d]", ti)
		if !nameRe.MatchString(t.Slug) {
			return fmt.Errorf("configfile: %s.slug %q must be lowercase alphanumerics and hyphens, 1 to 63 characters", where, t.Slug)
		}
		if prev, dup := slugs[t.Slug]; dup {
			return fmt.Errorf("configfile: %s.slug %q duplicates tenants[%d]", where, t.Slug, prev)
		}
		slugs[t.Slug] = ti
		if err := f.checkModels(where+".models", t.Models); err != nil {
			return err
		}
		if err := checkLimits(where+".limits", t.Limits); err != nil {
			return err
		}
		if t.Runner != nil && t.Runner.ActiveDeadlineSeconds < 0 {
			return fmt.Errorf("configfile: %s.runner.activeDeadlineSeconds must not be negative", where)
		}
		if len(t.Installations) == 0 {
			return fmt.Errorf("configfile: %s (%s) must list at least one installation", where, t.Slug)
		}
		for ii, in := range t.Installations {
			iwhere := fmt.Sprintf("%s.installations[%d]", where, ii)
			if !nameRe.MatchString(in.Name) {
				return fmt.Errorf("configfile: %s.name %q must be lowercase alphanumerics and hyphens, 1 to 63 characters", iwhere, in.Name)
			}
			if owner, dup := installations[in.Name]; dup {
				return fmt.Errorf("configfile: %s.name %q duplicates an installation in tenant %q; names are hook paths and must be unique",
					iwhere, in.Name, owner)
			}
			installations[in.Name] = t.Slug
			if in.Account == "" {
				return fmt.Errorf("configfile: %s.account is required", iwhere)
			}
			if err := in.validate(iwhere); err != nil {
				return err
			}
		}
		repos := map[string]int{}
		for ri, r := range t.Repositories {
			rwhere := fmt.Sprintf("%s.repositories[%d]", where, ri)
			if r.Name == "" || !strings.Contains(r.Name, "/") {
				return fmt.Errorf("configfile: %s.name must be \"owner/repo\", got %q", rwhere, r.Name)
			}
			if prev, dup := repos[r.Name]; dup {
				return fmt.Errorf("configfile: %s.name %q duplicates repositories[%d]", rwhere, r.Name, prev)
			}
			repos[r.Name] = ri
			if f.InstallationFor(&t, r.Name) == nil {
				owner, _, _ := strings.Cut(r.Name, "/")
				return fmt.Errorf("configfile: %s.name %q: no installation in tenant %q has account %q", rwhere, r.Name, t.Slug, owner)
			}
			for gi, g := range r.Ignore {
				if !doublestar.ValidatePattern(g) || strings.TrimSpace(g) == "" {
					return fmt.Errorf("configfile: %s.ignore[%d] %q is not a valid glob", rwhere, gi, g)
				}
			}
		}
	}
	return nil
}

func (in Installation) validate(where string) error {
	switch in.Forge {
	case ForgeGitHub:
		if in.App == nil {
			return fmt.Errorf("configfile: %s: a github installation needs an app", where)
		}
		if !in.Token.empty() || !in.WebhookSecret.empty() {
			return fmt.Errorf("configfile: %s: a github installation takes app credentials, not token or webhookSecret", where)
		}
		if (in.App.ClientID == "") == in.App.ClientIDFrom.empty() {
			return fmt.Errorf("configfile: %s.app: set exactly one of clientId or clientIdFrom", where)
		}
		if in.App.clientID == "" {
			return fmt.Errorf("configfile: %s.app.clientIdFrom resolved to an empty value", where)
		}
		if in.App.privateKey.Value() == "" {
			return fmt.Errorf("configfile: %s.app.privateKey is required", where)
		}
		if in.App.webhookSecret.Value() == "" {
			return fmt.Errorf("configfile: %s.app.webhookSecret is required", where)
		}
	case ForgeGitLab, ForgeForgejo:
		if in.App != nil {
			return fmt.Errorf("configfile: %s: a %s installation takes a token, not an app", where, in.Forge)
		}
		if in.token.Value() == "" {
			return fmt.Errorf("configfile: %s.token is required", where)
		}
		if in.webhookSecret.Value() == "" {
			return fmt.Errorf("configfile: %s.webhookSecret is required", where)
		}
	default:
		return fmt.Errorf("configfile: %s.forge must be %s, %s or %s, got %q", where, ForgeGitHub, ForgeGitLab, ForgeForgejo, in.Forge)
	}
	return nil
}

func (f *File) checkModels(where string, m Models) error {
	for role, ref := range map[string]ModelRef{"review": m.Review, "fallback": m.Fallback} {
		if ref == "" {
			continue
		}
		p := ref.Provider()
		if p == "" || ref.Model() == "" {
			return fmt.Errorf("configfile: %s.%s must be \"<provider>/<model>\", got %q", where, role, ref)
		}
		if _, ok := f.Providers[p]; !ok {
			return fmt.Errorf("configfile: %s.%s references provider %q, which is not declared under providers", where, role, p)
		}
	}
	return nil
}

func checkLimits(where string, l Limits) error {
	if l.Concurrency < 0 || l.ReviewsPerDay < 0 || l.TokensPerMonth < 0 {
		return fmt.Errorf("configfile: %s: limits must not be negative", where)
	}
	return nil
}

// compileFilter compiles a CEL filter and smoke-tests it against a sample PR,
// so a filter that type-checks but fails at runtime (a field of the wrong
// type, a non-boolean result) is caught at load rather than on the first
// webhook. An empty filter compiles to nil, meaning "no restriction".
func compileFilter(expr string) (*prfilter.Program, error) {
	if strings.TrimSpace(expr) == "" {
		return nil, nil
	}
	prg, err := prfilter.Compile(expr)
	if err != nil {
		return nil, err
	}
	if _, err := prg.Eval(SamplePR()); err != nil {
		return nil, fmt.Errorf("smoke test against a sample pull request: %w", err)
	}
	return prg, nil
}

// SamplePR is the pull request every filter is evaluated against at load. It
// is also the documented shape of the `pr` variable: every key here is always
// present at runtime.
func SamplePR() map[string]any {
	return map[string]any{
		"number":    1,
		"title":     "feat: sample",
		"author":    "octocat",
		"state":     "open",
		"open":      true,
		"merged":    false,
		"draft":     false,
		"fork":      false,
		"headRef":   "feature",
		"headSha":   "0000000",
		"baseRef":   "main",
		"url":       "https://example.invalid/pull/1",
		"createdAt": time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		"labels":    []any{map[string]any{"name": "sample", "color": "ffffff"}},
		"body":      "sample body",
	}
}

func (r SecretRef) empty() bool { return r.Env == "" && r.File == "" }

// resolve reads the referenced value. Exactly one of env or file must be set;
// an unset variable or an unreadable file is an error, never an empty value,
// so a typo cannot silently disable authentication.
func (r SecretRef) resolve() (Secret, error) {
	switch {
	case r.Env != "" && r.File != "":
		return Secret{}, errors.New("set either env or file, not both")
	case r.Env != "":
		v, ok := os.LookupEnv(r.Env)
		if !ok {
			return Secret{}, fmt.Errorf("environment variable %s is not set", r.Env)
		}
		return Secret{value: strings.TrimRight(v, "\r\n")}, nil
	case r.File != "":
		b, err := os.ReadFile(r.File)
		if err != nil {
			return Secret{}, err
		}
		return Secret{value: strings.TrimRight(string(b), "\r\n")}, nil
	default:
		return Secret{}, errors.New("reference must set env or file")
	}
}
