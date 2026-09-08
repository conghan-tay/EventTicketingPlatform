# Implementation Plan

Derived from `system-design.md` and `decisions.md`. Progress is marked as steps complete.
Last updated: 2026-09-08

**Current build scope: Steps 0–6** (see D13). Steps 7–8 are specified but not built.

---

## 1. E2E testing strategy

Test-driven at the feature level. For each feature: write the E2E test, confirm it fails for the *expected*
reason, implement the minimum behavior, make it pass, keep all previously green tests green.

The E2E suite is the project's live progress dashboard.

### Fixtures

| Fixture | Purpose |
|---|---|
| `NewHarness(t)` / `Reset()` | Empties all tables and releases the clock, via `testsupport` |
| `CreateVenue(VenueOpts)` | Creates a venue plus its seat map; zero fields get defaults |
| `AddSeats(venueID, sections)` | Materializes seat topology from section specs |
| `CreateEvent(venueID, EventOpts)` | Creates a DRAFT event; `opts` controls title, category, dates, tiers |
| `PublishEvent(id)` | Publishes, materializing tickets |
| `SeedPublishedEvent(v, e)` | Composes the three above — the common setup |
| `SetClock(t)` / `AdvanceClock(d)` / `ClockNow()` | Deterministic time control (D11) |
| `TableCounts()` | Per-table row counts, for asserting inventory was materialized |
| `AsUser(uid)` / `Anonymous()` | Clients with and without credentials |

### Client

`e2e/client.go` — a typed HTTP client over the running app's base URL (default `http://localhost:4000`,
overridable via `E2E_BASE_URL`). Tests speak HTTP, not Go function calls, so serialization, validation,
routing, and auth are all genuinely exercised.

### Happy paths (minimal set proving the agreed requirements)

| # | Test | Step |
|---|---|---|
| H1 | Health check responds | 0 |
| H2 | Reset + seed + clock control work | 1 |
| H3 | Create venue → create event → publish → `GET /v1/events/:id` returns correct tiers and seat counts | 2 |
| H4 | Publishing a 200-seat venue materializes exactly 200 `AVAILABLE` tickets | 2 |
| H5 | Search by free text finds a published event | 3 |
| H6 | Search filters by date range, location radius, and category | 3 |
| H7 | Cursor pagination walks all results with no duplicates and no gaps | 3 |

### Sad paths (minimal set of important failures)

| # | Test | Step |
|---|---|---|
| S1 | `testsupport` refuses to run when the environment is not local/test | 1 |
| S2 | `GET /v1/events/:id` on an unknown id returns 404 | 2 |
| S3 | Creating an event with `ends_at` before `starts_at` is rejected 400 | 2 |
| S4 | Publishing an already-published event is rejected (or is idempotent — asserted explicitly) | 2 |
| S5 | Organizer endpoints reject unauthenticated calls with 401 | 2 |
| S6 | A `DRAFT` (unpublished) event never appears in search results | 3 |
| S7 | Malformed cursor returns 400, not a 500 | 3 |
| S8 | `limit` above the maximum is clamped, not honored blindly | 3 |

### Unit tests

Only where an E2E test cannot economically reach an important case.

| Target | Why a unit test | Step |
|---|---|---|
| Cursor encode/decode round-trip + tamper rejection | Branch-heavy; silent failure would corrupt pagination | 3 |
| Seat-map materialization arithmetic | Off-by-one in section/row/seat generation is silent | 2 |
| Search query builder — filter combinations | Combinatorial; an E2E per combination is wasteful | 3 |
| Concurrent claim: N goroutines, one seat → exactly one winner | Unreachable via E2E | 4 ✅ |
| Overlapping multi-seat claims never deadlock | Unreachable via E2E | 4 ✅ |
| Concurrent sellout claims every seat exactly once | Unreachable via E2E | 4 ✅ |
| Concurrent takeover of an expired lease has one winner | Unreachable via E2E | 4 ✅ |
| Reaper leaves a SOLD ticket sold | Hard-to-reach state | 6 ✅ |
| Concurrent reapers release each seat once (SKIP LOCKED) | Unreachable via E2E | 6 ✅ |
| Charge succeeds while hold expires → `COMPENSATING` | Hard-to-reach state; money-losing if wrong | 5 ✅ |
| Refund failure leaves the booking `COMPENSATING` | Compensation is fallible | 5 ✅ |

