package forge

import "testing"

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
