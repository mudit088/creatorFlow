-- An order is the buyer's intent: "I want these products for this amount."
-- It is created before any money moves and is never trusted to be paid until a
-- signature-verified webhook says so.
--
-- profile_id is ON DELETE RESTRICT, not CASCADE. Financial records outlive the
-- account that produced them; a creator who deletes their profile must not
-- silently erase the receipts of everyone who bought from them. The cost is
-- real and worth stating: once a creator has a single order, DELETE FROM
-- profiles fails on this constraint -- and would fail again further down, since
-- the cascade into products is itself blocked by order_items. Account deletion
-- therefore becomes a soft-delete feature rather than a DELETE, which is the
-- correct outcome for a system that handles money.
CREATE TABLE orders (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    profile_id   uuid        NOT NULL REFERENCES profiles(id) ON DELETE RESTRICT,
    -- Buyers are guests: there is no account behind a purchase. This email is
    -- the identity an entitlement is later bound to, so citext matters --
    -- Rahul@x.com and rahul@x.com have to be the same buyer.
    buyer_email  citext      NOT NULL,
    status       text        NOT NULL DEFAULT 'pending',
    total_minor  bigint      NOT NULL,
    currency     char(3)     NOT NULL DEFAULT 'INR',
    -- The provider's order id. NULL for the moment between our INSERT and the
    -- Razorpay call, deliberately: our row exists first, so a Razorpay timeout
    -- leaves a visible orphaned pending order rather than a payment we have no
    -- record of.
    provider_order_id text,
    -- Client-supplied, so a double-clicked Buy button reuses one order instead
    -- of creating two. Unique as a partial index below: most rows carry one, but
    -- a NULL must never collide with another NULL.
    idempotency_key   text,
    paid_at      timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT orders_status_valid CHECK (status IN ('pending', 'paid', 'failed', 'cancelled', 'refunded')),
    CONSTRAINT orders_total_nonneg CHECK (total_minor >= 0),
    CONSTRAINT orders_currency_valid CHECK (currency = 'INR'),
    CONSTRAINT orders_email_shape CHECK (position('@' IN buyer_email) > 1),
    -- Paired consistency. "Paid" and "has a paid timestamp" are one fact stored
    -- twice, and application code forgetting the second half is the normal
    -- failure, not the exceptional one.
    CONSTRAINT orders_paid_consistent CHECK ((status = 'paid') = (paid_at IS NOT NULL))
);

CREATE TRIGGER orders_set_updated_at
    BEFORE UPDATE ON orders
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE UNIQUE INDEX orders_idempotency_key_unique
    ON orders (idempotency_key)
    WHERE idempotency_key IS NOT NULL;

CREATE UNIQUE INDEX orders_provider_order_id_unique
    ON orders (provider_order_id)
    WHERE provider_order_id IS NOT NULL;

-- The creator dashboard reads "my orders, newest first". DESC in the index so
-- that read is a backwards scan of a matching order, not a sort.
CREATE INDEX orders_profile_created_idx ON orders (profile_id, created_at DESC);

-- Feeds the job that expires abandoned checkouts. Partial, because that job is
-- the only reader and only ever wants pending rows -- a shrinking minority of
-- the table as the system ages.
CREATE INDEX orders_pending_idx ON orders (created_at) WHERE status = 'pending';

-- What was bought, frozen at the moment of buying.
--
-- title_snapshot and unit_price_minor are not denormalisation for speed, they
-- are correctness: a receipt from March must still say what the buyer actually
-- agreed to, after the creator has renamed the product and doubled the price.
-- Joining to products for display data would silently rewrite history.
CREATE TABLE order_items (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id         uuid        NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
    -- RESTRICT: a sold product cannot be deleted out from under its receipts.
    -- The foreign key still has to exist because delivery in phase 9 resolves
    -- files through it; the snapshots cover display, not access.
    product_id       uuid        NOT NULL REFERENCES products(id) ON DELETE RESTRICT,
    title_snapshot   text        NOT NULL,
    unit_price_minor bigint      NOT NULL,
    quantity         integer     NOT NULL DEFAULT 1,
    created_at       timestamptz NOT NULL DEFAULT now(),

    -- One line per product per order. A buyer wanting "two" of a digital file is
    -- a buyer who misunderstood; quantity stays for shape but is pinned to 1
    -- until there is a reason for it not to be.
    CONSTRAINT order_items_unique_product UNIQUE (order_id, product_id),
    CONSTRAINT order_items_quantity_valid CHECK (quantity = 1),
    CONSTRAINT order_items_price_nonneg   CHECK (unit_price_minor >= 0),
    CONSTRAINT order_items_title_len      CHECK (char_length(title_snapshot) BETWEEN 1 AND 120)
);

CREATE INDEX order_items_order_idx   ON order_items (order_id);
CREATE INDEX order_items_product_idx ON order_items (product_id);

-- Note what is NOT here: a constraint that orders.total_minor equals the sum of
-- its items. That is a cross-row invariant, which a CHECK cannot express. The
-- honest options are a constraint trigger or writing both inside one
-- transaction; this project takes the transaction, and says so rather than
-- leaving the reader to assume the database is guarding it.

