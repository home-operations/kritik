package configfile

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"

	"go.yaml.in/yaml/v3"
)

// Origin is where a tenant is declared.
type Origin string

// Tenant origins.
const (
	OriginFile      Origin = "file"
	OriginDashboard Origin = "dashboard"
)

// Valid reports whether o is an origin.
func (o Origin) Valid() bool { return o == OriginFile || o == OriginDashboard }

func (o Origin) String() string { return string(o) }

// Origin reports where the tenant is declared: the operator's file, or the
// dashboard.
func (t *Tenant) Origin() Origin {
	if t.origin == "" {
		return OriginFile
	}
	return t.origin
}

// where names the tenant in an error: its index for a file tenant, its slug
// for a dashboard one, whose index is an artefact of merging.
func (t *Tenant) where(index int) string {
	if t.Origin() == OriginDashboard {
		return "dashboard[" + t.Slug + "]"
	}
	return fmt.Sprintf("tenants[%d]", index)
}

// DashboardTenant is a tenant the dashboard manages: its spec is the JSON
// form of a tenant entry in the file, and Revision increments on each
// write.
type DashboardTenant struct {
	Slug     string
	Spec     json.RawMessage
	Revision int64
}

// MergeError is a dashboard tenant that failed to decode, resolve or
// validate against the file.
type MergeError struct {
	Slug string
	Err  error
}

func (e *MergeError) Error() string { return fmt.Sprintf("dashboard tenant %q: %v", e.Slug, e.Err) }

func (e *MergeError) Unwrap() error { return e.Err }

