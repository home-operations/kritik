package configfile

import (
	"bytes"
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

// InstanceSpec is the spec as the store holds it: its JSON and the
// revision each write increments. Revision 0 is no spec stored yet.
type InstanceSpec struct {
	Spec     json.RawMessage
	Revision int64
}

// MergeError is a spec that failed to decode, resolve or validate. Its
// error names the offending key by its path in the spec.
type MergeError struct {
	Err error
}

func (e *MergeError) Error() string { return "instance spec: " + e.Err.Error() }

func (e *MergeError) Unwrap() error { return e.Err }

// DecodeSpec strictly decodes a spec, rejecting unknown keys as Parse does.
// Its secrets are not resolved. An empty spec is the zero Spec.
func DecodeSpec(raw json.RawMessage) (Spec, error) {
	var s Spec
	if len(bytes.TrimSpace(raw)) == 0 {
		return s, nil
	}
	// JSON is YAML, and decoding with the YAML decoder keeps the file's keys,
	// duration strings and strictness for both.
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil && !errors.Is(err, io.EOF) {
		return Spec{}, fmt.Errorf("configfile: spec: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return Spec{}, errors.New("configfile: spec must hold one document")
	}
	return s, nil
}

// Merge returns the running configuration: file's sign-in, its connections
// and the spec's, and the spec's settings over the file's instance
// defaults (ADR-0015), with the spec's secrets opened
// with open and its filters compiled, validated as a whole. Any error is a
// *MergeError. When file is itself a merged File, its spec is replaced.
// file is not modified.
//
// A file connection whose name, or one of whose accounts, a spec
// connection already holds is left out, and listed by Skipped, rather than
// failing the merge (ADR-0014 §2.2): no dashboard write can claim what the
// file holds (ValidateSpec refuses it), so the clash is a file edit's, and
// running the file connection would take the dashboard's accounts over.
func Merge(file *File, spec InstanceSpec, open Opener) (*File, error) {
	if file.base != nil {
		file = file.base
	}
	s, err := DecodeSpec(spec.Spec)
	if err != nil {
		return nil, &MergeError{Err: err}
	}
	if err := s.resolve(open); err != nil {
		return nil, &MergeError{Err: err}
	}
	providers, defaults, embedding := file.underSpec(&s)
	out := &File{
		Auth: file.Auth, Providers: providers, Defaults: defaults, Polling: s.Polling, Indexing: s.Indexing,
		Tools: s.Tools, Retention: s.Retention, Egress: s.Egress, Embedding: embedding,
		hash: mergedHash(file.hash, spec), base: file, spec: spec, envConnection: file.envConnection,
		envProvider: file.envProvider, envKeys: file.envKeys, specDefaults: s.Defaults, specProviders: map[string]bool{},
	}
	for name := range s.Providers {
		out.specProviders[name] = true
	}
	held := claims(s.Connections)
	for _, in := range file.Connections {
		if reason := in.clash(held); reason != "" {
			out.skipped = append(out.skipped, SkippedConnection{Name: in.Name, Reason: reason})
			continue
		}
		out.Connections = append(out.Connections, in)
	}
	out.Connections = append(out.Connections, s.Connections...)
	if err := out.validate(&s); err != nil {
		return nil, &MergeError{Err: err}
	}
	out.Accounts, out.unserved = servedAccounts(out.Connections, s.Accounts)
	return out, nil
}

// servedAccounts is every account conns serve, in their order, with its
// entry from the spec where there is one, and the entries no connection
// serves.
func servedAccounts(conns []Connection, entries []Account) (served, unserved []Account) {
	byKey := map[string]*Account{}
	for i := range entries {
		byKey[entries[i].Key()] = &entries[i]
	}
	used := map[string]bool{}
	for _, in := range conns {
		for _, name := range in.Accounts {
			key := AccountKey(in.Forge, name)
			if used[key] {
				continue
			}
			used[key] = true
			if e, ok := byKey[key]; ok {
				served = append(served, *e)
			} else {
				served = append(served, Account{Forge: in.Forge, Name: name})
			}
		}
	}
	for _, e := range entries {
		if !used[e.Key()] {
			unserved = append(unserved, e)
		}
	}
	return served, unserved
}

// mergedHash is the parsed file's hash when no spec is stored, so a
// deployment without one reports the hash it always has. The spec is hashed
// as well as its revision, which a restored database could repeat.
func mergedHash(fileHash string, spec InstanceSpec) string {
	if spec.Revision == 0 && len(spec.Spec) == 0 {
		return fileHash
	}
	sum := sha256.Sum256(spec.Spec)
	h := sha256.New()
	h.Write([]byte(fileHash + "\n" + strconv.FormatInt(spec.Revision, 10) + ":" + hex.EncodeToString(sum[:])))
	return hex.EncodeToString(h.Sum(nil))
}

// Spec returns the spec merged into f; the zero InstanceSpec for a parsed
// file.
func (f *File) Spec() InstanceSpec { return f.spec }

// ValidateSpec reports whether next would merge onto the file f was built
// from. It also refuses next a connection name, or an account, a file
// connection declares, running or skipped, unless stored, the spec next
// replaces, already held it: the file claims its names first, but a later
// file edit does not take them from the dashboard connection holding them.
func ValidateSpec(f *File, stored, next InstanceSpec, open Opener) error {
	base := f
	if f.base != nil {
		base = f.base
	}
	if err := claimsFileConnections(base, stored, next); err != nil {
		return err
	}
	_, err := Merge(base, next, open)
	return err
}

// claimsFileConnections is the error for the first connection name or
// account next takes that a file connection declares and stored did not
// hold. A spec that does not decode is left for Merge to report.
func claimsFileConnections(file *File, stored, next InstanceSpec) error {
	s, err := DecodeSpec(next.Spec)
	if err != nil {
		return nil
	}
	var had map[string]string
	if prev, err := DecodeSpec(stored.Spec); err == nil {
		had = claims(prev.Connections)
	}
	for i, in := range s.Connections {
		for _, fc := range file.Connections {
			if fc.Name == in.Name && had["connection "+in.Name] == "" {
				return &MergeError{Err: fmt.Errorf("configfile: connections[%d].name %q is declared in the configuration file", i, in.Name)}
			}
			for ai, a := range in.Accounts {
				if fc.Serves(a) && fc.Forge == in.Forge && had["account "+AccountKey(in.Forge, a)] == "" {
					return &MergeError{Err: fmt.Errorf(
						"configfile: connections[%d].accounts[%d] %q is served by connection %q of the configuration file", i, ai, a, fc.Name)}
				}
			}
		}
	}
	return nil
}

// claims maps each connection name and account key conns hold, as
// "connection <name>" and "account <key>", to the connection holding it.
func claims(conns []Connection) map[string]string {
	held := map[string]string{}
	for _, in := range conns {
		held["connection "+in.Name] = in.Name
		for _, a := range in.Accounts {
			held["account "+AccountKey(in.Forge, a)] = in.Name
		}
	}
	return held
}

// SkippedConnection is a file connection the running configuration leaves
// out because a spec connection already holds its name or one of its
// accounts.
type SkippedConnection struct {
	Name   string
	Reason string
}

// Skipped lists the file connections Merge left out, in file order.
func (f *File) Skipped() []SkippedConnection { return f.skipped }

// Unserved lists the spec's account entries no running connection serves:
// kept, for when one does again, but not run.
func (f *File) Unserved() []Account { return f.unserved }

// FileConnections are the connections the configuration file and its
// environment declare, running or skipped.
func (f *File) FileConnections() []Connection {
	if f.base != nil {
		f = f.base
	}
	return slices.Clone(f.Connections)
}
