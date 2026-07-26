# C-1 · Relay chain model and billing basis

**Status:** proposed, awaiting sign-off
**Blocks:** M1 onward — this decision shapes the schema, and the schema is
cheapest to shape before the metering core is written
**Referenced by:** design document §4.3, §5.3, §7.4, appendix C-1, appendix D.1

---

## Why this is the one blocking item

Appendix D.1 rates every subsystem at 70% design completeness or better except
relay chains, which sits at 20% and is marked as blocking. The design document
carries a single `nodes.parent_id` column and no accompanying design, while
relayed deployments — user connects to a domestic entry, the entry forwards to
an overseas landing node — are the mainstream shape rather than an exotic one.

The reason it blocks rather than merely lags is that it changes the schema. A
metering core written against one relay model and retrofitted to another is not
a refactor; it is a re-verification of every billing invariant.

## Decision

### 1. Replace `parent_id` with an ordered hop table

```sql
CREATE TABLE node_relay_hops (
    node_id     BIGINT   NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    hop_index   SMALLINT NOT NULL CHECK (hop_index >= 0),
    via_node_id BIGINT   NOT NULL REFERENCES nodes(id),
    PRIMARY KEY (node_id, hop_index),
    CONSTRAINT ck_relay_no_self CHECK (via_node_id <> node_id)
);
```

A `parent_id` expresses exactly one level. Supporting a second level later
means changing the column and every query that reads it, at a point when there
is production data in the table. A hop table expresses single-level relaying as
a chain of length one and costs one join today.

This deliberately does **not** decide whether multi-level relaying is ever
exposed in the UI. It decides only that the schema stops being the thing that
blocks the answer. The product question can stay open.

### 2. `nodes.role` — exactly one node in a chain bills

```sql
role TEXT NOT NULL DEFAULT 'entry' CHECK (role IN ('entry','relay','exit'))
```

Only a node with `role = 'entry'` may submit traffic reports. Ingest rejects a
report from any other role, logs it at CRITICAL, and raises an operator alarm.

This is the load-bearing half of the decision. `uq_ledger` is
`(user_id, node_id, subscription_id, bucket_at)` — so if a landing node runs
its own agent and reports the same `user_id`, the rows land under a *different*
`node_id` and the unique index does not fire. Nothing crashes, nothing is
logged, and every user behind that chain is billed twice, indefinitely.

That failure mode has three properties that make it the worst kind: it is
silent, it is systematic rather than sporadic, and by the time a user
reconstructs their own usage well enough to dispute it, months of ledger have
accumulated. Section 7.11 describes the remedy for a discovered billing error
as refunding in full to buy back trust. The cheaper move is to make the
configuration impossible to express.

**Deviation from principle ①, stated explicitly.** The design's first principle
is that any invariant expressible as a database constraint becomes one. This
invariant is expressible — a composite foreign key on `(node_id, role)` against
a `UNIQUE (id, role)` on `nodes` would enforce it in PostgreSQL. It is not
used, for one reason: it puts a foreign key lookup on every row of the hottest
write path in the system, and `traffic_ledger` carries no foreign keys at all
today precisely because of that path. The enforcement instead sits at ingest,
plus invariant I9 below. This is a considered exception, not an oversight, and
it is the only place in the schema where a checkable invariant is left to
application code.

### 3. Relay cost is a multiplier, never a double count

Relay capacity costs more per gigabyte than direct capacity. That cost is
expressed through `rate_bp` on the entry node — a relayed entry might carry
`rate_bp = 150` — and never by counting the relayed segment a second time.

A multiplier is visible in the node name the user sees, auditable in
`rate_bp_snapshot` on every ledger row, and reproducible by hand from a
receipt. Implicit double counting is none of those, and six months later
nobody on the operator's side remembers it either.

### 4. Landing-node cost stays in `machines.monthly_cost`

The expense of running a landing node is an operator cost, visible in the
finance view. It is not user billing and does not touch the ledger.

## Invariant I9, added to §7.11

```
I9  No traffic_ledger row references a node whose role is not 'entry'
```

Runs hourly with the other eight. Any violation means a misconfigured node is
billing users directly, so it alarms immediately rather than accumulating.

## What this decision does not settle

- **How many levels to expose.** The schema supports N. Whether the admin UI
  offers more than one hop is a product call that can wait.
- **Whether relayed traffic gets its own metric.** Probably worth splitting
  relayed from direct in the finance view, but that is reporting, not schema.
- **Failover between chains.** If an entry can fall back to a second landing
  node, the hop list becomes ordered alternatives rather than an ordered path.
  Worth confirming before M2 wires configuration delivery.

## Consequences if this is rejected

Choosing exit-side billing instead is coherent, but it inverts assumptions in
§7.4 attribution and §5.3 report semantics, and it makes `rate_bp` meaningless
on the node the user actually connects to. That version needs its own pass
through the invariants.

Choosing to defer entirely means `nodes` gains a relay model after
`traffic_ledger` is partitioned and populated — the expand-contract dance from
§4.5, on the largest table in the system, with a lock budget measured in
milliseconds.

---

**Requested action:** confirm items 1–4, or reject with the billing basis you
want instead. Everything already implemented follows this proposal; reversing
it is cheap now and expensive after M1.
