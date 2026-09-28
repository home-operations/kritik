package webapi

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/home-operations/kritik/internal/configfile"
)

// A tenant spec in the API is the JSON form of a tenant entry in the file,
// except at each SecretRef position: a client writes {"value": "..."} to
// set a secret, {"keep": true} to keep the one stored, or, for a webhook
// secret, {"generate": true}; a read shows {"set": bool}. The env, file and
// sealed forms never cross the API: env and file would read the server's
// own environment and disk, and sealed would let a client replay
// ciphertext lifted from elsewhere.

// secretKey is a SecretRef position within one connection.
type secretKey struct {
	// path is dotted from the connection, e.g. "app.privateKey".
	path string
	// generatable secrets may be minted by the server.
	generatable bool
	// bound secrets authenticate to one forge identity acting on the
	// connection's accounts, so a kept one only stays kept while the
	// connection still names the same forge and accounts.
	bound bool
}

// sealedKey is SecretRef.Sealed's spec key.
const sealedKey = "sealed"

// providerKey is a tenant provider's apiKey, bound to the provider's type
// and endpoint by keepProviderRef.
var providerKey = secretKey{path: "apiKey", bound: true}

var secretKeys = []secretKey{
	{path: "app.clientIdFrom", bound: true},
	{path: "app.privateKey", bound: true},
	{path: "app.webhookSecret", generatable: true},
}

// specError is a tenant spec the API refuses, at path ("" for the whole
// spec), in the form the file's validation errors use.
type specError struct {
	path string
	msg  string
	// code defaults to CodeInvalidSpec.
	code ErrorCode
}

func (e *specError) errorCode() ErrorCode {
	if e.code == "" {
		return CodeInvalidSpec
	}
	return e.code
}

func (e *specError) Error() string {
	if e.path == "" {
		return e.msg
	}
	return e.path + ": " + e.msg
}

// sealedSpec is a client's spec made storable.
type sealedSpec struct {
	spec json.RawMessage
	// generated maps "connections[<name>].<key>" to each secret the
	// server generated, returned to the client exactly once.
	generated map[string]string
	// changed lists the same logical paths, and "providers.<name>.apiKey"
	// for a provider key, for every secret given a new value, for the audit
	// log.
	changed []string
}

// sealSpec turns a client's spec into the stored form: each secret given
// a value or generated is sealed with seal, and each kept one is copied
// from stored, the spec it replaces (nil on create), matching
// connections and providers by name.
func sealSpec(spec, stored json.RawMessage, seal func([]byte) (string, error), generate func() (string, error)) (sealedSpec, error) {
	var out sealedSpec
	root, err := decodeObject(spec)
	if err != nil {
		return out, &specError{msg: "spec must be a JSON object"}
	}
	var prev map[string]any
	if stored != nil {
		if prev, err = decodeObject(stored); err != nil {
			return out, fmt.Errorf("webapi: stored spec: %w", err)
		}
	}
	// at seals or keeps the secret at parent[leaf], if there is one.
	at := func(parent map[string]any, leaf string, k secretKey, where, logical string, keep func() (any, *specError)) error {
		v, ok := parent[leaf]
		if !ok || v == nil {
			return nil
		}
		ref, err := sealRef(v, k, where, keep)
		if err != nil {
			return err
		}
		switch {
		case ref.generate:
			plain, err := generate()
			if err != nil {
				return fmt.Errorf("webapi: generate secret: %w", err)
			}
			ref.value = plain
			if out.generated == nil {
				out.generated = map[string]string{}
			}
			out.generated[logical] = plain
		case ref.kept != nil:
			parent[leaf] = ref.kept
			return nil
		}
		sealed, err := seal([]byte(ref.value))
		if err != nil {
			return fmt.Errorf("webapi: seal %s: %w", logical, err)
		}
		parent[leaf] = map[string]any{sealedKey: sealed}
		out.changed = append(out.changed, logical)
		return nil
	}
	for i, in := range objects(root["connections"]) {
		name, _ := in["name"].(string)
		for _, k := range secretKeys {
			parent, leaf := lookupParent(in, k.path)
			where := fmt.Sprintf("connections[%d].%s", i, k.path)
			logical := fmt.Sprintf("connections[%s].%s", name, k.path)
			if err := at(parent, leaf, k, where, logical, func() (any, *specError) { return keepRef(prev, in, k, where) }); err != nil {
				return out, err
			}
		}
	}
	providers := objectMap(root["providers"])
	for _, name := range slices.Sorted(maps.Keys(providers)) {
		p := objectMap(providers[name])
		where := "providers." + name + ".apiKey"
		keep := func() (any, *specError) { return keepProviderRef(prev, name, p, where) }
		if err := at(p, "apiKey", providerKey, where, where, keep); err != nil {
			return out, err
		}
	}
	if out.spec, err = json.Marshal(root); err != nil {
		return out, fmt.Errorf("webapi: encode spec: %w", err)
	}
	return out, nil
}

