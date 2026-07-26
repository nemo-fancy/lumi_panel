package domain

import "fmt"

// OrderStatus is a node in the order lifecycle (§8.2).
type OrderStatus string

const (
	OrderPending            OrderStatus = "pending"
	OrderPaid               OrderStatus = "paid"
	OrderCompleted          OrderStatus = "completed"
	OrderProvisioningFailed OrderStatus = "provisioning_failed"
	OrderCancelled          OrderStatus = "cancelled"
	OrderRefunded           OrderStatus = "refunded"
)

// orderTransitions is the whole state machine. Anything not listed is refused.
//
// Default-deny is the point: a payment callback that arrives twice, or out of
// order, or for an order that was already cancelled, has to bounce off this
// table rather than fall through into provisioning.
var orderTransitions = map[OrderStatus][]OrderStatus{
	OrderPending: {OrderPaid, OrderCancelled},
	// Provisioning can fail after the money has arrived. That branch must
	// exist: without it the only options are losing the payment or shipping
	// twice.
	OrderPaid:               {OrderCompleted, OrderProvisioningFailed},
	OrderProvisioningFailed: {OrderCompleted, OrderRefunded},
	OrderCompleted:          {OrderRefunded},
	OrderCancelled:          nil,
	OrderRefunded:           nil,
}

// ErrIllegalTransition reports a refused order state change.
type ErrIllegalTransition struct {
	From, To OrderStatus
}

func (e ErrIllegalTransition) Error() string {
	return fmt.Sprintf("domain: illegal order transition %s -> %s", e.From, e.To)
}

// CanTransition reports whether from -> to is permitted.
func CanTransition(from, to OrderStatus) bool {
	for _, allowed := range orderTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// Transition validates a state change, returning ErrIllegalTransition when the
// edge does not exist.
//
// Callers must still hold the order row (SELECT ... FOR UPDATE) across this
// check and the write. The state machine rejects an illegal edge; only the row
// lock rejects two concurrent legal ones (§8.3).
func Transition(from, to OrderStatus) error {
	if !CanTransition(from, to) {
		return ErrIllegalTransition{From: from, To: to}
	}
	return nil
}

// IsKnown reports whether s is a state this machine models.
func IsKnown(s OrderStatus) bool {
	_, ok := orderTransitions[s]
	return ok
}

// IsTerminal reports whether an order can no longer change state.
//
// An unmodelled status is not terminal. Reading the transition map alone would
// make it so -- a missing key yields a nil slice of length zero -- which
// inverts the default-deny intent: a status added to the database CHECK
// without being wired up here would silently become an end state that no
// transition can leave, rather than an error somebody notices.
func IsTerminal(s OrderStatus) bool {
	edges, ok := orderTransitions[s]
	return ok && len(edges) == 0
}

// IsPaid reports whether the money has arrived, regardless of whether
// provisioning has caught up.
//
// A second payment against an order in any of these states is a duplicate
// payment, not a retry: it is credited to the user's balance and flagged
// refundable rather than provisioning a second time (§8.3).
func IsPaid(s OrderStatus) bool {
	switch s {
	case OrderPaid, OrderCompleted, OrderProvisioningFailed, OrderRefunded:
		return true
	default:
		return false
	}
}
