-- Schema regression checks, run against a scratch database after applying
-- every migration. See scripts/verify-schema.sh.
--
-- These exist because the first version of this schema declared
-- traffic_ledger PARTITION BY RANGE and created no partitions, so the very
-- first ledger insert failed -- after the node had already been ACKed and had
-- cleared its counter. Nothing in the repository would have caught it: Go
-- tests do not execute SQL, and the file parses fine.

\set ON_ERROR_STOP on

-- ---------------------------------------------------------------------------
-- Partitioning
-- ---------------------------------------------------------------------------
DO $$
DECLARE n INT;
BEGIN
    SELECT count(*) INTO n FROM pg_inherits WHERE inhparent = 'traffic_ledger'::regclass;
    ASSERT n >= 2, format('traffic_ledger has %s partitions, want the current month and the next', n);
END $$;

-- A row for right now must find a home.
INSERT INTO traffic_ledger VALUES (1, 1, 0, 100, 200, 300, 100, now(), now());

-- The §7.6 additive UPSERT must merge rather than duplicate. Both inserts use
-- the same literal bucket: now() is transaction-scoped, so two autocommitted
-- statements would produce two different bucket_at values and never collide.
BEGIN;
INSERT INTO traffic_ledger VALUES (2, 1, 0, 100, 200, 300, 100, '2026-07-26 12:00:00+00', now());
INSERT INTO traffic_ledger VALUES (2, 1, 0, 100, 200, 300, 100, '2026-07-26 12:00:00+00', now())
ON CONFLICT (user_id, node_id, subscription_id, bucket_at) DO UPDATE
    SET billed_bytes = traffic_ledger.billed_bytes + EXCLUDED.billed_bytes;
DO $$
DECLARE billed BIGINT;
BEGIN
    SELECT billed_bytes INTO billed FROM traffic_ledger WHERE user_id = 2;
    ASSERT billed = 600, format('additive upsert produced %s, want 600', billed);
END $$;
COMMIT;

-- Retention detaches what is past the window and leaves the rest alone.
SELECT lumi_ensure_ledger_partition('2020-01-15'::date);
DO $$
DECLARE detached TEXT[];
BEGIN
    SELECT array_agg(p) INTO detached FROM lumi_detach_expired_ledger_partitions(35) AS p;
    ASSERT detached @> ARRAY['traffic_ledger_p202001'],
        format('expired partition was not detached; got %s', detached);
    ASSERT (SELECT count(*) FROM pg_inherits WHERE inhparent = 'traffic_ledger'::regclass) >= 2,
        'retention detached a live partition';
    -- DETACH, not DROP: the archived rows are still reachable.
    ASSERT to_regclass('traffic_ledger_p202001') IS NOT NULL, 'the expired partition was dropped';
END $$;

-- Creating the same partition twice must be a no-op, since the scheduler
-- calls it on every tick.
SELECT lumi_ensure_ledger_partition(now()::date);
SELECT lumi_ensure_ledger_partition(now()::date);

-- ---------------------------------------------------------------------------
-- Constraints that encode a business rule
-- ---------------------------------------------------------------------------
INSERT INTO users (email, password_hash, uuid, token, balance)
VALUES ('checks@example.invalid', 'x', gen_random_uuid(), 'tok-checks', 500);

-- §8.6 clawback after the freeze period must be able to go negative.
UPDATE users SET balance = balance - 800 WHERE email = 'checks@example.invalid';
DO $$
DECLARE b BIGINT;
BEGIN
    SELECT balance INTO b FROM users WHERE email = 'checks@example.invalid';
    ASSERT b = -300, format('balance is %s, want -300; a CHECK is blocking commission clawback', b);
END $$;

-- The financial trail must outlive any attempt to delete the user.
INSERT INTO balance_logs (user_id, delta, balance_before, balance_after, reason)
SELECT id, -800, 500, -300, 'commission_clawback' FROM users WHERE email = 'checks@example.invalid';
DO $$
BEGIN
    BEGIN
        DELETE FROM users WHERE email = 'checks@example.invalid';
        RAISE EXCEPTION 'balance_logs did not prevent the user from being deleted';
    EXCEPTION WHEN foreign_key_violation THEN
        NULL;
    END;
END $$;

-- A zero or negative rate snapshot would silently zero-rate a recomputation.
DO $$
BEGIN
    BEGIN
        INSERT INTO traffic_ledger VALUES (3, 1, 0, 1, 1, 1, 0, now(), now());
        RAISE EXCEPTION 'rate_bp_snapshot = 0 was accepted';
    EXCEPTION WHEN check_violation THEN
        NULL;
    END;
END $$;

-- Order amounts must add up.
INSERT INTO plans (name, kind, transfer_bytes, prices, reset_policy)
VALUES ('verify', 'primary', 1, '{"month":990}', 'monthly_1st');
DO $$
DECLARE uid BIGINT; pid BIGINT;
BEGIN
    SELECT id INTO uid FROM users WHERE email = 'checks@example.invalid';
    SELECT id INTO pid FROM plans WHERE name = 'verify';
    BEGIN
        INSERT INTO orders (order_no, user_id, plan_id, status, period,
                            amount, discount_amount, balance_amount, payable_amount)
        VALUES ('verify-bad', uid, pid, 'pending', 'month', 990, 0, 0, 1);
        RAISE EXCEPTION 'an order whose amounts do not reconcile was accepted';
    EXCEPTION WHEN check_violation THEN
        NULL;
    END;

    INSERT INTO orders (order_no, user_id, plan_id, status, period,
                        amount, discount_amount, balance_amount, payable_amount)
    VALUES ('verify-good', uid, pid, 'pending', 'month', 990, 90, 100, 800);
END $$;

-- At most one active primary per user, whatever order the writes arrive in.
DO $$
DECLARE uid BIGINT; pid BIGINT;
BEGIN
    SELECT id INTO uid FROM users WHERE email = 'checks@example.invalid';
    SELECT id INTO pid FROM plans WHERE name = 'verify';

    INSERT INTO subscriptions (user_id, plan_id, kind, status, transfer_bytes, started_at)
    VALUES (uid, pid, 'primary', 'active', 1, now());

    BEGIN
        INSERT INTO subscriptions (user_id, plan_id, kind, status, transfer_bytes, started_at)
        VALUES (uid, pid, 'primary', 'active', 1, now());
        RAISE EXCEPTION 'a second active primary was accepted';
    EXCEPTION WHEN unique_violation THEN
        NULL;
    END;

    -- Expire first, then insert: the order a plan change must follow.
    UPDATE subscriptions SET status = 'expired' WHERE user_id = uid AND kind = 'primary';
    INSERT INTO subscriptions (user_id, plan_id, kind, status, transfer_bytes, started_at)
    VALUES (uid, pid, 'primary', 'active', 1, now());
END $$;

-- A relay hop must not point at its own node.
DO $$
DECLARE mid BIGINT; nid BIGINT;
BEGIN
    INSERT INTO machines (name, api_key_hash) VALUES ('m1', 'h1') RETURNING id INTO mid;
    INSERT INTO nodes (machine_id, name, protocol, host, port)
    VALUES (mid, 'n1', 'trojan', 'example.invalid', 443) RETURNING id INTO nid;

    BEGIN
        INSERT INTO node_relay_hops (node_id, hop_index, via_node_id) VALUES (nid, 0, nid);
        RAISE EXCEPTION 'a node was allowed to relay through itself';
    EXCEPTION WHEN check_violation THEN
        NULL;
    END;
END $$;

SELECT 'schema verification passed' AS result;
