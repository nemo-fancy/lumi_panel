package metering

import (
	"sync"
	"testing"
	"time"

	"github.com/nemo-fancy/lumi_panel/internal/domain"
)

const gb = int64(1) << 30

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestLadderDeductsAcrossFlushes(t *testing.T) {
	now := at("2026-07-26T12:00:00Z")
	l := NewQuotaLadder()
	l.Rebuild(1, []domain.SubQuota{
		{ID: 10, Kind: domain.SubPrimary, TransferBytes: 10 * gb},
	})

	// Three consecutive flushes must see the balance falling, not the same
	// starting balance three times.
	for i := 0; i < 3; i++ {
		rows := l.Attribute(1, 3*gb, now)
		if len(rows) != 1 || rows[0].SubscriptionID != 10 {
			t.Fatalf("flush %d attributed %+v", i, rows)
		}
	}

	got := l.Snapshot(1)
	if len(got) != 1 || got[0].UsedBytes != 9*gb {
		t.Fatalf("used = %d, want %d", got[0].UsedBytes, 9*gb)
	}

	// The fourth flush overruns: 1 GiB fits, 2 GiB spills to overflow.
	rows := l.Attribute(1, 3*gb, now)
	want := []domain.LedgerRow{
		{SubscriptionID: 10, Billed: 1 * gb},
		{SubscriptionID: domain.OverflowSubscriptionID, Billed: 2 * gb},
	}
	if len(rows) != 2 || rows[0] != want[0] || rows[1] != want[1] {
		t.Fatalf("overrun attributed %+v, want %+v", rows, want)
	}
}

// TestLadderSortsOnInstall checks that callers do not have to know the
// deduction order. Requiring them to would mean every caller is a place to get
// it wrong.
func TestLadderSortsOnInstall(t *testing.T) {
	exp := at("2026-08-01T00:00:00Z")
	l := NewQuotaLadder()
	l.Rebuild(1, []domain.SubQuota{
		{ID: 10, Kind: domain.SubPrimary, TransferBytes: 10 * gb},
		{ID: 20, Kind: domain.SubDataPack, TransferBytes: 1 * gb, ExpiredAt: &exp},
	})

	got := l.Snapshot(1)
	if got[0].ID != 20 {
		t.Errorf("first in deduction order is %d, want the data pack 20", got[0].ID)
	}
}

// TestLadderCopiesInput stops the ladder from aliasing a slice the caller
// still owns -- typically rows loaded from the store and reused elsewhere.
func TestLadderCopiesInput(t *testing.T) {
	subs := []domain.SubQuota{{ID: 10, Kind: domain.SubPrimary, TransferBytes: 10 * gb}}
	l := NewQuotaLadder()
	l.Rebuild(1, subs)

	l.Attribute(1, 5*gb, at("2026-07-26T12:00:00Z"))

	if subs[0].UsedBytes != 0 {
		t.Errorf("the caller's slice was mutated: used = %d", subs[0].UsedBytes)
	}
}

// TestSnapshotIsACopy protects the same boundary in the other direction: the
// invariant checker reading a snapshot must not be able to write through it.
func TestSnapshotIsACopy(t *testing.T) {
	l := NewQuotaLadder()
	l.Rebuild(1, []domain.SubQuota{{ID: 10, Kind: domain.SubPrimary, TransferBytes: 10 * gb}})

	snap := l.Snapshot(1)
	snap[0].UsedBytes = 999

	if l.Snapshot(1)[0].UsedBytes != 0 {
		t.Error("mutating a snapshot changed the ladder")
	}
}

// TestUnknownUserOverflows covers a hydrate that missed somebody. Dropping the
// traffic would hide the gap; routing it to overflow puts it on the dashboard
// metric that is already watched.
func TestUnknownUserOverflows(t *testing.T) {
	l := NewQuotaLadder()
	rows := l.Attribute(999, 5*gb, at("2026-07-26T12:00:00Z"))

	if len(rows) != 1 || rows[0].SubscriptionID != domain.OverflowSubscriptionID || rows[0].Billed != 5*gb {
		t.Fatalf("unknown user attributed %+v", rows)
	}
}

func TestReconcileOverwritesDrift(t *testing.T) {
	l := NewQuotaLadder()
	l.Rebuild(1, []domain.SubQuota{{ID: 10, Kind: domain.SubPrimary, TransferBytes: 10 * gb}})
	l.Attribute(1, 3*gb, at("2026-07-26T12:00:00Z"))

	if !l.Reconcile(1, 10, 7*gb) {
		t.Fatal("Reconcile did not find the subscription")
	}
	if got := l.Snapshot(1)[0].UsedBytes; got != 7*gb {
		t.Errorf("used = %d, want the ledger's 7 GiB", got)
	}
	if l.Reconcile(1, 999, 0) {
		t.Error("Reconcile claimed to update a subscription that does not exist")
	}
}

