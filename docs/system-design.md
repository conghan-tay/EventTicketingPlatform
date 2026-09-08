# Event Ticketing Platform — System Design

Status: agreed. Source of truth for implementation.
Last updated: 2026-09-08

---

## 1. Scope

An application where users can **view events, search events, and book tickets**, at a target scale of
**100M daily users** and **100k new events per day**.

### Actors

| Actor | Role |
|---|---|
| **Attendee** | Views events, searches, holds and buys tickets |
| **Organizer** | Creates venues and events, publishes them for sale |
| **System** | Expires abandoned holds, reconciles ambiguous payments |

### Functional requirements

1. **A user can view an event** — details, pricing tiers, and seat-level availability.
2. **A user can search events** — free text over title/description, filtered by date range, location
   radius, and category, with cursor pagination.
3. **A user can book tickets** — select specific seats, hold them, pay, and receive a confirmed booking,
   with **zero possibility of two users owning the same seat**.

Supporting (deliberately thin, required to make the above testable): an organizer can create a venue with a
seat map, create an event, and publish it.

### Out of scope (deferred)

Refunds, cancellations, ticket transfer/resale, dynamic pricing, seat-map rendering, recommendations,
multi-currency, QR/entry scanning, virtual waiting room, real-time seat updates.

---

## 2. Non-functional requirements

Only qualities that materially shape this system.

| Requirement | Target | Design consequence |
|---|---|---|
| **Oversell rate** | **Exactly zero** — hard invariant | Conditional writes on the ticket row; booking fails closed |
| Catalog read p95 | < 150 ms (cache/CDN hit) | Versioned cache keys, CDN, denormalized read model |
| Search p95 | < 500 ms | GIN index on tsvector; named trigger for OpenSearch |
| Hold claim p99 | < 300 ms under onsale spike | Microsecond transactions; no external calls under lock |
| Catalog availability | 99.95%, **may serve stale** | Browse survives a booking-database outage |
| Booking availability | 99.9%, **fails closed** | Refuse to sell rather than risk an oversell |
| Durability | Bookings + payments RPO 0; search index rebuildable | Synchronous commit on the money path only |
| Consistency | Catalog/search eventual (≤5s); inventory claim strongly consistent | Seat map is advisory; the claim is authoritative |
| Security | Bearer auth; identity from verified context only | Never trust a user id in a request body |

### Capacity estimation

| Quantity | Estimate | Decision it informs |
|---|---|---|
| Catalog reads | 100M DAU × ~7 = 700M/day → **8k RPS avg, ~80k peak** | One Postgres cannot serve this; CDN + cache required |
| Bookings | ~1% of DAU → 1M/day → **~12 writes/s** | Aggregate write volume needs no sharding at all |
| Onsale burst | 50k seats in ~60s, ~500k competing users → **~1k claims/s on one event** | Real problem is hot-key contention + connection exhaustion |
| Active inventory | 100k events/day × 90-day window × ~200 seats → **~1.8B ticket rows (~400GB)** | Hash-partition `tickets` by `event_id`; sharding deferred |
| Active catalog | ~9M events × ~2KB = **~18GB** | Fits entirely in cache/CDN; catalog reads rarely reach Postgres |

**Governing conclusion.** This is a **~1000:1 read-heavy system with a trivial global write rate but extreme
per-event write contention**. Therefore: do not shard, do not add a queue to the write path, do not add a
workflow engine. The complexity budget goes to (a) the seat-claim invariant and (b) the read path.

The system splits into two paths that must not share a cache, a connection pool, or a code path:

| Path | Consistency | Scaling tool |
|---|---|---|
| Catalog + search (reads) | Eventually consistent, ≤5s stale acceptable | CDN, versioned cache keys, read replicas |
| Inventory + booking (writes) | Strongly consistent | Conditional writes on the seat row, leader only |

### Assumptions

- Average venue ~200 seats; largest venues ~100k seats.
- Events remain on sale ~90 days.
- Reserved seating for all events (general admission is modelled later as a degenerate case).
- Single region for the initial build; multi-region is a documented later step.

---

## 3. Core entities

