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
)

// The instance spec in the API is its stored JSON except at each SecretRef
// position: a client writes {"value": "..."} to set a secret, {"keep": true}
// to keep the one stored, or, for a webhook secret, {"generate": true}; a
// read shows {"set": bool}. The env, file and sealed forms never cross the
// API: env and file would read the server's own environment and disk, and
// sealed would let a client replay ciphertext lifted from elsewhere.

// sealedKey is SecretRef.Sealed's spec key, and valueKey the write form
// that sets a secret.
const (
	sealedKey = "sealed"
	valueKey  = "value"
)

// nameKey names a connection or an account entry in the spec, and
// apiKeyKey is a provider's or the embedder's key.
const (
	nameKey   = "name"
	apiKeyKey = "apiKey"
)

// secretPos is one SecretRef position in a spec.
type secretPos struct {
	parent map[string]any
	leaf   string
	// where is the position's path in the spec, logical the same with
	// connections and accounts named rather than indexed, for the audit log
	// and generated secrets.
	where, logical string
	// generatable secrets may be minted by the server.
	generatable bool
	// stored is the sealed ref at the same logical position of a stored
	// spec, and whether keeping it is refused because what the secret acts
	// for changed; nil when there is none.
	stored func(prev map[string]any) (ref any, err *specError)
}

// connectionSecrets are the SecretRef positions within a connection. A
// bound one authenticates to one forge identity acting on the
// connection's accounts, so a kept one only stays kept while the
// connection still names the same forge and accounts.
var connectionSecrets = []struct {
	path               string
	generatable, bound bool
}{
	{path: "app.clientIdFrom", bound: true},
	{path: "app.privateKey", bound: true},
	{path: "app.webhookSecret", generatable: true},
}

// secretPositions lists every SecretRef position root holds: its
// connections' credentials, its providers' and its accounts' providers'
// keys, its egress credentials and its embedder's key.
func secretPositions(root map[string]any) []secretPos {
	out := make([]secretPos, 0, 3*len(objects(root["connections"])))
	for i, in := range objects(root["connections"]) {
		name, _ := in["name"].(string)
		for _, k := range connectionSecrets {
			parent, leaf := lookupParent(in, k.path)
			out = append(out, secretPos{
				parent: parent, leaf: leaf, generatable: k.generatable,
				where:   fmt.Sprintf("connections[%d].%s", i, k.path),
				logical: fmt.Sprintf("connections[%s].%s", name, k.path),
				stored: func(prev map[string]any) (any, *specError) {
					for _, old := range objects(prev["connections"]) {
						if old["name"] != name || name == "" {
							continue
						}
						ref := sealedAt(lookupParent(old, k.path))
						if ref != nil && k.bound && identityOf(old) != identityOf(in) {
							return nil, &specError{code: CodeReenterSecret, msg: "the connection's forge or accounts changed; enter this secret again"}
						}
						return ref, nil
					}
					return nil, nil
				},
			})
		}
	}
	out = append(out, providerSecrets(root, "providers", "providers", func(prev map[string]any) map[string]any { return prev })...)
	for i, a := range objects(root["accounts"]) {
		key := entryKey(a)
		slug, _ := a["forge"].(string)
		name, _ := a["name"].(string)
		out = append(out, providerSecrets(a, fmt.Sprintf("accounts[%d].providers", i), fmt.Sprintf("accounts[%s/%s].providers", slug, name),
			func(prev map[string]any) map[string]any {
				for _, old := range objects(prev["accounts"]) {
					if entryKey(old) == key {
						return old
					}
				}
				return nil
			})...)
	}
	egress := objectMap(root["egress"])
	creds := objectMap(egress["credentials"])
	for _, host := range slices.Sorted(maps.Keys(creds)) {
		out = append(out, secretPos{
			parent: creds, leaf: host, where: "egress.credentials." + host, logical: "egress.credentials." + host,
			stored: func(prev map[string]any) (any, *specError) {
				return sealedAt(objectMap(objectMap(prev["egress"])["credentials"]), host), nil
			},
		})
	}
	if emb := objectMap(root["embedding"]); emb != nil {
		out = append(out, secretPos{
			parent: emb, leaf: apiKeyKey, where: "embedding.apiKey", logical: "embedding.apiKey",
			stored: func(prev map[string]any) (any, *specError) {
				old := objectMap(prev["embedding"])
				ref := sealedAt(old, apiKeyKey)
				// The embedder has no type, so its endpoint is its baseUrl alone.
				if ref != nil && providerEndpoint(old) != providerEndpoint(emb) {
					return nil, &specError{code: CodeReenterSecret, msg: "the embedder's endpoint changed; enter this key again"}
				}
				return ref, nil
			},
		})
	}
	return out
}

// providerSecrets are the apiKey positions of owner's providers, found in
// a stored spec through within. A kept key stays kept only while its
// provider keeps its type and endpoint: kept under others, the key would be
// sent where it was never meant to go.
func providerSecrets(owner map[string]any, where, logical string, within func(prev map[string]any) map[string]any) []secretPos {
	providers := objectMap(owner["providers"])
	out := make([]secretPos, 0, len(providers))
	for _, name := range slices.Sorted(maps.Keys(providers)) {
		p := objectMap(providers[name])
		out = append(out, secretPos{
			parent: p, leaf: apiKeyKey, where: where + "." + name + ".apiKey", logical: logical + "." + name + ".apiKey",
			stored: func(prev map[string]any) (any, *specError) {
				old := objectMap(objectMap(within(prev)["providers"])[name])
				ref := sealedAt(old, apiKeyKey)
				if ref != nil && providerEndpoint(old) != providerEndpoint(p) {
					return nil, &specError{code: CodeReenterSecret, msg: "the provider's type or endpoint changed; enter this key again"}
				}
				return ref, nil
			},
		})
	}
	return out
}