// secretInput is one secret position's write form, decoded.
type secretInput struct {
	value    string
	generate bool
	kept     any
}

// sealRef reads the write form at where; keep looks up the stored ref.
func sealRef(v any, k secretKey, where string, keep func() (any, *specError)) (secretInput, error) {
	var in secretInput
	m, ok := v.(map[string]any)
	if !ok || len(m) != 1 {
		return in, &specError{path: where, msg: `must be exactly one of {"value": "..."}, {"keep": true} or {"generate": true}`}
	}
	for form, x := range m {
		switch form {
		case "value":
			s, ok := x.(string)
			if !ok || s == "" {
				return in, &specError{path: where, msg: "value must be a non-empty string"}
			}
			in.value = s
		case "keep":
			if x != true {
				return in, &specError{path: where, msg: "keep must be true"}
			}
			ref, err := keep()
			if err != nil {
				return in, err
			}
			in.kept = ref
		case "generate":
			if x != true {
				return in, &specError{path: where, msg: "generate must be true"}
			}
			if !k.generatable {
				return in, &specError{path: where, msg: "only a webhook secret can be generated"}
			}
			in.generate = true
		case "env", "file", "sealed":
			return in, &specError{path: where, msg: form + " references are not accepted from the dashboard; give a value"}
		default:
			return in, &specError{path: where, msg: `must be exactly one of {"value": "..."}, {"keep": true} or {"generate": true}`}
		}
	}
	return in, nil
}

// keepRef is the sealed ref stored at k under the stored connection with
// next's name. A bound secret is kept only while the connection still
// names the same forge and accounts: kept under others, a key would act for
// an account it was never meant for.
func keepRef(stored, next map[string]any, k secretKey, where string) (any, *specError) {
	name, _ := next["name"].(string)
	ref, prev := storedRef(stored, name, k.path)
	if ref == nil {
		return nil, &specError{path: where, msg: "has no stored value to keep"}
	}
	if k.bound && identityOf(prev) != identityOf(next) {
		return nil, &specError{path: where, code: CodeReenterSecret,
			msg: "the connection's forge or accounts changed; enter this secret again"}
	}
	return ref, nil
}

// keepProviderRef is the sealed key stored for the provider named name,
// kept only while the provider keeps its type and endpoint: kept under
// others, the key would be sent where it was never meant to go.
func keepProviderRef(stored map[string]any, name string, next map[string]any, where string) (any, *specError) {
	prev := objectMap(objectMap(stored["providers"])[name])
	ref := objectMap(prev["apiKey"])
	sealed, _ := ref[sealedKey].(string)
	if sealed == "" {
		return nil, &specError{path: where, msg: "has no stored value to keep"}
	}
	if providerEndpoint(prev) != providerEndpoint(next) {
		return nil, &specError{path: where, code: CodeReenterSecret, msg: "the provider's type or endpoint changed; enter this key again"}
	}
	return map[string]any{sealedKey: sealed}, nil
}

// providerEndpoint is where a provider's key is sent: its type, and its
// baseUrl where case and a trailing slash do not count.
func providerEndpoint(p map[string]any) string {
	typ, _ := p["type"].(string)
	base, _ := p["baseUrl"].(string)
	return typ + " " + strings.TrimRight(strings.ToLower(strings.TrimSpace(base)), "/")
}

