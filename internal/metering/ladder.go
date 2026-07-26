// Package metering is the traffic accounting core (§7).
//
// Every panel in this lineage bottlenecks here rather than in its HTTP
// framework. The naive shape -- one UPDATE users SET u = u + ? per user per
// node per minute -- is roughly 16,600 write queries per second at 200 nodes
// and 5,000 users, with every user's row contended by every node they touch.
// That is the actual reason a mid-sized deployment needs an oversized machine.
package metering

import (
	"sync"
	"time"

	"github.com/nemo-fancy/lumi_panel/internal/domain"
)

// QuotaLadder is the Flusher's in-memory authoritative view of how much
// allowance each subscription has left (§7.3).
//
// Attribution needs per-subscription remaining balance at flush time, and none
// of the obvious sources work: querying the database is an N+1, the Redis hot
// layer only holds a cross-subscription total, and subscriptions.used_bytes
// trails an hour behind. The ladder exists because the Flusher was already
// constrained to a single instance, which makes an in-memory authority
// affordable.
//
// The cost has to be stated plainly, because it is the sharpest edge in the
// v1 architecture: this makes the Flusher stateful and single-instance. If
// somebody ever starts a second one to "scale out", every flush is attributed
// twice and users are billed twice. The guard is a Redis lock with a fencing
// token, held by the process, checked on every flush -- deployment discipline
// is not a guard.
type QuotaLadder struct {
	mu      sync.RWMutex
	ladders map[int64][]domain.SubQuota
}

// NewQuotaLadder builds an empty ladder.
func NewQuotaLadder() *QuotaLadder {
	return &QuotaLadder{ladders: make(map[int64][]domain.SubQuota)}
}

// Rebuild installs a user's subscriptions with authoritative counters,
// replacing whatever was held.
//
// This is the hourly job's entry point, and its input must come from a SUM
// over traffic_ledger -- the one source that is auditable row by row. It is
// what stops small attribution drift from compounding across days.
//
// Startup hydrate also uses it, followed by a replay of any unacknowledged
// write-ahead log.
func (l *QuotaLadder) Rebuild(userID int64, subs []domain.SubQuota) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ladders[userID] = prepare(subs)
}

// Sync installs a user's subscription set while preserving the counters the
// ladder already holds for subscriptions it already knew about.
//
// This is what the subscription-changed event subscriber calls. Its natural
// input is the subscriptions table, whose used_bytes column is a rollup that
// trails by up to an hour -- so overwriting counters from it would roll a
// user's usage backwards by that much on every plan change or data-pack
// purchase, handing back an hour of traffic each time. Only Rebuild, fed from
// the ledger, is entitled to move a counter downward.
//
// New subscriptions take the counter they arrive with; ones already present
// keep the ladder's value and pick up any change to allowance or expiry.
func (l *QuotaLadder) Sync(userID int64, subs []domain.SubQuota) {
	l.mu.Lock()
	defer l.mu.Unlock()

	known := make(map[int64]int64, len(l.ladders[userID]))
	for _, s := range l.ladders[userID] {
		known[s.ID] = s.UsedBytes
	}

	next := prepare(subs)
	for i := range next {
		if used, ok := known[next[i].ID]; ok {
			next[i].UsedBytes = used
		}
	}
	l.ladders[userID] = next
}

// prepare copies, de-duplicates and orders a subscription set.
//
// De-duplication is not defensive tidiness. Two entries sharing an ID each
// contribute their own allowance, so the pair drains past what the
// subscription actually holds -- landing in exactly the over-quota state
// invariant I8 alarms on, without any traffic having overrun anything.
func prepare(subs []domain.SubQuota) []domain.SubQuota {
	out := make([]domain.SubQuota, 0, len(subs))
	seen := make(map[int64]bool, len(subs))
	for _, s := range subs {
		if seen[s.ID] {
			continue
		}
		seen[s.ID] = true
		out = append(out, copyQuota(s))
	}
	return domain.SortForDeduction(out)
}