// sealedAt is the sealed ref at parent[leaf], nil when there is none.
func sealedAt(parent map[string]any, leaf string) any {
	ref, _ := parent[leaf].(map[string]any)
	if s, _ := ref[sealedKey].(string); s != "" {
		return map[string]any{sealedKey: s}
	}
	return nil
}

// specError is a spec the API refuses, at path ("" for the whole spec), in
// the form the configuration's validation errors use.
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
	// generated maps each secret the server generated, by its logical
	// path, to its value, returned to the client exactly once.
	generated map[string]string
	// changed lists the logical paths of every secret given a new value,
	// for the audit log.
	changed []string
}

// sealSpec turns a client's spec into the stored form: each secret given
// a value or generated is sealed with seal, and each kept one is copied
// from stored, the spec it replaces (empty for none).
func sealSpec(spec, stored json.RawMessage, seal func([]byte) (string, error), generate func() (string, error)) (sealedSpec, error) {
	var out sealedSpec
	root, err := decodeObject(spec)
	if err != nil {
		return out, &specError{msg: "spec must be a JSON object"}
	}
	prev := map[string]any{}
	if len(bytes.TrimSpace(stored)) > 0 {
		if prev, err = decodeObject(stored); err != nil {
			return out, fmt.Errorf("webapi: stored spec: %w", err)
		}
	}
	for _, pos := range secretPositions(root) {
		v, ok := pos.parent[pos.leaf]
		if !ok || v == nil {
			continue
		}
		in, err := sealRef(v, pos, prev)
		if err != nil {
			return out, err
		}
		switch {
		case in.generate:
			plain, err := generate()
			if err != nil {
				return out, fmt.Errorf("webapi: generate secret: %w", err)
			}
			in.value = plain
			if out.generated == nil {
				out.generated = map[string]string{}
			}
			out.generated[pos.logical] = plain
		case in.kept != nil:
			pos.parent[pos.leaf] = in.kept
			continue
		}
		sealed, err := seal([]byte(in.value))
		if err != nil {
			return out, fmt.Errorf("webapi: seal %s: %w", pos.logical, err)
		}
		pos.parent[pos.leaf] = map[string]any{sealedKey: sealed}
		out.changed = append(out.changed, pos.logical)
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

// sealRef reads the write form at pos; a keep takes the stored ref from
// prev.
func sealRef(v any, pos secretPos, prev map[string]any) (secretInput, error) {
	var in secretInput
	m, ok := v.(map[string]any)
	if !ok || len(m) != 1 {
		return in, &specError{path: pos.where, msg: `must be exactly one of {"value": "..."}, {"keep": true} or {"generate": true}`}
	}
	for form, x := range m {
		switch form {
		case valueKey:
			s, ok := x.(string)
			if !ok || s == "" {
				return in, &specError{path: pos.where, msg: "value must be a non-empty string"}
			}
			in.value = s
		case "keep":
			if x != true {
				return in, &specError{path: pos.where, msg: "keep must be true"}
			}
			ref, err := pos.stored(prev)
			if err != nil {
				err.path = pos.where
				return in, err
			}
			if ref == nil {
				return in, &specError{path: pos.where, msg: "has no stored value to keep"}
			}
			in.kept = ref
		case "generate":
			if x != true {
				return in, &specError{path: pos.where, msg: "generate must be true"}
			}
			if !pos.generatable {
				return in, &specError{path: pos.where, msg: "only a webhook secret can be generated"}
			}
			in.generate = true
		case "env", "file", "sealed":
			return in, &specError{path: pos.where, msg: form + " references are not accepted from the dashboard; give a value"}
		default:
			return in, &specError{path: pos.where, msg: `must be exactly one of {"value": "..."}, {"keep": true} or {"generate": true}`}
		}
	}
	return in, nil
}

// providerEndpoint is where a provider's key is sent: its type, and its
// baseUrl where case and a trailing slash do not count.
func providerEndpoint(p map[string]any) string {
	typ, _ := p["type"].(string)
	base, _ := p["baseUrl"].(string)
	return typ + " " + endpointOf(base)
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

// redactSpec replaces every secret position in a stored spec, or in an
// account entry of one, with {"set": bool}.
func redactSpec(spec json.RawMessage) (json.RawMessage, error) {
	return rewriteSecrets(spec, func(v any) any { return map[string]any{"set": refSet(v)} })
}

// keepSecrets replaces every stored secret in spec with {"keep": true}, so
// a spec a write edits in part keeps the rest as it is stored.
func keepSecrets(spec json.RawMessage) json.RawMessage {
	out, err := rewriteSecrets(spec, func(v any) any {
		if refSet(v) {
			return map[string]any{"keep": true}
		}
		return v
	})
	if err != nil {
		return spec
	}
	return out
}

// rewriteSecrets replaces the value at each secret position of spec, or of
// an account entry, with what fn makes of it.
func rewriteSecrets(spec json.RawMessage, fn func(any) any) (json.RawMessage, error) {
	root, err := decodeObject(spec)
	if err != nil {
		return nil, fmt.Errorf("webapi: rewrite secrets: %w", err)
	}
	positions := secretPositions(root)
	if _, isEntry := root["name"]; isEntry {
		positions = providerSecrets(root, "providers", "providers", func(map[string]any) map[string]any { return nil })
	}
	for _, pos := range positions {
		if v, ok := pos.parent[pos.leaf]; ok {
			pos.parent[pos.leaf] = fn(v)
		}
	}
	out, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("webapi: rewrite secrets: %w", err)
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
