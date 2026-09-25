package configfile

import (
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/home-operations/kritik/internal/egress"
)

// githubHost is where a GitHub installation without a host lives.
const githubHost = "github.com"

// validateEgress checks the allowlist entries are bare hostnames and each
// credential names a host that is allowed, explicitly or implicitly.
func (f *File) validateEgress() error {
	for i, h := range f.Egress.AllowHosts {
		if err := checkHost(h); err != nil {
			return fmt.Errorf("configfile: egress.allowHosts[%d]: %w", i, err)
		}
	}
	rules := f.EgressRules()
	for host, secret := range f.Egress.credentials {
		if err := checkHost(host); err != nil {
			return fmt.Errorf("configfile: egress.credentials.%s: %w", host, err)
		}
		if !rules.Allows(host) {
			return fmt.Errorf("configfile: egress.credentials.%s: host is not in egress.allowHosts", host)
		}
		if secret.Value() == "" {
			return fmt.Errorf("configfile: egress.credentials.%s resolved to an empty value", host)
		}
	}
	return nil
}

// checkHost accepts a lowercase hostname, optionally with a leading "*.",
// and nothing else: no scheme, port or path.
func checkHost(h string) error {
	bare := strings.TrimPrefix(h, "*.")
	if bare == "" || strings.ContainsAny(bare, "/:@ ") || strings.HasPrefix(bare, "*") || h != strings.ToLower(h) {
		return fmt.Errorf("%q must be a lowercase hostname, optionally prefixed with \"*.\"", h)
	}
	return nil
}

// EgressRules is what the gateway allows for this file: the configured
// hosts and every installation's forge host, since runners fetch from it,
// plus the credentials as Authorization header values. Provider endpoints
// are not among them: a runner reaches its model through the gateway's
// model endpoint, and the worker calls the provider (ADR-0004).
func (f *File) EgressRules() egress.Rules {
	hosts := slices.Clone(f.Egress.AllowHosts)
	add := func(h string) {
		h = strings.ToLower(h)
		if h != "" && !slices.Contains(hosts, h) {
			hosts = append(hosts, h)
		}
	}
	for _, t := range f.Tenants {
		for _, i := range t.Installations {
			switch {
			case i.Host != "":
				add(hostOf(i.Host))
			case i.Forge == ForgeGitHub:
				add(githubHost)
			}
		}
	}
	creds := make(map[string]string, len(f.Egress.credentials))
	for host, secret := range f.Egress.credentials {
		creds[host] = "Bearer " + secret.Value()
	}
	return egress.Rules{Hosts: hosts, Credentials: creds}
}

// hostOf is the hostname of a URL or a bare host, without a port.
func hostOf(s string) string {
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
