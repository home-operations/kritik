package store

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
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

// ListenHandlers bundles the callbacks Listen drives. OnEvent and OnConfig
// are called synchronously, one at a time, from a single dedicated
// goroutine that also drains the notification buffer between Listen and
// its underlying LISTEN connection (see Listen's doc comment) — so neither
// callback may block for long: a slow callback delays every notification
// queued behind it, and once the buffer fills, newer notifications are
// dropped rather than stalling the LISTEN connection itself. A callback
// that needs to do slow work should hand off to its own goroutine or queue
// rather than doing it inline.
//
// OnReconnect, if set, is invoked after every successful (re)connect after
// the first — never after Listen's initial connection, only after each one
// that follows a disconnect. A NOTIFY sent while no listener was connected
// is lost, so consumers use OnReconnect to re-fetch whatever state they'd
// otherwise have learned about incrementally: e.g. the web SSE broadcaster
// telling browsers to resync, or a config source re-merging tenant specs.
type ListenHandlers struct {
	OnEvent     func(Event)
	OnConfig    func(slug string)
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

// listenBackoff returns how long to wait before reconnect attempt number
// attempt (0-based). It uses "equal jitter": half of the exponential delay
// is guaranteed, and a random amount up to the other half is added, so the
// wait is never near-zero but also never fully predictable.
func listenBackoff(attempt int) time.Duration {
	shift := min(attempt, 5) // 1s<<5 == 32s already exceeds listenBackoffMax; avoid shifting further
	delay := min(listenBackoffMin<<shift, listenBackoffMax)
	half := delay / 2
	return half + time.Duration(rand.Int64N(int64(half)+1))
}

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

// Listen holds one dedicated connection LISTENing on kritik_events and
// kritik_config, reconnecting with backoff on any error, until ctx ends
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
// A malformed kritik_events payload is logged and skipped rather than
// ending the listener. kritik_config payloads are raw tenant slugs, passed
// to OnConfig unparsed.
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
	var reconnecting bool
	for attempt := 0; ; attempt++ {
		err := s.listenOnce(ctx, handlers, notifications, warner, reconnecting)
		if ctx.Err() != nil {
			return
		}
		reconnecting = true
		if err != nil {
			s.logger.Warn("event listener disconnected, retrying", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(listenBackoff(attempt)):
		}
	}
}

// listenOnce opens one dedicated connection off the application pool's
// config and blocks handling notifications on it until ctx ends or the
// connection fails. isReconnect is true once Listen has already connected
// successfully at least once before.
func (s *Store) listenOnce(
	ctx context.Context,
	handlers ListenHandlers,
	notifications chan<- func(),
	warner *dropWarner,
	isReconnect bool,
) error {
	connConfig := s.app.Config().ConnConfig.Copy()
	if connConfig.RuntimeParams == nil {
		connConfig.RuntimeParams = map[string]string{}
	}
	connConfig.RuntimeParams["application_name"] = "kritik-listen"

	conn, err := pgx.ConnectConfig(ctx, connConfig)
	if err != nil {
		return fmt.Errorf("store: listen connect: %w", err)
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = conn.Close(closeCtx)
	}()

	if _, err := conn.Exec(ctx, `LISTEN kritik_events`); err != nil {
		return fmt.Errorf("store: listen kritik_events: %w", err)
	}
	if _, err := conn.Exec(ctx, `LISTEN kritik_config`); err != nil {
		return fmt.Errorf("store: listen kritik_config: %w", err)
	}

	if isReconnect && handlers.OnReconnect != nil {
		handlers.OnReconnect()
	}

	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("store: wait for notification: %w", err)
		}
		var fn func()
		switch n.Channel {
		case "kritik_events":
			event, err := parseEvent(n.Payload)
			if err != nil {
				s.logger.Warn("dropped malformed event notification", "error", err, "payload", n.Payload)
				continue
			}
			if handlers.OnEvent == nil {
				continue
			}
			fn = func() { handlers.OnEvent(event) }
		case "kritik_config":
			if handlers.OnConfig == nil {
				continue
			}
			slug := n.Payload
			fn = func() { handlers.OnConfig(slug) }
		default:
			continue
		}
		select {
		case notifications <- fn:
		default:
			warner.drop()
		}
	}
}
