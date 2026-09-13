# Move the hold lease from Postgres to Redis locks

Status: **implemented**. See D27 and D28 in `../decisions.md`.
Last updated: 2026-09-13

Deviations from the plan as written, found during implementation:

- **Key helpers live in `internal/lockkeys`**, not only in `booking/locks.go`. `catalog` needs the same key
  format to overlay held seats onto the seat map, and duplicating the format in two services would break the
  seat map silently the first time either side changed.
- **The lease record outlives its locks** by a one-hour grace, carrying a `Status` field. Without it,
  `GET /v1/holds/:id` returned 404 the moment a lease lapsed, and returned 404 for the hold that had just
  produced a booking.
- **`createHold` rejects already-booked seats.** Redis knows nothing about bookings, so an acquire would
  happily lock a seat sold an hour earlier and the buyer would be compensated for a seat they never had a
  chance at.
- **The E2E lock-loss test asserts something stronger than planned.** Verification step 4 expected a
  compensation. In practice the layered checks refuse the losing buyer *before* the provider is called, so no
  money moves at all. The compensation path is unreachable from E2E and remains covered by the
  `duringPaymentHook` unit test.

---

## Context

Today a hold is a row in the `holds` table plus mutable state on the ticket row
(`tickets.status = 'HELD'`, `hold_id`, `hold_expires_at`), claimed inside one Postgres transaction
using `SELECT ... ORDER BY ticket_id FOR UPDATE` and swept by a cron reaper every minute
(`booking/holds.go`, `booking/reaper.go`).

The change replaces that lease with a Redis lock: `SET ticket:{id} {user_id} NX EX ttl`, with
expiry handled by Redis TTL rather than by a reaper. The ticket table drops to two states,
`AVAILABLE` and `BOOKED`. Motivation is speed of acquisition and release under onsale contention,
plus free expiry.

**One caveat to record, since it contradicts a claim in the current design docs.** §7.1 of
`docs/system-design.md` argues Postgres row locking is already adequate here — concurrent claims
touch *different* rows, so they rarely conflict, and locks are held for microseconds; the stated
bottleneck is connection exhaustion, not lock contention. This change is therefore not a fix for a
measured problem in this codebase, and it trades a single-transaction guarantee for a two-system
design. It is being made deliberately; the plan below is built to preserve the hard invariant
regardless.

### Decisions taken

| # | Decision |
|---|---|
| 1 | **Postgres keeps the conditional write.** The commit stays `WHERE status = 'AVAILABLE'`, so a lost Redis lock can never produce an oversell — only a compensation. |
| 2 | **go-redis on a self-managed Redis.** `encore.dev/storage/cache` exposes no `EVAL` and no client handle, so Lua requires a separate instance plus its own config, secrets and pooling. |
| 3 | **Per-event sorted set for the seat map.** `ZADD event:{id}:locks {expires_at} {ticket_id}`, read with `ZRANGEBYSCORE key {now} +inf`. Lock `SET` and `ZADD` happen in one Lua script. |
| 4 | **Redis key TTL is authoritative** for expiry. The ZSET is a secondary index for reads. |
| 5 | **miniredis for unit tests** — supports `EVAL`, ZSET ops, `PTTL` and `FastForward`, so unit tests stay deterministic and the real Lua scripts still execute. |

## Redis key design

| Key | Type | Purpose |
|---|---|---|
| `lock:{ticket_id}` | string → `user_id` | The lock. TTL is authoritative for expiry. |
| `event:{event_id}:locks` | zset, score = expiry unix | Seat map and availability overlay |
| `hold:{hold_id}` | string → JSON `{user_id, event_id, ticket_ids, token}` | Purchase needs the hold's ticket set; `tickets.hold_id` no longer exists |
| `user:{user_id}:holds` | zset, score = expiry unix | Quota — `ZCOUNT key {now} +inf` replaces the `holds` count query |
| `idem:hold:{user_id}:{key}` | string → `hold_id` | Replaces `holds_idempotency_idx` |

ZSET members are not removed by key TTL, so every acquire also runs
`ZREMRANGEBYSCORE key -inf {now}` to stop hot events accumulating dead members.

## Implementation

### 1. Lock layer — new `booking/locks.go`

A `Locker` interface over go-redis, constructed from a connection string in Encore secrets. Three
Lua scripts, all taking `now` as an argument so behaviour is explicit rather than implicit in
server time:

- **acquire** — for each ticket, fail if `EXISTS lock:{id}`; otherwise `SET` each with `EX`, `ZADD`
  to the event and user sets. Atomic across the whole seat set, which is what replaces
  `ORDER BY ticket_id FOR UPDATE`. All keys for one event share a hash tag so they land on one slot.
- **release** — compare-and-delete: `DEL` only where `GET == user_id`, plus `ZREM`. This is the
  operation Encore's cache could not do safely and is the main reason for the raw client.
- **extend** — compare-and-`PEXPIRE`, for the D9 lease extension.

### 2. Booking service

- `booking/holds.go` — `createHold` loses its transaction entirely: validate, check quota via
  `ZCOUNT`, call acquire, write the `hold:{id}` record. `assertOnSale` still reads Postgres.
  `loadHold` reads from Redis. `releaseHold` calls the release script.
