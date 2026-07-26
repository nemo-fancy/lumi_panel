-- 0002_business.sql -- orders, payments, coupons, tickets, commissions,
-- settings, audit, content. These tables were named but not specified in the
-- design document; they follow the same conventions as 0001.

-- ===========================================================================
-- orders and payments
-- ===========================================================================
CREATE TABLE orders (
    id              BIGSERIAL PRIMARY KEY,
    order_no        TEXT UNIQUE NOT NULL,
    user_id         BIGINT NOT NULL REFERENCES users(id),
    plan_id         BIGINT NOT NULL REFERENCES plans(id),
    -- The state machine lives in domain/order.go; this CHECK only bounds the
    -- vocabulary. Legal transitions are enforced in a transaction holding the
    -- row, because a CHECK cannot see the previous value.
    status          TEXT NOT NULL CHECK (status IN
                        ('pending','paid','completed','provisioning_failed','cancelled','refunded')),
    period          TEXT NOT NULL,
    -- Every amount is in the smallest currency unit.
    amount          BIGINT NOT NULL CHECK (amount >= 0),
    discount_amount BIGINT NOT NULL DEFAULT 0 CHECK (discount_amount >= 0),
    balance_amount  BIGINT NOT NULL DEFAULT 0 CHECK (balance_amount >= 0),
    payable_amount  BIGINT NOT NULL CHECK (payable_amount >= 0),
    coupon_id       BIGINT,
    change_policy   TEXT,
    -- Retry bookkeeping for provisioning_failed (§8.2).
    provision_attempts SMALLINT NOT NULL DEFAULT 0,
    provisioned_at  TIMESTAMPTZ,
    paid_at         TIMESTAMPTZ,
    cancelled_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- The four amounts are only meaningful in relation to each other.
    -- Constraining each to be non-negative individually still admits an order
    -- whose payable amount has nothing to do with its price.
    CONSTRAINT ck_orders_amounts CHECK (
        payable_amount = amount - discount_amount - balance_amount
    )
);
CREATE INDEX idx_orders_user    ON orders(user_id, created_at DESC);
CREATE INDEX idx_orders_pending ON orders(created_at) WHERE status = 'pending';

CREATE TABLE payments (
    id          BIGSERIAL PRIMARY KEY,
    order_id    BIGINT NOT NULL REFERENCES orders(id),
    provider    TEXT NOT NULL,
    trade_no    TEXT NOT NULL,
    amount      BIGINT NOT NULL CHECK (amount >= 0),
    status      TEXT NOT NULL CHECK (status IN ('success','failed','refunded')),
    -- Set when this payment arrived for an order that was already paid. The
    -- money is credited to the user's balance and flagged for the operator
    -- rather than provisioning a second time (§8.3).
    is_surplus  BOOLEAN NOT NULL DEFAULT FALSE,
    raw_payload JSONB NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- The callback idempotency key. A channel that retries its notification
    -- ten times must produce one payment row.
    UNIQUE (provider, trade_no)
);
CREATE INDEX idx_payments_order ON payments(order_id);

-- ===========================================================================
-- coupons
-- ===========================================================================
CREATE TABLE coupons (
    id           BIGSERIAL PRIMARY KEY,
    code         TEXT UNIQUE NOT NULL,
    kind         TEXT NOT NULL CHECK (kind IN ('percent','amount')),
    value        BIGINT NOT NULL CHECK (value > 0),
    usage_limit  INTEGER CHECK (usage_limit IS NULL OR usage_limit > 0),
    used_count   INTEGER NOT NULL DEFAULT 0 CHECK (used_count >= 0),
    per_user_limit SMALLINT,
    plan_ids     BIGINT[] NOT NULL DEFAULT '{}',
    starts_at    TIMESTAMPTZ,
    ends_at      TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Redemption is UPDATE ... WHERE used_count < usage_limit RETURNING, so
    -- the check-then-act race cannot oversubscribe. This CHECK is the backstop
    -- that turns a logic bug into a failed transaction instead of a free
    -- product.
    CONSTRAINT ck_coupon_not_oversold CHECK (usage_limit IS NULL OR used_count <= usage_limit)
);

CREATE TABLE coupon_usages (
    coupon_id BIGINT NOT NULL REFERENCES coupons(id) ON DELETE CASCADE,
    order_id  BIGINT NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
    user_id   BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    used_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (coupon_id, order_id)
);
CREATE INDEX idx_coupon_usage_user ON coupon_usages(coupon_id, user_id);

ALTER TABLE orders ADD CONSTRAINT fk_orders_coupon
    FOREIGN KEY (coupon_id) REFERENCES coupons(id);

-- ===========================================================================
-- balance and commissions
-- ===========================================================================
-- Every balance movement writes a row here, with the value before and after
-- and the business reference that caused it. Without the before/after pair a
-- discrepancy can be detected but not located.
CREATE TABLE balance_logs (
    id            BIGSERIAL PRIMARY KEY,
    -- No cascade. This is the reconciliation trail behind invariant I5, and
    -- deleting a user must not be able to destroy it. Users are anonymised in
    -- place rather than deleted (§12.7), so the restriction never fires in
    -- normal operation -- it exists to stop the one path that would.
    user_id       BIGINT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    delta         BIGINT NOT NULL,
    balance_before BIGINT NOT NULL,
    balance_after  BIGINT NOT NULL,
    reason        TEXT NOT NULL,
    ref_type      TEXT,
    ref_id        BIGINT,
    operator_id   BIGINT REFERENCES users(id),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT ck_balance_log_arith CHECK (balance_after = balance_before + delta)
);
CREATE INDEX idx_balance_logs_user ON balance_logs(user_id, created_at DESC);