| Entity | Responsibility | Lifecycle |
|---|---|---|
| **Venue** | Physical place; owns immutable seat topology | — |
| **Seat** | A seat *template* in a venue (section, row, number) | — |
| **Event** | A performance at a venue at a time, with an onsale window | `DRAFT → ON_SALE → CLOSED / CANCELLED` |
| **PriceTier** | Price for a section within an event | — |
| **Ticket** | **The contested resource** — one row per (event, seat); the sellable instance | `AVAILABLE → HELD → SOLD` |
| **Hold** | A user's expiring lease over 1..N tickets, with an opaque token | `ACTIVE → CONVERTED / EXPIRED / RELEASED` |
| **Booking** | Durable result of a successful purchase | `PENDING → CONFIRMED / FAILED / COMPENSATING → COMPENSATED` |
| **Payment** | Idempotent record of an external charge effect | `NOT_STARTED → IN_PROGRESS → COMPLETED / FAILED` |
| **User** | Attendee identity | — |

### Ownership and invariants

- `booking` service is authoritative for `tickets`, `holds`, `bookings`. **It is the only writer of inventory.**
- `organizer` service is authoritative for `venues`, `seats`, `events`, `price_tiers`.
- `payments` service is authoritative for `payments`.
- `catalog` and `search` own **no** authoritative state — they serve derived, cacheable projections.

The **Seat vs Ticket** split is deliberate: `Seat` is venue-level and immutable; `Ticket` is event-level and
contested. Publishing an event materializes one `Ticket` per `Seat`.

**There is deliberately no `available_count` column on `events`.** Such a column would be a single hot row
serializing every claim for that event. Availability counts are derived and eventually consistent. This
follows the contention playbook: *protect the seat row, not a venue-level counter.*

---

## 4. Interfaces

REST/HTTP — the framework default for resource-oriented product APIs. No GraphQL (clients do not need
materially different projections). No WebSocket in v1 (no server-driven latency requirement yet).

### Catalog — public, cacheable, eventually consistent

```
GET  /v1/events/:id                        → event detail (ETag, Cache-Control, versioned cache key)
GET  /v1/events/:id/availability           → per-section counts (5s TTL, explicitly stale)
GET  /v1/events/:id/seats?section=         → seat-level map (advisory only, NOT authoritative)
GET  /v1/search?q=&from=&to=&lat=&lng=&radius_km=&category=&cursor=&limit=
```

Search results are ordered by event start time, soonest first — not by relevance (D16). Free text matches a
stemmed `tsvector` or a trigram substring on the title, and `websearch_to_tsquery` is used so hostile input
cannot raise a syntax error. `lat`, `lng` and `radius_km` must be supplied together; a venue with no
coordinates is excluded from radius results because its location is unknown.

### Booking — `auth`, strongly consistent, never cached

```
POST   /v1/events/:id/holds     {seat_ids[]} + Idempotency-Key
                                → 201 {hold_id, hold_token, expires_at, seats[], total_cents}
                                → 409 SEAT_UNAVAILABLE {lost_seat_ids[]} | HOLD_LIMIT_EXCEEDED
                                → 422 EVENT_NOT_ON_SALE
GET    /v1/holds/:id            → status + seconds_remaining
DELETE /v1/holds/:id            → release early
POST   /v1/holds/:id/purchase   {payment_method} + X-Hold-Token + Idempotency-Key
                                → 201 {booking_id, status}
                                → 409 HOLD_EXPIRED | HOLD_NOT_OWNED | 402 PAYMENT_FAILED
GET    /v1/bookings/:id         → booking status
GET    /v1/bookings             → my bookings (cursor paginated)
```

### Organizer — `auth`

```
POST /v1/venues                     POST /v1/venues/:venueID/seats
POST /v1/events                     POST /v1/events/:eventID/publish   → materializes tickets
```

### Internal — `private` (cron in cloud; invoked directly by tests locally)

```
POST /internal/holds/reap            → releases expired holds, returns count
POST /internal/bookings/reconcile    → resolves ambiguous IN_PROGRESS payments
```

### Contracts

- **Identity** comes only from the verified auth context. A user id in a request body is never trusted.
- **Idempotency**: both mutating booking endpoints are idempotent on `Idempotency-Key`.
- **Pagination**: keyset/cursor everywhere. Never `OFFSET`.
- **Errors** return business results (`SEAT_UNAVAILABLE`, `HOLD_EXPIRED`), never a generic 500.

---

## 5. Data flow

### Booking (the multi-step path)

```
select seats → conditional claim (AVAILABLE→HELD, one short tx) → return hold + token
→ user pays → charge with idempotency key (outside all locks)
→ conditional convert (HELD→SOLD, fenced by hold token; same tx as booking insert + outbox row)
→ CONFIRMED
```

Failure branches:

