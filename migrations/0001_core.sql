-- 0001_core.sql -- users, plans, subscriptions, machines, nodes, traffic.
--
-- Conventions applied throughout (§4.3):
--   * money is BIGINT in the smallest currency unit; FLOAT and DECIMAL are banned
--   * timestamps are TIMESTAMPTZ and the application works in UTC
--   * soft delete is deleted_at, never an is_deleted flag
--   * dynamic fields are JSONB validated in the application, never a wide table
--   * anything expressible as a database constraint is a database constraint

CREATE EXTENSION IF NOT EXISTS citext;

-- ===========================================================================
-- users
-- ===========================================================================
CREATE TABLE users (
    id              BIGSERIAL PRIMARY KEY,
    email           CITEXT UNIQUE NOT NULL,
    -- argon2id: a user-chosen password is low entropy, so a slow hash is what
    -- makes offline cracking expensive.
    password_hash   TEXT NOT NULL,
    -- Marks a hash imported from a competitor (bcrypt or md5) that is upgraded
    -- to argon2id transparently on the user's next successful login (§14).
    password_legacy TEXT,
    uuid            UUID UNIQUE NOT NULL,
    token           TEXT UNIQUE NOT NULL,
    is_admin        BOOLEAN NOT NULL DEFAULT FALSE,
    is_staff        BOOLEAN NOT NULL DEFAULT FALSE,
    banned_at       TIMESTAMPTZ,
    balance         BIGINT NOT NULL DEFAULT 0,
    commission_rate SMALLINT,
    invite_user_id  BIGINT REFERENCES users(id),
    speed_limit     INTEGER,
    device_limit    SMALLINT,
    remark          TEXT,
    -- Deleting a user anonymises them. Physical deletion would cascade into
    -- traffic_ledger, which is a financial record that has to survive (§12.7).
    deleted_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Balance deliberately carries no >= 0 constraint.
    --
    -- Overdraft is already prevented where it matters, by the conditional
    -- UPDATE ... WHERE balance >= amount RETURNING that every spend goes
    -- through (§8.5). A CHECK on top of that would not add protection -- it
    -- would remove a legitimate state: clawing back a commission after the
    -- freeze period, when the inviter has already withdrawn, must be able to
    -- drive the balance negative and freeze withdrawals until it is made good
    -- (§8.6). With the CHECK in place that transaction aborts, and the
    -- realistic response is to clamp the deduction at zero -- which loses the
    -- shortfall silently and breaks invariant I5 forever.
    CONSTRAINT ck_users_no_self_invite CHECK (invite_user_id IS DISTINCT FROM id)
);
CREATE INDEX idx_users_invite ON users(invite_user_id) WHERE invite_user_id IS NOT NULL;

