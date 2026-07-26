// Package events is the only sanctioned exit for side effects (§9).
//
// The reason it exists is a failure mode, not an abstraction preference.
// Handlers in the competing panels update a table, clear a cache and send a
// notification inline; miss any one of the three and the system is
// inconsistent, and every new admin action re-implements the same three steps.
// That is where "I banned them and they can still connect" comes from, and why
// the standard answer in this ecosystem is "clear your cache".
//
// Here a handler validates, writes its primary state, and publishes. Fan-out
// is the bus's problem.
//
// This is an architectural decision, not a feature, and it cannot be deferred
// to v1.1: retrofitting it means rewriting every admin action written before
// it, and the inconsistencies shipped in the meantime are permanent.
package events

import (
	"context"
	"log/slog"
	"sync"
)

// Type names a domain event.
type Type string

const (
	UserBanned          Type = "user.banned"
	UserUnbanned        Type = "user.unbanned"
	SubscriptionChanged Type = "subscription.changed"
	TrafficReset        Type = "traffic.reset"
	QuotaAdjusted       Type = "quota.adjusted"
	NodeStateChanged    Type = "node.state_changed"
	NodeRateChanged     Type = "node.rate_changed"
	NodeUnhealthy       Type = "node.unhealthy"
	PlanVisibility      Type = "plan.visibility_changed"
	OrderProvisioned    Type = "order.provisioned"
	OrderRefunded       Type = "order.refunded"
	SubTokenReset       Type = "subscription.token_reset"
	NoticePublished     Type = "notice.published"
)

// Event is one published fact.
type Event struct {
	Type       Type
	UserID     int64
	TargetID   int64
	OperatorID int64
	Reason     string
	Payload    map[string]any
}

// Handler consumes an event.
//
// Two obligations, both load-bearing (§9.3). A handler must be idempotent,
// because delivery retries. And it must fail in isolation: a notification that
// cannot be sent does not roll back the ban that triggered it. Returning an
// error schedules a retry for that handler alone.
type Handler func(ctx context.Context, e Event) error

// subscription pairs a handler with a name used in logs and in the linkage
// tests.
type subscription struct {
	name string
	fn   Handler
}

// Bus dispatches events to registered subscribers.
type Bus struct {
	mu   sync.RWMutex
	subs map[Type][]subscription
	log  *slog.Logger
}

// NewBus builds an empty bus.
func NewBus(log *slog.Logger) *Bus {
	if log == nil {
		log = slog.Default()
	}
	return &Bus{subs: make(map[Type][]subscription), log: log}
}

// Subscribe registers a named handler for a type.
//
// The name is not decoration: the linkage matrix test asserts, per event, that
// every side effect named in §9.2 is registered. That assertion is the only
// reliable guard against a new admin action quietly missing one of its
// fan-out paths.
func (b *Bus) Subscribe(t Type, name string, fn Handler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs[t] = append(b.subs[t], subscription{name: name, fn: fn})
}

// Subscribers lists the registered handler names for a type, in registration
// order. Used by the linkage tests.
func (b *Bus) Subscribers(t Type) []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	names := make([]string, 0, len(b.subs[t]))
	for _, s := range b.subs[t] {
		names = append(names, s.name)
	}
	return names
}

// Publish delivers an event to every subscriber.
//
// Every handler runs even if an earlier one failed, and the failures are
// collected rather than propagated: the caller has already committed the
// primary state change, so there is nothing sensible for it to do with a
// notification error. Failures are logged and, in the production wiring,
// handed to the retry queue.
func (b *Bus) Publish(ctx context.Context, e Event) []error {
	b.mu.RLock()
	subs := make([]subscription, len(b.subs[e.Type]))
	copy(subs, b.subs[e.Type])
	b.mu.RUnlock()

	var errs []error
	for _, s := range subs {
		if err := s.fn(ctx, e); err != nil {
			errs = append(errs, err)
			b.log.LogAttrs(ctx, slog.LevelError, "event subscriber failed",
				slog.String("event", string(e.Type)),
				slog.String("subscriber", s.name),
				slog.String("error", err.Error()),
			)
		}
	}
	return errs
}