| Branch | Handling |
|---|---|
| Payment declined | Release hold fenced by `hold_id` → booking `FAILED`. Clean. |
| Charge succeeded but convert lost (hold expired mid-payment) | The money-losing case → `COMPENSATING` → refund → `COMPENSATED` + alert. **Mitigated preemptively:** only charge if remaining TTL > payment timeout budget; otherwise extend the hold (conditional on token) first. |
| Crash between charge and convert | `reconcile` job finds `IN_PROGRESS` payments and resolves against the provider. **Never blind-retries a charge.** |
| Hold abandoned | Reaper releases it, fenced by `hold_id` and `status='HELD'`. |

### Search indexing

**No pipeline.** A `search_vector tsvector GENERATED ALWAYS AS (...) STORED` column with a GIN index keeps
the search index in the same transaction as the write — zero indexing lag, zero staleness, nothing to
rebuild. This is the main practical benefit of starting on Postgres FTS rather than an external engine.

---

## 6. High-level architecture

A **modular Encore application** — separate services with strict boundaries, so the read/write split is
enforced at the service layer and each service can be scaled or extracted independently.

```
                    ┌────────── CDN (public catalog only) ──────────┐
                    │                                               │
client ──► Encore API gateway ──┬──► catalog  ─► cache ─► Postgres (replica-eligible)
                                ├──► search   ─────────► Postgres (GIN / FTS)
                                ├──► booking  ─────────► Postgres (LEADER ONLY, never cached)
                                ├──► payments ─────────► provider (mock, Stripe-shaped)
                                ├──► organizer ────────► Postgres
                                └──► testsupport (local + test environments only)
```

| Service | Responsibility | Why separate |
|---|---|---|
| **catalog** | Event/venue reads | Cacheable, replica-eligible, degrades to stale |
| **search** | Query + rank | A heavy query cannot starve the booking pool; contains the OpenSearch swap |
| **booking** | `tickets`, `holds`, `bookings` | The only inventory writer; leader reads only |
| **payments** | Idempotent charge effects | Wraps the provider behind an interface |
| **organizer** | Authoring + ticket materialization | Write patterns unrelated to attendee traffic |
| **testsupport** | Reset, seed, clock control | Refuses to run outside local/test environments |

### Schema highlights

```sql
CREATE TABLE tickets (
  ticket_id       BIGSERIAL,
  event_id        BIGINT NOT NULL,
  seat_id         BIGINT NOT NULL,
  price_tier_id   BIGINT NOT NULL,
  status          ticket_status NOT NULL DEFAULT 'AVAILABLE',
  hold_id         UUID,
  hold_expires_at TIMESTAMPTZ,
  booking_id      UUID,
  PRIMARY KEY (event_id, ticket_id),
  UNIQUE (event_id, seat_id),                                   -- final integrity boundary
  CHECK (status <> 'HELD' OR (hold_id IS NOT NULL AND hold_expires_at IS NOT NULL)),
  CHECK (status <> 'SOLD' OR booking_id IS NOT NULL)
) PARTITION BY HASH (event_id);

CREATE INDEX ON tickets (event_id, status) WHERE status = 'AVAILABLE';
CREATE INDEX ON tickets (hold_expires_at) WHERE status = 'HELD';   -- reaper scans only held rows

ALTER TABLE events ADD COLUMN version INT NOT NULL DEFAULT 1;      -- bumped in-tx, feeds cache key
ALTER TABLE events ADD COLUMN search_vector tsvector
  GENERATED ALWAYS AS (to_tsvector('english', title || ' ' || coalesce(description,''))) STORED;
CREATE INDEX ON events USING GIN (search_vector);
CREATE INDEX ON events (starts_at, event_id) WHERE status = 'ON_SALE';   -- keyset pagination
```

---

## 7. Deep dives

Three, selected by evidence: a correctness invariant that can fail, a quantified NFR the baseline misses,
and a money-losing failure mode.

### 7.1 Seat contention and the hold lease

**Invariant:** *a ticket has at most one owner, and a SOLD ticket is never released.*

Decision-ladder position: **conditional write (level 2) + lease (level 6)**. A lease is required because
exclusivity must span user think-time and an external payment call.

Multi-seat holds are **all-or-nothing**, which requires a deterministic lock order to avoid deadlock when
two users claim overlapping seat sets in opposite orders:

```sql
BEGIN;
-- ORDER BY is load-bearing: it prevents deadlock between concurrent overlapping claims
SELECT ticket_id, status, hold_expires_at
  FROM tickets
 WHERE event_id = $1 AND ticket_id = ANY($2)
 ORDER BY ticket_id
   FOR UPDATE;

-- application asserts every ticket is claimable; if not → ROLLBACK, return 409 with lost_seat_ids
UPDATE tickets
   SET status = 'HELD', hold_id = $3, hold_expires_at = $4
 WHERE event_id = $1 AND ticket_id = ANY($2) AND status = 'AVAILABLE';
-- affected rows MUST equal len(seat_ids), else ROLLBACK
INSERT INTO holds (...) VALUES (...);
COMMIT;
```

