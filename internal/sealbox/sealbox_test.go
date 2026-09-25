package sealbox

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func testKey(t *testing.T, seed byte) []byte {
	t.Helper()
	key := make([]byte, keyLen)
	for i := range key {
		key[i] = seed
	}
	return key
}

func TestNewKeyring(t *testing.T) {
	valid := testKey(t, 1)
	valid2 := testKey(t, 2)

	tests := map[string]struct {
		current    []byte
		old        [][]byte
		wantErr    error // checked via errors.Is when set
		wantAnyErr bool  // some error expected, sentinel not checked
		wantOK     bool  // NewKeyring must succeed
	}{
		"valid current only":     {current: valid, wantOK: true},
		"valid current plus old": {current: valid, old: [][]byte{valid2}, wantOK: true},
		"no current key":         {current: nil, wantErr: ErrNoKey},
		"empty current key":      {current: []byte{}, wantErr: ErrNoKey},
		"current too short":      {current: valid[:16], wantAnyErr: true},
		"current too long":       {current: append(append([]byte{}, valid...), 0), wantAnyErr: true},
		"old key too short":      {current: valid, old: [][]byte{valid2[:16]}, wantAnyErr: true},
		"duplicate current+old":  {current: valid, old: [][]byte{valid}, wantAnyErr: true},
		"duplicate among old":    {current: valid, old: [][]byte{valid2, valid2}, wantAnyErr: true},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			kr, err := NewKeyring(tt.current, tt.old...)
			switch {
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("NewKeyring() err = %v, want %v", err, tt.wantErr)
				}
			case tt.wantAnyErr:
				if err == nil {
					t.Fatal("NewKeyring() expected an error, got nil")
				}
			case tt.wantOK:
				if err != nil {
					t.Fatalf("NewKeyring() unexpected err: %v", err)
				}
				if kr == nil {
					t.Fatal("NewKeyring() returned nil keyring with nil error")
				}
			default:
				t.Fatal("test case must set wantErr, wantAnyErr, or wantOK")
			}
		})
	}
}

func TestNewKeyringClonesKeys(t *testing.T) {
	current := testKey(t, 1)
	old := testKey(t, 2)
	currentCopy := append([]byte(nil), current...)
	oldCopy := append([]byte(nil), old...)

	// A value sealed under the real, unmutated old key: used below to prove
	// kr retained its own copy rather than aliasing the caller's old-key
	// slice.
	refOld, err := NewKeyring(oldCopy)
	if err != nil {
		t.Fatalf("NewKeyring() err: %v", err)
	}
	sealedUnderOld, err := refOld.Seal([]byte("sealed under the real old key"))
	if err != nil {
		t.Fatalf("Seal() err: %v", err)
	}

	kr, err := NewKeyring(current, old)
	if err != nil {
		t.Fatalf("NewKeyring() err: %v", err)
	}

	// Mutate the caller's buffers after NewKeyring returns. A Keyring that
	// stores these by reference would now seal and open using garbage key
	// material instead of what was actually passed in.
	clear(current)
	clear(old)

	if _, err := kr.Open(sealedUnderOld); err != nil {
		t.Fatalf("Open() of a value sealed under the real old key failed after the caller zeroed its buffer: %v", err)
	}

	sealed, err := kr.Seal([]byte("sealed after the caller zeroed current"))
	if err != nil {
		t.Fatalf("Seal() err: %v", err)
	}
	refCurrent, err := NewKeyring(currentCopy)
	if err != nil {
		t.Fatalf("NewKeyring() err: %v", err)
	}
	opened, err := refCurrent.Open(sealed)
	if err != nil {
		t.Fatalf("a reference Keyring holding the real, unmutated current key could not open a value kr sealed after the caller zeroed its buffer: %v", err)
	}
	if string(opened) != "sealed after the caller zeroed current" {
		t.Fatalf("Open() = %q, want %q", opened, "sealed after the caller zeroed current")
	}
}

func TestParseKey(t *testing.T) {
	raw := testKey(t, 7)
	std := base64.StdEncoding.EncodeToString(raw)
	urlSafe := base64.URLEncoding.EncodeToString(raw)
	rawStd := base64.RawStdEncoding.EncodeToString(raw)
	rawURL := base64.RawURLEncoding.EncodeToString(raw)

	tests := map[string]struct {
		in      string
		wantErr bool
	}{
		"standard padded":    {in: std},
		"url-safe padded":    {in: urlSafe},
		"standard unpadded":  {in: rawStd},
		"url-safe unpadded":  {in: rawURL},
		"trailing newline":   {in: std + "\n"},
		"leading whitespace": {in: "  " + std},
		"invalid base64":     {in: "not-valid-base64!!", wantErr: true},
		"wrong length":       {in: base64.StdEncoding.EncodeToString(raw[:16]), wantErr: true},
		"empty":              {in: "", wantErr: true},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			key, err := ParseKey(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatal("ParseKey() expected an error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseKey() unexpected err: %v", err)
			}
			if !bytes.Equal(key, raw) {
				t.Fatalf("ParseKey() = %x, want %x", key, raw)
			}
		})
	}
}

