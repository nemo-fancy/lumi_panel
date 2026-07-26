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

// Valid reports whether r is a usable multiplier.
func (r RateBP) Valid() bool { return r > 0 }

// ValidateDelta checks one reported (up, down) pair at the ingest boundary.
// Callers do this once per report; BilledBytes below stays allocation- and
// branch-free for the hot path.
func ValidateDelta(up, down int64, rate RateBP) error {
	if up < 0 || down < 0 {
		return ErrNegativeDelta
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
// Overflow: (up+down) tops out around 1e12 for a single 5-minute bucket, and
// multiplying by the maximum int16 rate (32767) stays under 3.3e16 -- well
// inside int64's 9.2e18.
//
// Inputs are assumed already checked by ValidateDelta.
func BilledBytes(up, down int64, rate RateBP) int64 {
	return (up + down) * int64(rate) / int64(RateBPUnit)
}
