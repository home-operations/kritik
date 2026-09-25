package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// EventKind is the kind of row a kritik_events notification describes.
type EventKind string

const (
	EventReview    EventKind = "review"
	EventRunnerRun EventKind = "runner_run"
	EventIndexRun  EventKind = "index_run"
	EventFollowup  EventKind = "followup"
	EventModelCall EventKind = "model_call"
)

// Valid reports whether k is one of the known event kinds.
func (k EventKind) Valid() bool {
	switch k {
	case EventReview, EventRunnerRun, EventIndexRun, EventFollowup, EventModelCall:
		return true
	}
	return false
}

func (k EventKind) String() string { return string(k) }

// Event is one row change published on the kritik_events channel: a new or
// changed reviews, runner_runs, index_runs, followups or model_calls row.
// ReviewID is nil for a row whose table has no review_id column, or whose
// review_id is NULL.
type Event struct {
	TenantID string
	Kind     EventKind
	ID       string
	ReviewID *string
}

// eventPayload mirrors the JSON kritik_notify_event() publishes.
type eventPayload struct {
	TenantID string  `json:"tenant_id"`
	Kind     string  `json:"kind"`
	ID       string  `json:"id"`
	ReviewID *string `json:"review_id"`
}

// parseEvent decodes one kritik_events notification payload.
func parseEvent(payload string) (Event, error) {
	var p eventPayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return Event{}, fmt.Errorf("store: parse event payload: %w", err)
	}
	kind := EventKind(p.Kind)
	if !kind.Valid() {
		return Event{}, fmt.Errorf("store: parse event payload: unknown kind %q", p.Kind)
	}
	if p.TenantID == "" || p.ID == "" {
		return Event{}, fmt.Errorf("store: parse event payload: missing tenant_id or id")
	}
	return Event{TenantID: p.TenantID, Kind: kind, ID: p.ID, ReviewID: p.ReviewID}, nil
}

// listenRetry is how long Listen waits before reconnecting after its
// connection is lost.
const listenRetry = 5 * time.Second

// Listen holds one connection LISTENing on kritik_events and kritik_config
// until ctx ends, reconnecting with backoff on any error. Events decode to
// onEvent; a malformed kritik_events payload is logged and skipped rather
// than ending the listener. kritik_config payloads are raw tenant slugs,
// passed to onConfig unparsed.
func (s *Store) Listen(ctx context.Context, onEvent func(Event), onConfig func(slug string)) error {
	for {
		err := s.listenOnce(ctx, onEvent, onConfig)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			s.logger.Warn("event listener disconnected, retrying", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(listenRetry):
		}
	}
}

// listenOnce opens one dedicated connection off the application pool's
// config and blocks handling notifications on it until ctx ends or the
// connection fails.
func (s *Store) listenOnce(ctx context.Context, onEvent func(Event), onConfig func(slug string)) error {
	conn, err := pgx.ConnectConfig(ctx, s.app.Config().ConnConfig.Copy())
	if err != nil {
		return fmt.Errorf("store: listen connect: %w", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()

	if _, err := conn.Exec(ctx, `LISTEN kritik_events`); err != nil {
		return fmt.Errorf("store: listen kritik_events: %w", err)
	}
	if _, err := conn.Exec(ctx, `LISTEN kritik_config`); err != nil {
		return fmt.Errorf("store: listen kritik_config: %w", err)
	}

	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("store: wait for notification: %w", err)
		}
		switch n.Channel {
		case "kritik_events":
			event, err := parseEvent(n.Payload)
			if err != nil {
				s.logger.Warn("dropped malformed event notification", "error", err, "payload", n.Payload)
				continue
			}
			onEvent(event)
		case "kritik_config":
			onConfig(n.Payload)
		}
	}
}
