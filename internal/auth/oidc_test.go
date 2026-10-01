package auth

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestRolesOf(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want []string
	}{
		{name: "no claim", in: nil, want: []string{}},
		{name: "list keeps only strings", in: []any{"a", 1, "b"}, want: []string{"a", "b"}},
		{name: "map keys sorted", in: map[string]any{"z": map[string]any{}, "a": true}, want: []string{"a", "z"}},
		{name: "single string", in: "admin", want: []string{"admin"}},
		{name: "number", in: 42, want: []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rolesOf(tt.in)
			if got == nil || !slices.Equal(got, tt.want) {
				t.Fatalf("rolesOf(%v) = %#v, want %#v", tt.in, got, tt.want)
			}
		})
	}
}

func TestFlexBool(t *testing.T) {
	tests := []struct {
		in      string
		want    bool
		wantErr bool
	}{
		{in: `true`, want: true},
		{in: `false`},
		{in: `"true"`, want: true},
		{in: `"false"`},
		{in: `"yes"`},
		{in: `1`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			var c idClaims
			err := json.Unmarshal([]byte(`{"email_verified":`+tt.in+`}`), &c)
			if (err != nil) != tt.wantErr || bool(c.EmailVerified) != tt.want {
				t.Fatalf("email_verified %s = %v, %v; want %v, error %v", tt.in, c.EmailVerified, err, tt.want, tt.wantErr)
			}
		})
	}
}
