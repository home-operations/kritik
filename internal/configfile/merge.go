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

// Origin is where an account is declared.
type Origin string

// Account origins.
const (
	OriginFile      Origin = "file"
	OriginDashboard Origin = "dashboard"
)

// Valid reports whether o is an origin.
func (o Origin) Valid() bool { return o == OriginFile || o == OriginDashboard }

func (o Origin) String() string { return string(o) }

// Origin reports where the account is declared: the operator's file, or the
// dashboard.
func (t *Account) Origin() Origin {
	if t.origin == "" {
		return OriginFile
	}
	return t.origin
}

// where names the account in an error: its index for a file account, its slug
// for a dashboard one, whose index is an artefact of merging.
func (t *Account) where(index int) string {
	if t.Origin() == OriginDashboard {
		return "dashboard[" + t.Slug + "]"
	}
	return fmt.Sprintf("accounts[%d]", index)
}

// DashboardAccount is an account the dashboard manages: its spec is the JSON
// form of an account entry in the file, and Revision increments on each
// write.
type DashboardAccount struct {
	Slug     string
	Spec     json.RawMessage
	Revision int64
}

// MergeError is a dashboard account that failed to decode, resolve or
// validate against the file.
type MergeError struct {
	Slug string
	Err  error
}

func (e *MergeError) Error() string { return fmt.Sprintf("dashboard account %q: %v", e.Slug, e.Err) }

func (e *MergeError) Unwrap() error { return e.Err }

