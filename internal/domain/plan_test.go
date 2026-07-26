package domain

import (
	"errors"
	"testing"
	"time"
)

func expiry(s string) *time.Time {
	t := ts(s)
	return &t
}

func TestRemainingDays(t *testing.T) {
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}

	tests := []struct {
		name        string
		at, expired string
		loc         *time.Location
		want        int
	}{
		{"ten whole days", "2026-07-16T00:00:00Z", "2026-07-26T00:00:00Z", time.UTC, 10},
		{"partial days floor to the day boundary", "2026-07-16T23:00:00Z", "2026-07-26T01:00:00Z", time.UTC, 10},
		{"already expired", "2026-07-26T00:00:00Z", "2026-07-16T00:00:00Z", time.UTC, 0},
		{"same day", "2026-07-26T01:00:00Z", "2026-07-26T23:00:00Z", time.UTC, 0},
		// 16:00Z is already the 17th in Shanghai, which moves the day boundary
		// and therefore the count.
		{"local day boundary shifts the count", "2026-07-16T16:00:00Z", "2026-07-26T00:00:00Z", shanghai, 9},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := RemainingDays(ts(tc.at), ts(tc.expired), tc.loc); got != tc.want {
				t.Errorf("RemainingDays = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestRemainingDaysAcrossDST is the case a duration-based implementation gets
// wrong. Across a spring-forward two consecutive local midnights are 23 hours
// apart, so dividing the elapsed duration by 24 hours silently loses a day --
// and the user is charged for six days while holding the plan for seven.
func TestRemainingDaysAcrossDST(t *testing.T) {
	tests := []struct {
		name        string
		zone        string
		at, expired string
		want        int
	}{
		{"spring forward", "America/New_York", "2026-03-07T12:00:00-05:00", "2026-03-14T12:00:00-04:00", 7},
		{"fall back", "America/New_York", "2026-10-31T12:00:00-04:00", "2026-11-07T12:00:00-05:00", 7},
		{"southern hemisphere spring forward", "America/Santiago", "2026-09-05T12:00:00-04:00", "2026-09-12T12:00:00-03:00", 7},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			loc, err := time.LoadLocation(tc.zone)
			if err != nil {
				t.Skipf("tzdata unavailable: %v", err)
			}
			at, expired := ts(tc.at), ts(tc.expired)
			if got := RemainingDays(at, expired, loc); got != tc.want {
				t.Errorf("RemainingDays = %d, want %d calendar days", got, tc.want)
			}
		})
	}
}

func TestApplyChangeReset(t *testing.T) {
	out, err := ApplyChange(ChangeRequest{
		Policy:  ChangeReset,
		Old:     PlanSnapshot{ID: 1, TransferBytes: 100 * gb, PeriodDays: 30, Price: 1000},
		New:     PlanSnapshot{ID: 2, TransferBytes: 300 * gb, PeriodDays: 30, Price: 2000},
		Current: LiveSubscription{TransferBytes: 100 * gb, UsedBytes: 40 * gb, ExpiredAt: expiry("2026-08-10T00:00:00Z")},
		At:      ts("2026-07-26T00:00:00Z"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if out.Mode != ModeReplace {
		t.Errorf("mode = %s, want %s", out.Mode, ModeReplace)
	}
	if out.Charge != 2000 {
		t.Errorf("charge = %d, want the full new price 2000", out.Charge)
	}
	if out.TransferBytes != 300*gb || out.UsedBytes != 0 {
		t.Errorf("allowance = %d used = %d, want a full fresh allowance", out.TransferBytes, out.UsedBytes)
	}
	if want := ts("2026-08-25T00:00:00Z"); out.ExpiredAt == nil || !out.ExpiredAt.Equal(want) {
		t.Errorf("expiry = %v, want %s (period restarts from the change date)", out.ExpiredAt, want)
	}
}

// TestApplyChangeStackAddsAllowance is the §8.4 rule the table states
// outright: stacking accumulates traffic. Replacing the allowance instead
// makes the user pay full price for a top-up and lose everything they already
// held -- and because stacking mutates the live row, there is no second row
// left to reconstruct it from.
func TestApplyChangeStackAddsAllowance(t *testing.T) {
	out, err := ApplyChange(ChangeRequest{
		Policy:  ChangeStack,
		Old:     PlanSnapshot{ID: 1, TransferBytes: 500 * gb, PeriodDays: 30, Price: 5000},
		New:     PlanSnapshot{ID: 2, TransferBytes: 100 * gb, PeriodDays: 30, Price: 1000},
		Current: LiveSubscription{TransferBytes: 500 * gb, UsedBytes: 120 * gb, ExpiredAt: expiry("2026-08-10T00:00:00Z")},
		At:      ts("2026-07-26T00:00:00Z"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if out.Mode != ModeExtend {
		t.Errorf("mode = %s, want %s", out.Mode, ModeExtend)
	}
	if want := 600 * gb; out.TransferBytes != want {
		t.Errorf("allowance = %d, want %d (500 held + 100 added)", out.TransferBytes, want)
	}
	// The counter carries over. Zeroing it would forgive traffic already spent
	// this period, which combined with a replaced allowance makes a stack
	// indistinguishable from a reset.
	if want := 120 * gb; out.UsedBytes != want {
		t.Errorf("used = %d, want %d carried over", out.UsedBytes, want)
	}
	if want := ts("2026-09-09T00:00:00Z"); out.ExpiredAt == nil || !out.ExpiredAt.Equal(want) {
		t.Errorf("expiry = %v, want %s (appended to the existing expiry)", out.ExpiredAt, want)
	}
}

// TestApplyChangeStackAccumulates checks that a second stack builds on the
// first. Pricing against the plan's allowance rather than the live
// subscription's would silently discard the first top-up.
func TestApplyChangeStackAccumulates(t *testing.T) {
	plan := PlanSnapshot{ID: 1, TransferBytes: 100 * gb, PeriodDays: 30, Price: 1000}
	current := LiveSubscription{TransferBytes: 100 * gb, ExpiredAt: expiry("2026-08-10T00:00:00Z")}

	for i := 1; i <= 3; i++ {
		out, err := ApplyChange(ChangeRequest{
			Policy: ChangeStack, Old: plan, New: plan,
			Current: current, At: ts("2026-07-26T00:00:00Z"),
		})
		if err != nil {
			t.Fatal(err)
		}
		current.TransferBytes = out.TransferBytes
		current.ExpiredAt = out.ExpiredAt

		if want := int64(100+100*i) * gb; current.TransferBytes != want {
			t.Fatalf("after stack %d allowance = %d, want %d", i, current.TransferBytes, want)
		}
	}
}

func TestApplyChangeStackFromAnExpiredPlan(t *testing.T) {
	// Renewing after expiry must not backdate the new period to a date in the
	// past, which would hand the user a subscription that expires immediately.
	out, err := ApplyChange(ChangeRequest{
		Policy:  ChangeStack,
		Old:     PlanSnapshot{ID: 1, TransferBytes: 100 * gb, PeriodDays: 30, Price: 1000},
		New:     PlanSnapshot{ID: 1, TransferBytes: 100 * gb, PeriodDays: 30, Price: 1000},
		Current: LiveSubscription{TransferBytes: 100 * gb, ExpiredAt: expiry("2026-07-01T00:00:00Z")},
		At:      ts("2026-07-26T00:00:00Z"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := ts("2026-08-25T00:00:00Z"); out.ExpiredAt == nil || !out.ExpiredAt.Equal(want) {
		t.Errorf("expiry = %v, want %s (measured from now, not from the stale expiry)", out.ExpiredAt, want)
	}
}

// TestPermanentSubscriptions covers the NULL expiry the DDL allows. A bare
// time.Time cannot represent it, so it arrives as the zero time and reads as
// "expired in year 1" -- which inverts deny, truncates stack, and annihilates
// prorate.
func TestPermanentSubscriptions(t *testing.T) {
	plan := PlanSnapshot{ID: 1, TransferBytes: 100 * gb, PeriodDays: 30, Price: 1000}
	permanent := LiveSubscription{TransferBytes: 100 * gb, UsedBytes: 10 * gb}
	at := ts("2026-07-26T00:00:00Z")

	t.Run("deny refuses forever", func(t *testing.T) {
		_, err := ApplyChange(ChangeRequest{Policy: ChangeDeny, Old: plan, New: plan, Current: permanent, At: at})
		if !errors.Is(err, ErrChangeDenied) {
			t.Fatalf("got %v, want ErrChangeDenied; a subscription that never expires never becomes changeable", err)
		}
	})

	t.Run("stack stays permanent", func(t *testing.T) {
		out, err := ApplyChange(ChangeRequest{Policy: ChangeStack, Old: plan, New: plan, Current: permanent, At: at})
		if err != nil {
			t.Fatal(err)
		}
		if out.ExpiredAt != nil {
			t.Errorf("expiry = %v, want nil; stacking gave a never-expiring subscription an end date", out.ExpiredAt)
		}
		if want := 200 * gb; out.TransferBytes != want {
			t.Errorf("allowance = %d, want %d", out.TransferBytes, want)
		}
	})

	t.Run("prorate refuses", func(t *testing.T) {
		_, err := ApplyChange(ChangeRequest{Policy: ChangeProrate, Old: plan, New: plan, Current: permanent, At: at})
		if !errors.Is(err, ErrProratePermanent) {
			t.Fatalf("got %v, want ErrProratePermanent; there are no remaining days to prorate against", err)
		}
	})
}

func TestApplyChangeProrateUpgrade(t *testing.T) {
	out, err := ApplyChange(ChangeRequest{
		Policy:  ChangeProrate,
		Old:     PlanSnapshot{ID: 1, TransferBytes: 100 * gb, PeriodDays: 30, Price: 3000},
		New:     PlanSnapshot{ID: 2, TransferBytes: 300 * gb, PeriodDays: 30, Price: 9000},
		Current: LiveSubscription{TransferBytes: 100 * gb, ExpiredAt: expiry("2026-08-10T00:00:00Z")},
		At:      ts("2026-07-26T00:00:00Z"),
		Loc:     time.UTC,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 15 days remain. New plan value floor(9000*15/30) = 4500; old plan value
	// surrendered ceil(3000*15/30) = 1500; difference 3000.
	if out.Charge != 3000 {
		t.Errorf("charge = %d, want 3000", out.Charge)
	}
	if out.Credit != 0 {
		t.Errorf("credit = %d, want 0 on an upgrade", out.Credit)
	}
	if want := ts("2026-08-10T00:00:00Z"); out.ExpiredAt == nil || !out.ExpiredAt.Equal(want) {
		t.Errorf("expiry = %v, want the original %s", out.ExpiredAt, want)
	}
	if want := 150 * gb; out.TransferBytes != want {
		t.Errorf("allowance = %d, want %d prorated to the remaining half period", out.TransferBytes, want)
	}
}

// TestApplyChangeProrateDowngradeCredits pins the rule that a negative price
// difference becomes balance, never a refund. Refunding cash the user never
// paid is a cash-out path, not a refund (§8.4).
func TestApplyChangeProrateDowngradeCredits(t *testing.T) {
	out, err := ApplyChange(ChangeRequest{
		Policy:  ChangeProrate,
		Old:     PlanSnapshot{ID: 1, TransferBytes: 300 * gb, PeriodDays: 30, Price: 9000},
		New:     PlanSnapshot{ID: 2, TransferBytes: 100 * gb, PeriodDays: 30, Price: 3000},
		Current: LiveSubscription{TransferBytes: 300 * gb, ExpiredAt: expiry("2026-08-10T00:00:00Z")},
		At:      ts("2026-07-26T00:00:00Z"),
		Loc:     time.UTC,
	})
	if err != nil {
		t.Fatal(err)
	}

	if out.Charge != 0 {
		t.Errorf("charge = %d, want 0 on a downgrade", out.Charge)
	}
	if out.Credit != 3000 {
		t.Errorf("credit = %d, want 3000", out.Credit)
	}
}

// TestProrateRoundingDirection exercises both divisions with values that leave
// real remainders in both, so floorDiv, ceilDiv and plain truncation all give
// different answers. Without that the three are indistinguishable and the
// "rounding favours the user" claim is untested.
func TestProrateRoundingDirection(t *testing.T) {
	// 3 days of a 7-day period.
	//   gained      = floor(1000*3/7) = floor(428.57) = 428
	//   surrendered = ceil(  800*3/7) = ceil (342.86) = 343
	//   charge      = 85
	// Plain truncation would surrender 342 and charge 86.
	out, err := ApplyChange(ChangeRequest{
		Policy:  ChangeProrate,
		Old:     PlanSnapshot{ID: 1, TransferBytes: 10 * gb, PeriodDays: 7, Price: 800},
		New:     PlanSnapshot{ID: 2, TransferBytes: 20 * gb, PeriodDays: 7, Price: 1000},
		Current: LiveSubscription{TransferBytes: 10 * gb, ExpiredAt: expiry("2026-07-29T00:00:00Z")},
		At:      ts("2026-07-26T00:00:00Z"),
		Loc:     time.UTC,
	})
	if err != nil {
		t.Fatal(err)
	}

	if out.Charge != 85 {
		t.Errorf("charge = %d, want 85", out.Charge)
	}

	truncated := Money(1000*3/7) - Money(800*3/7) // 428 - 342 = 86
	if out.Charge >= truncated {
		t.Errorf("charge %d is not below the truncated %d; the ceiling on the surrendered value is not being applied",
			out.Charge, truncated)
	}
}

// TestProrateCreditRoundingDirection is the mirror: on a downgrade the same
// two roundings must maximise the credit.
func TestProrateCreditRoundingDirection(t *testing.T) {
	out, err := ApplyChange(ChangeRequest{
		Policy:  ChangeProrate,
		Old:     PlanSnapshot{ID: 1, TransferBytes: 20 * gb, PeriodDays: 7, Price: 1000},
		New:     PlanSnapshot{ID: 2, TransferBytes: 10 * gb, PeriodDays: 7, Price: 800},
		Current: LiveSubscription{TransferBytes: 20 * gb, ExpiredAt: expiry("2026-07-29T00:00:00Z")},
		At:      ts("2026-07-26T00:00:00Z"),
		Loc:     time.UTC,
	})
	if err != nil {
		t.Fatal(err)
	}

	// gained = floor(800*3/7) = 342; surrendered = ceil(1000*3/7) = 429.
	if out.Credit != 87 {
		t.Errorf("credit = %d, want 87", out.Credit)
	}
	truncated := Money(1000*3/7) - Money(800*3/7) // 86
	if out.Credit <= truncated {
		t.Errorf("credit %d is not above the truncated %d", out.Credit, truncated)
	}
}

func TestApplyChangeDeny(t *testing.T) {
	req := ChangeRequest{
		Policy:  ChangeDeny,
		Old:     PlanSnapshot{ID: 1, TransferBytes: 100 * gb, PeriodDays: 30, Price: 1000},
		New:     PlanSnapshot{ID: 2, TransferBytes: 200 * gb, PeriodDays: 30, Price: 2000},
		Current: LiveSubscription{TransferBytes: 100 * gb, ExpiredAt: expiry("2026-08-10T00:00:00Z")},
		At:      ts("2026-07-26T00:00:00Z"),
	}

	if _, err := ApplyChange(req); !errors.Is(err, ErrChangeDenied) {
		t.Fatalf("change before expiry returned %v, want ErrChangeDenied", err)
	}

	// Once expired, a deny plan behaves like reset.
	req.At = ts("2026-08-11T00:00:00Z")
	out, err := ApplyChange(req)
	if err != nil {
		t.Fatalf("change after expiry returned %v, want success", err)
	}
	if out.Mode != ModeReplace || out.Charge != 2000 {
		t.Errorf("post-expiry change = %+v, want a full-price replacement", out)
	}
}

func TestApplyChangeRejectsBadPeriods(t *testing.T) {
	if _, err := ApplyChange(ChangeRequest{
		Policy: ChangeReset,
		Old:    PlanSnapshot{PeriodDays: 30},
		New:    PlanSnapshot{PeriodDays: 0},
	}); !errors.Is(err, ErrBadPeriod) {
		t.Fatalf("a zero-day new period returned %v, want ErrBadPeriod", err)
	}

	// The old plan's period only matters to proration, so reset and stack must
	// not demand it. A fresh purchase has no meaningful old plan.
	if _, err := ApplyChange(ChangeRequest{
		Policy:  ChangeReset,
		New:     PlanSnapshot{TransferBytes: gb, PeriodDays: 30, Price: 100},
		Current: LiveSubscription{},
		At:      ts("2026-07-26T00:00:00Z"),
	}); err != nil {
		t.Errorf("reset with no old plan returned %v, want success", err)
	}

	if _, err := ApplyChange(ChangeRequest{
		Policy:  ChangeProrate,
		Old:     PlanSnapshot{PeriodDays: 0},
		New:     PlanSnapshot{PeriodDays: 30, Price: 100},
		Current: LiveSubscription{ExpiredAt: expiry("2026-08-10T00:00:00Z")},
		At:      ts("2026-07-26T00:00:00Z"),
	}); !errors.Is(err, ErrBadPeriod) {
		t.Errorf("prorate with a zero-day old period returned %v, want ErrBadPeriod", err)
	}
}

func TestApplyChangeRejectsUnknownPolicy(t *testing.T) {
	if _, err := ApplyChange(ChangeRequest{
		Policy: "upgrade_maybe",
		New:    PlanSnapshot{PeriodDays: 30},
	}); !errors.Is(err, ErrUnknownPolicy) {
		t.Fatalf("got %v, want ErrUnknownPolicy", err)
	}
}