// copyQuota deep-copies the timestamps a SubQuota points at, so a snapshot
// handed to a caller shares no memory with the ladder.
func copyQuota(s domain.SubQuota) domain.SubQuota {
	if s.ExpiredAt != nil {
		t := *s.ExpiredAt
		s.ExpiredAt = &t
	}
	if s.LastResetAt != nil {
		t := *s.LastResetAt
		s.LastResetAt = &t
	}
	return s
}

// Drop removes a user, e.g. once every subscription has expired.
func (l *QuotaLadder) Drop(userID int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.ladders, userID)
}

// Snapshot returns a copy of a user's ladder, for the invariant checker (I3)
// and the diagnose endpoint.
func (l *QuotaLadder) Snapshot(userID int64) []domain.SubQuota {
	l.mu.RLock()
	defer l.mu.RUnlock()

	subs := l.ladders[userID]
	cp := make([]domain.SubQuota, len(subs))
	for i, s := range subs {
		cp[i] = copyQuota(s)
	}
	return cp
}

// Users returns every user ID currently held. Used by the hourly rebuild.
func (l *QuotaLadder) Users() []int64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	ids := make([]int64, 0, len(l.ladders))
	for id := range l.ladders {
		ids = append(ids, id)
	}
	return ids
}

// Attribute splits a billed amount across a user's subscriptions and deducts
// the result from the ladder in one atomic step.
//
// bucketAt is the five-minute bucket the traffic belongs to. It decides three
// things at once: which subscriptions were live, which ledger rows are
// written, and -- via the reset epoch -- which of those rows count against the
// current period.
//
// Splitting and deducting cannot be separated. Two flush batches for the same
// user attributing against the same pre-deduction balance would each believe
// the allowance was available, and the overage would never reach the
// sub_id = 0 overflow row where it is measured.
//
// A user with no ladder entry attributes entirely to overflow rather than
// silently vanishing: unattributed traffic is a fact worth recording, and
// arriving here means hydrate missed somebody.
func (l *QuotaLadder) Attribute(userID int64, billed int64, bucketAt time.Time) []domain.LedgerRow {
	if billed <= 0 {
		return nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	subs, ok := l.ladders[userID]
	if !ok {
		return []domain.LedgerRow{{SubscriptionID: domain.OverflowSubscriptionID, Billed: billed}}
	}

	rows := domain.Attribute(billed, subs, bucketAt)

	// A batch that was in flight when a reset landed carries a bucket from
	// before the reset instant. Its ledger rows are still written -- the
	// historical record has to stay complete -- but they must not consume the
	// fresh allowance, which the user has not touched yet (§7.9).
	for _, r := range rows {
		if r.SubscriptionID == domain.OverflowSubscriptionID {
			continue
		}
		for i := range subs {
			if subs[i].ID != r.SubscriptionID {
				continue
			}
			if domain.CountsTowardCurrentPeriod(bucketAt, subs[i].LastResetAt) {
				subs[i].UsedBytes += r.Billed
			}
			break
		}
	}
	return rows
}

// Reconcile overwrites a single subscription's counter with an authoritative
// value from the ledger.
func (l *QuotaLadder) Reconcile(userID, subscriptionID, usedBytes int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	subs := l.ladders[userID]
	for i := range subs {
		if subs[i].ID == subscriptionID {
			subs[i].UsedBytes = usedBytes
			return true
		}
	}
	return false
}

// Reset applies a period rollover: the counter returns to zero, the epoch
// advances, and the reset instant is recorded.
//
// resetAt is what later flushes compare their bucket against, so it must be
// the same bucket-aligned instant written to subscriptions.last_reset_at. A
// reset that did not land on a bucket boundary would leave one bucket
// straddling it, with no way to split the traffic inside.
func (l *QuotaLadder) Reset(userID, subscriptionID int64, epoch int32, resetAt time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	subs := l.ladders[userID]
	for i := range subs {
		if subs[i].ID == subscriptionID {
			aligned := domain.AlignBucket(resetAt)
			subs[i].UsedBytes = 0
			subs[i].ResetEpoch = epoch
			subs[i].LastResetAt = &aligned
			return true
		}
	}
	return false
}
