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
	"fmt"
	"log/slog"
	"runtime/debug"
	"sort"
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
// because delivery is retried. And it must fail in isolation: a notification
// that cannot be sent does not roll back the ban that triggered it.
//
// A returned error is collected and logged, and every other subscriber still
// runs. Retry itself is the caller's job -- the production wiring hands the
// failures to the asynq queue. Publish does not schedule anything.
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
// The name is not decoration: VerifyWiring checks the registered names against
// the §9.2 linkage matrix at startup, which is what stops a new admin action
// from quietly missing one of its fan-out paths.
//
// Registering the same name twice for the same event panics. A duplicate is
// always a wiring mistake, and the consequence -- one side effect firing twice
// per event, so a user gets two emails and two audit rows -- is much harder to
// notice than a failure to start.
func (b *Bus) Subscribe(t Type, name string, fn Handler) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for _, s := range b.subs[t] {
		if s.name == name {
			panic(fmt.Sprintf("events: %q is already subscribed to %s", name, t))
		}
	}
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
//
// A handler that panics is contained the same way one that returns an error
// is. A panic is how a Go handler fails in practice -- a nil map, a nil
// pointer on a config that was not loaded -- and without containment it would
// abandon the rest of the fan-out midway: the ban is already committed, the
// node-plane revocation never happens, the audit record is never written, and
// the admin sees a 500 and retries, producing a second partial fan-out. That
// is precisely the inconsistency this package exists to prevent, arriving
// through the one failure mode that was not isolated.
func (b *Bus) Publish(ctx context.Context, e Event) []error {
	b.mu.RLock()
	subs := make([]subscription, len(b.subs[e.Type]))
	copy(subs, b.subs[e.Type])
	b.mu.RUnlock()

	var errs []error
	for _, s := range subs {
		if err := b.deliver(ctx, s, e); err != nil {
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

// deliver runs one subscriber, converting a panic into an error.
func (b *Bus) deliver(ctx context.Context, s subscription, e Event) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("subscriber %q panicked: %v\n%s", s.name, r, debug.Stack())
		}
	}()
	return s.fn(ctx, e)
}

// RequiredSubscribers is the linkage matrix of §9.2: for each event, the side
// effects that must be wired up before the process is allowed to serve.
//
// It is a declaration, not documentation. VerifyWiring checks the running
// registry against it at startup, so a new admin action that forgets one of
// its fan-out paths stops the process instead of shipping an inconsistency.
var RequiredSubscribers = map[Type][]string{
	UserBanned:          {"nodeplane.revoke", "auth.revoke_sessions", "subplane.invalidate", "notify.user", "audit.record"},
	UserUnbanned:        {"nodeplane.grant", "notify.user", "audit.record"},
	SubscriptionChanged: {"nodeplane.sync_groups", "metering.update_ladder", "subplane.invalidate", "notify.user", "audit.record"},
	TrafficReset:        {"nodeplane.grant", "metering.reset_ladder", "notify.user", "audit.record"},
	QuotaAdjusted:       {"metering.update_ladder", "audit.record"},
	NodeStateChanged:    {"nodeplane.push_config", "subplane.invalidate", "audit.record"},
	NodeRateChanged:     {"metering.rate_snapshot", "subplane.invalidate", "audit.record"},
	NodeUnhealthy:       {"subplane.invalidate", "admin.alert"},
	PlanVisibility:      {"audit.record"},
	OrderProvisioned:    {"nodeplane.grant", "metering.update_ladder", "notify.user", "audit.record"},
	OrderRefunded:       {"nodeplane.revoke", "metering.update_ladder", "notify.user", "audit.record"},
	SubTokenReset:       {"subplane.invalidate", "notify.user", "audit.record"},
	NoticePublished:     {"audit.record"},
}

// VerifyWiring reports every subscriber that RequiredSubscribers demands and
// the registry does not have.
//
// Called once at startup, after wiring and before the listener opens. The
// design asks for a test that asserts each event reaches all of its side
// effects, and a test alone cannot do that honestly: a test that registers the
// handlers it then looks for is checking its own fixture. Only the real
// registry can be checked against the matrix, and only at run time.
func (b *Bus) VerifyWiring() []error {
	b.mu.RLock()
	defer b.mu.RUnlock()

	var missing []error
	for _, t := range sortedTypes(RequiredSubscribers) {
		registered := make(map[string]bool, len(b.subs[t]))
		for _, s := range b.subs[t] {
			registered[s.name] = true
		}
		for _, name := range RequiredSubscribers[t] {
			if !registered[name] {
				missing = append(missing, fmt.Errorf("event %s has no subscriber %q", t, name))
			}
		}
	}
	return missing
}

// sortedTypes keeps VerifyWiring's output stable across runs.
func sortedTypes(m map[Type][]string) []Type {
	out := make([]Type, 0, len(m))
	for t := range m {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
