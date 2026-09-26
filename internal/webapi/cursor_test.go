package webapi

import (
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/home-operations/kritik/internal/store"
)

func TestCursorRoundTrip(t *testing.T) {
	at := time.Date(2026, 9, 1, 12, 30, 0, 123, time.UTC)
	tests := []struct {
		name string
		c    store.Cursor
	}{
		{"time keyed", store.Cursor{T: at, ID: "0b7e7a8e-8f0c-4c6e-8a55-1f8f0c0f1a2b"}},
		{"text keyed", store.Cursor{S: "acme/widgets", ID: "id-1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeCursor(encodeCursor(tt.c))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !got.T.Equal(tt.c.T) || got.S != tt.c.S || got.ID != tt.c.ID {
				t.Errorf("round trip = %+v, want %+v", got, tt.c)
			}
		})
	}
}

func TestParsePage(t *testing.T) {
	valid := encodeCursor(store.Cursor{S: "a", ID: "b"})
	tests := []struct {
		name      string
		query     string
		wantLimit int
		wantFirst bool
		wantCode  ErrorCode
	}{
		{"defaults", "", defaultLimit, true, ""},
		{"explicit limit", "limit=10", 10, true, ""},
		{"limit capped", "limit=5000", maxLimit, true, ""},
		{"zero limit", "limit=0", 0, false, CodeBadRequest},
		{"negative limit", "limit=-1", 0, false, CodeBadRequest},
		{"non-numeric limit", "limit=ten", 0, false, CodeBadRequest},
		{"cursor", "cursor=" + valid, defaultLimit, false, ""},
		{"garbage cursor", "cursor=@@", 0, false, CodeInvalidCursor},
		{"non-json cursor", "cursor=bm90IGpzb24", 0, false, CodeInvalidCursor},
		{"cursor without id", "cursor=" + encodeCursor(store.Cursor{S: "a"}), 0, false, CodeInvalidCursor},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/x?"+tt.query, nil)
			p, err := parsePage(r)
			if tt.wantCode != "" {
				e, ok := errors.AsType[*apiError](err)
				if !ok || e.code != tt.wantCode || e.status != 400 {
					t.Fatalf("err = %v, want 400 %s", err, tt.wantCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePage: %v", err)
			}
			if p.Limit != tt.wantLimit || p.After.First() != tt.wantFirst {
				t.Errorf("page = %+v, want limit %d first %v", p, tt.wantLimit, tt.wantFirst)
			}
		})
	}
}

func TestNewPage(t *testing.T) {
	if got := newPage([]int(nil), nil); got.Items == nil || got.NextCursor != nil {
		t.Errorf("empty page = %+v, want [] and null cursor", got)
	}
	next := store.Cursor{S: "a", ID: "b"}
	got := newPage([]int{1}, &next)
	if got.NextCursor == nil {
		t.Fatal("NextCursor = nil")
	}
	if c, err := decodeCursor(*got.NextCursor); err != nil || c.ID != "b" {
		t.Errorf("NextCursor decodes to %+v, %v", c, err)
	}
}