-- One row per payment attempt. An order has many: a declined card followed by a
-- successful retry is two rows, and discarding the first discards the answer to
-- "why does this customer think they were charged twice?".
CREATE TABLE payments (
    id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id            uuid        NOT NULL REFERENCES orders(id) ON DELETE RESTRICT,
    provider            text        NOT NULL DEFAULT 'razorpay',
    -- The provider's payment id, unique across the table. This is the
    -- money-level idempotency guard: however many times a webhook is replayed,
    -- the second attempt to record the same capture is rejected by the database
    -- rather than by a code path someone has to remember to write.
    provider_payment_id text        NOT NULL UNIQUE,
    status              text        NOT NULL,
    amount_minor        bigint      NOT NULL,
    currency            char(3)     NOT NULL DEFAULT 'INR',
    -- Razorpay's own failure reason, kept as sent. Support questions get
    -- answered out of these two columns.
    error_code          text,
    error_description   text,
    captured_at         timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT payments_provider_valid CHECK (provider IN ('razorpay')),
    CONSTRAINT payments_status_valid   CHECK (status IN ('created', 'authorized', 'captured', 'failed', 'refunded')),
    CONSTRAINT payments_amount_positive CHECK (amount_minor > 0),
    CONSTRAINT payments_currency_valid  CHECK (currency = 'INR'),
    CONSTRAINT payments_captured_consistent
        CHECK ((status = 'captured') = (captured_at IS NOT NULL))
);

CREATE TRIGGER payments_set_updated_at
    BEFORE UPDATE ON payments
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE INDEX payments_order_idx ON payments (order_id);

-- Every webhook Razorpay delivers, stored raw before it is believed.
--
-- Two things make this table worth having. First, the unique event id is the
-- delivery-level idempotency guard: Razorpay retries until it gets a 2xx, so
-- the handler inserts here first with ON CONFLICT DO NOTHING and treats "no row
-- inserted" as "already handled, return 200". A SELECT-then-INSERT would be a
-- check-then-act race that two concurrent retries both pass.
--
-- Second, signature_valid records how a stored event was authenticated rather
-- than leaving that implicit.
--
-- Note what the handler actually does today, because the two differ on purpose:
-- a delivery that fails verification is logged and refused with a 401, and no
-- row is written. Writing one would make this table something any
-- unauthenticated caller could fill at will. The column stays because storing
-- rejected deliveries is genuinely useful — a burst of them means a wrong
-- secret or someone probing — and becomes affordable once the per-IP rate
-- limiter lands in phase 11. Until then every row here is signature_valid.
CREATE TABLE payment_webhooks (
    id                uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    provider          text        NOT NULL DEFAULT 'razorpay',
    provider_event_id text        NOT NULL,
    event_type        text        NOT NULL,
    raw_payload       jsonb       NOT NULL,
    signature_valid   boolean     NOT NULL,
    -- NULL means received but not yet applied. A row that stays NULL is a
    -- webhook that died halfway; it is the queue for manual replay.
    processed_at      timestamptz,
    process_error     text,
    received_at       timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT payment_webhooks_provider_valid CHECK (provider IN ('razorpay')),
    CONSTRAINT payment_webhooks_event_unique UNIQUE (provider, provider_event_id)
);

CREATE INDEX payment_webhooks_unprocessed_idx
    ON payment_webhooks (received_at)
    WHERE processed_at IS NULL;

-- The right to download. Created in the same transaction that marks the order
-- paid, so "paid" and "can download" can never disagree.
--
-- Phase 9 reads this table and nothing else: the download endpoint does not
-- re-derive access by walking orders and joining items, because that logic
-- would then exist in two places and drift. One row, one question: may this
-- email download this product?
CREATE TABLE entitlements (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id    uuid        NOT NULL REFERENCES orders(id) ON DELETE RESTRICT,
    product_id  uuid        NOT NULL REFERENCES products(id) ON DELETE RESTRICT,
    -- Copied from the order rather than joined. The buyer has no account, so
    -- this email is the whole identity, and nothing that happens to the order
    -- later should be able to change who an already-granted access belonged to.
    buyer_email citext      NOT NULL,
    -- Set on refund or chargeback. Soft, not a DELETE: "this access was revoked
    -- on the 4th" is information, "no row" is not.
    revoked_at  timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),

    -- Makes the webhook transaction safely re-runnable. If a retry ever gets
    -- past the webhook guard, this constraint still refuses to grant the same
    -- access twice.
    CONSTRAINT entitlements_unique_grant UNIQUE (order_id, product_id)
);

CREATE TRIGGER entitlements_set_updated_at
    BEFORE UPDATE ON entitlements
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- "Everything this buyer owns" -- the buyer's library, and the download check.
-- Partial on live grants, because a revoked entitlement is never the answer to
-- an access question.
CREATE INDEX entitlements_buyer_active_idx
    ON entitlements (buyer_email, product_id)
    WHERE revoked_at IS NULL;
