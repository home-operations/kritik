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
	if t.Slug != d.Slug {
		return Tenant{}, fmt.Errorf("configfile: tenant spec slug %q does not match %q", t.Slug, d.Slug)
	}
	t.origin = OriginDashboard
	return t, nil
}

// Merge returns a File holding file's tenants plus every dashboard tenant,
// its secrets opened with open and its filters compiled, validated as a
// whole so a slug or installation name a dashboard tenant shares with any
// other tenant is rejected as a duplicate in the file would be. An error
// about a dashboard tenant is a *MergeError. When file is itself a merged
// File, its dashboard tenants are replaced, not added to. file is not
// modified.
func Merge(file *File, dash []DashboardTenant, open Opener) (*File, error) {
	if file.base != nil {
		file = file.base
	}
	out := *file
	out.base = file
	out.dashboard = slices.SortedFunc(slices.Values(dash), func(a, b DashboardTenant) int { return cmp.Compare(a.Slug, b.Slug) })
	out.Tenants = slices.Clone(file.Tenants)
	for _, d := range out.dashboard {
		t, err := DecodeTenant(d)
		if err != nil {
			return nil, &MergeError{Slug: d.Slug, Err: err}
		}
		if err := t.resolve(t.where(0), refPolicy{dashboard: true, open: open}); err != nil {
			return nil, &MergeError{Slug: d.Slug, Err: err}
		}
		out.Tenants = append(out.Tenants, t)
	}
	if err := out.validateTenants(); err != nil {
		return nil, err
	}
	out.hash = mergedHash(file.hash, out.dashboard)
	return &out, nil
}

// mergedHash is the parsed file's hash when no tenant is merged in, so a
// deployment without dashboard tenants reports the hash it always has.
func mergedHash(fileHash string, sorted []DashboardTenant) string {
	if len(sorted) == 0 {
		return fileHash
	}
	h := sha256.New()
	h.Write([]byte(fileHash))
	for _, d := range sorted {
		h.Write([]byte("\n" + d.Slug + ":" + strconv.FormatInt(d.Revision, 10)))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Dashboard returns the dashboard tenants merged into f, sorted by slug;
// none for a parsed file.
func (f *File) Dashboard() []DashboardTenant { return slices.Clone(f.dashboard) }

// ValidateDashboard reports whether d would merge into f: f's dashboard
// tenants with d added, or replacing the one with d's slug, merged onto the
// file f was built from.
func ValidateDashboard(f *File, d DashboardTenant, open Opener) error {
	dash := slices.DeleteFunc(f.Dashboard(), func(e DashboardTenant) bool { return e.Slug == d.Slug })
	_, err := Merge(f, append(dash, d), open)
	return err
}
