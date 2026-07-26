package domain

import "time"

// BucketSize is the granularity at which traffic is accumulated and billed.
// It is also the alignment every reset instant is snapped to (§7.9).
const BucketSize = 5 * time.Minute

// ResetPolicy enumerates how a plan's traffic allowance replenishes.
type ResetPolicy string

const (
	ResetMonthlyFirst  ResetPolicy = "monthly_1st"
	ResetMonthlySignup ResetPolicy = "monthly_signup"
	ResetNever         ResetPolicy = "never"
)

// AlignBucket floors t to the enclosing bucket boundary in UTC.
//
// Every reset executes on a bucket boundary. Under monthly_signup the natural
// reset instant is an arbitrary time-of-day, which would leave one bucket
// straddling the reset point with no way to split the traffic inside it
// between the old and new period. Snapping costs at most five minutes of
// misattribution and buys a hard answer to "which period does this bucket
// belong to".
func AlignBucket(t time.Time) time.Time {
	return t.UTC().Truncate(BucketSize)
}

// NextReset computes the next replenishment instant after now for a
// subscription that started at signup, evaluated against the operator's
// billing timezone.
//
// The billing timezone matters more than it looks (§8.7): a UTC panel serving
// UTC+8 users resets at 08:00 local, and users read that as their traffic
// being eaten. Period boundaries are therefore local-midnight anchored even
// though everything is stored in UTC.
//
// A nil loc is treated as UTC. ResetNever yields the zero time.
func NextReset(policy ResetPolicy, signup, now time.Time, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.UTC
	}
	switch policy {
	case ResetNever:
		return time.Time{}

	case ResetMonthlyFirst:
		l := now.In(loc)
		next := time.Date(l.Year(), l.Month(), 1, 0, 0, 0, 0, loc).AddDate(0, 1, 0)
		return AlignBucket(next)

	case ResetMonthlySignup:
		s := signup.In(loc)
		l := now.In(loc)

		// Anchor on the signup day-of-month. Months shorter than the anchor
		// day clamp to their own last day, so a 31st signup resets on the 28th
		// in February rather than skidding into March.
		next := monthlyAnchor(l.Year(), l.Month(), s.Day(), loc)
		if !next.After(l) {
			y, m := l.Year(), l.Month()+1
			if m > time.December {
				y, m = y+1, time.January
			}
			next = monthlyAnchor(y, m, s.Day(), loc)
		}
		return AlignBucket(next)

	default:
		return time.Time{}
	}
}

// monthlyAnchor builds local midnight on the given day of the given month,
// clamping day to the month's length.
func monthlyAnchor(year int, month time.Month, day int, loc *time.Location) time.Time {
	lastDay := time.Date(year, month+1, 0, 0, 0, 0, 0, loc).Day()
	if day > lastDay {
		day = lastDay
	}
	return time.Date(year, month, day, 0, 0, 0, 0, loc)
}

// CountsTowardCurrentPeriod reports whether a flushed bucket belongs to the
// subscription's current billing period.
//
// A flush in flight across a reset would otherwise be deducted from the fresh
// allowance it never consumed. Buckets earlier than the reset instant are
// still written to the ledger -- the historical record stays complete -- but
// they do not move the current period's counter (§7.9).
func CountsTowardCurrentPeriod(bucketAt time.Time, lastResetAt *time.Time) bool {
	if lastResetAt == nil {
		return true
	}
	return !bucketAt.Before(AlignBucket(*lastResetAt))
}

// Entitled reports whether a subscription authorises traffic at instant at.
//
// This is the authoritative check, and it is computed rather than read from a
// status column (§7.9). The stored status is a cache for listings and
// dashboards only. When the cron that maintains it dies -- and one day it
// will -- authorisation stays correct.
func Entitled(s SubQuota, at time.Time) bool {
	return s.ActiveAt(at) && s.UsedBytes < s.TransferBytes
}
