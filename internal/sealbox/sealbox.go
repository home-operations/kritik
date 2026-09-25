// Package sealbox implements envelope encryption for values that must sit
// at rest outside the runner boundary — dashboard-entered credentials — and
// be opened by every non-runner replica when it builds its config snapshot.
//
// Seal generates a fresh random 256-bit data-encryption key (DEK) per call,
// wraps it with the keyring's current key-encryption key (KEK) under
// AES-256-GCM, then encrypts the plaintext under the DEK with a second,
// independent AES-256-GCM operation. Both layers carry the format version
// and key id as GCM additional data, so the "ks1.<keyid>." header cannot be
// swapped onto a ciphertext sealed under a different key without failing
// authentication. A sealed value is the self-describing string:
//
//	ks1.<keyid>.<base64url-no-pad payload>
//
// where payload is wrapNonce(12B) || wrappedDEK(48B: 32B DEK + 16B GCM tag)
// || dataNonce(12B) || GCM(plaintext).
package sealbox

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

const (
	version = "ks1"
	keyLen  = 32 // AES-256 key-encryption and data-encryption keys
	idLen   = 8  // bytes of sha256(key) kept as the key id
	// idHexLen is idLen hex-encoded: the exact length of the second
	// "." segment of a well-formed sealed value.
	idHexLen = idLen * 2
	nonceLen = 12 // crypto/cipher.NewGCM's standard nonce length
	// gcmOverhead is the authentication tag size NewGCM adds with its
	// standard nonce length; crypto/cipher fixes this at 16 bytes and it
	// does not depend on the key, so it's the same for both GCM layers.
	gcmOverhead   = 16
	wrappedDEKLen = nonceLen + keyLen + gcmOverhead
	// minBlobLen is the smallest a decoded payload can be: both nonces,
	// the wrapped DEK, and an outer ciphertext for zero-length plaintext
	// (just its GCM tag).
	minBlobLen = wrappedDEKLen + nonceLen + gcmOverhead
)

var (
	// ErrNoKey is returned by Seal when the Keyring holds no current
	// key, which only happens for a zero-value Keyring rather than one
	// returned by NewKeyring; NewKeyring itself returns ErrNoKey when
	// given no current key at all.
	ErrNoKey = errors.New("sealbox: no current key")
	// ErrMalformed is returned when a sealed value isn't the ks1 wire
	// format: wrong prefix, wrong segment count, a key id that isn't
	// exactly idHexLen hex characters, invalid base64, or a decoded
	// payload too short to hold its fixed-size segments.
	ErrMalformed = errors.New("sealbox: malformed sealed value")
	// ErrUnknownKey is returned when a sealed value names a key id this
	// Keyring holds neither as its current key nor among its old keys.
	ErrUnknownKey = errors.New("sealbox: unknown key id")
	// ErrOpen is returned when a sealed value's format and key id are
	// valid but a GCM authentication check fails: the value was
	// tampered with, or its header was spliced onto a ciphertext sealed
	// under a different key that happens to share this key's id.
	ErrOpen = errors.New("sealbox: open failed")
)

// Keyring holds the key-encryption keys used to seal and open values: one
// current key used for every new Seal, plus any older keys retained so
// values sealed before a rotation can still be opened.
type Keyring struct {
	currentID string
	keys      map[string][]byte // key id -> 32-byte key
}

// NewKeyring builds a Keyring from the current 32-byte key-encryption key
// and any older keys that may still need to open previously sealed values.
// Every key, current or old, must be exactly 32 bytes, and no two keys
// (compared by content, which is exactly what KeyID collides on) may
// repeat. NewKeyring copies every key it's given, so the caller is free to
// reuse or zero its own buffers once it returns.
func NewKeyring(current []byte, old ...[]byte) (*Keyring, error) {
	if len(current) == 0 {
		return nil, ErrNoKey
	}
	if err := checkKeyLen(current); err != nil {
		return nil, err
	}

	keys := make(map[string][]byte, len(old)+1)
	keys[KeyID(current)] = bytes.Clone(current)
	for _, k := range old {
		if err := checkKeyLen(k); err != nil {
			return nil, err
		}
		id := KeyID(k)
		if _, dup := keys[id]; dup {
			return nil, fmt.Errorf("sealbox: duplicate key %s", id)
		}
		keys[id] = bytes.Clone(k)
	}

	return &Keyring{currentID: KeyID(current), keys: keys}, nil
}

func checkKeyLen(key []byte) error {
	if len(key) != keyLen {
		return fmt.Errorf("sealbox: key must be %d bytes, got %d", keyLen, len(key))
	}
	return nil
}

// keyEncodings are tried in order by ParseKey; padded encodings are tried
// before their raw counterparts since a padded input can't be mistaken for
// a shorter raw one, but the reverse isn't true.
var keyEncodings = []*base64.Encoding{
	base64.StdEncoding,
	base64.URLEncoding,
	base64.RawStdEncoding,
	base64.RawURLEncoding,
}

// ParseKey decodes a standard or URL-safe base64 32-byte key, padded or
// not, with surrounding whitespace trimmed first.
func ParseKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)

	var lastErr error
	for _, enc := range keyEncodings {
		key, err := enc.DecodeString(s)
		if err != nil {
			lastErr = err
			continue
		}
		if len(key) != keyLen {
			lastErr = fmt.Errorf("key decodes to %d bytes, want %d", len(key), keyLen)
			continue
		}
		return key, nil
	}
	return nil, fmt.Errorf("sealbox: parse key: %w", lastErr)
}

