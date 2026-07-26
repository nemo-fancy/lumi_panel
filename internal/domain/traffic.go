package domain

import "errors"

// RateBP is a billing multiplier expressed in integer basis points, where
// 100 == 1.00x.
//
// Floating point is banned for this value (§7.5). float32 carries a 24-bit
// mantissa, so 0.3 and 1.5 are repeating binaries; the same total traffic
// split into different batches then rounds in different directions and the
// ledger stops balancing. Reconciling that after the fact is impossible
// because no single row is wrong -- only the sum is.
type RateBP int16

// RateBPUnit is the neutral multiplier (1.00x).
const RateBPUnit RateBP = 100

// ErrNegativeDelta is returned when a node reports a negative traffic delta.
// Reports are deltas since the last acknowledged push (§5.3), so a negative
// value means either a node-side counter bug or a tampered report. Both must
// be rejected loudly rather than folded into the ledger.
var ErrNegativeDelta = errors.New("domain: traffic delta must not be negative")

// ErrInvalidRate is returned for a non-positive billing multiplier. A zero
// rate would make traffic free without leaving any trace of the decision.
var ErrInvalidRate = errors.New("domain: rate_bp must be positive")

// ErrImplausibleDelta is returned for a report large enough to threaten the
// billing arithmetic.
var ErrImplausibleDelta = errors.New("domain: traffic delta exceeds the plausible maximum")

// MaxReportBytes bounds a single reported direction.
//
// 2^47 is 140 TB: a five-minute bucket at 3.7 Tbps, or a fortnight of a
// saturated gigabit link accumulated across a long outage. Nothing legitimate
// reaches it.
//
// The bound exists because multiplying by the largest int16 rate must not
// overflow: 2^48 total bytes times 32767 is 9.2e15 short of int64's ceiling.
// Without it a node -- which is only semi-trusted, and may simply be running
// a buggy build -- can pick a value that wraps. Wrapping negative makes
// BilledBytes return a negative amount, which Attribute discards silently:
// the traffic disappears with no ledger row, no overflow row and no error, so
// no invariant ever sees it. Wrapping positive bills a user petabytes on a row
// that looks entirely ordinary.
const MaxReportBytes int64 = 1 << 47

// Valid reports whether r is a usable multiplier.
func (r RateBP) Valid() bool { return r > 0 }

// ValidateDelta checks one reported (up, down) pair at the ingest boundary.
// Callers do this once per report; BilledBytes below stays allocation- and
// branch-free for the hot path.
func ValidateDelta(up, down int64, rate RateBP) error {
	if up < 0 || down < 0 {
		return ErrNegativeDelta
	}
	// Each direction is bounded before they are added, so the sum itself
	// cannot overflow on the way to being checked.
	if up > MaxReportBytes || down > MaxReportBytes || up+down > MaxReportBytes {
		return ErrImplausibleDelta
	}
	if !rate.Valid() {
		return ErrInvalidRate
	}
	return nil
}

// BilledBytes converts a reported traffic delta into billed bytes.
//
// Rounding is toward zero, i.e. in the user's favour. This direction is part
// of the published billing terms (§C-4) and must not be changed without
// changing that document too.
//
// Inputs must already have passed ValidateDelta, which is what makes the
// arithmetic overflow-free: it caps up+down at MaxReportBytes, so the product
// with the largest possible rate stays inside int64. Calling this on
// unvalidated input is a bug, and a silent one.
func BilledBytes(up, down int64, rate RateBP) int64 {
	return (up + down) * int64(rate) / int64(RateBPUnit)
}
