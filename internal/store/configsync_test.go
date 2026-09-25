package store

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestIsConfigContentError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"managed by another origin", fmt.Errorf("store: tenant x: %w", ErrManagedBy), true},
		{"unique violation, a concurrent insert", fmt.Errorf("store: upsert: %w", &pgconn.PgError{Code: "23505"}), false},
		{"foreign key violation, a concurrent delete", &pgconn.PgError{Code: "23503"}, false},
		{"not null violation", &pgconn.PgError{Code: "23502"}, true},
		{"check violation", &pgconn.PgError{Code: "23514"}, true},
		{"value too long", &pgconn.PgError{Code: "22001"}, true},
		{"connection failure", &pgconn.PgError{Code: "08006"}, false},
		{"serialization failure", &pgconn.PgError{Code: "40001"}, false},
		{"context canceled", context.Canceled, false},
		{"other", errors.New("boom"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsConfigContentError(tt.err); got != tt.want {
				t.Fatalf("IsConfigContentError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