-- ===========================================================================
-- permission groups
-- ===========================================================================
-- Node visibility is decided here and nowhere else (§6.6). Granting one user
-- one node means creating a group holding that node. There is deliberately no
-- user-level override: one would invalidate every group-keyed render cache and
-- open the door to serving user A the nodes of user B's group.
CREATE TABLE groups (
    id         BIGSERIAL PRIMARY KEY,
    name       TEXT NOT NULL,
    remark     TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ===========================================================================
-- plans
-- ===========================================================================
CREATE TABLE plans (
    id              BIGSERIAL PRIMARY KEY,
    name            TEXT NOT NULL,
    -- Two kinds only. A trial is a short, cheap primary, not a third kind:
    -- a separate kind would need its own rules everywhere the other two are
    -- handled, for no behavioural difference.
    kind            TEXT NOT NULL CHECK (kind IN ('primary','data_pack')),
    group_ids       BIGINT[] NOT NULL DEFAULT '{}',
    transfer_bytes  BIGINT NOT NULL CHECK (transfer_bytes >= 0),
    period_days     INTEGER NOT NULL CHECK (period_days > 0),
    speed_limit     INTEGER,
    device_limit    SMALLINT,
    prices          JSONB NOT NULL,
    reset_policy    TEXT NOT NULL CHECK (reset_policy IN ('monthly_1st','monthly_signup','never')),
    change_policy   TEXT NOT NULL DEFAULT 'prorate'
                    CHECK (change_policy IN ('reset','prorate','stack','deny')),
    stock           INTEGER CHECK (stock IS NULL OR stock >= 0),
    is_visible      BOOLEAN NOT NULL DEFAULT TRUE,
    is_renewable    BOOLEAN NOT NULL DEFAULT TRUE,
    audience        JSONB NOT NULL DEFAULT '{}',
    sort            INTEGER NOT NULL DEFAULT 0,
    deleted_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ===========================================================================
-- subscriptions
-- ===========================================================================
CREATE TABLE subscriptions (
    id              BIGSERIAL PRIMARY KEY,
    user_id         BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    plan_id         BIGINT NOT NULL REFERENCES plans(id),
    kind            TEXT NOT NULL CHECK (kind IN ('primary','data_pack')),
    status          TEXT NOT NULL CHECK (status IN ('active','expired','suspended')),
    transfer_bytes  BIGINT NOT NULL CHECK (transfer_bytes >= 0),
    -- A rollup result, refreshed hourly. Not the authority for entitlement:
    -- that is computed (§7.9).
    used_bytes      BIGINT NOT NULL DEFAULT 0 CHECK (used_bytes >= 0),
    reset_epoch     INT NOT NULL DEFAULT 0,
    started_at      TIMESTAMPTZ NOT NULL,
    expired_at      TIMESTAMPTZ,
    last_reset_at   TIMESTAMPTZ,
    next_reset_at   TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- At most one active primary per user, enforced by the database.
--
-- This constrains write order, and the constraint is easy to trip: replacing a
-- plan by inserting the new primary before expiring the old one violates it.
-- Both statements belong in one transaction, expire first, insert second.
-- domain.ModeReplace documents the same rule on the Go side.
CREATE UNIQUE INDEX uq_sub_one_primary ON subscriptions(user_id)
    WHERE kind = 'primary' AND status = 'active';

CREATE INDEX idx_sub_user_active ON subscriptions(user_id) WHERE status = 'active';
CREATE INDEX idx_sub_expiring    ON subscriptions(expired_at) WHERE status = 'active';
-- Rebuilding group:users:{gid} resolves users -> active subscriptions -> plans
-- .group_ids. Without this the join seq-scans subscriptions on every
-- invalidation, which defeats the point of invalidating only the one or two
-- affected groups (§5.5).
CREATE INDEX idx_sub_plan         ON subscriptions(plan_id);

-- ===========================================================================
-- machines and nodes
-- ===========================================================================
CREATE TABLE machines (
    id              BIGSERIAL PRIMARY KEY,
    name            TEXT NOT NULL,
    -- SHA-256, not argon2. An api_key is 32 bytes of high-entropy random, so a
    -- slow hash buys no security against brute force -- but it does turn a
    -- reconnect storm into a self-inflicted denial of service.
    api_key_hash    TEXT NOT NULL,
    agent_version   TEXT,
    upstream_base   TEXT,
    capabilities    JSONB NOT NULL DEFAULT '{}',
    monthly_cost    BIGINT,
    last_seen_at    TIMESTAMPTZ,
    online          BOOLEAN NOT NULL DEFAULT FALSE,
    load_avg        REAL,
    deleted_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX uq_machines_api_key ON machines(api_key_hash);

CREATE TABLE nodes (
    id              BIGSERIAL PRIMARY KEY,
    machine_id      BIGINT REFERENCES machines(id) ON DELETE SET NULL,
    name            TEXT NOT NULL,
    protocol        TEXT NOT NULL,
    host            TEXT NOT NULL,
    port            INTEGER NOT NULL CHECK (port BETWEEN 1 AND 65535),
    server_port     INTEGER CHECK (server_port IS NULL OR server_port BETWEEN 1 AND 65535),
    group_ids       BIGINT[] NOT NULL DEFAULT '{}',

    -- Where this node sits in a relay chain (decision C-1).
    --
    --   entry -- users connect here, and this is the only role that may bill
    --   relay -- an intermediate hop, carries no user session of its own
    --   exit  -- the landing node a chain terminates at
    --
    -- Exactly one node in a chain bills the user: the entry. A landing node
    -- running its own agent and reporting the same user_id would land on a
    -- different node_id, so uq_ledger would not catch it and the user would be
    -- billed twice -- quietly, for months. Ingest rejects a traffic report
    -- from a non-entry node outright.
    role            TEXT NOT NULL DEFAULT 'entry' CHECK (role IN ('entry','relay','exit')),

    -- Integer basis points: 100 = 1.00x. Floating point makes the same total
    -- traffic bill differently depending on how it was batched.
    --
    -- Relay cost is expressed here, on the entry node, and never by counting
    -- the relayed segment twice. A multiplier is visible in the node name,
    -- auditable, and something a user can reproduce; implicit double-counting
    -- is none of those.
    rate_bp         SMALLINT NOT NULL DEFAULT 100 CHECK (rate_bp > 0),

    settings        JSONB NOT NULL DEFAULT '{}',
    visible_to      TEXT NOT NULL DEFAULT 'all' CHECK (visible_to IN ('all','lumi_only')),
    tags            TEXT[] NOT NULL DEFAULT '{}',
    health_status   TEXT NOT NULL DEFAULT 'unknown'
                    CHECK (health_status IN ('unknown','healthy','degraded','down')),
    is_visible      BOOLEAN NOT NULL DEFAULT TRUE,
    sort            INTEGER NOT NULL DEFAULT 0,
    deleted_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_nodes_groups ON nodes USING GIN(group_ids);

-- Same machine, same listening port, once. Without this the conflict surfaces
-- as a node that will not start, long after the configuration was saved.
CREATE UNIQUE INDEX uq_node_machine_port ON nodes(machine_id, server_port)
    WHERE machine_id IS NOT NULL AND server_port IS NOT NULL;

-- Relay chain, as an ordered list of hops rather than a parent pointer
-- (decision C-1).
--
-- A parent_id expresses one level and nothing more, so supporting a second
-- level later would mean changing the column and every query that reads it.
-- A hop table expresses single-level relaying as a chain of length one, which
-- costs one join now and no migration later. Whether multi-level is ever
-- exposed in the UI stays an open product question; the schema simply stops
-- being the thing that blocks it.
CREATE TABLE node_relay_hops (
    node_id     BIGINT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    hop_index   SMALLINT NOT NULL CHECK (hop_index >= 0),
    via_node_id BIGINT NOT NULL REFERENCES nodes(id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (node_id, hop_index),
    -- A node relaying through itself is a configuration loop that would show up
    -- as a hung connection rather than an error.
    CONSTRAINT ck_relay_no_self CHECK (via_node_id <> node_id)
);
CREATE INDEX idx_relay_via ON node_relay_hops(via_node_id);

CREATE TABLE node_configs (
    id          BIGSERIAL PRIMARY KEY,
    node_id     BIGINT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    version     INTEGER NOT NULL,
    payload     JSONB NOT NULL,
    etag        TEXT NOT NULL,
    operator_id BIGINT REFERENCES users(id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    UNIQUE (node_id, version)
);

-- ===========================================================================
-- traffic
-- ===========================================================================
-- subscription_id is NOT NULL DEFAULT 0, and the 0 is a sentinel meaning
-- "overflow, no owner".
--
-- It cannot be nullable. PostgreSQL treats NULLs in a unique index as distinct
-- from each other, so a nullable column here would make uq_ledger
-- unenforceable: ON CONFLICT would never fire, rows would multiply without
-- bound, and the same traffic would be billed on every retry.
CREATE TABLE traffic_ledger (
    user_id          BIGINT NOT NULL,
    node_id          BIGINT NOT NULL,
    subscription_id  BIGINT NOT NULL DEFAULT 0,
    up_bytes         BIGINT NOT NULL CHECK (up_bytes >= 0),
    down_bytes       BIGINT NOT NULL CHECK (down_bytes >= 0),
    billed_bytes     BIGINT NOT NULL CHECK (billed_bytes >= 0),
    -- Constrained the same way nodes.rate_bp is. A snapshot of zero or below
    -- would zero-rate or negate a historical recomputation, and the row would
    -- look entirely ordinary.
    rate_bp_snapshot SMALLINT NOT NULL CHECK (rate_bp_snapshot > 0),
    -- When the panel received it, floored to five minutes. Billing and
    -- idempotency key off this.
    bucket_at        TIMESTAMPTZ NOT NULL,
    -- The window the node claims the traffic belongs to. Charts only: a node
    -- with a skewed clock must not be able to move money.
    origin_at        TIMESTAMPTZ NOT NULL
) PARTITION BY RANGE (bucket_at);

CREATE UNIQUE INDEX uq_ledger
    ON traffic_ledger(user_id, node_id, subscription_id, bucket_at);

-- The hourly rollup and invariant I1 both scan by time, and bucket_at sits
-- fourth in uq_ledger, so that index cannot serve them. Without this the
-- rollup seq-scans the live partition -- roughly 4.3 million rows a day at the
-- capacity §4.4 plans for -- once an hour.
CREATE INDEX idx_ledger_bucket ON traffic_ledger(bucket_at);
CREATE INDEX idx_ledger_sub    ON traffic_ledger(subscription_id, bucket_at);

-- ---------------------------------------------------------------------------
-- Partition management
-- ---------------------------------------------------------------------------
-- A range-partitioned table with no partitions rejects every insert:
--
--   ERROR: no partition of relation "traffic_ledger" found for row
--
-- Which would be survivable if it failed early. It does not: the node has
-- already been ACKed by then (§5.3), so the traffic is accepted, the node
-- clears its counter, and the row is then unbillable. Partitions are part of
-- the schema, not an operational afterthought.
--
-- A DEFAULT partition is explicitly not used. It would unblock inserts by
-- silently absorbing everything, and attaching a real partition afterwards
-- requires a full scan of the default holding ACCESS EXCLUSIVE -- the exact
-- lock event §4.5 forbids.
CREATE OR REPLACE FUNCTION lumi_ensure_ledger_partition(target DATE)
RETURNS TEXT
LANGUAGE plpgsql AS $$
DECLARE
    start_at DATE := date_trunc('month', target)::DATE;
    end_at   DATE := (date_trunc('month', target) + INTERVAL '1 month')::DATE;
    part     TEXT := 'traffic_ledger_p' || to_char(start_at, 'YYYYMM');
BEGIN
    IF to_regclass(part) IS NOT NULL THEN
        RETURN part;
    END IF;
    EXECUTE format(
        'CREATE TABLE %I PARTITION OF traffic_ledger FOR VALUES FROM (%L) TO (%L)',
        part, start_at, end_at
    );
    RETURN part;
END;
$$;

COMMENT ON FUNCTION lumi_ensure_ledger_partition(DATE) IS
    'Idempotent. The scheduler calls it for now and now+7d so a month boundary '
    'is always crossed with the next partition already in place (§4.4).';

-- Detaches partitions wholly older than the retention window and returns their
-- names for archival. DETACH, never DROP: §4.4 keeps 35 days online and
-- archives the rest, and a financial record should not be destroyed by a
-- routine job.
CREATE OR REPLACE FUNCTION lumi_detach_expired_ledger_partitions(retain_days INT DEFAULT 35)
RETURNS SETOF TEXT
LANGUAGE plpgsql AS $$
DECLARE
    cutoff TIMESTAMPTZ := now() - make_interval(days => retain_days);
    part   RECORD;
BEGIN
    FOR part IN
        -- The upper bound has to be read back out of the catalogue, and
        -- pg_get_expr is the only way to get at it. It renders a TIMESTAMPTZ
        -- bound in full -- '2026-08-01 00:00:00+00', not '2026-08-01' -- so
        -- the capture must take everything up to the closing quote. A
        -- date-shaped pattern silently matches nothing, which would leave
        -- every partition in place while the function reports success.
        SELECT c.relname,
               (regexp_match(pg_get_expr(c.relpartbound, c.oid), 'TO \(''([^'']+)'''))[1] AS upper_bound
        FROM pg_class c
        JOIN pg_inherits i ON i.inhrelid = c.oid
        WHERE i.inhparent = 'traffic_ledger'::regclass
    LOOP
        IF part.upper_bound IS NULL THEN
            RAISE WARNING 'could not read the upper bound of partition %; leaving it attached', part.relname;
            CONTINUE;
        END IF;
        IF part.upper_bound::TIMESTAMPTZ <= cutoff THEN
            EXECUTE format('ALTER TABLE traffic_ledger DETACH PARTITION %I', part.relname);
            RETURN NEXT part.relname;
        END IF;
    END LOOP;
END;
$$;

-- Bootstrap: the current month and the next one, so a fresh install can take
-- traffic immediately and survives its first month boundary even if the
-- scheduler has not been wired up yet.
SELECT lumi_ensure_ledger_partition(now()::DATE);
SELECT lumi_ensure_ledger_partition((now() + INTERVAL '1 month')::DATE);

-- The idempotency gate. An additive UPSERT is not idempotent, so replaying the
-- write-ahead log after a crash would add the same batch a second time. This
-- table is what makes the replay a no-op.
CREATE TABLE traffic_batches (
    batch_id   UUID PRIMARY KEY,
    flushed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_batches_flushed ON traffic_batches(flushed_at);

-- node_id = 0 means "all nodes combined". A sentinel rather than NULL because
-- primary key columns are implicitly NOT NULL, so NULL cannot express the
-- aggregate row.
CREATE TABLE traffic_daily (
    user_id      BIGINT NOT NULL,
    day          DATE   NOT NULL,
    node_id      BIGINT NOT NULL DEFAULT 0,
    billed_bytes BIGINT NOT NULL CHECK (billed_bytes >= 0),

    PRIMARY KEY (user_id, day, node_id)
);
-- The primary key leads with user_id, so cross-user reporting over a date
-- range cannot range-scan it.
CREATE INDEX idx_traffic_daily_day ON traffic_daily(day);

CREATE TABLE devices (
    user_id     BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    fingerprint TEXT NOT NULL,
    last_ip     INET,
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (user_id, fingerprint)
);