// KeyID identifies a key by the hex encoding of the first 8 bytes of
// sha256(key). It never fails and is not secret: it's the routing
// information carried in the clear in every sealed value's header.
func KeyID(key []byte) string {
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:idLen])
}

// Seal encrypts plaintext under a fresh random DEK wrapped by the
// Keyring's current key, returning the ks1 wire format described in the
// package doc.
func (k *Keyring) Seal(plaintext []byte) (string, error) {
	if k == nil {
		return "", ErrNoKey
	}
	kek, ok := k.keys[k.currentID]
	if !ok {
		return "", ErrNoKey
	}

	dek := make([]byte, keyLen)
	if _, err := rand.Read(dek); err != nil {
		return "", fmt.Errorf("sealbox: generate dek: %w", err)
	}
	defer clear(dek)

	aad := additionalData(k.currentID)

	wrapNonce, wrapped, err := sealLayer(kek, dek, aad)
	if err != nil {
		return "", err
	}
	dataNonce, ciphertext, err := sealLayer(dek, plaintext, aad)
	if err != nil {
		return "", err
	}

	blob := make([]byte, 0, len(wrapNonce)+len(wrapped)+len(dataNonce)+len(ciphertext))
	blob = append(blob, wrapNonce...)
	blob = append(blob, wrapped...)
	blob = append(blob, dataNonce...)
	blob = append(blob, ciphertext...)

	return version + "." + k.currentID + "." + base64.RawURLEncoding.EncodeToString(blob), nil
}

// Open decrypts a value produced by Seal (by this Keyring or one sharing
// one of its keys). It satisfies configfile's Opener interface.
func (k *Keyring) Open(sealed string) ([]byte, error) {
	if k == nil {
		return nil, ErrNoKey
	}
	id, blob, err := parseSealed(sealed)
	if err != nil {
		return nil, err
	}

	kek, ok := k.keys[id]
	if !ok {
		return nil, ErrUnknownKey
	}

	aad := additionalData(id)
	wrapNonce := blob[:nonceLen]
	wrapped := blob[nonceLen:wrappedDEKLen]
	dataNonce := blob[wrappedDEKLen : wrappedDEKLen+nonceLen]
	ciphertext := blob[wrappedDEKLen+nonceLen:]

	dek, err := openLayer(kek, wrapNonce, wrapped, aad)
	if err != nil {
		return nil, ErrOpen
	}
	defer clear(dek)

	plaintext, err := openLayer(dek, dataNonce, ciphertext, aad)
	if err != nil {
		return nil, ErrOpen
	}
	return plaintext, nil
}

// NeedsRotation reports whether sealed was sealed under a key other than
// the Keyring's current one, meaning it should be re-sealed the next time
// it's written. A malformed sealed value is reported as not needing
// rotation: Open is the authority on whether it's usable at all.
func (k *Keyring) NeedsRotation(sealed string) bool {
	if k == nil {
		return false
	}
	id, _, err := parseSealed(sealed)
	if err != nil {
		return false
	}
	return id != k.currentID
}

// parseSealed validates the wire format and returns the key id and decoded
// payload, without touching any key material.
func parseSealed(sealed string) (id string, blob []byte, err error) {
	if strings.ContainsAny(sealed, "\r\n") {
		return "", nil, ErrMalformed
	}
	parts := strings.SplitN(sealed, ".", 3)
	if len(parts) != 3 || parts[0] != version {
		return "", nil, ErrMalformed
	}
	id = parts[1]
	if len(id) != idHexLen || !isLowerHex(id) {
		return "", nil, ErrMalformed
	}

	// Strict rejects a payload whose unused trailing bits aren't zero, so
	// a base64 string can't have more than one valid decoding.
	blob, err = base64.RawURLEncoding.Strict().DecodeString(parts[2])
	if err != nil {
		return "", nil, ErrMalformed
	}
	if len(blob) < minBlobLen {
		return "", nil, ErrMalformed
	}
	return id, blob, nil
}

// isLowerHex reports whether s consists only of lowercase hex digits, the
// only form KeyID ever produces.
func isLowerHex(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// additionalData binds a GCM layer to the format version and key id it was
// sealed under, so neither can be swapped in the sealed string without
// failing authentication.
func additionalData(id string) []byte {
	return []byte(version + "." + id)
}

// sealLayer runs one AES-256-GCM encryption with a fresh random nonce,
// returning the nonce alongside the ciphertext.
func sealLayer(key, plaintext, aad []byte) (nonce, ciphertext []byte, err error) {
	aead, err := newGCM(key)
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, nonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("sealbox: generate nonce: %w", err)
	}
	return nonce, aead.Seal(nil, nonce, plaintext, aad), nil
}

// openLayer runs one AES-256-GCM decryption. Its error is deliberately not
// wrapped with detail: callers turn any failure into the single ErrOpen so
// a caller can't distinguish "wrong tag" from "wrong length" and use that
// as an oracle.
func openLayer(key, nonce, ciphertext, aad []byte) ([]byte, error) {
	aead, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	return aead.Open(nil, nonce, ciphertext, aad)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("sealbox: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("sealbox: %w", err)
	}
	return gcm, nil
}
