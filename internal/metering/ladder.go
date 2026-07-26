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

// Set installs a user's subscriptions, sorting them into deduction order.
//
// Called on hydrate at startup, on the hourly rebuild, and from the
// subscription-changed event subscriber. The input slice is copied: the caller
// usually owns rows loaded from the store and must not observe the ladder
// mutating them.
func (l *QuotaLadder) Set(userID int64, subs []domain.SubQuota) {
	cp := make([]domain.SubQuota, len(subs))
	copy(cp, subs)
	domain.SortForDeduction(cp)

	l.mu.Lock()
	defer l.mu.Unlock()
	l.ladders[userID] = cp
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
	copy(cp, subs)
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
// Splitting and deducting cannot be separated. Two flush batches for the same
// user attributing against the same pre-deduction balance would each believe
// the allowance was available, and the overage would never reach the
// sub_id = 0 overflow row where it is measured.
//
// A user with no ladder entry attributes entirely to overflow rather than
// silently vanishing: unattributed traffic is a fact worth recording, and
// arriving here means hydrate missed somebody.
func (l *QuotaLadder) Attribute(userID int64, billed int64, at time.Time) []domain.LedgerRow {
	if billed <= 0 {
		return nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	subs, ok := l.ladders[userID]
	if !ok {
		return []domain.LedgerRow{{SubscriptionID: domain.OverflowSubscriptionID, Billed: billed}}
	}

	rows := domain.Attribute(billed, subs, at)
	domain.ApplyAttribution(subs, rows)
	return rows
}

// Reconcile overwrites a subscription's counter with an authoritative value.
//
// The hourly job recomputes each counter as a SUM over traffic_ledger and
// pushes it here, which is what stops small attribution drift from compounding
// over days. The ledger wins by definition: it is the only record a human can
// audit row by row.
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

// Reset applies a period rollover: the counter returns to zero and the epoch
// advances.
//
// The epoch is what protects an in-flight flush. A batch that was already on
// its way when the reset landed carries an older bucket timestamp; it is still
// written to the ledger, because the historical record must stay complete, but
// it is not counted against the fresh allowance (§7.9).
func (l *QuotaLadder) Reset(userID, subscriptionID int64, epoch int32) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	subs := l.ladders[userID]
	for i := range subs {
		if subs[i].ID == subscriptionID {
			subs[i].UsedBytes = 0
			subs[i].ResetEpoch = epoch
			return true
		}
	}
	return false
}