// storedRef is the sealed ref stored at path under the connection named
// name, and that connection, if there is one.
func storedRef(stored map[string]any, name, path string) (any, map[string]any) {
	if name == "" {
		return nil, nil
	}
	for _, in := range objects(stored["connections"]) {
		if in["name"] != name {
			continue
		}
		parent, leaf := lookupParent(in, path)
		ref, ok := parent[leaf].(map[string]any)
		if s, _ := ref[sealedKey].(string); !ok || s == "" {
			return nil, nil
		}
		return map[string]any{sealedKey: ref[sealedKey]}, in
	}
	return nil, nil
}

// connectionIdentity is who a connection's credentials speak for, and
// for which accounts.
type connectionIdentity struct {
	forge, accounts string
}

// identityOf normalises a connection's forge and accounts. The accounts
// count as a set, without case.
func identityOf(in map[string]any) connectionIdentity {
	forge, _ := in["forge"].(string)
	var accounts []string
	list, _ := in["accounts"].([]any)
	for _, a := range list {
		if s, ok := a.(string); ok {
			accounts = append(accounts, strings.ToLower(s))
		}
	}
	slices.Sort(accounts)
	return connectionIdentity{forge: forge, accounts: strings.Join(slices.Compact(accounts), ",")}
}

// redactSpec replaces every secret position in a stored or file spec with
// {"set": bool}.
func redactSpec(spec json.RawMessage) (json.RawMessage, error) {
	root, err := decodeObject(spec)
	if err != nil {
		return nil, fmt.Errorf("webapi: redact spec: %w", err)
	}
	for _, in := range objects(root["connections"]) {
		for _, k := range secretKeys {
			parent, leaf := lookupParent(in, k.path)
			v, ok := parent[leaf]
			if !ok {
				continue
			}
			parent[leaf] = map[string]any{"set": refSet(v)}
		}
	}
	for _, p := range objectMap(root["providers"]) {
		pm := objectMap(p)
		if v, ok := pm["apiKey"]; ok {
			pm["apiKey"] = map[string]any{"set": refSet(v)}
		}
	}
	out, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("webapi: redact spec: %w", err)
	}
	return out, nil
}

// refSet reports whether a SecretRef in spec form points at anything.
func refSet(v any) bool {
	m, _ := v.(map[string]any)
	for _, k := range []string{"env", "file", sealedKey} {
		if s, _ := m[k].(string); s != "" {
			return true
		}
	}
	return false
}

// renderFileTenant is a file tenant in spec form, secrets redacted. It
// goes through YAML because the file's keys are the yaml tags.
func renderFileTenant(t *configfile.Tenant) (json.RawMessage, error) {
	raw, err := yaml.Marshal(t)
	if err != nil {
		return nil, fmt.Errorf("webapi: render tenant: %w", err)
	}
	var tree any
	if err := yaml.Unmarshal(raw, &tree); err != nil {
		return nil, fmt.Errorf("webapi: render tenant: %w", err)
	}
	spec, err := json.Marshal(tree)
	if err != nil {
		return nil, fmt.Errorf("webapi: render tenant: %w", err)
	}
	return redactSpec(spec)
}

// decodeObject decodes a JSON object, keeping numbers exact.
func decodeObject(raw json.RawMessage) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, fmt.Errorf("not an object")
	}
	return m, nil
}

// objectMap is v when it is an object, else nil.
func objectMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// objects is v's elements that are objects, when v is an array.
func objects(v any) []map[string]any {
	list, _ := v.([]any)
	out := make([]map[string]any, 0, len(list))
	for _, x := range list {
		if m, ok := x.(map[string]any); ok {
			out = append(out, m)
		} else {
			// Keep indexes aligned with the spec for error paths.
			out = append(out, map[string]any{})
		}
	}
	return out
}

// lookupParent walks path's dotted prefix under m, returning the object
// holding its last segment (nil when a step is missing) and that segment.
func lookupParent(m map[string]any, path string) (map[string]any, string) {
	segs := strings.Split(path, ".")
	for _, s := range segs[:len(segs)-1] {
		m, _ = m[s].(map[string]any)
	}
	return m, segs[len(segs)-1]
}

// generateWebhookSecret is 32 random bytes, hex.
func generateWebhookSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