---

## 2. Dependencies

All versions verified against `proxy.golang.org` and Docker Hub on 2026-09-08. None written from memory.

| Name | Version | Purpose |
|---|---|---|
| Go | 1.26.5 | Language (confirmed installed) |
| Encore CLI | 1.57.11 | App framework, local infra, test databases |
| encore.dev | v1.57.13 | Runtime: `sqldb`, `cache`, `cron`, `auth`, `et` |
| Postgres | 18 | Provisioned by Encore locally |
| Redis | 8 | Provisioned by Encore for `cache` (Step 7) |
| github.com/stretchr/testify | v1.12.1 | Assertions |
| github.com/google/uuid | v1.6.0 | Hold / booking / idempotency identifiers |
| github.com/jackc/pgx/v5 | v5.11.0 | Transitive via Encore; direct only if `sqldb.Driver` is needed |

**Dropped because Encore supersedes them:** `chi` (Encore generates routing), `golang-migrate` (Encore runs
migrations), `testcontainers-go` (`encore test` provisions test databases), `go-redis` (Encore `cache`),
`caarlos0/env` (Encore config + secrets).

### Encore constraints confirmed from documentation

- Migrations live in `<service>/migrations/`, named `N_name.up.sql`; Encore applies `up` automatically.
- `db.Begin(ctx)` / `tx.Exec` / `tx.Commit` / `tx.Rollback` exist — the `ORDER BY ... FOR UPDATE` pattern works.
- `et.NewTestDatabase` gives a fully-migrated, per-test isolated database cloned from a template.
- **Cron jobs do not run locally or in preview environments** → the reaper must be an invokable endpoint (D10).
- Cron endpoints must be `func(ctx) error` or `func(ctx) (*T, error)` and idempotent.
- A service needing dependency injection uses a service struct with `//encore:service` + `initService`.
- **Query strings do not support pointer types** (`*time.Time` is rejected outright). Optional filters must
  therefore arrive as strings and be parsed in application code — which is why `search.Params` is all
  strings and `parse()` owns the validation.
- The per-service database role has CRUD privileges but **not table ownership**, so `TRUNCATE` is denied.
  Test cleanup uses ordered `DELETE` instead (see `store.TruncateAll`).

---

## 3. Build plan

### Step 0 — Scaffold and dev environment — ✅ done

**What to build**
- Encore app (`encore.app`), `go.mod`, service directory layout.
- `Makefile`: `run`, `test`, `e2e`, `fmt`, `check`.
- `README.md`: what the project is, how to run it, how to test it.
- `.gitignore`; git repository with the `EventTicketingPlatform` remote.

**Definition of done**
- `encore run` boots with no errors; dev dashboard reachable.
- `encore test ./...` exits 0.
- `GET /health` returns 200 (test H1 green).

---

### Step 1 — E2E harness + testsupport service — ✅ done

**What to build**
- `e2e/client.go`, `e2e/fixtures.go`, `e2e/main_test.go` (base URL, health wait, reset between tests).
- `testsupport` service: `reset`, `seed`, `clock/set`, `clock/advance`, `clock/now`.
  **Hard-guarded** to refuse any environment other local/test (S1).
- `internal/clock`: `Clock` interface, real implementation, and a controllable test implementation (D11).
- Bearer-token auth handler so `auth` endpoints are callable from tests.

**Definition of done**
- H2 green: reset, seed, and clock control all observably work.
- S1 green: the environment guard rejects non-local/test.
- `make e2e` boots the app, waits for health, runs the suite, tears down.

---

### Step 2 — Catalog vertical slice — ✅ done

