package domain

import (
	"testing"
	"time"
)

func TestAlignBucket(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"2026-07-26T12:03:59Z", "2026-07-26T12:00:00Z"},
		{"2026-07-26T12:05:00Z", "2026-07-26T12:05:00Z"},
		{"2026-07-26T12:09:59.999Z", "2026-07-26T12:05:00Z"},
		{"2026-07-26T23:59:59Z", "2026-07-26T23:55:00Z"},
	}

	for _, tc := range tests {
		if got := AlignBucket(ts(tc.in)); !got.Equal(ts(tc.want)) {
			t.Errorf("AlignBucket(%s) = %s, want %s", tc.in, got.Format(time.RFC3339), tc.want)
		}
	}
}

// TestResetInstantsLandOnBucketBoundaries is the property that makes period
// attribution decidable: no bucket may ever straddle a reset.
func TestResetInstantsLandOnBucketBoundaries(t *testing.T) {
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}

	// A signup at an arbitrary time of day is exactly the case that produces a
	// ragged reset instant under monthly_signup.
	signup := ts("2026-01-17T03:47:23Z")

	for i := 0; i < 400; i++ {
		now := signup.AddDate(0, 0, i)
		next := NextReset(ResetMonthlySignup, signup, now, shanghai)
		if next.IsZero() {
			t.Fatalf("day %d produced no reset instant", i)
		}
		if !next.Equal(AlignBucket(next)) {
			t.Fatalf("day %d: reset at %s is not on a bucket boundary", i, next.Format(time.RFC3339))
		}
		if !next.After(now) {
			t.Fatalf("day %d: reset at %s is not after now %s", i, next, now)
		}
	}
}

// TestNextResetClampsShortMonths guards the day-31 case. A naive AddDate(0,1,0)
// on 31 January yields 3 March, so a user signed up on the 31st would skip
// February's reset entirely.
func TestNextResetClampsShortMonths(t *testing.T) {
	utc := time.UTC
	signup := ts("2026-01-31T10:00:00Z")
	now := ts("2026-02-05T00:00:00Z")

	next := NextReset(ResetMonthlySignup, signup, now, utc)
	want := ts("2026-02-28T00:00:00Z")
	if !next.Equal(want) {
		t.Errorf("NextReset = %s, want %s", next.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// TestNextResetHonoursBillingTimezone is the case that generates tickets when
// it is wrong: a UTC panel serving UTC+8 users resets mid-morning local time,
// and users read that as traffic disappearing (§8.7).
func TestNextResetHonoursBillingTimezone(t *testing.T) {
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}

	now := ts("2026-07-26T00:00:00Z")
	next := NextReset(ResetMonthlyFirst, time.Time{}, now, shanghai)

	local := next.In(shanghai)
	if local.Hour() != 0 || local.Minute() != 0 {
		t.Errorf("reset lands at %s local, want local midnight", local.Format("15:04"))
	}
	if local.Day() != 1 || local.Month() != time.August {
		t.Errorf("reset lands on %s, want 1 August", local.Format("2 Jan"))
	}
}

func TestNextResetNever(t *testing.T) {
	if got := NextReset(ResetNever, time.Now(), time.Now(), time.UTC); !got.IsZero() {
		t.Errorf("ResetNever produced %s, want the zero time", got)
	}
}

// TestCountsTowardCurrentPeriod covers the in-flight flush that crosses a
// reset. The bucket still reaches the ledger, but it must not consume the
// fresh allowance.
func TestCountsTowardCurrentPeriod(t *testing.T) {
	reset := ts("2026-08-01T00:00:00Z")

	tests := []struct {
		name     string
		bucketAt string
		want     bool
	}{
		{"ten seconds before the reset", "2026-07-31T23:55:00Z", false},
		{"exactly at the reset", "2026-08-01T00:00:00Z", true},
		{"ten seconds after the reset", "2026-08-01T00:05:00Z", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := CountsTowardCurrentPeriod(ts(tc.bucketAt), &reset); got != tc.want {
				t.Errorf("CountsTowardCurrentPeriod(%s) = %v, want %v", tc.bucketAt, got, tc.want)
			}
		})
	}

	if !CountsTowardCurrentPeriod(ts("2020-01-01T00:00:00Z"), nil) {
		t.Error("a subscription that has never reset must count everything")
	}
}