// DecodeTenant strictly decodes a dashboard tenant's spec, rejecting unknown
// keys as Parse does, and checks the spec names the tenant's own slug. The
// tenant's secrets are not resolved.
func DecodeTenant(d DashboardTenant) (Tenant, error) {
	// JSON is YAML, and decoding with the YAML decoder keeps the file's keys,
	// duration strings and strictness for both.
	dec := yaml.NewDecoder(bytes.NewReader(d.Spec))
	dec.KnownFields(true)
	var t Tenant
	if err := dec.Decode(&t); err != nil {
		if errors.Is(err, io.EOF) {
			return Tenant{}, errors.New("configfile: tenant spec is empty")
		}
		return Tenant{}, fmt.Errorf("configfile: tenant spec: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return Tenant{}, errors.New("configfile: tenant spec must hold one document")
	}
	if t.Slug != d.Slug {
		return Tenant{}, fmt.Errorf("configfile: tenant spec slug %q does not match %q", t.Slug, d.Slug)
	}
	t.origin = OriginDashboard
	return t, nil
}

// Merge returns a File holding file's tenants plus every dashboard tenant,
// its secrets opened with open and its filters compiled, validated as a
// whole so a slug or connection name two dashboard tenants share is
// rejected as a duplicate in the file would be. An error about a dashboard
// tenant is a *MergeError. When file is itself a merged File, its dashboard
// tenants are replaced, not added to. file is not modified.
//
// A file tenant whose slug or connection name a dashboard tenant already
// holds is left out, and listed by Skipped, rather than failing the merge
// (ADR-0010 §2.3). No dashboard write can claim what the file holds
// (ValidateDashboard refuses it), so the clash is a file edit's, and ids
// derive from names: running the file tenant would take over the
// dashboard tenant's rows, its members and its history with them.
func Merge(file *File, dash []DashboardTenant, open Opener) (*File, error) {
	if file.base != nil {
		file = file.base
	}
	out := *file
	out.base = file
	out.dashboard = slices.SortedFunc(slices.Values(dash), func(a, b DashboardTenant) int { return cmp.Compare(a.Slug, b.Slug) })
	var decoded []Tenant
	held := map[string]string{}
	for _, d := range out.dashboard {
		t, err := DecodeTenant(d)
		if err != nil {
			return nil, &MergeError{Slug: d.Slug, Err: err}
		}
		if err := t.resolve(t.where(0), refPolicy{dashboard: true, open: open}); err != nil {
			return nil, &MergeError{Slug: d.Slug, Err: err}
		}
		for _, name := range t.names() {
			held[name] = t.Slug
		}
		decoded = append(decoded, t)
	}
	out.Tenants, out.skipped = nil, nil
	for _, t := range file.Tenants {
		if reason := t.clash(held); reason != "" {
			out.skipped = append(out.skipped, SkippedTenant{Slug: t.Slug, Reason: reason})
			continue
		}
		out.Tenants = append(out.Tenants, t)
	}
	out.Tenants = append(out.Tenants, decoded...)
	if err := out.validateTenants(); err != nil {
		return nil, err
	}
	out.hash = mergedHash(file.hash, out.dashboard)
	return &out, nil
}

// mergedHash is the parsed file's hash when no tenant is merged in, so a
// deployment without dashboard tenants reports the hash it always has. Each
// spec is hashed as well as its revision: a tenant deleted and created again
// starts over at revision 1.
func mergedHash(fileHash string, sorted []DashboardTenant) string {
	if len(sorted) == 0 {
		return fileHash
	}
	h := sha256.New()
	h.Write([]byte(fileHash))
	for _, d := range sorted {
		spec := sha256.Sum256(d.Spec)
		h.Write([]byte("\n" + d.Slug + ":" + strconv.FormatInt(d.Revision, 10) + ":" + hex.EncodeToString(spec[:])))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Dashboard returns the dashboard tenants merged into f, sorted by slug;
// none for a parsed file.
func (f *File) Dashboard() []DashboardTenant { return slices.Clone(f.dashboard) }

// ValidateDashboard reports whether d would merge into f: dash with d
// added, or replacing the one with d's slug, merged onto the file f was
// built from. It also refuses d a slug or connection name any file tenant
// declares, running or skipped, unless d's stored version already held it:
// the file claims its names first, but a later file edit does not take
// them from the dashboard tenant holding them. dash is not modified.
func ValidateDashboard(f *File, dash []DashboardTenant, d DashboardTenant, open Opener) error {
	base := f
	if f.base != nil {
		base = f.base
	}
	if err := claimsFileNames(base, dash, d); err != nil {
		return err
	}
	rest := slices.DeleteFunc(slices.Clone(dash), func(e DashboardTenant) bool { return e.Slug == d.Slug })
	_, err := Merge(f, append(rest, d), open)
	return err
}

// claimsFileNames is the error for the first slug or connection name d
// takes that a tenant of file declares and d's stored version in dash did
// not hold, in the words validateTenant uses for a duplicate. A spec that
// does not decode is left for Merge to report.
func claimsFileNames(file *File, dash []DashboardTenant, d DashboardTenant) error {
	next, err := DecodeTenant(d)
	if err != nil {
		return nil
	}
	had := map[string]bool{}
	for _, e := range dash {
		if e.Slug != d.Slug {
			continue
		}
		if prev, err := DecodeTenant(e); err == nil {
			for _, name := range prev.names() {
				had[name] = true
			}
		}
	}
	for i := range file.Tenants {
		t := &file.Tenants[i]
		if t.Slug == next.Slug && !had["slug "+next.Slug] {
			return &MergeError{Slug: d.Slug, Err: fmt.Errorf("configfile: %s.slug %q duplicates tenants[%d]", next.where(0), next.Slug, i)}
		}
		for ii, in := range next.Connections {
			if !had["connection "+in.Name] && slices.ContainsFunc(t.Connections, func(x Connection) bool { return x.Name == in.Name }) {
				return &MergeError{Slug: d.Slug, Err: fmt.Errorf(
					"configfile: %s.connections[%d].name %q duplicates a connection in tenant %q; names are hook paths and must be unique",
					next.where(0), ii, in.Name, t.Slug)}
			}
		}
	}
	return nil
}

// SkippedTenant is a file tenant the running configuration leaves out
// because a dashboard tenant already holds its slug or one of its
// connection names.
type SkippedTenant struct {
	Slug   string
	Reason string
}

// Skipped lists the file tenants Merge left out, in file order.
func (f *File) Skipped() []SkippedTenant { return f.skipped }

// Declares reports whether the configuration file declares a tenant with
// slug, whether or not the running configuration left it out.
func (f *File) Declares(slug string) bool {
	if f.base != nil {
		f = f.base
	}
	return slices.ContainsFunc(f.Tenants, func(t Tenant) bool { return t.Slug == slug })
}

// names are the instance-wide names a tenant holds: its slug and its
// connections' names, which are hook paths.
func (t *Tenant) names() []string {
	out := make([]string, 0, 1+len(t.Connections))
	out = append(out, "slug "+t.Slug)
	for _, in := range t.Connections {
		out = append(out, "connection "+in.Name)
	}
	return out
}

// clash says which of held, a map from name to the dashboard tenant
// holding it, keeps t from running, or "" when none does.
func (t *Tenant) clash(held map[string]string) string {
	if d, ok := held["slug "+t.Slug]; ok {
		return fmt.Sprintf("dashboard tenant %q already holds the slug", d)
	}
	for _, in := range t.Connections {
		if d, ok := held["connection "+in.Name]; ok {
			return fmt.Sprintf("dashboard tenant %q already holds connection name %q", d, in.Name)
		}
	}
	return ""
}
