package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

// EventKind is the kind of row a kritika_events notification describes.
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

// Event is one row change published on the kritika_events channel: a new or
// changed reviews, runner_runs, index_runs, followups or model_calls row.
// ReviewID is nil for a row whose table has no review_id column, or whose
// review_id is NULL.
type Event struct {
	AccountID string
	Kind      EventKind
	ID        string
	ReviewID  *string
}

// eventPayload mirrors the JSON kritika_notify_event() publishes.
type eventPayload struct {
	AccountID string  `json:"account_id"`
	Kind      string  `json:"kind"`
	ID        string  `json:"id"`
	ReviewID  *string `json:"review_id"`
}

// parseEvent decodes one kritika_events notification payload.
func parseEvent(payload string) (Event, error) {
	var p eventPayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return Event{}, fmt.Errorf("store: parse event payload: %w", err)
	}
	kind := EventKind(p.Kind)
	if !kind.Valid() {
		return Event{}, fmt.Errorf("store: parse event payload: unknown kind %q", p.Kind)
	}
	if p.AccountID == "" || p.ID == "" {
		return Event{}, errors.New("store: parse event payload: missing account_id or id")
	}
	return Event{AccountID: p.AccountID, Kind: kind, ID: p.ID, ReviewID: p.ReviewID}, nil
}

// ListenHandlers bundles the callbacks Listen drives. OnEvent and
// OnReconnect are both funneled through the same bounded queue and invoked
// synchronously, one at a time, in the order they were queued, from a
// single dedicated goroutine that drains it (see Listen's doc comment) —
// so none of them may block for long: a slow callback delays every
// notification queued behind it. A callback that needs to do slow work
// should hand off to its own goroutine or queue rather than doing it
// inline.
//
// Once the queue is full, a new OnEvent notification is dropped
// (see dropWarner). OnReconnect is treated as more important than any
// single dropped event — a consumer that misses it can go on serving
// stale state indefinitely — so if the queue is full when OnReconnect is
// due, the oldest queued notification is evicted to make room rather than
// losing the reconnect signal itself (see enqueueReconnect).
//
// OnReconnect, if set, is invoked after every successful (re)connect after
// the first — never after Listen's initial connection, only after each one
// that follows a disconnect that got far enough to reach the notification
// loop (i.e. LISTEN itself succeeded). A NOTIFY sent while no listener was
// connected is lost, so consumers use OnReconnect to re-fetch whatever
// state they'd otherwise have learned about incrementally: e.g. the web
// SSE broadcaster telling browsers to resync.
type ListenHandlers struct {
	OnEvent     func(Event)
	OnReconnect func()
}

// listenBufferSize bounds the queue of notifications waiting for
// ListenHandlers callbacks to run. It only needs to absorb a burst: under
// steady state the consumer goroutine drains it as fast as Postgres can
// deliver notifications.
const listenBufferSize = 1024

// listenBackoffMin and listenBackoffMax bound Listen's reconnect delay.
// The delay grows exponentially between them (doubling per failed
// attempt, capped at listenBackoffMax) and is jittered so that many
// listeners disconnected by the same event (e.g. a Postgres failover)
// don't all reconnect in lockstep.
const (
	listenBackoffMin = 1 * time.Second
	listenBackoffMax = 30 * time.Second
)

// dropWarner logs that the listen buffer is full and notifications are
// being dropped, at most once per second, summarizing how many were
// dropped since the last warning. It is only ever called from Listen's
// single read loop, so it needs no locking of its own.
type dropWarner struct {
	logger     *slog.Logger
	dropped    int
	lastWarnAt time.Time
}

func (w *dropWarner) drop() {
	w.dropped++
	if now := time.Now(); now.Sub(w.lastWarnAt) >= time.Second {
		w.logger.Warn("event listener buffer full, dropping notifications",
			"dropped_since_last_warning", w.dropped)
		w.dropped = 0
		w.lastWarnAt = now
	}
}

