package forge

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPermissionValid(t *testing.T) {
	tests := []struct {
		name string
		perm Permission
		want bool
	}{
		{"none", PermissionNone, true},
		{"read", PermissionRead, true},
		{"triage", PermissionTriage, true},
		{"write", PermissionWrite, true},
		{"maintain", PermissionMaintain, true},
		{"admin", PermissionAdmin, true},
		{"empty", Permission(""), false},
		{"unknown", Permission("owner"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.perm.Valid(); got != tt.want {
				t.Errorf("Permission(%q).Valid() = %v, want %v", tt.perm, got, tt.want)
			}
		})
	}
}

func TestCanWrite(t *testing.T) {
	tests := []struct {
		perm Permission
		want bool
	}{
		{PermissionNone, false},
		{PermissionRead, false},
		{PermissionTriage, false},
		{PermissionWrite, true},
		{PermissionMaintain, true},
		{PermissionAdmin, true},
		{Permission("unknown"), false},
	}
	for _, tt := range tests {
		t.Run(string(tt.perm), func(t *testing.T) {
			if got := CanWrite(tt.perm); got != tt.want {
				t.Errorf("CanWrite(%q) = %v, want %v", tt.perm, got, tt.want)
			}
		})
	}
}

func TestStatusDescription(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"short text stays", "kritik: 2 finding(s)", "kritik: 2 finding(s)"},
		{"exactly the limit stays", strings.Repeat("a", MaxStatusDescription), strings.Repeat("a", MaxStatusDescription)},
		{"longer is cut with an ellipsis", strings.Repeat("a", 150), strings.Repeat("a", MaxStatusDescription-1) + "…"},
		{"characters count, not bytes", strings.Repeat("é", 150), strings.Repeat("é", MaxStatusDescription-1) + "…"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := StatusDescription(tt.in)
			if got != tt.want || !utf8.ValidString(got) || utf8.RuneCountInString(got) > MaxStatusDescription {
				t.Fatalf("StatusDescription = %q", got)
			}
		})
	}
}
