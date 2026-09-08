# Event Ticketing Platform

An event ticketing system where users can **view events, search events, and book tickets**, designed for a
target scale of **100M daily users** and **100k new events per day**.

Built with [Encore.dev](https://encore.dev) and Go, backed by PostgreSQL.

## The design in one paragraph

The capacity math produces the finding that shapes everything: this is a **~1000:1 read-heavy system with a
trivial global write rate (~12 bookings/second) but extreme per-event write contention** (~1k claim attempts
per second on a single hot event). So the system splits into two paths that deliberately share neither a
cache nor a code path — a cacheable, eventually-consistent **catalog/search** path, and a strongly
consistent, never-cached **inventory** path where correctness lives in a conditional write on the individual
seat row.

Read [`docs/system-design.md`](docs/system-design.md) for the full design and
[`docs/decisions.md`](docs/decisions.md) for why each significant choice was made.

## Documentation

| Document | Contents |
|---|---|
| [`docs/system-design.md`](docs/system-design.md) | Requirements, capacity math, entities, interfaces, architecture, deep dives |
| [`docs/decisions.md`](docs/decisions.md) | Architecture decision record with rationale and tradeoffs |
| [`docs/implementation-plan.md`](docs/implementation-plan.md) | E2E test strategy, dependencies, ordered build plan |

## Prerequisites

- [Go](https://go.dev/dl/) 1.26+
- [Encore CLI](https://encore.dev/docs/install) 1.57+
- Docker (Encore provisions PostgreSQL and Redis locally)

## Running

```bash
encore run          # or: make run
```

The API listens on `http://localhost:4000`. The local dev dashboard, with request tracing and an API
explorer, is at `http://localhost:9400`.

```bash
curl http://localhost:4000/health
```

## Testing

The E2E suite is the project's progress dashboard — as features land, more tests turn green.

```bash
make test    # unit + service integration (Encore provisions isolated test databases)
make e2e     # boots the app, waits for /health, runs the HTTP E2E suite, tears down
make check   # everything CI would run
```

E2E tests are behind the `e2e` build tag so `encore test ./...` does not pick them up. `make e2e` always
supplies the tag, so they can never be silently skipped.

Two testing choices worth knowing about:

- **Tests speak HTTP**, not Go function calls, so routing, serialisation, validation and auth are genuinely
  exercised.
- **Time is injected** ([`internal/clock`](internal/clock/clock.go)). Business logic never calls
  `time.Now()`, so hold expiry is tested by advancing a clock rather than sleeping.

## Build status

| Step | Scope | Status |
|---|---|---|
| 0 | Scaffold and dev environment | ✅ done |
| 1 | E2E harness + testsupport service | ✅ done |
| 2 | Catalog vertical slice (venues, events, publish, event detail) | ✅ done |
| 3 | Search (text, date, location, category, cursor pagination) | ✅ done |
| 4 | Holds — the contention core | ✅ done |
| 5 | Purchase and the payment saga | ✅ done |
| 6 | Hold expiry reaper | ✅ done |
| 7 | Read-path hardening (versioned cache, ETag, single-flight) | ✅ done |
| 8 | Load proof — zero oversell under concurrency | ✅ done |

## The invariant, proven

The system exists to guarantee one thing: **a seat has at most one owner**. That is not asserted, it is
tested at three levels.

**Unit, under maximal contention** — 32 goroutines released simultaneously at a single seat yield exactly
one winner and 31 clean conflicts. Overlapping multi-seat claims submitted in *opposite* orders never
deadlock; remove the `ORDER BY ticket_id` from the locking read and that test fails with
`deadlock detected`.

**End to end** — a sold seat can never be held again; a declined payment releases seats; an ambiguous
provider failure releases nothing.

**Load proof** (`make loadproof`) — a 5,000-seat event sold out through the real HTTP stack by 60 concurrent
workers:

```
holds created   : 625
hold conflicts  : 481        <- real contention, not a serialised run
purchases ok    : 625
other errors    : 0
Total:5000  Available:0  Held:0  Sold:5000
DuplicateSeats:0  SoldWithoutBooking:0  HeldWithoutHold:0  UnresolvedCompensations:0
ConfirmedBookings:625  TicketsInConfirmedBookings:5000  Consistent:true
```

Every seat sold exactly once, none stranded, no oversell — with 481 genuine conflicts along the way. The
test fails if no conflicts occur, because a run without contention would prove nothing.