func TestKeyID(t *testing.T) {
	a := testKey(t, 1)
	b := testKey(t, 2)

	firstID := KeyID(a)
	if len(firstID) != idHexLen {
		t.Fatalf("KeyID() length = %d, want %d", len(firstID), idHexLen)
	}
	if secondID := KeyID(a); firstID != secondID {
		t.Fatal("KeyID() not deterministic")
	}
	if KeyID(a) == KeyID(b) {
		t.Fatal("KeyID() collided for distinct keys")
	}
}

func TestSealOpenRoundTrip(t *testing.T) {
	current := testKey(t, 1)
	kr, err := NewKeyring(current)
	if err != nil {
		t.Fatalf("NewKeyring() err: %v", err)
	}

	large := make([]byte, 1<<20)
	if _, err := rand.Read(large); err != nil {
		t.Fatalf("rand.Read() err: %v", err)
	}

	tests := map[string][]byte{
		"empty":    {},
		"nil":      nil,
		"small":    []byte("hello, sealbox"),
		"1 MiB":    large,
		"has null": []byte("a\x00b"),
	}

	for name, plaintext := range tests {
		t.Run(name, func(t *testing.T) {
			sealed, err := kr.Seal(plaintext)
			if err != nil {
				t.Fatalf("Seal() err: %v", err)
			}
			opened, err := kr.Open(sealed)
			if err != nil {
				t.Fatalf("Open() err: %v", err)
			}
			if !bytes.Equal(opened, plaintext) {
				t.Fatalf("Open() = %q, want %q", opened, plaintext)
			}
		})
	}
}

func TestSealIsRandomized(t *testing.T) {
	kr, err := NewKeyring(testKey(t, 1))
	if err != nil {
		t.Fatalf("NewKeyring() err: %v", err)
	}

	plaintext := []byte("same input, twice")
	first, err := kr.Seal(plaintext)
	if err != nil {
		t.Fatalf("Seal() err: %v", err)
	}
	second, err := kr.Seal(plaintext)
	if err != nil {
		t.Fatalf("Seal() err: %v", err)
	}
	if first == second {
		t.Fatal("Seal() produced identical output for two calls with the same plaintext")
	}
}

func TestSealNoCurrentKey(t *testing.T) {
	var kr Keyring
	if _, err := kr.Seal([]byte("x")); !errors.Is(err, ErrNoKey) {
		t.Fatalf("Seal() on zero-value Keyring err = %v, want %v", err, ErrNoKey)
	}
}

func TestNilKeyringReceiver(t *testing.T) {
	var kr *Keyring
	if _, err := kr.Seal([]byte("x")); !errors.Is(err, ErrNoKey) {
		t.Fatalf("Seal() on nil *Keyring err = %v, want %v", err, ErrNoKey)
	}
	if _, err := kr.Open("ks1.0000000000000000.AA"); !errors.Is(err, ErrNoKey) {
		t.Fatalf("Open() on nil *Keyring err = %v, want %v", err, ErrNoKey)
	}
}

func TestOpenWrongKeyring(t *testing.T) {
	sealer, err := NewKeyring(testKey(t, 1))
	if err != nil {
		t.Fatalf("NewKeyring() err: %v", err)
	}
	sealed, err := sealer.Seal([]byte("secret"))
	if err != nil {
		t.Fatalf("Seal() err: %v", err)
	}

	opener, err := NewKeyring(testKey(t, 2))
	if err != nil {
		t.Fatalf("NewKeyring() err: %v", err)
	}
	if _, err := opener.Open(sealed); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("Open() with unrelated keyring err = %v, want %v", err, ErrUnknownKey)
	}
}

func TestOldKeyOpensAndNeedsRotation(t *testing.T) {
	oldKey := testKey(t, 1)
	newKey := testKey(t, 2)

	oldKeyring, err := NewKeyring(oldKey)
	if err != nil {
		t.Fatalf("NewKeyring() err: %v", err)
	}
	sealed, err := oldKeyring.Seal([]byte("rotate me"))
	if err != nil {
		t.Fatalf("Seal() err: %v", err)
	}

	rotated, err := NewKeyring(newKey, oldKey)
	if err != nil {
		t.Fatalf("NewKeyring() err: %v", err)
	}

	opened, err := rotated.Open(sealed)
	if err != nil {
		t.Fatalf("Open() with rotated keyring err: %v", err)
	}
	if string(opened) != "rotate me" {
		t.Fatalf("Open() = %q, want %q", opened, "rotate me")
	}
	if !rotated.NeedsRotation(sealed) {
		t.Fatal("NeedsRotation() = false for value sealed under old key, want true")
	}

	freshlySealed, err := rotated.Seal([]byte("current"))
	if err != nil {
		t.Fatalf("Seal() err: %v", err)
	}
	if rotated.NeedsRotation(freshlySealed) {
		t.Fatal("NeedsRotation() = true for value sealed under current key, want false")
	}
}

