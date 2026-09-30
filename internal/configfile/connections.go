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
		where := fmt.Sprintf("apps[%d]", i)
		if !nameRe.MatchString(in.Name) {
			return fmt.Errorf("configfile: %s.name %q must be lowercase alphanumerics and hyphens, 1 to 63 characters", where, in.Name)
		}
		if prev, dup := names[in.Name]; dup {
			return fmt.Errorf("configfile: %s.name %q duplicates apps[%d]; names are hook paths and must be unique", where, in.Name, prev)
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
				return fmt.Errorf("configfile: %s.accounts[%d] %q is served by app %q already; an account is served by one app",
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
// environment's app.
const connectionEnvPrefix = "KRITIK_APPS_"

// DefaultEnvConnection names the environment's app when KRITIK_APPS_NAME
// is unset.
const DefaultEnvConnection = "github"

// overlayConnectionEnv declares the one app the environment may (ADR-0014
// §2.2): it replaces the file's app of its name whole, or joins them. It
// returns the app's name, "" when no KRITIK_APPS_* variable is set; a
// secret's variable carries the value itself (ADR-0022 §2.2). A variable
// that names no key is an error, so a typo is refused rather than ignored.
func overlayConnectionEnv(conns *[]Connection, environ []string) (string, error) {
	in := Connection{Name: DefaultEnvConnection, Forge: ForgeGitHub}
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
		case "CLIENT_ID":
			in.App.ClientID = ValueOrRef{Value: value}
		case "PRIVATE_KEY":
			in.App.PrivateKey = SecretRef{Env: name}
		case "WEBHOOK_SECRET":
			in.App.WebhookSecret = SecretRef{Env: name}
		default:
			return "", fmt.Errorf("configfile: environment variable %s names no app setting", name)
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
	return name != "" && f.envConnection == name
}