func TestResetClearsCounterAndAdvancesEpoch(t *testing.T) {
	l := NewQuotaLadder()
	l.Rebuild(1, []domain.SubQuota{{ID: 10, Kind: domain.SubPrimary, TransferBytes: 10 * gb, ResetEpoch: 3}})
	l.Attribute(1, 5*gb, at("2026-07-26T12:00:00Z"))

	if !l.Reset(1, 10, 4, at("2026-07-26T13:00:00Z")) {
		t.Fatal("Reset did not find the subscription")
	}

	got := l.Snapshot(1)[0]
	if got.UsedBytes != 0 {
		t.Errorf("used = %d after reset, want 0", got.UsedBytes)
	}
	if got.ResetEpoch != 4 {
		t.Errorf("epoch = %d, want 4", got.ResetEpoch)
	}
}

// TestConcurrentAttributionConservesBytes is the property that makes the
// single-instance constraint bearable: within one process, parallel flushes
// must not double-spend the same allowance.
//
// It does not, and cannot, prove anything about two processes. That is what
// the Redis lock with a fencing token is for -- if a second Flusher ever runs,
// every user is billed twice and no test in this package would notice.
func TestConcurrentAttributionConservesBytes(t *testing.T) {
	const (
		workers = 16
		each    = 64
		chunk   = int64(1) << 20 // 1 MiB
	)

	now := at("2026-07-26T12:00:00Z")
	l := NewQuotaLadder()
	l.Rebuild(1, []domain.SubQuota{{ID: 10, Kind: domain.SubPrimary, TransferBytes: 32 * gb}})

	var (
		mu         sync.Mutex
		attributed int64
		wg         sync.WaitGroup
	)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				for _, r := range l.Attribute(1, chunk, now) {
					mu.Lock()
					attributed += r.Billed
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()

	want := int64(workers*each) * chunk
	if attributed != want {
		t.Errorf("attributed %d bytes, want %d", attributed, want)
	}
	if got := l.Snapshot(1)[0].UsedBytes; got != want {
		t.Errorf("ladder recorded %d bytes used, want %d", got, want)
	}
}

func TestDropRemovesUser(t *testing.T) {
	l := NewQuotaLadder()
	l.Rebuild(1, []domain.SubQuota{{ID: 10, Kind: domain.SubPrimary, TransferBytes: gb}})
	l.Drop(1)

	if len(l.Users()) != 0 {
		t.Errorf("Users() = %v after Drop, want empty", l.Users())
	}
}

// TestSyncPreservesCounters covers the source problem. The subscription-changed
// subscriber's natural input is the subscriptions table, whose used_bytes is a
// rollup trailing by up to an hour -- so installing counters from it would roll
// a user's usage backwards by that much on every plan change or data-pack
// purchase, handing back an hour of traffic each time.
func TestSyncPreservesCounters(t *testing.T) {
	l := NewQuotaLadder()
	l.Rebuild(1, []domain.SubQuota{{ID: 10, Kind: domain.SubPrimary, TransferBytes: 10 * gb}})
	l.Attribute(1, 4*gb, at("2026-07-26T12:00:00Z"))

	// A stale rollup arrives alongside a newly purchased data pack.
	l.Sync(1, []domain.SubQuota{
		{ID: 10, Kind: domain.SubPrimary, TransferBytes: 10 * gb, UsedBytes: 1 * gb},
		{ID: 20, Kind: domain.SubDataPack, TransferBytes: 5 * gb, UsedBytes: 0},
	})

	got := l.Snapshot(1)
	byID := map[int64]domain.SubQuota{}
	for _, s := range got {
		byID[s.ID] = s
	}
	if byID[10].UsedBytes != 4*gb {
		t.Errorf("the existing counter was overwritten by a stale rollup: %d, want %d", byID[10].UsedBytes, 4*gb)
	}
	if byID[20].UsedBytes != 0 {
		t.Errorf("the new pack arrived with %d used, want 0", byID[20].UsedBytes)
	}
	if len(got) != 2 {
		t.Errorf("ladder holds %d subscriptions, want 2", len(got))
	}
}

// TestSyncPicksUpAllowanceChanges confirms Sync is not simply ignoring its
// input: an adjusted allowance or expiry must land, only the counter is
// protected.
func TestSyncPicksUpAllowanceChanges(t *testing.T) {
	l := NewQuotaLadder()
	l.Rebuild(1, []domain.SubQuota{{ID: 10, Kind: domain.SubPrimary, TransferBytes: 10 * gb}})
	l.Attribute(1, 4*gb, at("2026-07-26T12:00:00Z"))

	l.Sync(1, []domain.SubQuota{{ID: 10, Kind: domain.SubPrimary, TransferBytes: 50 * gb}})

	got := l.Snapshot(1)[0]
	if got.TransferBytes != 50*gb {
		t.Errorf("allowance = %d, want the updated %d", got.TransferBytes, 50*gb)
	}
	if got.UsedBytes != 4*gb {
		t.Errorf("used = %d, want %d preserved", got.UsedBytes, 4*gb)
	}
}

// TestRebuildOverwritesCounters is the other half: the hourly job, fed from a
// SUM over traffic_ledger, is the only thing entitled to move a counter down.
func TestRebuildOverwritesCounters(t *testing.T) {
	l := NewQuotaLadder()
	l.Rebuild(1, []domain.SubQuota{{ID: 10, Kind: domain.SubPrimary, TransferBytes: 10 * gb}})
	l.Attribute(1, 4*gb, at("2026-07-26T12:00:00Z"))

	l.Rebuild(1, []domain.SubQuota{{ID: 10, Kind: domain.SubPrimary, TransferBytes: 10 * gb, UsedBytes: 3 * gb}})

	if got := l.Snapshot(1)[0].UsedBytes; got != 3*gb {
		t.Errorf("used = %d, want the ledger's %d", got, 3*gb)
	}
}

// TestDuplicateSubscriptionIDsAreCollapsed stops a repeated ID from
// contributing its allowance twice, which would drain the subscription past
// what it holds -- landing in the over-quota state invariant I8 alarms on
// without any traffic having overrun anything.
func TestDuplicateSubscriptionIDsAreCollapsed(t *testing.T) {
	l := NewQuotaLadder()
	l.Rebuild(1, []domain.SubQuota{
		{ID: 10, Kind: domain.SubPrimary, TransferBytes: 5 * gb},
		{ID: 10, Kind: domain.SubPrimary, TransferBytes: 5 * gb},
	})

	if got := l.Snapshot(1); len(got) != 1 {
		t.Fatalf("ladder holds %d entries, want 1", len(got))
	}

	rows := l.Attribute(1, 8*gb, at("2026-07-26T12:00:00Z"))
	want := []domain.LedgerRow{
		{SubscriptionID: 10, Billed: 5 * gb},
		{SubscriptionID: domain.OverflowSubscriptionID, Billed: 3 * gb},
	}
	if len(rows) != 2 || rows[0] != want[0] || rows[1] != want[1] {
		t.Fatalf("attributed %+v, want %+v", rows, want)
	}
	if got := l.Snapshot(1)[0].UsedBytes; got != 5*gb {
		t.Errorf("used = %d, want %d; the subscription was drained past its allowance", got, 5*gb)
	}
}

// TestFlushAcrossAResetIsRecordedButNotCharged covers §7.9. A batch already in
// flight when a reset landed carries a bucket from before it. Its ledger rows
// are still written -- the historical record has to stay complete -- but they
// must not consume the fresh allowance the user has not touched.
func TestFlushAcrossAResetIsRecordedButNotCharged(t *testing.T) {
	l := NewQuotaLadder()
	l.Rebuild(1, []domain.SubQuota{{ID: 10, Kind: domain.SubPrimary, TransferBytes: 10 * gb}})

	resetAt := at("2026-08-01T00:00:00Z")
	l.Attribute(1, 6*gb, at("2026-07-31T23:00:00Z"))
	l.Reset(1, 10, 1, resetAt)

	// A late flush for a bucket from before the reset.
	rows := l.Attribute(1, 2*gb, at("2026-07-31T23:55:00Z"))
	if len(rows) != 1 || rows[0].SubscriptionID != 10 || rows[0].Billed != 2*gb {
		t.Fatalf("the late bucket produced %+v; it must still reach the ledger", rows)
	}
	if got := l.Snapshot(1)[0].UsedBytes; got != 0 {
		t.Errorf("used = %d after a pre-reset bucket, want 0; the fresh allowance was charged", got)
	}

	// A bucket from after the reset counts normally.
	l.Attribute(1, 3*gb, at("2026-08-01T00:05:00Z"))
	if got := l.Snapshot(1)[0].UsedBytes; got != 3*gb {
		t.Errorf("used = %d, want %d", got, 3*gb)
	}
}

// TestSnapshotDoesNotAliasTimestamps covers the pointers inside SubQuota. A
// shallow struct copy shares them, so a caller adjusting an expiry on what it
// believes is its own copy would silently move the ladder's.
func TestSnapshotDoesNotAliasTimestamps(t *testing.T) {
	exp := at("2026-08-01T00:00:00Z")
	l := NewQuotaLadder()
	l.Rebuild(1, []domain.SubQuota{
		{ID: 10, Kind: domain.SubDataPack, TransferBytes: gb, ExpiredAt: &exp},
	})

	snap := l.Snapshot(1)
	*snap[0].ExpiredAt = at("2030-01-01T00:00:00Z")

	if got := l.Snapshot(1)[0].ExpiredAt; !got.Equal(exp) {
		t.Errorf("the ladder's expiry moved to %s; the snapshot aliased it", got)
	}
	if !exp.Equal(at("2026-08-01T00:00:00Z")) {
		t.Error("the caller's original timestamp was mutated")
	}
}
