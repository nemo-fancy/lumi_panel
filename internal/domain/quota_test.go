package domain

import (
	"testing"
	"time"
)

func ts(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func ptr(t time.Time) *time.Time { return &t }

const gb = int64(1) << 30

// TestAttributeAcrossPrimaryAndPacks is the launch-checklist case: a user
// holding one primary and two data packs sends traffic that spans all three,
// and every resulting ledger row is checked individually (§15).
func TestAttributeAcrossPrimaryAndPacks(t *testing.T) {
	now := ts("2026-07-26T12:00:00Z")

	subs := SortForDeduction([]SubQuota{
		{ID: 10, Kind: SubPrimary, TransferBytes: 100 * gb, UsedBytes: 90 * gb},
		{ID: 20, Kind: SubDataPack, TransferBytes: 5 * gb, UsedBytes: 4 * gb, ExpiredAt: ptr(ts("2026-08-10T00:00:00Z"))},
		{ID: 30, Kind: SubDataPack, TransferBytes: 3 * gb, UsedBytes: 0, ExpiredAt: ptr(ts("2026-08-01T00:00:00Z"))},
	})

	// Deduction order is fixed: soonest-expiring pack, then the later pack,
	// then the primary.
	wantOrder := []int64{30, 20, 10}
	for i, s := range subs {
		if s.ID != wantOrder[i] {
			t.Fatalf("deduction order[%d] = %d, want %d", i, s.ID, wantOrder[i])
		}
	}

	// 3 GiB in pack 30, 1 GiB left in pack 20, 10 GiB left on the primary.
	// Sending 15 GiB exhausts all three and spills 1 GiB.
	rows := Attribute(15*gb, subs, now)

	want := []LedgerRow{
		{SubscriptionID: 30, Billed: 3 * gb},
		{SubscriptionID: 20, Billed: 1 * gb},
		{SubscriptionID: 10, Billed: 10 * gb},
		{SubscriptionID: OverflowSubscriptionID, Billed: 1 * gb},
	}
	if len(rows) != len(want) {
		t.Fatalf("got %d rows, want %d: %+v", len(rows), len(want), rows)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, rows[i], want[i])
		}
	}

	// Conservation: attribution must never create or destroy bytes.
	var total int64
	for _, r := range rows {
		total += r.Billed
	}
	if total != 15*gb {
		t.Errorf("attributed total = %d, want %d", total, 15*gb)
	}
}

func TestAttributeSkipsExpiredPacks(t *testing.T) {
	now := ts("2026-07-26T12:00:00Z")

	subs := SortForDeduction([]SubQuota{
		{ID: 1, Kind: SubDataPack, TransferBytes: 5 * gb, ExpiredAt: ptr(ts("2026-07-01T00:00:00Z"))},
		{ID: 2, Kind: SubPrimary, TransferBytes: 10 * gb},
	})

	rows := Attribute(2*gb, subs, now)
	if len(rows) != 1 || rows[0].SubscriptionID != 2 {
		t.Fatalf("expired pack absorbed traffic: %+v", rows)
	}
}

// TestPermanentPacksDrainLast pins the ordering rule that a pack with no
// expiry is spent after packs that will be lost.
func TestPermanentPacksDrainLast(t *testing.T) {
	subs := SortForDeduction([]SubQuota{
		{ID: 1, Kind: SubDataPack, TransferBytes: gb},
		{ID: 2, Kind: SubDataPack, TransferBytes: gb, ExpiredAt: ptr(ts("2026-09-01T00:00:00Z"))},
		{ID: 3, Kind: SubDataPack, TransferBytes: gb, ExpiredAt: ptr(ts("2026-08-01T00:00:00Z"))},
	})

	want := []int64{3, 2, 1}
	for i, s := range subs {
		if s.ID != want[i] {
			t.Fatalf("order[%d] = %d, want %d", i, s.ID, want[i])
		}
	}
}

// TestSortForDeductionIsTotal checks that equal keys still produce one stable
// order. Attribution that reshuffles between restarts would make the same
// traffic land on different subscriptions, which is indistinguishable from a
// billing bug when a user disputes it.
func TestSortForDeductionIsTotal(t *testing.T) {
	exp := ts("2026-08-01T00:00:00Z")
	build := func() []SubQuota {
		return []SubQuota{
			{ID: 3, Kind: SubDataPack, TransferBytes: gb, ExpiredAt: ptr(exp)},
			{ID: 1, Kind: SubDataPack, TransferBytes: gb, ExpiredAt: ptr(exp)},
			{ID: 2, Kind: SubDataPack, TransferBytes: gb, ExpiredAt: ptr(exp)},
		}
	}

	first := SortForDeduction(build())
	for i := 0; i < 200; i++ {
		got := SortForDeduction(build())
		for j := range got {
			if got[j].ID != first[j].ID {
				t.Fatalf("iteration %d diverged at %d: %d vs %d", i, j, got[j].ID, first[j].ID)
			}
		}
	}
}

func TestAttributeAllOverflow(t *testing.T) {
	now := ts("2026-07-26T12:00:00Z")
	subs := []SubQuota{{ID: 1, Kind: SubPrimary, TransferBytes: gb, UsedBytes: gb}}

	rows := Attribute(500, subs, now)
	if len(rows) != 1 || rows[0].SubscriptionID != OverflowSubscriptionID || rows[0].Billed != 500 {
		t.Fatalf("exhausted quota did not overflow cleanly: %+v", rows)
	}
}

func TestAttributeZeroProducesNoRows(t *testing.T) {
	subs := []SubQuota{{ID: 1, Kind: SubPrimary, TransferBytes: gb}}
	if rows := Attribute(0, subs, time.Now()); rows != nil {
		t.Fatalf("zero billed produced rows: %+v", rows)
	}
	if rows := Attribute(-5, subs, time.Now()); rows != nil {
		t.Fatalf("negative billed produced rows: %+v", rows)
	}
}

func TestApplyAttributionIgnoresOverflow(t *testing.T) {
	subs := []SubQuota{{ID: 7, Kind: SubPrimary, TransferBytes: 10 * gb, UsedBytes: 0}}
	rows := []LedgerRow{
		{SubscriptionID: 7, Billed: 3 * gb},
		{SubscriptionID: OverflowSubscriptionID, Billed: 2 * gb},
	}

	ApplyAttribution(subs, rows)
	if subs[0].UsedBytes != 3*gb {
		t.Errorf("used = %d, want %d (overflow must not be charged to a subscription)", subs[0].UsedBytes, 3*gb)
	}
}

func TestEntitled(t *testing.T) {
	now := ts("2026-07-26T12:00:00Z")
	past, future := ts("2026-07-01T00:00:00Z"), ts("2026-08-01T00:00:00Z")

	tests := []struct {
		name string
		sub  SubQuota
		want bool
	}{
		{"live and under quota", SubQuota{TransferBytes: 10 * gb, UsedBytes: gb, ExpiredAt: &future}, true},
		{"never expires", SubQuota{TransferBytes: 10 * gb, UsedBytes: gb}, true},
		{"expired", SubQuota{TransferBytes: 10 * gb, UsedBytes: gb, ExpiredAt: &past}, false},
		{"quota exhausted", SubQuota{TransferBytes: gb, UsedBytes: gb, ExpiredAt: &future}, false},
		{"over quota", SubQuota{TransferBytes: gb, UsedBytes: 2 * gb, ExpiredAt: &future}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Entitled(tc.sub, now); got != tc.want {
				t.Errorf("Entitled() = %v, want %v", got, tc.want)
			}
		})
	}
}
