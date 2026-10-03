package store

import (
	"log/slog"
	"slices"
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
			payload: `{"account_id":"a1f6c9d0-0000-0000-0000-000000000001","kind":"review","id":"` + reviewID + `","review_id":"` + reviewID + `"}`,
			want: Event{
				AccountID: "a1f6c9d0-0000-0000-0000-000000000001",
				Kind:      EventReview,
				ID:        reviewID,
				ReviewID:  &reviewID,
			},
		},
		{
			name:    "index_run with null review_id",
			payload: `{"account_id":"a1f6c9d0-0000-0000-0000-000000000001","kind":"index_run","id":"c1f6c9d0-0000-0000-0000-000000000001","review_id":null}`,
			want: Event{
				AccountID: "a1f6c9d0-0000-0000-0000-000000000001",
				Kind:      EventIndexRun,
				ID:        "c1f6c9d0-0000-0000-0000-000000000001",
				ReviewID:  nil,
			},
		},
		{
			name:    "unknown kind",
			payload: `{"account_id":"a1f6c9d0-0000-0000-0000-000000000001","kind":"bogus","id":"c1f6c9d0-0000-0000-0000-000000000001"}`,
			wantErr: true,
		},
		{
			name:    "missing account_id",
			payload: `{"kind":"review","id":"` + reviewID + `"}`,
			wantErr: true,
		},
		{
			name:    "missing id",
			payload: `{"account_id":"a1f6c9d0-0000-0000-0000-000000000001","kind":"review"}`,
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
			if got.AccountID != tt.want.AccountID || got.Kind != tt.want.Kind || got.ID != tt.want.ID {
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

func TestEnqueueReconnect(t *testing.T) {
	t.Run("delivers immediately when the queue has room", func(t *testing.T) {
		notifications := make(chan func(), 2)
		warner := &dropWarner{logger: slog.New(slog.DiscardHandler)}

		called := false
		enqueueReconnect(notifications, func() { called = true }, warner)

		select {
		case fn := <-notifications:
			fn()
		default:
			t.Fatal("expected the reconnect callback to be queued")
		}
		if !called {
			t.Error("queued callback was not the reconnect callback")
		}
		if warner.dropped != 0 {
			t.Errorf("dropped = %d, want 0 (room was available, nothing should be evicted)", warner.dropped)
		}
	})

	t.Run("evicts the oldest queued item when full rather than dropping the reconnect signal", func(t *testing.T) {
		notifications := make(chan func(), 1)
		// lastWarnAt starts non-zero so drop()'s once-per-second warning does
		// not fire (and reset the counter) on this very first call, letting
		// the assertion below observe the increment.
		warner := &dropWarner{logger: slog.New(slog.DiscardHandler), lastWarnAt: time.Now()}

		oldestRan := false
		notifications <- func() { oldestRan = true } // fill the queue

		reconnectRan := false
		enqueueReconnect(notifications, func() { reconnectRan = true }, warner)

		if len(notifications) != 1 {
			t.Fatalf("len(notifications) = %d, want 1 (evict-then-send should leave exactly the reconnect callback queued)", len(notifications))
		}
		(<-notifications)()
		if oldestRan {
			t.Error("evicted callback ran; it should have been discarded, not invoked")
		}
		if !reconnectRan {
			t.Error("reconnect callback was not delivered after eviction")
		}
		if warner.dropped != 1 {
			t.Errorf("dropped = %d, want 1 (the evicted item should be counted as a drop)", warner.dropped)
		}
	})
}

// TestDeliverResyncsAfterADrop: a full queue drops the event and queues one
// resync for the whole run of drops, and a later run queues another.
func TestDeliverResyncsAfterADrop(t *testing.T) {
	notifications := make(chan func(), 2)
	warner := &dropWarner{logger: slog.New(slog.DiscardHandler), lastWarnAt: time.Now()}
	var ran []string
	event := func(name string) func() { return func() { ran = append(ran, name) } }
	resync := func() { ran = append(ran, "resync") }
	drain := func() {
		for len(notifications) > 0 {
			(<-notifications)()
		}
	}

	dropping := false
	for _, name := range []string{"a", "b", "c", "d"} {
		dropping = deliver(notifications, event(name), resync, dropping, warner)
	}
	if !dropping {
		t.Fatal("the events past the queue's room were not reported dropped")
	}
	drain()
	// c's drop evicted a for the resync; d's found one already waiting.
	if want := []string{"b", "resync"}; !slices.Equal(ran, want) {
		t.Fatalf("ran = %v, want %v", ran, want)
	}

	ran = nil
	if dropping = deliver(notifications, event("e"), resync, dropping, warner); dropping {
		t.Fatal("an event with room in the queue was dropped")
	}
	for _, name := range []string{"f", "g"} {
		dropping = deliver(notifications, event(name), resync, dropping, warner)
	}
	drain()
	if want := []string{"f", "resync"}; !slices.Equal(ran, want) {
		t.Fatalf("ran = %v, want %v: a new run of drops queues its own resync", ran, want)
	}

	if deliver(notifications, event("h"), nil, false, warner); len(notifications) != 1 {
		t.Fatalf("queue holds %d, want the one event", len(notifications))
	}
}
