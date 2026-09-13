-- The seat lease moves from Postgres to Redis.
--
-- A hold used to be a row in `holds` plus mutable state on the ticket row, swept by a
-- cron reaper. It is now a Redis key whose TTL expires on its own. What remains in
-- Postgres is the durable outcome: a ticket is AVAILABLE or BOOKED, nothing else.
--
-- The invariant is unchanged and still lives here. UNIQUE (event_id, seat_id) plus the
-- conditional write in convertToSold are what make an oversell unrepresentable; Redis
-- is an optimistic gate in front of that, not a replacement for it.

-- The reaper is gone, and so is the index that served it.
DROP INDEX IF EXISTS tickets_hold_expiry_idx;

-- This index has 'AVAILABLE' in its predicate, so it has to be rebuilt around the
-- enum swap below rather than altered in place.
DROP INDEX IF EXISTS tickets_available_idx;

ALTER TABLE tickets DROP CONSTRAINT IF EXISTS tickets_held_has_hold;
ALTER TABLE tickets DROP CONSTRAINT IF EXISTS tickets_sold_has_booking;
ALTER TABLE tickets DROP CONSTRAINT IF EXISTS tickets_available_is_clean;

-- bookings.hold_id stays as a plain UUID for audit — it still records which lease
-- produced the booking — but it can no longer reference a table that is being dropped.
ALTER TABLE bookings DROP CONSTRAINT IF EXISTS bookings_hold_id_fkey;

-- Any lease in flight at migration time is meaningless: Redis starts empty, so nothing
-- would ever release these rows. Return them to inventory rather than strand them.
UPDATE tickets
   SET status = 'AVAILABLE', hold_id = NULL, hold_expires_at = NULL
 WHERE status = 'HELD';

ALTER TABLE tickets DROP COLUMN hold_id;
ALTER TABLE tickets DROP COLUMN hold_expires_at;

-- Postgres cannot remove a value from an enum, so the type is replaced. SOLD becomes
-- BOOKED; HELD ceases to exist.
CREATE TYPE ticket_status_v2 AS ENUM ('AVAILABLE', 'BOOKED');

ALTER TABLE tickets
    ALTER COLUMN status DROP DEFAULT,
    ALTER COLUMN status TYPE ticket_status_v2
        USING (CASE WHEN status::text = 'SOLD' THEN 'BOOKED' ELSE 'AVAILABLE' END)::ticket_status_v2,
    ALTER COLUMN status SET DEFAULT 'AVAILABLE'::ticket_status_v2;

DROP TYPE ticket_status;
ALTER TYPE ticket_status_v2 RENAME TO ticket_status;

-- A booked ticket must say who bought it; an available one must carry no residue.
ALTER TABLE tickets ADD CONSTRAINT tickets_booked_has_booking
    CHECK (status <> 'BOOKED' OR booking_id IS NOT NULL);
ALTER TABLE tickets ADD CONSTRAINT tickets_available_is_clean
    CHECK (status <> 'AVAILABLE' OR booking_id IS NULL);

CREATE INDEX tickets_available_idx ON tickets (event_id, price_tier_id)
    WHERE status = 'AVAILABLE';

DROP TABLE holds;
DROP TYPE hold_status;