CREATE TABLE invite_commissions (
    id           BIGSERIAL PRIMARY KEY,
    inviter_id   BIGINT NOT NULL REFERENCES users(id),
    invitee_id   BIGINT NOT NULL REFERENCES users(id),
    order_id     BIGINT NOT NULL REFERENCES orders(id),
    amount       BIGINT NOT NULL CHECK (amount >= 0),
    -- The freeze period must outlast the payment channel's dispute window,
    -- otherwise a commission is withdrawable before the payment that funded it
    -- is final (§8.6).
    status       TEXT NOT NULL CHECK (status IN
                    ('pending','available','withdrawing','withdrawn','voided','clawed_back')),
    available_at TIMESTAMPTZ,
    risk_flag    TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    UNIQUE (order_id)
);
CREATE INDEX idx_commission_inviter ON invite_commissions(inviter_id, status);

-- ===========================================================================
-- tickets
-- ===========================================================================
CREATE TABLE tickets (
    id         BIGSERIAL PRIMARY KEY,
    user_id    BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    subject    TEXT NOT NULL,
    category   TEXT NOT NULL,
    status     TEXT NOT NULL CHECK (status IN ('open','pending','closed')),
    priority   SMALLINT NOT NULL DEFAULT 0,
    -- The diagnose snapshot taken when the ticket was opened, so an agent sees
    -- the same picture the user did (§8.9).
    context    JSONB NOT NULL DEFAULT '{}',
    closed_at  TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_tickets_open ON tickets(status, updated_at DESC);

CREATE TABLE ticket_messages (
    id         BIGSERIAL PRIMARY KEY,
    ticket_id  BIGINT NOT NULL REFERENCES tickets(id) ON DELETE CASCADE,
    author_id  BIGINT REFERENCES users(id),
    is_staff   BOOLEAN NOT NULL DEFAULT FALSE,
    body       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_ticket_messages ON ticket_messages(ticket_id, created_at);

-- ===========================================================================
-- settings, audit, content
-- ===========================================================================
CREATE TABLE settings (
    key        TEXT PRIMARY KEY,
    value      JSONB NOT NULL,
    -- When true, value holds AES-256-GCM ciphertext rather than plaintext.
    -- Payment keys, SMTP passwords and bot tokens live here; a logical backup
    -- left on the server is useless without the master key, which is the whole
    -- point of encrypting them (§12.2).
    is_secret  BOOLEAN NOT NULL DEFAULT FALSE,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE settings_history (
    id          BIGSERIAL PRIMARY KEY,
    key         TEXT NOT NULL,
    old_value   JSONB,
    new_value   JSONB,
    operator_id BIGINT REFERENCES users(id),
    reason      TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_settings_history_key ON settings_history(key, created_at DESC);

-- Append-only. No UPDATE or DELETE path exists in the application, and the
-- database role used at runtime should not hold those grants either.
CREATE TABLE audit_logs (
    id          BIGSERIAL PRIMARY KEY,
    operator_id BIGINT REFERENCES users(id),
    ip          INET,
    user_agent  TEXT,
    action      TEXT NOT NULL,
    target_type TEXT,
    target_id   BIGINT,
    diff        JSONB NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_audit_target   ON audit_logs(target_type, target_id, created_at DESC);
CREATE INDEX idx_audit_operator ON audit_logs(operator_id, created_at DESC);

CREATE TABLE notices (
    id         BIGSERIAL PRIMARY KEY,
    title      TEXT NOT NULL,
    body       TEXT NOT NULL,
    audience   JSONB NOT NULL DEFAULT '{}',
    is_pinned  BOOLEAN NOT NULL DEFAULT FALSE,
    published_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE knowledge (
    id         BIGSERIAL PRIMARY KEY,
    title      TEXT NOT NULL,
    body       TEXT NOT NULL,
    category   TEXT,
    audience   JSONB NOT NULL DEFAULT '{}',
    sort       INTEGER NOT NULL DEFAULT 0,
    is_visible BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- A hard bounce suppresses the address immediately; soft bounces suppress
-- after repeated failure. Skipping this is how a sending domain ends up
-- blacklisted and registration stops working altogether (§8.8).
CREATE TABLE email_suppressions (
    email       CITEXT PRIMARY KEY,
    reason      TEXT NOT NULL CHECK (reason IN ('hard_bounce','soft_bounce','complaint','manual')),
    soft_count  SMALLINT NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Every subscription request whose User-Agent matched no rule.
--
-- This turns "support absorbs the long tail" from a vague policy into a
-- measurement: once a client crosses a user-count threshold it has earned a
-- renderer, and nobody has to guess or wait for complaints (§9.4).
CREATE TABLE unknown_clients (
    id          BIGSERIAL PRIMARY KEY,
    user_agent  TEXT NOT NULL,
    user_id     BIGINT REFERENCES users(id) ON DELETE SET NULL,
    seen_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_unknown_clients_ua ON unknown_clients(user_agent, seen_at DESC);