**What to build**
- Migrations: `venues`, `seats`, `events`, `price_tiers`, `tickets`, `holds` (tables created now so the Step 4–5
  booking path needs no migration rewrite), plus indexes from the design.
- `organizer` service: `POST /v1/venues`, `POST /v1/venues/:venueID/seats`, `POST /v1/events`,
  `POST /v1/events/:eventID/publish` (materializes one ticket per seat).
- `catalog` service: `GET /v1/events/:id`, `GET /v1/events/:id/availability`, `GET /v1/events/:id/seats`.

**Definition of done**
- H3, H4 green; S2–S5 green.
- Publishing a 200-seat venue materializes exactly 200 `AVAILABLE` tickets.
- Seat-map materialization unit test green.
- All Step 0–1 tests still green.

---

### Step 3 — Search — ✅ done

**What to build**
- Migration: `search_vector` generated column + GIN index; geo columns + index; `(starts_at, event_id)` index.
- `search` service: `GET /v1/search` with free text, date range, location radius, category, keyset pagination.
- `SearchIndex` interface with a Postgres implementation (D6 seam).
- Opaque, strictly-validated cursor encoding (deliberately unsigned — see D17).

**Definition of done**
- H5, H6, H7 green; S6, S7, S8 green.
- Cursor round-trip and query-builder unit tests green.
- `DRAFT` events never appear in results.
- All Step 0–2 tests still green.

---

### Step 4 — Holds (the contention core) — ✅ done

**What to build:** `POST /v1/events/:id/holds` with the ordered-`FOR UPDATE` all-or-nothing claim, hold
token, per-user seat limit, `Idempotency-Key` handling; `GET`/`DELETE /v1/holds/:id`.

**Definition of done:** happy path plus `SEAT_UNAVAILABLE` (naming lost seats), `HOLD_LIMIT_EXCEEDED`,
`EVENT_NOT_ON_SALE`, and idempotency replay all green. Concurrency unit tests green: N goroutines racing for
one seat yield exactly one winner; overlapping multi-seat claims never deadlock; partial availability rolls
back completely.

---

### Step 5 — Purchase and the payment saga — ✅ done

**What to build:** `payments` with an injectable mock provider (scriptable to succeed/fail/hang), idempotent
effect record, token-fenced `HELD → SOLD` conversion in the same transaction as the booking insert and
outbox row, and the D9 TTL pre-check.

**Definition of done:** hold → purchase → `CONFIRMED` with tickets `SOLD`. Declined payment releases seats and
marks `FAILED`. Wrong/missing hold token rejected. Replayed `Idempotency-Key` charges exactly once. Unit test:
charge succeeding while the hold expires lands in `COMPENSATING`.

---

### Step 6 — Hold expiry reaper — ✅ done

**What to build:** `POST /internal/holds/reap` with the `SKIP LOCKED` fenced query; `cron.NewJob` wired for
cloud only (D10).

**Definition of done:** hold seats → advance clock past TTL → reap → seats `AVAILABLE` and buyable by a
second user. Unit test: reaping a SOLD ticket whose old hold expired leaves it SOLD.

---

### Step 7 — Read-path hardening — ☐ *not in current scope*

**What to build:** `event_view` read model, in-transaction `events.version` bump, Encore versioned cache
keyspace, 5s-TTL availability summary, ETag + `Cache-Control`, single-flight coalescing, cache-outage
concurrency limiter.

**Definition of done:** repeat fetch is a cache hit (asserted via metric); an event update bumps the version
and the next read reflects it with no explicit purge; `If-None-Match` returns 304.

---

### Step 8 — Load proof and observability — ☐ *not in current scope*

**What to build:** a concurrency harness driving a simulated onsale (N workers, one event, all seats), plus
counters for claim conflicts, hold conversion, reaper volume, oversells, and cache hit rate.

**Definition of done:** the harness sells every seat of a 5,000-seat event under heavy concurrency with
**tickets sold == capacity, zero oversells, and zero seats stranded in `HELD`** after a final reap.
This is the run that validates the system's central invariant.
