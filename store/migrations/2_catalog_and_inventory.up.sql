-- Venues, events and inventory.
--
-- The tickets and holds tables are created here, ahead of the booking path being
-- built, so adding it later needs no migration rewrite (see docs/implementation-plan.md).

CREATE TABLE venues (
    venue_id   BIGSERIAL PRIMARY KEY,
    name       TEXT   NOT NULL,
    city       TEXT   NOT NULL,
    country    TEXT   NOT NULL,
    -- Nullable: a venue may be registered before its coordinates are known, in which
    -- case it simply does not appear in radius-filtered search results.
    latitude   DOUBLE PRECISION,
    longitude  DOUBLE PRECISION,
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT venues_name_not_blank CHECK (length(btrim(name)) > 0),
    CONSTRAINT venues_lat_range  CHECK (latitude  IS NULL OR (latitude  BETWEEN -90  AND 90)),
    CONSTRAINT venues_lon_range  CHECK (longitude IS NULL OR (longitude BETWEEN -180 AND 180)),
    -- Coordinates are meaningful only as a pair.
    CONSTRAINT venues_latlon_together CHECK ((latitude IS NULL) = (longitude IS NULL))
);

-- Seat is a venue-level template and is immutable once an event has been published
-- against it. Ticket (below) is the event-level, contested instance.
CREATE TABLE seats (
    seat_id     BIGSERIAL PRIMARY KEY,
    venue_id    BIGINT NOT NULL REFERENCES venues (venue_id) ON DELETE CASCADE,
    section     TEXT   NOT NULL,
    row_label   TEXT   NOT NULL,
    seat_number INT    NOT NULL CHECK (seat_number > 0),
    UNIQUE (venue_id, section, row_label, seat_number)
);

CREATE INDEX seats_venue_section_idx ON seats (venue_id, section, seat_id);

CREATE TYPE event_status AS ENUM ('DRAFT', 'ON_SALE', 'CLOSED', 'CANCELLED');

CREATE TABLE events (
    event_id     BIGSERIAL PRIMARY KEY,
    venue_id     BIGINT NOT NULL REFERENCES venues (venue_id),
    organizer_id TEXT   NOT NULL,
    title        TEXT   NOT NULL,
    description  TEXT   NOT NULL DEFAULT '',
    category     TEXT   NOT NULL,
    status       event_status NOT NULL DEFAULT 'DRAFT',
    starts_at    TIMESTAMPTZ NOT NULL,
    ends_at      TIMESTAMPTZ NOT NULL,
    onsale_at    TIMESTAMPTZ NOT NULL,
    -- Incremented in the same transaction as any change, and used as part of the
    -- cache key so old entries become unreachable without an explicit purge (D4).
    version      INT NOT NULL DEFAULT 1,
    created_at   TIMESTAMPTZ NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL,
    CONSTRAINT events_title_not_blank CHECK (length(btrim(title)) > 0),
    CONSTRAINT events_window          CHECK (ends_at > starts_at),
    CONSTRAINT events_onsale_before_start CHECK (onsale_at <= starts_at)
);

-- Supports keyset pagination over the public listing. Partial, because only on-sale
-- events are ever listed.
CREATE INDEX events_onsale_keyset_idx ON events (starts_at, event_id)
    WHERE status = 'ON_SALE';
CREATE INDEX events_venue_idx ON events (venue_id);
CREATE INDEX events_organizer_idx ON events (organizer_id);

CREATE TABLE price_tiers (
    price_tier_id BIGSERIAL PRIMARY KEY,
    event_id      BIGINT NOT NULL REFERENCES events (event_id) ON DELETE CASCADE,
    section       TEXT   NOT NULL,
    price_cents   BIGINT NOT NULL CHECK (price_cents >= 0),
    UNIQUE (event_id, section)
);

CREATE TYPE ticket_status AS ENUM ('AVAILABLE', 'HELD', 'SOLD');

