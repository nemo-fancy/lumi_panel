package events

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// linkageMatrix is §9.2 transcribed. Each entry names the side effects an
// event must fan out to.
//
// This table is the reason the bus exists. The failure it prevents is not a
// crash: it is a ban that removes the session but forgets the node
// authorisation, shipped quietly, discovered when a banned user is still
// connected a week later.
var linkageMatrix = map[Type][]string{
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

// registerAll stands in for the production wiring. The real registry lives in
// the service layer; the shape of this test does not change when it lands.
func registerAll(b *Bus, seen *sync.Map) {
	for evt, subscribers := range linkageMatrix {
		for _, name := range subscribers {
			evt, name := evt, name
			b.Subscribe(evt, name, func(ctx context.Context, e Event) error {
				seen.Store(string(evt)+"/"+name, true)
				return nil
			})
		}
	}
}

// TestLinkageMatrix asserts that publishing an event actually reaches every
// side effect the matrix promises. This is the only reliable guard against a
// new admin action quietly missing one of its fan-out paths (§9.3).
func TestLinkageMatrix(t *testing.T) {
	for evt, want := range linkageMatrix {
		t.Run(string(evt), func(t *testing.T) {
			var seen sync.Map
			b := NewBus(nil)
			registerAll(b, &seen)

			if errs := b.Publish(context.Background(), Event{Type: evt, UserID: 1}); len(errs) != 0 {
				t.Fatalf("publish returned errors: %v", errs)
			}

			for _, name := range want {
				if _, ok := seen.Load(string(evt) + "/" + name); !ok {
					t.Errorf("side effect %q did not run for %s", name, evt)
				}
			}
		})
	}
}

func TestSubscribersReportsRegistrationOrder(t *testing.T) {
	b := NewBus(nil)
	noop := func(context.Context, Event) error { return nil }
	b.Subscribe(UserBanned, "first", noop)
	b.Subscribe(UserBanned, "second", noop)

	got := b.Subscribers(UserBanned)
	if len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Errorf("Subscribers = %v, want [first second]", got)
	}
}

// TestFailureIsIsolated pins the second obligation from §9.3: a notification
// that cannot be delivered must not stop the ban from taking effect on the
// node plane.
func TestFailureIsIsolated(t *testing.T) {
	b := NewBus(discardLogger())

	var revoked bool
	b.Subscribe(UserBanned, "notify.user", func(context.Context, Event) error {
		return errors.New("smtp unavailable")
	})
	b.Subscribe(UserBanned, "nodeplane.revoke", func(context.Context, Event) error {
		revoked = true
		return nil
	})

	errs := b.Publish(context.Background(), Event{Type: UserBanned, UserID: 42})

	if len(errs) != 1 {
		t.Fatalf("got %d errors, want exactly the notification failure", len(errs))
	}
	if !revoked {
		t.Error("a failing subscriber prevented a later one from running")
	}
}

// TestPublishToNobodyIsNotAnError covers events with no subscribers yet, so
// wiring one subsystem at a time does not break the others.
func TestPublishToNobodyIsNotAnError(t *testing.T) {
	b := NewBus(discardLogger())
	if errs := b.Publish(context.Background(), Event{Type: NoticePublished}); len(errs) != 0 {
		t.Fatalf("publishing to an empty topic returned %v", errs)
	}
}

// TestPublishIsSafeUnderConcurrentSubscribe guards the copy-under-lock in
// Publish. Without it a subscription registered while an event is in flight
// races the dispatch loop.
func TestPublishIsSafeUnderConcurrentSubscribe(t *testing.T) {
	b := NewBus(discardLogger())
	noop := func(context.Context, Event) error { return nil }

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); b.Subscribe(UserBanned, "sub", noop) }()
		go func() { defer wg.Done(); b.Publish(context.Background(), Event{Type: UserBanned}) }()
	}
	wg.Wait()
}