// Listen holds one dedicated connection LISTENing on kritika_events,
// reconnecting with backoff on any error, until ctx ends
// (the only condition under which Listen returns).
//
// The connection's read loop never calls a handler directly: it decodes
// each notification and pushes it onto a bounded buffered channel, which a
// separate goroutine drains to invoke ListenHandlers. This keeps a slow or
// stuck callback from stalling WaitForNotification — a LISTENer that stops
// reading blocks Postgres's own NOTIFY queue cleanup, which can eventually
// apply backpressure to unrelated writers. See ListenHandlers for the
// resulting callback contract.
//
// A malformed kritika_events payload is logged and skipped rather than
// ending the listener.
func (s *Store) Listen(ctx context.Context, handlers ListenHandlers) {
	notifications := make(chan func(), listenBufferSize)
	consumerDone := make(chan struct{})
	go func() {
		defer close(consumerDone)
		for {
			select {
			case fn := <-notifications:
				fn()
			case <-ctx.Done():
				return
			}
		}
	}()
	defer func() {
		<-consumerDone
	}()

	warner := &dropWarner{logger: s.logger}
	// connectedOnce stays false until an attempt reaches the loop, so a
	// failed first connection doesn't make the next one look like a
	// reconnect. attempt counts failures since the last connection that
	// reached the loop, so a long-lived connection's drop starts the
	// backoff over.
	var connectedOnce bool
	attempt := 0
	for {
		reachedLoop, err := s.listenOnce(ctx, handlers, notifications, warner, connectedOnce)
		if reachedLoop {
			connectedOnce, attempt = true, 0
		}
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			s.logger.Warn("event listener disconnected, retrying", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(Backoff(attempt, listenBackoffMin, listenBackoffMax)):
		}
		attempt++
	}
}

// listenOnce opens one dedicated connection off the application pool's
// config and blocks handling notifications on it until ctx ends or the
// connection fails. isReconnect is true once a previous attempt has
// already reached the notification loop successfully; it controls whether OnReconnect fires once this
// attempt does the same. The returned bool reports whether this attempt
// itself reached the notification loop, regardless of isReconnect and
// regardless of how the attempt eventually ended.
func (s *Store) listenOnce(
	ctx context.Context,
	handlers ListenHandlers,
	notifications chan func(),
	warner *dropWarner,
	isReconnect bool,
) (reachedLoop bool, err error) {
	connConfig := s.app.Config().ConnConfig.Copy()
	if connConfig.RuntimeParams == nil {
		connConfig.RuntimeParams = map[string]string{}
	}
	connConfig.RuntimeParams["application_name"] = "kritika-listen"

	conn, err := pgx.ConnectConfig(ctx, connConfig)
	if err != nil {
		return false, fmt.Errorf("store: listen connect: %w", err)
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = conn.Close(closeCtx)
	}()

	if _, err := conn.Exec(ctx, `LISTEN kritika_events`); err != nil {
		return false, fmt.Errorf("store: listen kritika_events: %w", err)
	}

	// From here on this attempt counts as having reached the loop,
	// regardless of how WaitForNotification eventually ends.
	reachedLoop = true

	if isReconnect && handlers.OnReconnect != nil {
		enqueueReconnect(notifications, handlers.OnReconnect, warner)
	}

	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return reachedLoop, nil
			}
			return reachedLoop, fmt.Errorf("store: wait for notification: %w", err)
		}
		if n.Channel != "kritika_events" {
			continue
		}
		event, err := parseEvent(n.Payload)
		if err != nil {
			s.logger.Warn("dropped malformed event notification", "error", err, "payload", n.Payload)
			continue
		}
		if handlers.OnEvent == nil {
			continue
		}
		fn := func() { handlers.OnEvent(event) }
		select {
		case notifications <- fn:
		default:
			warner.drop()
		}
	}
}

// enqueueReconnect delivers fn (an OnReconnect callback) through the same
// notification queue as OnEvent/OnConfig, so all three run one at a time,
// in order, on the consumer goroutine. Unlike a regular notification, a
// reconnect signal is not safe to drop silently — a consumer that misses
// it can go on serving stale state indefinitely — so if the queue is
// full, the oldest queued notification is evicted (and counted as a drop)
// to make room. This is only ever called from the single producer
// goroutine that also sends OnEvent/OnConfig closures onto notifications,
// so the evict-then-send is race-free: nothing else competes for the slot
// freed by the eviction.
func enqueueReconnect(notifications chan func(), fn func(), warner *dropWarner) {
	select {
	case notifications <- fn:
		return
	default:
	}
	select {
	case <-notifications:
		warner.drop()
	default:
	}
	notifications <- fn
}
