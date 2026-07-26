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

// TestSortForDeductionIsTotal checks that the order does not depend on the
// order the input arrived in.
//
// Feeding the same permutation repeatedly only proves the sort is stable,
// which sort.SliceStable guarantees for free -- it would pass with the
// tiebreak deleted entirely. The rows come from a database query, so their
// arrival order can differ between restarts; the output must not.
func TestSortForDeductionIsTotal(t *testing.T) {
	exp := ts("2026-08-01T00:00:00Z")
	base := []SubQuota{
		{ID: 3, Kind: SubDataPack, TransferBytes: gb, ExpiredAt: ptr(exp)},
		{ID: 1, Kind: SubDataPack, TransferBytes: gb, ExpiredAt: ptr(exp)},
		{ID: 2, Kind: SubDataPack, TransferBytes: gb, ExpiredAt: ptr(exp)},
		{ID: 4, Kind: SubPrimary, TransferBytes: gb},
	}

	want := idsOf(SortForDeduction(append([]SubQuota(nil), base...)))

	for _, perm := range permutations(base) {
		got := idsOf(SortForDeduction(perm))
		for j := range got {
			if got[j] != want[j] {
				t.Fatalf("input order changed the result: got %v, want %v", got, want)
			}
		}
	}
}

// TestSortForDeductionHandlesUnknownKinds keeps the comparator a strict weak
// ordering even if a value outside the two known kinds reaches it. Comparing
// kinds for inequality alone leaves an unknown kind equivalent to both known
// ones while they are unequal to each other, which breaks transitivity and
// lets sort return an arbitrary permutation.
func TestSortForDeductionHandlesUnknownKinds(t *testing.T) {
	base := []SubQuota{
		{ID: 3, Kind: SubKind("weird"), TransferBytes: gb},
		{ID: 1, Kind: SubPrimary, TransferBytes: gb},
		{ID: 2, Kind: SubKind("weird"), TransferBytes: gb},
		{ID: 4, Kind: SubPrimary, TransferBytes: gb},
	}

	want := []int64{1, 4, 2, 3}
	for _, perm := range permutations(base) {
		got := idsOf(SortForDeduction(perm))
		for j := range got {
			if got[j] != want[j] {
				t.Fatalf("got %v, want %v (known kinds first, then unknown, each by ID)", got, want)
			}
		}
	}
}

// TestAttributeMergesDuplicateIDs guards the ledger write. Two rows sharing a
// subscription ID collide on uq_ledger, and PostgreSQL refuses an ON CONFLICT
// DO UPDATE that would touch the same row twice -- so the flush batch aborts,
// retries, and aborts again, wedging the Flusher on a batch it can never land.
func TestAttributeMergesDuplicateIDs(t *testing.T) {
	now := ts("2026-07-26T12:00:00Z")
	subs := []SubQuota{
		{ID: 10, Kind: SubDataPack, TransferBytes: 5},
		{ID: 10, Kind: SubDataPack, TransferBytes: 5},
	}

	rows := Attribute(8, subs, now)

	seen := map[int64]bool{}
	var total int64
	for _, r := range rows {
		if seen[r.SubscriptionID] {
			t.Errorf("subscription %d appears twice: %+v", r.SubscriptionID, rows)
		}
		seen[r.SubscriptionID] = true
		total += r.Billed
	}
	if total != 8 {
		t.Errorf("attributed %d, want 8", total)
	}
}

func idsOf(subs []SubQuota) []int64 {
	ids := make([]int64, len(subs))
	for i, s := range subs {
		ids[i] = s.ID
	}
	return ids
}

// permutations returns every ordering of subs, each in a fresh slice.
func permutations(subs []SubQuota) [][]SubQuota {
	if len(subs) <= 1 {
		return [][]SubQuota{append([]SubQuota(nil), subs...)}
	}
	var out [][]SubQuota
	for i := range subs {
		rest := make([]SubQuota, 0, len(subs)-1)
		rest = append(rest, subs[:i]...)
		rest = append(rest, subs[i+1:]...)
		for _, p := range permutations(rest) {
			out = append(out, append([]SubQuota{subs[i]}, p...))
		}
	}
	return out
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
