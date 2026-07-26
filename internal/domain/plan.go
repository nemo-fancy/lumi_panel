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

var (
	// ErrChangeDenied reports a plan change refused by a deny-policy plan that
	// has not expired yet.
	ErrChangeDenied = errors.New("domain: plan change denied until current plan expires")

	// ErrProratePermanent reports proration attempted against a subscription
	// that never expires. There is no remaining-days figure to prorate
	// against, and treating the missing expiry as "already expired" would
	// silently zero the allowance.
	ErrProratePermanent = errors.New("domain: cannot prorate a subscription with no expiry")

	// ErrBadPeriod reports a non-positive plan period.
	ErrBadPeriod = errors.New("domain: plan period must be positive")

	// ErrUnknownPolicy reports an unrecognised change policy.
	ErrUnknownPolicy = errors.New("domain: unknown change policy")
)

// PlanSnapshot is the subset of a plan needed to price a change.
//
// It is a snapshot because prices move and a change must be priced against
// what was actually bought. PeriodDays is resolved from the period the user
// selected, not stored on the plan: one plan sells monthly and yearly prices
// out of the same row, so a single period length on the plan cannot describe
// either purchase. See PeriodDays.
type PlanSnapshot struct {
	ID            int64
	TransferBytes int64
	PeriodDays    int
	Price         Money // price for one whole period
}

// LiveSubscription is the state of the subscription being changed.
//
// Distinct from PlanSnapshot, and the distinction is load-bearing: stacking
// adds to what the subscription currently holds, which after an earlier stack
// is no longer what any plan says. Pricing against the plan's allowance would
// quietly discard everything accumulated since.
type LiveSubscription struct {
	TransferBytes int64
	UsedBytes     int64
	// ExpiredAt nil means the subscription never expires.
	ExpiredAt *time.Time
}

// ChangeRequest describes a requested move from a live subscription to a new
// plan.
type ChangeRequest struct {
	Policy  ChangePolicy
	Old     PlanSnapshot
	New     PlanSnapshot
	Current LiveSubscription
	At      time.Time
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
//
// Every field is stated absolutely rather than as a delta, including the ones
// a given mode does not move. "Leave this one alone" is not expressible, which
// means it cannot be assumed by mistake: an extend that means to preserve the
// counter says so by carrying the counter's current value.
type ChangeOutcome struct {
	Mode ChangeMode
	// ExpiredAt nil means the resulting subscription never expires.
	ExpiredAt     *time.Time
	TransferBytes int64
	// UsedBytes is the counter the resulting subscription ends up with.
	UsedBytes int64
	// Charge is what the user owes, never negative.
	Charge Money
	// Credit is the surplus from a downgrade. It is credited to the user's
	// balance rather than refunded to the payment channel: refunding a
	// difference the user never paid in cash is a cash-out path, not a
	// refund (§8.4).
	Credit Money
}

// RemainingDays counts whole calendar days from at until expiry, measured
// against local day boundaries in loc.
//
// Whole days rather than seconds, deliberately. Second-level proration
// produces amounts a user cannot reproduce by hand, and every one of those
// becomes a ticket.
//
// The count is taken between calendar dates, not as a duration between the two
// local midnights. Across a spring-forward those midnights are 23 hours apart,
// and dividing a duration by 24 hours loses the day -- so a user changing plan
// over a DST boundary would be charged for six days while holding the plan for
// seven. The error runs one way only, against the user, and reduces the credit
// on a downgrade too.
//
// Nil loc is treated as UTC.
func RemainingDays(at, expiredAt time.Time, loc *time.Location) int {
	if loc == nil {
		loc = time.UTC
	}
	a := at.In(loc)
	e := expiredAt.In(loc)

	// Rebuilding both dates in UTC turns the comparison into pure calendar
	// arithmetic, with no offset transition in between to absorb an hour.
	aDay := time.Date(a.Year(), a.Month(), a.Day(), 0, 0, 0, 0, time.UTC)
	eDay := time.Date(e.Year(), e.Month(), e.Day(), 0, 0, 0, 0, time.UTC)

	days := int(eDay.Sub(aDay) / (24 * time.Hour))
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
	if req.New.PeriodDays <= 0 {
		return ChangeOutcome{}, ErrBadPeriod
	}
	loc := req.Loc
	if loc == nil {
		loc = time.UTC
	}

	switch req.Policy {
	case ChangeDeny:
		// A subscription with no expiry never reaches the point where a deny
		// policy would allow a change. Reading a missing expiry as "expired
		// long ago" would invert the policy completely.
		if req.Current.ExpiredAt == nil || req.Current.ExpiredAt.After(req.At) {
			return ChangeOutcome{}, ErrChangeDenied
		}
		fallthrough

	case ChangeReset:
		expiry := req.At.AddDate(0, 0, req.New.PeriodDays)
		return ChangeOutcome{
			Mode:          ModeReplace,
			ExpiredAt:     &expiry,
			TransferBytes: req.New.TransferBytes,
			UsedBytes:     0,
			Charge:        req.New.Price,
		}, nil

	case ChangeStack:
		out := ChangeOutcome{
			Mode: ModeExtend,
			// Stacking adds allowance to what the subscription holds now.
			// Taking it from the plan instead would discard everything a
			// previous stack accumulated.
			TransferBytes: req.Current.TransferBytes + req.New.TransferBytes,
			// The counter carries over. Stacking grants more allowance; it
			// does not forgive what has already been spent this period.
			UsedBytes: req.Current.UsedBytes,
			Charge:    req.New.Price,
		}

		// Stacking onto a subscription that never expires keeps it that way.
		if req.Current.ExpiredAt != nil {
			base := *req.Current.ExpiredAt
			if base.Before(req.At) {
				// Renewing after a lapse measures from now. Extending from the
				// stale expiry would hand back a subscription that is already
				// over.
				base = req.At
			}
			expiry := base.AddDate(0, 0, req.New.PeriodDays)
			out.ExpiredAt = &expiry
		}
		return out, nil

	case ChangeProrate:
		if req.Current.ExpiredAt == nil {
			return ChangeOutcome{}, ErrProratePermanent
		}
		if req.Old.PeriodDays <= 0 {
			return ChangeOutcome{}, ErrBadPeriod
		}

		remaining := RemainingDays(req.At, *req.Current.ExpiredAt, loc)

		gained := floorDiv(int64(req.New.Price)*int64(remaining), int64(req.New.PeriodDays))
		surrendered := ceilDiv(int64(req.Old.Price)*int64(remaining), int64(req.Old.PeriodDays))
		diff := Money(gained - surrendered)

		expiry := *req.Current.ExpiredAt
		out := ChangeOutcome{
			Mode:      ModeReplace,
			ExpiredAt: &expiry,
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
		return ChangeOutcome{}, ErrUnknownPolicy
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
