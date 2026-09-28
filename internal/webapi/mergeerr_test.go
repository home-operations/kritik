package webapi

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/home-operations/kritik/internal/configfile"
)

func TestSpecFailure(t *testing.T) {
	merge := func(msg string) error { return &configfile.MergeError{Err: errors.New(msg)} }
	fine := func() error { return nil }
	broken := func() error { return errors.New("still broken") }
	tests := []struct {
		name     string
		err      error
		account  bool
		baseline func() error
		status   int
		code     ErrorCode
		path     string
		message  string
	}{
		{name: "a key of the spec", err: merge(`configfile: providers.p.type must be openai, got "x"`), baseline: fine,
			status: 422, code: CodeInvalidSpec, path: "providers.p.type", message: `providers.p.type must be openai, got "x"`},
		{name: "a bare key of the spec", err: merge("configfile: embedding: cannot open the key"), baseline: fine,
			status: 422, code: CodeInvalidSpec, path: "embedding", message: "embedding: cannot open the key"},
		{name: "a message naming no key", err: merge("configfile: no way to sign in"), baseline: fine,
			status: 422, code: CodeInvalidSpec, message: "no way to sign in"},
		{name: "a key of the account's entry", account: true, err: merge(`configfile: accounts[2].models.review references provider "q"`),
			baseline: fine, status: 422, code: CodeInvalidSpec, path: "models.review", message: `models.review references provider "q"`},
		{name: "the entry itself", account: true, err: merge("configfile: accounts[2]: account github/a duplicates accounts[0]"),
			baseline: broken, status: 422, code: CodeInvalidSpec, message: "accounts[2]: account github/a duplicates accounts[0]"},
		{name: "elsewhere, with the stored spec valid", account: true, err: merge("configfile: providers.p.apiKey: cannot open"),
			baseline: fine, status: 422, code: CodeInvalidSpec, message: "providers.p.apiKey: cannot open"},
		{name: "elsewhere, with the stored spec broken", account: true, err: merge("configfile: providers.p.apiKey: cannot open"),
			baseline: broken, status: 409, code: CodeConfigBlocked},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var err error
			if tt.account {
				err = accountSpecFailure(tt.err, 2, tt.baseline)
			} else {
				err = specFailure(tt.err, tt.baseline)
			}
			e, ok := errors.AsType[*apiError](err)
			if !ok || e.status != tt.status || e.code != tt.code {
				t.Fatalf("error = %v, want %d %s", err, tt.status, tt.code)
			}
			var d pathDetails
			if e.details != nil {
				_ = json.Unmarshal(e.details, &d)
			}
			if d.Path != tt.path || (tt.message != "" && e.message != tt.message) {
				t.Errorf("got %q at %q, want %q at %q", e.message, d.Path, tt.message, tt.path)
			}
		})
	}
}

func TestDecodeFailure(t *testing.T) {
	tests := []struct {
		spec, path, message string
	}{
		{`{"nope":1}`, "nope", "spec: field nope not found"},
		{`{"connections":[{"name":"a","bogus":true}]}`, "bogus", "spec: field bogus not found"},
	}
	for _, tt := range tests {
		t.Run(tt.spec, func(t *testing.T) {
			_, err := configfile.DecodeSpec(json.RawMessage(tt.spec))
			if err == nil {
				t.Fatal("decoded")
			}
			e, _ := errors.AsType[*apiError](decodeFailure(err))
			var d pathDetails
			_ = json.Unmarshal(e.details, &d)
			if e.status != 422 || e.message != tt.message || d.Path != tt.path {
				t.Errorf("got %d %q path %q, want %q path %q (from %v)", e.status, e.message, d.Path, tt.message, tt.path, err)
			}
		})
	}
}