- `booking/purchase.go` — `prepareLease` reads `hold:{hold_id}` instead of `SELECT ... FOR UPDATE`,
  and **prices from `price_tiers` joined on the ticket ids in the hold record**, since
  `tickets.hold_id` is gone. The D9 extension becomes the extend script. `convertToSold` keeps its
  `WHERE status = 'AVAILABLE'` guard (decision 1) and still writes booking, tickets and outbox in
  one transaction. Compensation path is unchanged.
- `booking/reaper.go` — **delete**, along with the cron registration and
  `POST /internal/holds/reap`.
- `booking/integrity.go` — drop `Held` and `HeldWithoutHold`; `Consistent` becomes
  `Total == Available + Booked` plus the existing duplicate-seat, sold-without-booking and
  compensation checks. The duplicate-seat query is unchanged and remains the direct oversell check.

### 3. Schema — new migration `store/migrations/5_redis_holds.up.sql`

Drop `tickets.hold_id`, `tickets.hold_expires_at`, `tickets_hold_expiry_idx`, and the
`status <> 'HELD'` CHECK. Replace the `ticket_status` enum with `AVAILABLE | BOOKED`. Drop the
`holds` table and the `bookings.hold_id` foreign key — keep `bookings.hold_id` as a plain UUID
column for audit, since a booking should still record which hold produced it.

### 4. Catalog

`catalog/catalog.go` — `buildAvailability` and `GetSeatMap` overlay
`ZRANGEBYSCORE event:{id}:locks {now} +inf` onto the Postgres rows to restore the three-state seat
map. One Redis round trip per read, not per seat. `GetSeatMap` stays uncached; the availability
snapshot keeps its 5s TTL.

### 5. testsupport

`testsupport/testsupport.go:43` — `Reset` must flush the lock Redis alongside `store.TruncateAll`,
or holds leak between tests. Remove `ReapHolds` from the harness (`e2e/booking_fixtures.go:244`).

## Test plan

**Delete (9)** — the reaper no longer exists: all 4 in `booking/reaper_test.go`, all 5 in
`e2e/reaper_test.go`.

**Rewrite for TTL expiry (6)** — unit tests use `mr.FastForward`, E2E uses a short TTL plus
`require.Eventually` rather than a fixed sleep:

| Test | File |
|---|---|
| `TestConcurrentTakeoverOfExpiredHold` | `booking/holds_concurrency_test.go:360` |
| `TestPurchaseExtendsShortLease` | `booking/purchase_test.go:188` — assert `PTTL`, not `hold_expires_at` |
| `TestExpiredHoldIsImmediatelyReclaimable` | `e2e/holds_test.go:296` |
| `TestPurchaseRejectsExpiredHold` | `e2e/purchase_test.go:224` |
| `TestPurchaseExtendsHoldWhenTTLIsShort` | `e2e/purchase_test.go:239` |
| load proof | `e2e/loadproof_test.go:119,124,287` |

**Rewrite against the Lua acquire (4)** — the four row-locking tests in
`holds_concurrency_test.go`. `TestOverlappingMultiSeatClaimsDoNotDeadlock` becomes a livelock test:
Redis has no deadlock to avoid, but two users claiming overlapping sets must still resolve to one
winner rather than both retrying forever.

**Add (2)** — a lock lapsing after purchase leaves the ticket `BOOKED` (preserves the intent of
`TestReaperNeverReleasesSoldSeats`); and quota is restored when a lock expires (preserves
`TestReaperRestoresHoldQuota`, now against `ZCOUNT`).

`HoldTTL` (`booking/booking.go:33`) must become configurable so test environments can use ~2s.

Unaffected: everything using the injected clock for *event scheduling* — `search_test.go`,
`catalog_test.go`, `harness_test.go`, and the onsale-window subtest at `e2e/holds_test.go:188`. D11
narrows to exclude hold expiry; it is not withdrawn.

## Verification

1. `go build ./...` then `encore test ./booking/...` — unit tests must stay sleep-free via
   miniredis `FastForward`.
2. `make e2e` (or the `e2e` build-tagged suite against a running app) — expect roughly 20-30s added
   from real TTL waits.
3. **The load proof is the acceptance test.** `e2e/loadproof_test.go` asserts
   `sold == capacity`, zero oversells and `Consistent` under concurrent contention. It must pass
   unchanged in its assertions, only in its expiry mechanics.
4. **Deliberate failure injection, which the old design did not need:** kill or `FLUSHALL` the lock
   Redis mid-run and confirm the conditional write still prevents an oversell —
   `Integrity.DuplicateSeats == 0` and `Sold == TicketsInConfirmedBookings` — with losses appearing
   as `COMPENSATING` bookings rather than double-sold seats. This is the test that proves decision 1
   is doing its job.
5. `GET /internal/booking/integrity/:eventID` returns `consistent: true` after each e2e run.

## Docs to update

`docs/system-design.md` §3 (ticket lifecycle, Hold entity), §5 (the sequence diagram — the claim
phase is no longer a Postgres transaction), §6 (flowchart gains a second Redis), §7.1 (the entire
contention deep-dive), §9 (`booking_tickets_reaped` disappears), §10. `docs/decisions.md` needs new
entries for the Redis lease, superseding the ones that argue for the in-database lease, and a
narrowing of D11.
