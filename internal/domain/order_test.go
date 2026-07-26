package domain

import "testing"

// TestOrderTransitionMatrix enumerates every ordered pair of states, so a new
// state added without wiring its edges fails here rather than in production.
func TestOrderTransitionMatrix(t *testing.T) {
	all := []OrderStatus{
		OrderPending, OrderPaid, OrderCompleted,
		OrderProvisioningFailed, OrderCancelled, OrderRefunded,
	}

	legal := map[OrderStatus]map[OrderStatus]bool{
		OrderPending:            {OrderPaid: true, OrderCancelled: true},
		OrderPaid:               {OrderCompleted: true, OrderProvisioningFailed: true},
		OrderProvisioningFailed: {OrderCompleted: true, OrderRefunded: true},
		OrderCompleted:          {OrderRefunded: true},
		OrderCancelled:          {},
		OrderRefunded:           {},
	}

	for _, from := range all {
		for _, to := range all {
			want := legal[from][to]
			if got := CanTransition(from, to); got != want {
				t.Errorf("CanTransition(%s, %s) = %v, want %v", from, to, got, want)
			}
		}
	}
}

// TestPaymentCannotResurrectACancelledOrder is the case a callback arriving
// late produces: the user cancelled, then the channel notified success.
// Provisioning here would ship a product against an order nobody expects.
func TestPaymentCannotResurrectACancelledOrder(t *testing.T) {
	if err := Transition(OrderCancelled, OrderPaid); err == nil {
		t.Fatal("a cancelled order accepted a payment")
	}
	if err := Transition(OrderRefunded, OrderPaid); err == nil {
		t.Fatal("a refunded order accepted a payment")
	}
}

// TestProvisioningFailureIsRecoverable covers the branch that exists because
// money can arrive and provisioning can still fail. Without it the only
// options are keeping the payment without delivering, or delivering twice.
func TestProvisioningFailureIsRecoverable(t *testing.T) {
	if err := Transition(OrderPaid, OrderProvisioningFailed); err != nil {
		t.Fatalf("paid -> provisioning_failed rejected: %v", err)
	}
	if err := Transition(OrderProvisioningFailed, OrderCompleted); err != nil {
		t.Fatalf("retry after provisioning failure rejected: %v", err)
	}
	if err := Transition(OrderProvisioningFailed, OrderRefunded); err != nil {
		t.Fatalf("manual refund after provisioning failure rejected: %v", err)
	}
}

func TestIsTerminal(t *testing.T) {
	terminal := map[OrderStatus]bool{
		OrderCancelled: true, OrderRefunded: true,
		OrderPending: false, OrderPaid: false,
		OrderCompleted: false, OrderProvisioningFailed: false,
	}
	for s, want := range terminal {
		if got := IsTerminal(s); got != want {
			t.Errorf("IsTerminal(%s) = %v, want %v", s, got, want)
		}
	}
}

// TestIsPaidCoversEveryPostPaymentState guards duplicate-payment handling. A
// second payment is a surplus, not a retry, in every state where money has
// already arrived -- including provisioning_failed, which is exactly the state
// a user retries payment from when they think their order is stuck.
func TestIsPaidCoversEveryPostPaymentState(t *testing.T) {
	paid := map[OrderStatus]bool{
		OrderPending: false, OrderCancelled: false,
		OrderPaid: true, OrderCompleted: true,
		OrderProvisioningFailed: true, OrderRefunded: true,
	}
	for s, want := range paid {
		if got := IsPaid(s); got != want {
			t.Errorf("IsPaid(%s) = %v, want %v", s, got, want)
		}
	}
}

func TestTransitionErrorNamesBothStates(t *testing.T) {
	err := Transition(OrderCompleted, OrderPending)
	if err == nil {
		t.Fatal("expected an error")
	}
	var e ErrIllegalTransition
	if !asIllegal(err, &e) {
		t.Fatalf("got %T, want ErrIllegalTransition", err)
	}
	if e.From != OrderCompleted || e.To != OrderPending {
		t.Errorf("error carries %s -> %s, want completed -> pending", e.From, e.To)
	}
}

func asIllegal(err error, target *ErrIllegalTransition) bool {
	e, ok := err.(ErrIllegalTransition)
	if ok {
		*target = e
	}
	return ok
}

// TestUnknownStatusIsNotTerminal covers the default-deny intent. Reading the
// transition map alone makes an unmodelled status terminal, because a missing
// key yields a nil slice -- so a status added to the database CHECK without
// being wired up here would silently become an end state nobody can leave.
func TestUnknownStatusIsNotTerminal(t *testing.T) {
	for _, s := range []OrderStatus{"", "disputed", "chargeback", "PENDING"} {
		if IsKnown(s) {
			t.Errorf("IsKnown(%q) = true, want false", s)
		}
		if IsTerminal(s) {
			t.Errorf("IsTerminal(%q) = true; an unmodelled status must not read as an end state", s)
		}
	}

	for _, s := range []OrderStatus{OrderPending, OrderPaid, OrderCompleted, OrderProvisioningFailed, OrderCancelled, OrderRefunded} {
		if !IsKnown(s) {
			t.Errorf("IsKnown(%s) = false", s)
		}
	}
}
