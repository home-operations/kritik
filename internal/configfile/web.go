package configfile

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Web configures the dashboard: who may sign in and who of them may
// operate it.
type Web struct {
	SignIn []SignIn `yaml:"signIn,omitempty"`
	// Operators are the identities allowed to change configuration, each
	// "<signIn name>:<login or subject>" or "email:<address>".
	Operators []string `yaml:"operators,omitempty"`
	// SessionTTL is how long a dashboard session lasts; zero means
	// DefaultSessionTTL.
	SessionTTL time.Duration `yaml:"sessionTTL,omitempty"`
}

// DefaultSessionTTL applies when the file sets no sessionTTL.
const DefaultSessionTTL = 12 * time.Hour

// Bounds on a configured sessionTTL.
const (
	minSessionTTL = 5 * time.Minute
	maxSessionTTL = 30 * 24 * time.Hour
)

// operatorEmail prefixes an operator matched by email address; a sign-in may
// not take it as its name.
const operatorEmail = "email"

// SignInType selects how a sign-in authenticates.
type SignInType string

// Sign-in types.
const (
	SignInOIDC    SignInType = "oidc"
	SignInGitHub  SignInType = "github"
	SignInForgejo SignInType = "forgejo"
)

// Valid reports whether s is a sign-in type.
func (s SignInType) Valid() bool { return s == SignInOIDC || s == SignInGitHub || s == SignInForgejo }

func (s SignInType) String() string { return string(s) }

// SignIn is one way to sign in to the dashboard.
type SignIn struct {
	Name string     `yaml:"name"`
	Type SignInType `yaml:"type"`
	// Issuer is the OIDC discovery issuer URL, https only.
	Issuer string `yaml:"issuer,omitempty"`
	// Host is the forge for github, github.com when unset, and forgejo,
	// where it is required.
	Host         string    `yaml:"host,omitempty"`
	ClientID     string    `yaml:"clientId"`
	ClientSecret SecretRef `yaml:"clientSecret"`
	Scopes       []string  `yaml:"scopes,omitempty"`

	clientSecret Secret
}

// ClientSecretValue returns the resolved client secret.
func (s SignIn) ClientSecretValue() Secret { return s.clientSecret }

// SessionTTLOrDefault returns the session lifetime or its default.
func (w Web) SessionTTLOrDefault() time.Duration {
	if w.SessionTTL > 0 {
		return w.SessionTTL
	}
	return DefaultSessionTTL
}

// SignInByName returns the sign-in with the given name.
func (w Web) SignInByName(name string) (SignIn, bool) {
	for _, s := range w.SignIn {
		if s.Name == name {
			return s, true
		}
	}
	return SignIn{}, false
}

func (w *Web) resolve() error {
	for i := range w.SignIn {
		s := &w.SignIn[i]
		if s.Type == SignInGitHub && s.Host == "" {
			s.Host = githubHost
		}
		v, err := s.ClientSecret.resolve(fileRefs)
		if err != nil {
			return fmt.Errorf("configfile: web.signIn[%d].clientSecret: %w", i, err)
		}
		s.clientSecret = v
	}
	return nil
}

func (w Web) validate() error {
	names := map[string]int{}
	for i, s := range w.SignIn {
		where := fmt.Sprintf("web.signIn[%d]", i)
		if !nameRe.MatchString(s.Name) {
			return fmt.Errorf("configfile: %s.name %q must be lowercase alphanumerics and hyphens, 1 to 63 characters", where, s.Name)
		}
		if s.Name == operatorEmail {
			return fmt.Errorf("configfile: %s.name %q is reserved for email operators", where, s.Name)
		}
		if prev, dup := names[s.Name]; dup {
			return fmt.Errorf("configfile: %s.name %q duplicates web.signIn[%d]", where, s.Name, prev)
		}
		names[s.Name] = i
		if err := s.validate(where); err != nil {
			return err
		}
	}
	for i, op := range w.Operators {
		kind, subject, ok := strings.Cut(op, ":")
		_, known := names[kind]
		if !ok || subject == "" || (kind != operatorEmail && !known) {
			return fmt.Errorf("configfile: web.operators[%d] %q must be \"email:<address>\" or \"<signIn name>:<login or subject>\"", i, op)
		}
	}
	if w.SessionTTL != 0 && (w.SessionTTL < minSessionTTL || w.SessionTTL > maxSessionTTL) {
		return fmt.Errorf("configfile: web.sessionTTL must be between %s and %s", minSessionTTL, maxSessionTTL)
	}
	return nil
}

func (s SignIn) validate(where string) error {
	switch s.Type {
	case SignInOIDC:
		if u, err := url.Parse(s.Issuer); err != nil || u.Scheme != "https" || u.Host == "" {
			return fmt.Errorf("configfile: %s.issuer %q must be an https URL", where, s.Issuer)
		}
		if s.Host != "" {
			return fmt.Errorf("configfile: %s.host is for github and forgejo sign-ins; oidc takes an issuer", where)
		}
	case SignInGitHub, SignInForgejo:
		if s.Issuer != "" {
			return fmt.Errorf("configfile: %s.issuer is for oidc sign-ins; %s takes a host", where, s.Type)
		}
		if s.Host == "" {
			return fmt.Errorf("configfile: %s.host is required for a %s sign-in", where, s.Type)
		}
		if hostOf(s.Host) == "" {
			return fmt.Errorf("configfile: %s.host %q is not a host or URL", where, s.Host)
		}
	default:
		return fmt.Errorf("configfile: %s.type must be %s, %s or %s, got %q", where, SignInOIDC, SignInGitHub, SignInForgejo, s.Type)
	}
	if s.ClientID == "" {
		return fmt.Errorf("configfile: %s.clientId is required", where)
	}
	if s.clientSecret.Value() == "" {
		return fmt.Errorf("configfile: %s.clientSecret resolved to an empty value", where)
	}
	return nil
}
