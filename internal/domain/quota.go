package domain

import (
	"sort"
	"time"
)

// SubKind enumerates the only two subscription shapes this panel models.
//
// The design deliberately narrowed from "any N concurrent subscriptions" to
// "one primary plus N data packs" (§4.2): the general form produced four S1
// defects worth of attribution ambiguity while the real-world case is only
// ever a main plan topped up with temporary traffic packs.
type SubKind string

const (
	SubPrimary  SubKind = "primary"
	SubDataPack SubKind = "data_pack"
)

// OverflowSubscriptionID is the sentinel used for traffic that arrived after
// every subscription was exhausted (§7.4).
//
// It is a real 0 rather than NULL because PostgreSQL treats NULLs in a unique
// index as distinct from one another, which would make uq_ledger unenforceable
// and ON CONFLICT never fire -- unbounded row growth plus double billing.
const OverflowSubscriptionID int64 = 0

// SubQuota is one subscription's remaining allowance as held by the Flusher's
// in-memory ladder (§7.3). It is a value type on purpose: attribution runs
// during flush and must not reach for the database.
type SubQuota struct {
	ID            int64
	Kind          SubKind
	TransferBytes int64
	UsedBytes     int64
	// ExpiredAt nil means "never expires", which is the normal shape for a
	// data pack.
	ExpiredAt *time.Time
	// LastResetAt is the bucket-aligned instant of the most recent period
	// rollover, or nil if the subscription has never reset. A flush whose
	// bucket predates it is written to the ledger but not counted against the
	// current period (§7.9).
	LastResetAt *time.Time
	ResetEpoch  int32
}

// Available reports the unused allowance, floored at zero. It can legitimately
// read as zero-or-less when an over-quota takedown lagged behind actual usage.
func (s SubQuota) Available() int64 {
	if s.UsedBytes >= s.TransferBytes {
		return 0
	}
	return s.TransferBytes - s.UsedBytes
}

// ActiveAt reports whether the subscription may absorb traffic at instant at.
func (s SubQuota) ActiveAt(at time.Time) bool {
	return s.ExpiredAt == nil || s.ExpiredAt.After(at)
}

// LedgerRow is one attributed slice of a billed amount, destined for
// traffic_ledger.
type LedgerRow struct {
	SubscriptionID int64
	Billed         int64
}

// SortForDeduction orders subscriptions into the fixed deduction sequence:
// data packs first (soonest expiry first), then the primary.
//
// Burning the soonest-to-expire allowance first is the point of the ordering:
// a pack the user is about to lose is worth more spent than kept. Packs that
// never expire therefore sort last within their group.
//
// Ties break on ID so the order is total and deterministic. Non-determinism
// here would mean the same traffic attributing differently across restarts --
// the same class of bug as iterating a Go map for UA matching (§6.4), and just
// as expensive to diagnose from a support ticket.
//
// SortForDeduction sorts in place and returns its argument for chaining.
// kindRank orders the subscription kinds for deduction.
//
// An explicit rank rather than an equality test, so the comparator stays a
// strict weak ordering even if a value outside the two known kinds ever
// reaches it. Comparing kinds for inequality alone makes an unknown kind
// compare equal to both known ones while they compare unequal to each other,
// which breaks transitivity and lets sort produce an arbitrary result. The
// database CHECK makes that unreachable today; the comparator should not
// depend on it.
func kindRank(k SubKind) int {
	switch k {
	case SubDataPack:
		return 0
	case SubPrimary:
		return 1
	default:
		return 2
	}
}

func SortForDeduction(subs []SubQuota) []SubQuota {
	sort.SliceStable(subs, func(i, j int) bool {
		a, b := subs[i], subs[j]
		if ra, rb := kindRank(a.Kind), kindRank(b.Kind); ra != rb {
			return ra < rb
		}
		if a.Kind == SubDataPack {
			switch {
			case a.ExpiredAt == nil && b.ExpiredAt == nil:
				// both permanent, fall through to the ID tiebreak
			case a.ExpiredAt == nil:
				return false // permanent packs drain last
			case b.ExpiredAt == nil:
				return true
			case !a.ExpiredAt.Equal(*b.ExpiredAt):
				return a.ExpiredAt.Before(*b.ExpiredAt)
			}
		}
		return a.ID < b.ID
	})
	return subs
}

// Attribute splits a billed amount across subscriptions in the order given.
//
// It runs during flush, never on the hot path, and never performs IO: the
// caller supplies the ladder slice already ordered by SortForDeduction.
//
// Any remainder left after every subscription is exhausted becomes a single
// row against OverflowSubscriptionID. The total of those rows is a headline
// operational metric -- it measures exactly what the over-quota takedown
// delay is costing, and it is the quantitative case for moving LNP into v1.1
// (§7.4). Target is under 0.1%.
//
// A zero or negative billed amount produces no rows at all; writing a zero row
// would inflate the ledger without conveying anything.
//
// At most one row is emitted per subscription ID, even if the input contains
// the same ID twice. Two rows sharing an ID would collide on uq_ledger, and
// PostgreSQL refuses an ON CONFLICT DO UPDATE that would touch one row twice
// -- so the whole flush batch aborts, retries, and aborts again, wedging the
// Flusher on a batch it can never land.
func Attribute(billed int64, subs []SubQuota, at time.Time) []LedgerRow {
	if billed <= 0 {
		return nil
	}

	var rows []LedgerRow
	// index maps a subscription ID to its row, so a repeated ID accumulates
	// instead of appending.
	index := make(map[int64]int, len(subs))
	remaining := billed

	add := func(id, amount int64) {
		if i, ok := index[id]; ok {
			rows[i].Billed += amount
			return
		}
		index[id] = len(rows)
		rows = append(rows, LedgerRow{SubscriptionID: id, Billed: amount})
	}

	for _, s := range subs {
		if !s.ActiveAt(at) {
			continue
		}
		avail := s.Available()
		if avail <= 0 {
			continue
		}
		take := avail
		if remaining < take {
			take = remaining
		}
		add(s.ID, take)
		remaining -= take
		if remaining == 0 {
			return rows
		}
	}

	add(OverflowSubscriptionID, remaining)
	return rows
}

// ApplyAttribution deducts attributed amounts back into the ladder slice so the
// Flusher's view stays current without re-reading the database (§7.3).
//
// Rows against OverflowSubscriptionID are skipped: overflow has no owner by
// definition.
func ApplyAttribution(subs []SubQuota, rows []LedgerRow) {
	for _, r := range rows {
		if r.SubscriptionID == OverflowSubscriptionID {
			continue
		}
		for i := range subs {
			if subs[i].ID == r.SubscriptionID {
				subs[i].UsedBytes += r.Billed
				break
			}
		}
	}
}
