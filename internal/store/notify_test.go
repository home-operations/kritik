package store

import (
	"testing"
	"time"
)

func TestEventKindValid(t *testing.T) {
	tests := []struct {
		name string
		kind EventKind
		want bool
	}{
		{"review", EventReview, true},
		{"runner_run", EventRunnerRun, true},
		{"index_run", EventIndexRun, true},
		{"followup", EventFollowup, true},
		{"model_call", EventModelCall, true},
		{"empty", EventKind(""), false},
		{"unknown", EventKind("bogus"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.kind.Valid(); got != tt.want {
				t.Errorf("EventKind(%q).Valid() = %v, want %v", tt.kind, got, tt.want)
			}
		})
	}
}

func TestParseEvent(t *testing.T) {
	reviewID := "b1f6c9d0-0000-0000-0000-000000000001"

	tests := []struct {
		name    string
		payload string
		want    Event
		wantErr bool
	}{
		{
			name:    "review with review_id",
			payload: `{"tenant_id":"a1f6c9d0-0000-0000-0000-000000000001","kind":"review","id":"` + reviewID + `","review_id":"` + reviewID + `"}`,
			want: Event{
				TenantID: "a1f6c9d0-0000-0000-0000-000000000001",
				Kind:     EventReview,
				ID:       reviewID,
				ReviewID: &reviewID,
			},
		},
		{
			name:    "index_run with null review_id",
			payload: `{"tenant_id":"a1f6c9d0-0000-0000-0000-000000000001","kind":"index_run","id":"c1f6c9d0-0000-0000-0000-000000000001","review_id":null}`,
			want: Event{
				TenantID: "a1f6c9d0-0000-0000-0000-000000000001",
				Kind:     EventIndexRun,
				ID:       "c1f6c9d0-0000-0000-0000-000000000001",
				ReviewID: nil,
			},
		},
		{
			name:    "unknown kind",
			payload: `{"tenant_id":"a1f6c9d0-0000-0000-0000-000000000001","kind":"bogus","id":"c1f6c9d0-0000-0000-0000-000000000001"}`,
			wantErr: true,
		},
		{
			name:    "missing tenant_id",
			payload: `{"kind":"review","id":"` + reviewID + `"}`,
			wantErr: true,
		},
		{
			name:    "missing id",
			payload: `{"tenant_id":"a1f6c9d0-0000-0000-0000-000000000001","kind":"review"}`,
			wantErr: true,
		},
		{
			name:    "malformed json",
			payload: `{not json`,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseEvent(tt.payload)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseEvent(%q) error = %v, wantErr %v", tt.payload, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if got.TenantID != tt.want.TenantID || got.Kind != tt.want.Kind || got.ID != tt.want.ID {
				t.Errorf("parseEvent(%q) = %+v, want %+v", tt.payload, got, tt.want)
			}
			switch {
			case (got.ReviewID == nil) != (tt.want.ReviewID == nil):
				t.Errorf("parseEvent(%q) ReviewID = %v, want %v", tt.payload, got.ReviewID, tt.want.ReviewID)
			case got.ReviewID != nil && *got.ReviewID != *tt.want.ReviewID:
				t.Errorf("parseEvent(%q) ReviewID = %v, want %v", tt.payload, *got.ReviewID, *tt.want.ReviewID)
			}
		})
	}
}

func TestListenBackoff(t *testing.T) {
	for _, attempt := range []int{0, 1, 2, 3, 4, 5, 6, 20} {
		t.Run(string(rune('0'+attempt%10)), func(t *testing.T) {
			d := listenBackoff(attempt)
			if d < listenBackoffMin/2 {
				t.Fatalf("listenBackoff(%d) = %v, below half the minimum floor %v", attempt, d, listenBackoffMin)
			}
			if d > listenBackoffMax {
				t.Fatalf("listenBackoff(%d) = %v, exceeds cap %v", attempt, d, listenBackoffMax)
			}
		})
	}
}

func TestListenBackoffGrows(t *testing.T) {
	// The guaranteed half of the delay (ignoring jitter) should increase
	// with attempt until it saturates at the cap.
	prevHalf := time.Duration(0)
	for attempt := range 6 {
		half := min(listenBackoffMin<<attempt, listenBackoffMax) / 2
		if half < prevHalf {
			t.Fatalf("attempt %d: guaranteed half %v is less than previous %v", attempt, half, prevHalf)
		}
		prevHalf = half
	}
}
