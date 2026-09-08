-- Bookings, payments and the transactional outbox.
--
-- The purchase flow is a saga, not a workflow engine (D7): two local commits with a
-- compensation. These tables carry the durable state that makes it recoverable.

CREATE TYPE booking_status AS ENUM (
    'CONFIRMED',
    'FAILED',
    -- Reached when a charge succeeded but the seats could not be converted. Money was
    -- taken for seats the user does not hold, so it must be refunded (D9). This state
    -- exists to make that condition explicit and alertable rather than invisible.
    'COMPENSATING',
    'COMPENSATED'
);

CREATE TABLE bookings (
    booking_id      UUID PRIMARY KEY,
    event_id        BIGINT NOT NULL REFERENCES events (event_id) ON DELETE CASCADE,
    hold_id         UUID   NOT NULL REFERENCES holds (hold_id),
    user_id         TEXT   NOT NULL,
    status          booking_status NOT NULL,
    total_cents     BIGINT NOT NULL CHECK (total_cents >= 0),
    payment_id      UUID,
    idempotency_key TEXT,
    failure_reason  TEXT,
    created_at      TIMESTAMPTZ NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL
);

-- A replayed purchase returns the original booking instead of charging again.
CREATE UNIQUE INDEX bookings_idempotency_idx ON bookings (user_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

CREATE INDEX bookings_user_idx ON bookings (user_id, created_at DESC);
-- Surfaces bookings stuck mid-compensation for operators and the reconcile job.
CREATE INDEX bookings_unresolved_idx ON bookings (updated_at)
    WHERE status = 'COMPENSATING';

CREATE TYPE payment_state AS ENUM ('IN_PROGRESS', 'COMPLETED', 'FAILED');

-- The idempotent record of an external charge.
--
-- IN_PROGRESS is deliberately treated as *ambiguous*, not as "not yet done": the
-- process can die after the provider charged the card but before we recorded it.
-- Recovery reconciles with the provider; it never blindly retries the charge.
CREATE TABLE payments (
    payment_id      UUID PRIMARY KEY,
    idempotency_key TEXT NOT NULL UNIQUE,
    user_id         TEXT NOT NULL,
    amount_cents    BIGINT NOT NULL CHECK (amount_cents >= 0),
    state           payment_state NOT NULL,
    provider_ref    TEXT,
    failure_reason  TEXT,
    refunded_at     TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL,
    CONSTRAINT payments_completed_has_ref
        CHECK (state <> 'COMPLETED' OR provider_ref IS NOT NULL)
);

-- Finds charges that may have completed at the provider but were never resolved here.
CREATE INDEX payments_in_progress_idx ON payments (updated_at)
    WHERE state = 'IN_PROGRESS';

ALTER TABLE bookings
    ADD CONSTRAINT bookings_payment_fk FOREIGN KEY (payment_id) REFERENCES payments (payment_id);

-- Transactional outbox: business state and the intent to notify are committed in one
-- transaction, so a crash cannot leave a confirmed booking with no notification, nor
-- a notification for a booking that rolled back.
CREATE TABLE outbox (
    outbox_id    BIGSERIAL PRIMARY KEY,
    topic        TEXT   NOT NULL,
    payload      JSONB  NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL,
    published_at TIMESTAMPTZ,
    attempts     INT NOT NULL DEFAULT 0
);

CREATE INDEX outbox_unpublished_idx ON outbox (outbox_id) WHERE published_at IS NULL;

-- Referential integrity for inventory ownership. Both are nullable: a ticket is
-- normally held by nobody and sold to nobody.
--
-- These are the constraints that make "a sold ticket points at a real booking" a
-- database guarantee rather than an application convention.
ALTER TABLE tickets
    ADD CONSTRAINT tickets_hold_fk    FOREIGN KEY (hold_id)    REFERENCES holds (hold_id),
    ADD CONSTRAINT tickets_booking_fk FOREIGN KEY (booking_id) REFERENCES bookings (booking_id);
