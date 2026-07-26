package domain

import (
	"errors"
	"time"
)

// ChangePolicy is the plan-change semantics an operator configures per plan
// (§8.4).
type ChangePolicy string

const (
	// ChangeReset restarts the period from the change date and replaces the
	// allowance outright.
	ChangeReset ChangePolicy = "reset"
	// ChangeProrate keeps the original expiry and charges the difference for
	// the days that remain.
	ChangeProrate ChangePolicy = "prorate"
	// ChangeStack appends a fresh period and adds the allowance on top.
	ChangeStack ChangePolicy = "stack"
	// ChangeDeny permits a change only once the current plan has expired.
	ChangeDeny ChangePolicy = "deny"
)

// ErrChangeDenied reports a plan change refused by a deny-policy plan that has
// not expired yet.
var ErrChangeDenied = errors.New("domain: plan change denied until current plan expires")

// PlanSnapshot is the subset of a plan needed to price a change. It is a
// snapshot because prices move, and a change must be priced against what the
// user actually bought.
type PlanSnapshot struct {
	ID            int64
	TransferBytes int64
	PeriodDays    int
	Price         Money // price for one whole period
}

// ChangeRequest describes a requested move from a live subscription to a new
// plan.
type ChangeRequest struct {
	Policy       ChangePolicy
	Old          PlanSnapshot
	New          PlanSnapshot
	OldExpiredAt time.Time
	At           time.Time
	// Loc is the operator's billing timezone. Nil is treated as UTC.
	Loc *time.Location
}

// ChangeMode says how the subscriptions table must be manipulated.
type ChangeMode string

const (
	// ModeReplace ends the current primary and starts a new one.
	//
	// The order of those two writes is not a matter of taste. The partial
	// unique index uq_sub_one_primary allows exactly one active primary per
	// user, so inserting the replacement before expiring the incumbent fails
	// the constraint. Both statements must run inside one transaction, expire
	// first, insert second.
	ModeReplace ChangeMode = "replace"

	// ModeExtend mutates the existing primary row in place.
	//
	// Stacking must not insert a row. Every renewal would otherwise leave an
	// expired subscription behind, and the ladder would re-scan that growing
	// tail on every Flusher hydrate for no benefit.
	ModeExtend ChangeMode = "extend"
)

// ChangeOutcome is the decision, not the execution. The service layer applies
// it; keeping the arithmetic here means it can be tested without a database.
type ChangeOutcome struct {
	Mode          ChangeMode
	ExpiredAt     time.Time
	TransferBytes int64
	// UsedBytes is the counter the resulting subscription starts from.
	UsedBytes int64
	// Charge is what the user owes, never negative.
	Charge Money
	// Credit is the surplus from a downgrade. It is credited to the user's
	// balance rather than refunded to the payment channel: refunding a
	// difference the user never paid in cash is a cash-out path, not a
	// refund (§8.4).
	Credit Money
}

// RemainingDays counts whole days from at until expiry, measured against local
// day boundaries in loc.
//
// Whole days rather than seconds, deliberately. Second-level proration
// produces amounts a user cannot reproduce by hand, and every one of those
// becomes a ticket. Nil loc is treated as UTC.
func RemainingDays(at, expiredAt time.Time, loc *time.Location) int {
	if loc == nil {
		loc = time.UTC
	}
	a := at.In(loc)
	e := expiredAt.In(loc)
	aMid := time.Date(a.Year(), a.Month(), a.Day(), 0, 0, 0, 0, loc)
	eMid := time.Date(e.Year(), e.Month(), e.Day(), 0, 0, 0, 0, loc)

	days := int(eMid.Sub(aMid).Hours() / 24)
	if days < 0 {
		return 0
	}
	return days
}

// ApplyChange prices and shapes a plan change.
//
// Rounding always favours the user: value gained is floored, value surrendered
// is ceilinged. This matches the floor-toward-the-user rule used for billed
// traffic (§7.5), so the two never disagree about which direction "in the
// user's favour" points.
func ApplyChange(req ChangeRequest) (ChangeOutcome, error) {
	if req.New.PeriodDays <= 0 || req.Old.PeriodDays <= 0 {
		return ChangeOutcome{}, errors.New("domain: plan period must be positive")
	}
	loc := req.Loc
	if loc == nil {
		loc = time.UTC
	}

	switch req.Policy {
	case ChangeDeny:
		if req.OldExpiredAt.After(req.At) {
			return ChangeOutcome{}, ErrChangeDenied
		}
		fallthrough

	case ChangeReset:
		return ChangeOutcome{
			Mode:          ModeReplace,
			ExpiredAt:     req.At.AddDate(0, 0, req.New.PeriodDays),
			TransferBytes: req.New.TransferBytes,
			UsedBytes:     0,
			Charge:        req.New.Price,
		}, nil

	case ChangeStack:
		base := req.OldExpiredAt
		if base.Before(req.At) {
			base = req.At
		}
		return ChangeOutcome{
			Mode:      ModeExtend,
			ExpiredAt: base.AddDate(0, 0, req.New.PeriodDays),
			// Extend keeps the existing counter: stacking adds allowance, it
			// does not forgive what has already been spent.
			TransferBytes: req.New.TransferBytes,
			Charge:        req.New.Price,
		}, nil

	case ChangeProrate:
		remaining := RemainingDays(req.At, req.OldExpiredAt, loc)

		gained := floorDiv(int64(req.New.Price)*int64(remaining), int64(req.New.PeriodDays))
		surrendered := ceilDiv(int64(req.Old.Price)*int64(remaining), int64(req.Old.PeriodDays))
		diff := Money(gained - surrendered)

		out := ChangeOutcome{
			Mode:      ModeReplace,
			ExpiredAt: req.OldExpiredAt,
			// The allowance is prorated to the days that remain, and the
			// counter restarts. Whether a prorated change should also forgive
			// traffic already spent is a billing-terms question, not an
			// implementation one -- it is listed in C-4 and this is the
			// assumed answer until that document is signed off.
			TransferBytes: floorDiv(req.New.TransferBytes*int64(remaining), int64(req.New.PeriodDays)),
			UsedBytes:     0,
		}
		if diff >= 0 {
			out.Charge = diff
		} else {
			out.Credit = -diff
		}
		return out, nil

	default:
		return ChangeOutcome{}, errors.New("domain: unknown change policy " + string(req.Policy))
	}
}

// floorDiv divides rounding toward negative infinity.
func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

// ceilDiv divides rounding toward positive infinity.
func ceilDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) == (b < 0)) {
		q++
	}
	return q
}
