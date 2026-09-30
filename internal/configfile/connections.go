package configfile

import (
	"fmt"
	"strings"
)

// Origin is where a connection is declared.
type Origin string

// OriginFile is the configuration file or its environment, which declare
// every connection (ADR-0019).
const OriginFile Origin = "file"

// Origin reports where the connection is declared.
func (i *Connection) Origin() Origin { return OriginFile }

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
// error, so a typo is refused rather than ignored, and so is a secret set
// both directly and by _FILE, since environ's order would pick one.
func overlayConnectionEnv(conns *[]Connection, environ []string) (string, error) {
	in := Connection{Name: DefaultEnvConnection, Forge: ForgeGitHub, App: &GitHubApp{}}
	setBy := map[string]string{}
	for _, kv := range environ {
		name, value, _ := strings.Cut(kv, "=")
		key, ok := strings.CutPrefix(name, connectionEnvPrefix)
		if !ok {
			continue
		}
		setting := strings.TrimSuffix(key, "_FILE")
		if prev, dup := setBy[setting]; dup {
			return "", fmt.Errorf("configfile: environment variables %s and %s set the same connection setting; set one", prev, name)
		}
		setBy[setting] = name
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
	if len(setBy) == 0 {
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
	return name != "" && f.envConnection == name
}
