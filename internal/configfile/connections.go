package configfile

import (
	"fmt"
	"strings"
)

// Origin is where a connection is declared.
type Origin string

// Connection origins.
const (
	OriginFile      Origin = "file"
	OriginDashboard Origin = "dashboard"
)

// Valid reports whether o is an origin.
func (o Origin) Valid() bool { return o == OriginFile || o == OriginDashboard }

func (o Origin) String() string { return string(o) }

// Origin reports where the connection is declared: the configuration file
// or its environment, or the dashboard.
func (i *Connection) Origin() Origin {
	if i.origin == "" {
		return OriginFile
	}
	return i.origin
}

// validateConnections checks one layer's connections: names that are hook
// paths, unique; accounts listed; credentials that resolved; and every
// account served by one connection alone.
func validateConnections(conns []Connection) error {
	names := map[string]int{}
	served := map[string]string{}
	for i := range conns {
		in := &conns[i]
		where := fmt.Sprintf("connections[%d]", i)
		if !nameRe.MatchString(in.Name) {
			return fmt.Errorf("configfile: %s.name %q must be lowercase alphanumerics and hyphens, 1 to 63 characters", where, in.Name)
		}
		if prev, dup := names[in.Name]; dup {
			return fmt.Errorf("configfile: %s.name %q duplicates connections[%d]; names are hook paths and must be unique", where, in.Name, prev)
		}
		names[in.Name] = i
		if err := validateAccountNames(in.Accounts, where); err != nil {
			return err
		}
		if err := in.validate(where); err != nil {
			return err
		}
		for ai, a := range in.Accounts {
			key := AccountKey(in.Forge, a)
			if other, dup := served[key]; dup {
				return fmt.Errorf("configfile: %s.accounts[%d] %q is served by connection %q already; an account is served by one connection",
					where, ai, a, other)
			}
			served[key] = in.Name
		}
	}
	return nil
}

// ValidConnectionName reports whether name may name a connection: it is
// the connection's hook path segment.
func ValidConnectionName(name string) bool { return nameRe.MatchString(name) }

// clash says which of held, the names and account keys the spec's
// connections hold, keeps the file's connection in from running, or ""
// when none does.
func (i *Connection) clash(held map[string]string) string {
	if d, ok := held["connection "+i.Name]; ok {
		return fmt.Sprintf("the dashboard's connection %q already holds the name", d)
	}
	for _, a := range i.Accounts {
		if d, ok := held["account "+AccountKey(i.Forge, a)]; ok {
			return fmt.Sprintf("the dashboard's connection %q already serves account %q", d, a)
		}
	}
	return ""
}

// connectionEnvPrefix starts every environment variable that declares the
// environment's connection.
const connectionEnvPrefix = "KRITIK_CONNECTIONS_"

// DefaultEnvConnection names the environment's connection when
// KRITIK_CONNECTIONS_NAME is unset.
const DefaultEnvConnection = "github"

// overlayConnectionEnv declares the one connection the environment may
// (ADR-0014 §2.2): it replaces the file's connection of its name whole, or
// joins them. It returns the connection's name, "" when no
// KRITIK_CONNECTIONS_* variable is set. A variable that names no key is an
// error, so a typo is refused rather than ignored.
func overlayConnectionEnv(conns *[]Connection, environ []string) (string, error) {
	in := Connection{Name: DefaultEnvConnection, Forge: ForgeGitHub, App: &GitHubApp{}}
	set := false
	for _, kv := range environ {
		name, value, _ := strings.Cut(kv, "=")
		key, ok := strings.CutPrefix(name, connectionEnvPrefix)
		if !ok {
			continue
		}
		set = true
		switch key {
		case "NAME":
			in.Name = value
		case "ACCOUNTS":
			in.Accounts = envList(value)
		case "APP_CLIENT_ID":
			in.App.ClientID = value
		case "APP_PRIVATE_KEY":
			in.App.PrivateKey = SecretRef{Env: name}
		case "APP_PRIVATE_KEY_FILE":
			in.App.PrivateKey = SecretRef{File: value}
		case "APP_WEBHOOK_SECRET":
			in.App.WebhookSecret = SecretRef{Env: name}
		case "APP_WEBHOOK_SECRET_FILE":
			in.App.WebhookSecret = SecretRef{File: value}
		default:
			return "", fmt.Errorf("configfile: environment variable %s names no connection setting", name)
		}
	}
	if !set {
		return "", nil
	}
	for i := range *conns {
		if (*conns)[i].Name == in.Name {
			(*conns)[i] = in
			return in.Name, nil
		}
	}
	*conns = append(*conns, in)
	return in.Name, nil
}

// ConnectionFromEnv reports whether the environment declared the
// connection named name.
func (f *File) ConnectionFromEnv(name string) bool {
	if f.base != nil {
		f = f.base
	}
	return name != "" && f.envConnection == name
}