-- The contested resource. One row per (event, seat).
--
-- There is deliberately no available_count column on events: that would be a single
-- hot row serialising every claim for the event (D3). Availability is derived.
--
-- Hash-partitioned by event_id because at target scale this table holds ~1.8B rows,
-- and because every access path is already scoped to a single event.
CREATE TABLE tickets (
    ticket_id       BIGSERIAL,
    event_id        BIGINT NOT NULL REFERENCES events (event_id) ON DELETE CASCADE,
    seat_id         BIGINT NOT NULL REFERENCES seats (seat_id),
    price_tier_id   BIGINT NOT NULL REFERENCES price_tiers (price_tier_id),
    status          ticket_status NOT NULL DEFAULT 'AVAILABLE',
    hold_id         UUID,
    hold_expires_at TIMESTAMPTZ,
    booking_id      UUID,
    PRIMARY KEY (event_id, ticket_id),
    -- The final integrity boundary: a seat cannot be sold twice for one event, even
    -- if application logic were to fail.
    UNIQUE (event_id, seat_id),
    CONSTRAINT tickets_held_has_hold
        CHECK (status <> 'HELD' OR (hold_id IS NOT NULL AND hold_expires_at IS NOT NULL)),
    CONSTRAINT tickets_sold_has_booking
        CHECK (status <> 'SOLD' OR booking_id IS NOT NULL),
    -- An available ticket must carry no residue from a previous hold or sale.
    CONSTRAINT tickets_available_is_clean
        CHECK (status <> 'AVAILABLE' OR (hold_id IS NULL AND hold_expires_at IS NULL AND booking_id IS NULL))
) PARTITION BY HASH (event_id);

CREATE TABLE tickets_p0 PARTITION OF tickets FOR VALUES WITH (MODULUS 8, REMAINDER 0);
CREATE TABLE tickets_p1 PARTITION OF tickets FOR VALUES WITH (MODULUS 8, REMAINDER 1);
CREATE TABLE tickets_p2 PARTITION OF tickets FOR VALUES WITH (MODULUS 8, REMAINDER 2);
CREATE TABLE tickets_p3 PARTITION OF tickets FOR VALUES WITH (MODULUS 8, REMAINDER 3);
CREATE TABLE tickets_p4 PARTITION OF tickets FOR VALUES WITH (MODULUS 8, REMAINDER 4);
CREATE TABLE tickets_p5 PARTITION OF tickets FOR VALUES WITH (MODULUS 8, REMAINDER 5);
CREATE TABLE tickets_p6 PARTITION OF tickets FOR VALUES WITH (MODULUS 8, REMAINDER 6);
CREATE TABLE tickets_p7 PARTITION OF tickets FOR VALUES WITH (MODULUS 8, REMAINDER 7);

-- Serves the availability count and the "cheapest available seat" lookup. Partial so
-- it stays small as inventory sells through.
CREATE INDEX tickets_available_idx ON tickets (event_id, price_tier_id)
    WHERE status = 'AVAILABLE';

-- The expiry reaper scans only held rows, so this index stays small regardless of
-- how much inventory exists.
CREATE INDEX tickets_hold_expiry_idx ON tickets (hold_expires_at)
    WHERE status = 'HELD';

CREATE INDEX tickets_seat_idx ON tickets (seat_id);

CREATE TYPE hold_status AS ENUM ('ACTIVE', 'CONVERTED', 'EXPIRED', 'RELEASED');

-- A hold is a lease: it grants exclusivity across user think-time and the payment
-- call, neither of which may happen inside a database transaction.
CREATE TABLE holds (
    hold_id         UUID PRIMARY KEY,
    event_id        BIGINT NOT NULL REFERENCES events (event_id) ON DELETE CASCADE,
    user_id         TEXT   NOT NULL,
    -- Fences the final HELD -> SOLD conversion, so a holder that lost its lease
    -- cannot sell the seat.
    hold_token      TEXT   NOT NULL,
    status          hold_status NOT NULL DEFAULT 'ACTIVE',
    expires_at      TIMESTAMPTZ NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL,
    idempotency_key TEXT
);

-- Makes a replayed request return the original hold rather than claiming more seats.
CREATE UNIQUE INDEX holds_idempotency_idx ON holds (user_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

CREATE INDEX holds_user_idx ON holds (user_id, created_at DESC);
CREATE INDEX holds_active_expiry_idx ON holds (expires_at) WHERE status = 'ACTIVE';
