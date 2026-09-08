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
| 7 | Read-path hardening (cache, ETag, read model) | ⬜ planned |
| 8 | Load proof — zero oversell under concurrency | ⬜ planned |

Steps 7–8 are fully specified in the implementation plan but not yet built.

The zero-oversell invariant is **proven by tests**: 32 goroutines racing for one seat yield exactly one
winner, and a 40-seat concurrent sellout claims every seat exactly once with none stranded. What is not yet
proven is behaviour under *sustained* onsale load — that is Step 8. The read path also still lacks its cache,
versioned keys and read model (Step 7), so availability is computed per request: correct, but not at the
80k RPS target.
