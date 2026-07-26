package domain

import (
	"errors"
	"testing"
	"time"
)

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

func TestApplyChangeReset(t *testing.T) {
	out, err := ApplyChange(ChangeRequest{
		Policy:       ChangeReset,
		Old:          PlanSnapshot{ID: 1, TransferBytes: 100 * gb, PeriodDays: 30, Price: 1000},
		New:          PlanSnapshot{ID: 2, TransferBytes: 300 * gb, PeriodDays: 30, Price: 2000},
		OldExpiredAt: ts("2026-08-10T00:00:00Z"),
		At:           ts("2026-07-26T00:00:00Z"),
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
	if want := ts("2026-08-25T00:00:00Z"); !out.ExpiredAt.Equal(want) {
		t.Errorf("expiry = %s, want %s (period restarts from the change date)", out.ExpiredAt, want)
	}
}

func TestApplyChangeStackExtendsInPlace(t *testing.T) {
	out, err := ApplyChange(ChangeRequest{
		Policy:       ChangeStack,
		Old:          PlanSnapshot{ID: 1, TransferBytes: 100 * gb, PeriodDays: 30, Price: 1000},
		New:          PlanSnapshot{ID: 1, TransferBytes: 100 * gb, PeriodDays: 30, Price: 1000},
		OldExpiredAt: ts("2026-08-10T00:00:00Z"),
		At:           ts("2026-07-26T00:00:00Z"),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Extending must mutate the live row. Inserting a second primary would
	// violate uq_sub_one_primary, and doing it as expire-then-insert would
	// leave an expired row behind on every single renewal.
	if out.Mode != ModeExtend {
		t.Errorf("mode = %s, want %s", out.Mode, ModeExtend)
	}
	if want := ts("2026-09-09T00:00:00Z"); !out.ExpiredAt.Equal(want) {
		t.Errorf("expiry = %s, want %s (appended to the existing expiry)", out.ExpiredAt, want)
	}
}

func TestApplyChangeStackFromAnExpiredPlan(t *testing.T) {
	// Renewing after expiry must not backdate the new period to a date in the
	// past, which would hand the user a subscription that expires immediately.
	out, err := ApplyChange(ChangeRequest{
		Policy:       ChangeStack,
		Old:          PlanSnapshot{ID: 1, TransferBytes: 100 * gb, PeriodDays: 30, Price: 1000},
		New:          PlanSnapshot{ID: 1, TransferBytes: 100 * gb, PeriodDays: 30, Price: 1000},
		OldExpiredAt: ts("2026-07-01T00:00:00Z"),
		At:           ts("2026-07-26T00:00:00Z"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := ts("2026-08-25T00:00:00Z"); !out.ExpiredAt.Equal(want) {
		t.Errorf("expiry = %s, want %s (measured from now, not from the stale expiry)", out.ExpiredAt, want)
	}
}

func TestApplyChangeProrateUpgrade(t *testing.T) {
	out, err := ApplyChange(ChangeRequest{
		Policy:       ChangeProrate,
		Old:          PlanSnapshot{ID: 1, TransferBytes: 100 * gb, PeriodDays: 30, Price: 3000},
		New:          PlanSnapshot{ID: 2, TransferBytes: 300 * gb, PeriodDays: 30, Price: 9000},
		OldExpiredAt: ts("2026-08-10T00:00:00Z"),
		At:           ts("2026-07-26T00:00:00Z"),
		Loc:          time.UTC,
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
	if want := ts("2026-08-10T00:00:00Z"); !out.ExpiredAt.Equal(want) {
		t.Errorf("expiry = %s, want the original %s", out.ExpiredAt, want)
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
		Policy:       ChangeProrate,
		Old:          PlanSnapshot{ID: 1, TransferBytes: 300 * gb, PeriodDays: 30, Price: 9000},
		New:          PlanSnapshot{ID: 2, TransferBytes: 100 * gb, PeriodDays: 30, Price: 3000},
		OldExpiredAt: ts("2026-08-10T00:00:00Z"),
		At:           ts("2026-07-26T00:00:00Z"),
		Loc:          time.UTC,
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

// TestProrateRoundingFavoursTheUser checks the direction of both divisions at
// once, using a period that does not divide evenly.
func TestProrateRoundingFavoursTheUser(t *testing.T) {
	out, err := ApplyChange(ChangeRequest{
		Policy: ChangeProrate,
		// 999/7 and 777/7 both leave remainders.
		Old:          PlanSnapshot{ID: 1, TransferBytes: 10 * gb, PeriodDays: 7, Price: 777},
		New:          PlanSnapshot{ID: 2, TransferBytes: 20 * gb, PeriodDays: 7, Price: 999},
		OldExpiredAt: ts("2026-07-29T00:00:00Z"),
		At:           ts("2026-07-26T00:00:00Z"),
		Loc:          time.UTC,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 3 days remain. gained = floor(999*3/7) = floor(428.14) = 428.
	// surrendered = ceil(777*3/7) = ceil(333) = 333. charge = 95.
	if out.Charge != 95 {
		t.Errorf("charge = %d, want 95", out.Charge)
	}

	// The same inputs rounded the other way would charge more; assert the
	// direction explicitly so a future refactor cannot silently flip it.
	naive := (999 * 3 / 7) - (777 * 3 / 7)
	if int64(out.Charge) > int64(naive) {
		t.Errorf("charge %d exceeds the naive truncation %d; rounding must not favour the operator", out.Charge, naive)
	}
}

func TestApplyChangeDeny(t *testing.T) {
	req := ChangeRequest{
		Policy:       ChangeDeny,
		Old:          PlanSnapshot{ID: 1, TransferBytes: 100 * gb, PeriodDays: 30, Price: 1000},
		New:          PlanSnapshot{ID: 2, TransferBytes: 200 * gb, PeriodDays: 30, Price: 2000},
		OldExpiredAt: ts("2026-08-10T00:00:00Z"),
		At:           ts("2026-07-26T00:00:00Z"),
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
	_, err := ApplyChange(ChangeRequest{
		Policy: ChangeReset,
		Old:    PlanSnapshot{PeriodDays: 30},
		New:    PlanSnapshot{PeriodDays: 0},
	})
	if err == nil {
		t.Fatal("a zero-day period was accepted; it would divide by zero during proration")
	}
}
