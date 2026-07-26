package events

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func noop(context.Context, Event) error { return nil }

// TestVerifyWiringDetectsAMissingSideEffect is the real form of the §9.3
// linkage guarantee.
//
// The version this replaces registered a handler for every entry in the
// matrix and then asserted that every entry in the matrix had fired -- the
// table was both the fixture and the oracle, so it could only fail if the bus
// stopped dispatching at all. It could never fail for the reason it existed:
// production wiring omitting one of the fan-out paths.
//
// VerifyWiring moves the check to the running registry, where the real
// subscriptions live, and this test checks the checker.
func TestVerifyWiringDetectsAMissingSideEffect(t *testing.T) {
	for evt, required := range RequiredSubscribers {
		for _, omitted := range required {
			b := NewBus(discardLogger())
			for e, names := range RequiredSubscribers {
				for _, name := range names {
					if e == evt && name == omitted {
						continue
					}
					b.Subscribe(e, name, noop)
				}
			}

			errs := b.VerifyWiring()
			if len(errs) != 1 {
				t.Fatalf("omitting %s/%s produced %d errors, want exactly 1: %v", evt, omitted, len(errs), errs)
			}
			if !strings.Contains(errs[0].Error(), omitted) {
				t.Errorf("the error does not name the missing subscriber: %v", errs[0])
			}
		}
	}
}

// TestVerifyWiringPassesWhenComplete is the other half: a fully wired bus must
// start.
func TestVerifyWiringPassesWhenComplete(t *testing.T) {
	b := NewBus(discardLogger())
	for evt, names := range RequiredSubscribers {
		for _, name := range names {
			b.Subscribe(evt, name, noop)
		}
	}
	if errs := b.VerifyWiring(); len(errs) != 0 {
		t.Fatalf("a complete wiring reported %v", errs)
	}
}

// TestPublishReachesEverySubscriber checks dispatch itself, which is all the
// old matrix test was ever actually verifying.
func TestPublishReachesEverySubscriber(t *testing.T) {
	b := NewBus(discardLogger())
	var fired sync.Map

	for _, name := range RequiredSubscribers[UserBanned] {
		name := name
		b.Subscribe(UserBanned, name, func(context.Context, Event) error {
			fired.Store(name, true)
			return nil
		})
	}

	if errs := b.Publish(context.Background(), Event{Type: UserBanned, UserID: 1}); len(errs) != 0 {
		t.Fatalf("publish returned %v", errs)
	}
	for _, name := range RequiredSubscribers[UserBanned] {
		if _, ok := fired.Load(name); !ok {
			t.Errorf("%q did not run", name)
		}
	}
}

func TestSubscribersReportsRegistrationOrder(t *testing.T) {
	b := NewBus(discardLogger())
	b.Subscribe(UserBanned, "first", noop)
	b.Subscribe(UserBanned, "second", noop)

	got := b.Subscribers(UserBanned)
	if len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Errorf("Subscribers = %v, want [first second]", got)
	}
}

// TestDuplicateSubscriptionPanics makes a wiring mistake loud. The alternative
// -- one side effect firing twice per event, so a user gets two emails and two
// audit rows -- is far harder to notice than a refusal to start.
func TestDuplicateSubscriptionPanics(t *testing.T) {
	b := NewBus(discardLogger())
	b.Subscribe(UserBanned, "notify.user", noop)

	defer func() {
		if recover() == nil {
			t.Error("registering the same subscriber twice was accepted")
		}
	}()
	b.Subscribe(UserBanned, "notify.user", noop)
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

// TestPanicIsIsolated covers the failure mode that isolation-by-error-return
// misses.
//
// A panic is how a Go handler fails in practice: a nil map, a nil client on a
// config that did not load. Without containment it abandons the rest of the
// fan-out midway -- the ban is committed, the node-plane revocation never
// happens, the audit row is never written -- and the admin sees a 500 and
// retries, producing a second partial fan-out.
func TestPanicIsIsolated(t *testing.T) {
	b := NewBus(discardLogger())

	var ran []string
	b.Subscribe(UserBanned, "notify.user", func(context.Context, Event) error {
		var m map[string]string
		m["boom"] = "nil map write"
		return nil
	})
	b.Subscribe(UserBanned, "nodeplane.revoke", func(context.Context, Event) error {
		ran = append(ran, "nodeplane.revoke")
		return nil
	})
	b.Subscribe(UserBanned, "audit.record", func(context.Context, Event) error {
		ran = append(ran, "audit.record")
		return nil
	})

	var errs []error
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("the panic escaped Publish: %v", r)
			}
		}()
		errs = b.Publish(context.Background(), Event{Type: UserBanned, UserID: 1})
	}()

	if len(errs) != 1 {
		t.Fatalf("got %d errors, want the panic reported as one", len(errs))
	}
	if !strings.Contains(errs[0].Error(), "panicked") {
		t.Errorf("the error does not identify a panic: %v", errs[0])
	}
	if len(ran) != 2 {
		t.Errorf("subscribers after the panicking one did not run: %v", ran)
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

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		i := i
		wg.Add(2)
		go func() { defer wg.Done(); b.Subscribe(UserBanned, fmt.Sprintf("sub-%d", i), noop) }()
		go func() { defer wg.Done(); b.Publish(context.Background(), Event{Type: UserBanned}) }()
	}
	wg.Wait()
}

// TestHandlerMaySubscribe checks that dispatching outside the lock leaves the
// bus reentrant. A subscriber that registers another subscriber, or publishes
// a follow-up event, must not deadlock.
func TestHandlerMaySubscribe(t *testing.T) {
	b := NewBus(discardLogger())

	done := make(chan struct{})
	b.Subscribe(UserBanned, "reentrant", func(ctx context.Context, e Event) error {
		b.Subscribe(NoticePublished, "late", noop)
		b.Subscribers(UserBanned)
		b.Publish(ctx, Event{Type: NoticePublished})
		close(done)
		return nil
	})

	b.Publish(context.Background(), Event{Type: UserBanned})
	<-done
}