**Why 1k claims/s on one event is fine:** concurrent claims touch **different rows**, so they do not
conflict. Contention appears only on genuinely popular *seats*, where it resolves as a fast conditional
failure. The real onsale risk is **connection exhaustion and retry storms**, addressed by PgBouncer,
per-user rate limits, and (deferred) the waiting room.

Locks are held for microseconds. **No external call ever occurs inside these transactions.**

Purchase conversion is fenced by the hold token, so a stale holder can never sell a seat it lost:

```sql
UPDATE tickets SET status='SOLD', booking_id=$4, hold_id=NULL, hold_expires_at=NULL
 WHERE event_id=$1 AND ticket_id=ANY($2) AND status='HELD' AND hold_id=$3;
```

Expiry reaper — fenced, and safe to run concurrently:

```sql
UPDATE tickets SET status='AVAILABLE', hold_id=NULL, hold_expires_at=NULL
 WHERE ticket_id IN (
   SELECT ticket_id FROM tickets
    WHERE status='HELD' AND hold_expires_at < $1
    LIMIT 1000 FOR UPDATE SKIP LOCKED    -- multiple reapers never fight
 );
```

`SKIP LOCKED` plus the `status='HELD'` predicate guarantees the reaper can never release a SOLD ticket.

**Tradeoff:** all-or-nothing holds mean a user competing for a popular row loses the whole set and must
retry. Accepted — partial holds are a worse experience and complicate pricing.

### 7.2 Read path for 80k RPS

Read-scaling ladder applied in order, stopping where the SLO is met:

1. **Indexes + keyset pagination** — no `OFFSET`, no full scans.
2. **Denormalized read model** — `event_view` carries venue and tier summary pre-joined, so event detail is
   a single primary-key lookup.
3. **Versioned cache keys** — `events.version` is incremented **in the same transaction** as any event
   update, and the version is part of the cache key. Old keys become unreachable and expire naturally, so
   there is **no invalidation race and no explicit purge**.
4. **Read replicas** for catalog and seat map. **Never** for a claim.
5. **CDN** on `GET /v1/events/:id` with short TTL + ETag. The ~18GB catalog fits comfortably.
6. **Sharding: deferred**, with `event_id` recorded as the partition key.

**The load-bearing decision:** availability reads are *deliberately* not strongly consistent. The seat map is
advisory; only the conditional write is authoritative. This is what makes the read path cacheable at all.
Single-flight coalescing plus TTL jitter guard against stampedes on a hot event.

**Cache failure policy:** cache unavailability falls back to Postgres behind a concurrency limiter, so a
cache outage cannot become an unbounded database stampede.

### 7.3 Payment saga

Shape: two local commits with a compensation — a **saga**, not a workflow engine. See
`decisions.md` for the Temporal evaluation and the named trigger to revisit.

The payment effect record is keyed on a unique `idempotency_key`, and `IN_PROGRESS` is treated as
**ambiguous**: reconcile against the provider, never blind-retry an irreversible charge.

---

## 8. Security

- Bearer-token auth via an Encore auth handler; `auth` access level on all attendee/organizer mutations.
- Hold ownership is verified on purchase (`hold.user_id == auth.uid`) **and** fenced by `X-Hold-Token`.
- Per-user and per-user-per-event rate limits on hold creation, to blunt scripted seat sniping.
- No PII in cache keys or CDN-cacheable responses; the catalog cache holds only public event data.
- Organizer endpoints authorize against event ownership, not merely authentication.

## 9. Observability

Metrics that identify the actual bottleneck: claim conflict rate, hold conversion rate, holds reaped,
oversell counter (**must remain exactly zero**), payment `IN_PROGRESS` age, compensation count, cache hit
rate, search p95, replica lag.

## 10. Known limitations and deferred work

| Item | Trigger to revisit |
|---|---|
| Virtual waiting room | First onsale where claim p99 breaches 300 ms. Seam: booking endpoints already validate an admission claim that is a no-op in v1. |
| OpenSearch | Search p95 > 500 ms, or > ~2M active events |
| Temporal | When refunds, cancellations, transfers, or organizer payouts land |
| Real-time seat updates (SSE) | Deferred with seat-map rendering |
| Sharding `tickets` | When one partitioned Postgres no longer absorbs write volume |
| Multi-region | Beyond the initial single-region build |