func TestNeedsRotationMalformed(t *testing.T) {
	kr, err := NewKeyring(testKey(t, 1))
	if err != nil {
		t.Fatalf("NewKeyring() err: %v", err)
	}
	if kr.NeedsRotation("not a sealed value") {
		t.Fatal("NeedsRotation() = true for a malformed value, want false")
	}
}

// splitSealed breaks a ks1 value into its three dot-separated segments for
// tamper tests, failing the test if the shape is unexpected.
func splitSealed(t *testing.T, sealed string) (prefix, id, payload string) {
	t.Helper()
	parts := strings.SplitN(sealed, ".", 3)
	if len(parts) != 3 {
		t.Fatalf("sealed value %q does not have 3 segments", sealed)
	}
	return parts[0], parts[1], parts[2]
}

// b64URLAlphabet is the RFC 4648 base64url alphabet, in symbol order, used
// by flipSpareBits to find a non-canonical encoding of an existing symbol.
const b64URLAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

// flipSpareBits sets a base64url payload's unused trailing bits to a
// non-zero value without changing the bytes it decodes to under a
// non-strict decoder, letting a test distinguish strict from non-strict
// decoding of the same underlying bytes.
func flipSpareBits(t *testing.T, encoded string) string {
	t.Helper()
	last := encoded[len(encoded)-1]
	idx := strings.IndexByte(b64URLAlphabet, last)
	if idx < 0 {
		t.Fatalf("payload's last byte %q is not in the base64url alphabet", last)
	}
	nonCanonical := idx | 0b11
	if nonCanonical == idx {
		t.Fatal("test fixture's last symbol already has non-zero spare bits; adjust the plaintext length")
	}
	return encoded[:len(encoded)-1] + string(b64URLAlphabet[nonCanonical])
}

func TestOpenTamperedSegments(t *testing.T) {
	kr, err := NewKeyring(testKey(t, 1), testKey(t, 9))
	if err != nil {
		t.Fatalf("NewKeyring() err: %v", err)
	}
	sealed, err := kr.Seal([]byte("tamper target"))
	if err != nil {
		t.Fatalf("Seal() err: %v", err)
	}
	prefix, id, payload := splitSealed(t, sealed)
	blob, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}

	flipByte := func(b []byte, i int) []byte {
		out := append([]byte{}, b...)
		out[i] ^= 0xFF
		return out
	}
	reencode := func(b []byte) string {
		return prefix + "." + id + "." + base64.RawURLEncoding.EncodeToString(b)
	}

	tests := map[string]struct {
		sealed  string
		wantErr error
	}{
		"wrong version prefix": {
			sealed:  "ks9." + id + "." + payload,
			wantErr: ErrMalformed,
		},
		"missing segment": {
			sealed:  prefix + "." + id,
			wantErr: ErrMalformed,
		},
		"non-hex key id": {
			sealed:  prefix + "." + strings.Repeat("z", idHexLen) + "." + payload,
			wantErr: ErrMalformed,
		},
		"uppercase key id": {
			sealed:  prefix + "." + strings.ToUpper(id) + "." + payload,
			wantErr: ErrMalformed,
		},
		"short key id": {
			sealed:  prefix + "." + id[:idHexLen-2] + "." + payload,
			wantErr: ErrMalformed,
		},
		"unknown key id": {
			sealed:  prefix + "." + strings.Repeat("a", idHexLen) + "." + payload,
			wantErr: ErrUnknownKey,
		},
		"key id swapped to another held key": {
			sealed:  prefix + "." + KeyID(testKey(t, 9)) + "." + payload,
			wantErr: ErrOpen,
		},
		"invalid base64 payload": {
			sealed:  prefix + "." + id + ".not!base64",
			wantErr: ErrMalformed,
		},
		"non-canonical base64 padding bits": {
			sealed:  prefix + "." + id + "." + flipSpareBits(t, payload),
			wantErr: ErrMalformed,
		},
		"carriage return in sealed value": {
			sealed:  sealed + "\r",
			wantErr: ErrMalformed,
		},
		"newline in sealed value": {
			sealed:  sealed + "\n",
			wantErr: ErrMalformed,
		},
		"truncated payload": {
			sealed:  reencode(blob[:minBlobLen-1]),
			wantErr: ErrMalformed,
		},
		"tampered wrap nonce": {
			sealed:  reencode(flipByte(blob, 0)),
			wantErr: ErrOpen,
		},
		"tampered wrapped dek": {
			sealed:  reencode(flipByte(blob, nonceLen+1)),
			wantErr: ErrOpen,
		},
		"tampered data nonce": {
			sealed:  reencode(flipByte(blob, wrappedDEKLen+1)),
			wantErr: ErrOpen,
		},
		"tampered ciphertext": {
			sealed:  reencode(flipByte(blob, len(blob)-1)),
			wantErr: ErrOpen,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := kr.Open(tt.sealed); !errors.Is(err, tt.wantErr) {
				t.Fatalf("Open() err = %v, want %v", err, tt.wantErr)
			}
		})
	}
}