// DecodeAccount strictly decodes a dashboard account's spec, rejecting unknown
// keys as Parse does, and checks the spec names the account's own slug. The
// account's secrets are not resolved.
func DecodeAccount(d DashboardAccount) (Account, error) {
	// JSON is YAML, and decoding with the YAML decoder keeps the file's keys,
	// duration strings and strictness for both.
	dec := yaml.NewDecoder(bytes.NewReader(d.Spec))
	dec.KnownFields(true)
	var t Account
	if err := dec.Decode(&t); err != nil {
		if errors.Is(err, io.EOF) {
			return Account{}, errors.New("configfile: account spec is empty")
		}
		return Account{}, fmt.Errorf("configfile: account spec: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return Account{}, errors.New("configfile: account spec must hold one document")
	}
	if t.Slug != d.Slug {
		return Account{}, fmt.Errorf("configfile: account spec slug %q does not match %q", t.Slug, d.Slug)
	}
	t.origin = OriginDashboard
	return t, nil
}

// Merge returns a File holding file's accounts plus every dashboard account,
// its secrets opened with open and its filters compiled, validated as a
// whole so a slug or connection name two dashboard accounts share is
// rejected as a duplicate in the file would be. An error about a dashboard
// account is a *MergeError. When file is itself a merged File, its dashboard
// accounts are replaced, not added to. file is not modified.
//
// A file account whose slug or connection name a dashboard account already
// holds is left out, and listed by Skipped, rather than failing the merge
// (ADR-0010 §2.3). No dashboard write can claim what the file holds
// (ValidateDashboard refuses it), so the clash is a file edit's, and ids
// derive from names: running the file account would take over the
// dashboard account's rows, its members and its history with them.
func Merge(file *File, dash []DashboardAccount, open Opener) (*File, error) {
	if file.base != nil {
		file = file.base
	}
	out := *file
	out.base = file
	out.dashboard = slices.SortedFunc(slices.Values(dash), func(a, b DashboardAccount) int { return cmp.Compare(a.Slug, b.Slug) })
	var decoded []Account
	held := map[string]string{}
	for _, d := range out.dashboard {
		t, err := DecodeAccount(d)
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
	out.Accounts, out.skipped = nil, nil
	for _, t := range file.Accounts {
		if reason := t.clash(held); reason != "" {
			out.skipped = append(out.skipped, SkippedAccount{Slug: t.Slug, Reason: reason})
			continue
		}
		out.Accounts = append(out.Accounts, t)
	}
	out.Accounts = append(out.Accounts, decoded...)
	if err := out.validateAccounts(); err != nil {
		return nil, err
	}
	out.hash = mergedHash(file.hash, out.dashboard)
	return &out, nil
}

// mergedHash is the parsed file's hash when no account is merged in, so a
// deployment without dashboard accounts reports the hash it always has. Each
// spec is hashed as well as its revision: an account deleted and created again
// starts over at revision 1.
func mergedHash(fileHash string, sorted []DashboardAccount) string {
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

// Dashboard returns the dashboard accounts merged into f, sorted by slug;
// none for a parsed file.
func (f *File) Dashboard() []DashboardAccount { return slices.Clone(f.dashboard) }

// ValidateDashboard reports whether d would merge into f: dash with d
// added, or replacing the one with d's slug, merged onto the file f was
// built from. It also refuses d a slug or connection name any file account
// declares, running or skipped, unless d's stored version already held it:
// the file claims its names first, but a later file edit does not take
// them from the dashboard account holding them. dash is not modified.
func ValidateDashboard(f *File, dash []DashboardAccount, d DashboardAccount, open Opener) error {
	base := f
	if f.base != nil {
		base = f.base
	}
	if err := claimsFileNames(base, dash, d); err != nil {
		return err
	}
	rest := slices.DeleteFunc(slices.Clone(dash), func(e DashboardAccount) bool { return e.Slug == d.Slug })
	_, err := Merge(f, append(rest, d), open)
	return err
}

// claimsFileNames is the error for the first slug or connection name d
// takes that an account of file declares and d's stored version in dash did
// not hold, in the words validateAccount uses for a duplicate. A spec that
// does not decode is left for Merge to report.
func claimsFileNames(file *File, dash []DashboardAccount, d DashboardAccount) error {
	next, err := DecodeAccount(d)
	if err != nil {
		return nil
	}
	had := map[string]bool{}
	for _, e := range dash {
		if e.Slug != d.Slug {
			continue
		}
		if prev, err := DecodeAccount(e); err == nil {
			for _, name := range prev.names() {
				had[name] = true
			}
		}
	}
	for i := range file.Accounts {
		t := &file.Accounts[i]
		if t.Slug == next.Slug && !had["slug "+next.Slug] {
			return &MergeError{Slug: d.Slug, Err: fmt.Errorf("configfile: %s.slug %q duplicates accounts[%d]", next.where(0), next.Slug, i)}
		}
		for ii, in := range next.Connections {
			if !had["connection "+in.Name] && slices.ContainsFunc(t.Connections, func(x Connection) bool { return x.Name == in.Name }) {
				return &MergeError{Slug: d.Slug, Err: fmt.Errorf(
					"configfile: %s.connections[%d].name %q duplicates a connection in account %q; names are hook paths and must be unique",
					next.where(0), ii, in.Name, t.Slug)}
			}
		}
	}
	return nil
}

// SkippedAccount is a file account the running configuration leaves out
// because a dashboard account already holds its slug or one of its
// connection names.
type SkippedAccount struct {
	Slug   string
	Reason string
}

// Skipped lists the file accounts Merge left out, in file order.
func (f *File) Skipped() []SkippedAccount { return f.skipped }

// Declares reports whether the configuration file declares an account with
// slug, whether or not the running configuration left it out.
func (f *File) Declares(slug string) bool {
	if f.base != nil {
		f = f.base
	}
	return slices.ContainsFunc(f.Accounts, func(t Account) bool { return t.Slug == slug })
}

// names are the instance-wide names an account holds: its slug and its
// connections' names, which are hook paths.
func (t *Account) names() []string {
	out := make([]string, 0, 1+len(t.Connections))
	out = append(out, "slug "+t.Slug)
	for _, in := range t.Connections {
		out = append(out, "connection "+in.Name)
	}
	return out
}

// clash says which of held, a map from name to the dashboard account
// holding it, keeps t from running, or "" when none does.
func (t *Account) clash(held map[string]string) string {
	if d, ok := held["slug "+t.Slug]; ok {
		return fmt.Sprintf("dashboard account %q already holds the slug", d)
	}
	for _, in := range t.Connections {
		if d, ok := held["connection "+in.Name]; ok {
			return fmt.Sprintf("dashboard account %q already holds connection name %q", d, in.Name)
		}
	}
	return ""
}
