# Architecture Decision Record

Significant architectural and technology decisions, with rationale and tradeoffs.
Last updated: 2026-09-08

---

## D1 — Reserved seating as the inventory model

**Context / requirement:** Users must book tickets with zero possibility of two users owning the same seat.
The inventory model determines the entity model and the contention strategy.

**Chosen approach:** Every ticket is an individually addressable seat instance — one `tickets` row per
(event, seat). The invariant is *a ticket has at most one owner*, enforced by a conditional write on that row.

**Why it fits:** The smallest authoritative item is the seat row itself, so the database can enforce the
invariant atomically. General admission is later expressible as a degenerate case (a pool of undifferentiated
tickets in one tier), so nothing is foreclosed.

**Alternatives considered:** General-admission-only (quantity against a pool) — simpler, but no seat
selection and a hotter single counter row. Both models from day one — more correct for a real product but
doubles inventory logic and test surface in the first build.

**Tradeoffs and consequences:** ~1.8B ticket rows at target scale, requiring hash partitioning by `event_id`.
Publishing a large venue materializes many rows and must be done asynchronously for the largest venues.

**Status:** accepted

---

## D2 — Hold-then-pay with an expiring lease

**Context / requirement:** Payment is an external call taking seconds, and it cannot happen inside a
database transaction holding row locks. Exclusivity must span user think-time plus the payment call.

**Chosen approach:** Claim seats into `HELD` with a `hold_id`, an opaque `hold_token`, and a ~10 minute
expiry. Payment runs entirely outside any lock. A final token-fenced conditional write converts `HELD → SOLD`.

**Why it fits:** This is decision-ladder level 2 (conditional write) plus level 6 (lease), which is the
correct combination when exclusivity must outlive one transaction. It keeps lock duration at microseconds
while still guaranteeing the invariant.

**Alternatives considered:** Instant single-step purchase — far simpler, no expiry reaper, but cannot
accommodate real payment latency and gives poor multi-ticket checkout UX.

**Tradeoffs and consequences:** Requires an expiry reaper, token fencing, and idempotency handling.
Introduces the paid-but-unseated failure mode addressed in D9.

**Status:** accepted

---

## D3 — No `available_count` column on events

**Context / requirement:** Availability must be displayable without serializing ticket claims.

**Chosen approach:** No aggregate counter on `events`. Availability counts are derived from `tickets` and
served eventually consistent through a short-TTL cache.

**Why it fits:** An `available_count` column would be a single hot row that every concurrent claim for that
event must update, serializing ~1k claims/s into one row lock and destroying claim latency. The contention
playbook names this exact mistake: *protecting an aggregate counter but not the specific resource being
claimed.* Concurrent claims on distinct seat rows do not conflict at all.

**Alternatives considered:** Maintaining the counter in the same transaction as the claim — rejected for the
serialization above. Maintaining it asynchronously — this is effectively what the derived cache does, without
a misleading column on the authoritative table.

**Tradeoffs and consequences:** Availability numbers are approximate and may be briefly stale. Accepted
explicitly: the seat map is advisory, and only the conditional write is authoritative.

**Status:** accepted

---

## D4 — Availability reads are deliberately eventually consistent

**Context / requirement:** Catalog reads run at ~8k RPS average and ~80k peak. Strongly consistent
availability reads would put all of that on the leader.

**Chosen approach:** Event detail, seat maps, and availability summaries are served from cache and read
replicas with bounded staleness (≤5s). They are documented as **advisory**. Only the claim is authoritative.

**Why it fits:** This is what makes the read path cacheable at all, and it is honest about reality — any
availability number is stale the moment it is rendered on a user's screen. Correctness lives entirely in the
conditional write, which is where it belongs.

**Alternatives considered:** Strongly consistent availability reads — would require leader reads at 80k RPS
and would still be stale by the time the user acts.

**Tradeoffs and consequences:** A user can select a seat that is already gone and receive a
`SEAT_UNAVAILABLE` conflict. This is expected and must be a good UX path, not an error state.

**Status:** accepted

---

## D5 — Encore.dev with Go

**Context / requirement:** Backend framework for the service layer.

**Preferred default:** Go (stack preference #1). **Chosen:** Encore.dev with Go (stack preference #2).

**Chosen approach:** Encore application with separate services per bounded context, using Encore's `sqldb`,
`cache`, `cron`, and `auth` primitives.

**Why it fits:** User's explicit choice. Encore provisions Postgres and Redis locally with no Docker Compose
to maintain, runs migrations automatically, provisions isolated test databases via `et.NewTestDatabase`,
supplies built-in tracing, and gives a much shorter path to a real cloud deployment later.

**Alternatives considered:** Plain Go with chi + pgx — was recommended, because the interesting parts of this
system are raw SQL contention semantics and HTTP-level E2E tests, and plain Go keeps both fully under our
control with a framework-agnostic test suite. The user chose Encore; both are in the preferred stack.

**Tradeoffs and consequences:** Encore owns deployment and the test harness. Verified this does not block the
design: `db.Begin` / `tx.Exec` / `tx.Commit` exist, so the `ORDER BY ... FOR UPDATE` pattern works, and
`sqldb.Driver[*pgxpool.Pool]` provides raw pgx access if ever needed. Removes five dependencies (`chi`,
`golang-migrate`, `testcontainers-go`, `go-redis`, env parsing).

**Status:** accepted

---

## D6 — Postgres full-text search now, `SearchIndex` seam for OpenSearch

**Context / requirement:** Free-text plus date, location, and category filters over ~9M active events at up
to 80k RPS.

**Preferred default:** PostgreSQL. **Chosen:** PostgreSQL FTS (`tsvector` + GIN + `pg_trgm`), behind a
`SearchIndex` interface.

**Why it fits:** Stays on the preferred stack, and crucially the `search_vector` is a **generated column**, so
the search index updates in the same transaction as the write — **no indexing pipeline, no lag, nothing to
rebuild**. Genuinely sufficient for a single-region launch.

**Alternatives considered:** OpenSearch from day one — purpose-built for faceted/geo/relevance-ranked search
at this scale and takes search load off the primary entirely, but requires a second datastore, an indexing
pipeline, and reindex tooling before the first search test can pass.

**Tradeoffs and consequences:** Postgres FTS will **not** hold at 80k RPS over 9M events. Relevance ranking
is weaker than a dedicated engine.
**Named trigger to revisit:** search p95 > 500 ms, or > ~2M active events. The `SearchIndex` interface makes
the swap a contained change.

**Status:** accepted

---

## D7 — Saga over Temporal for the payment flow

**Context / requirement:** Purchase spans a charge (external, irreversible) and a seat conversion (local),
with refund as compensation.

**Preferred default:** Temporal (stack preference for durable workflows).
**Chosen:** Transactional outbox + idempotent worker + reconciliation job.

**Why it fits:** The flow is two steps over a ~10-minute horizon with one external call. Temporal's real
value — durable timers, multi-day waits, complex branching, versioned long-lived histories — is entirely
unused here, and it would be significant operational surface for one hop. The stack document itself says not
to introduce Temporal for simple flows.

**Alternatives considered:** Temporal from the start — better observability of stuck purchases and
retries/compensation for free, at the cost of running the engine and versioning workflow code.

**Tradeoffs and consequences:** We hand-roll idempotency and reconciliation that Temporal would provide.
Acceptable at two steps.
**Named trigger to revisit:** when refunds, cancellations, transfers, or organizer payouts land — those are
genuinely long-lived multi-party flows and are the right moment to adopt Temporal.

**Status:** accepted

---

## D8 — Modular multi-service Encore app, not a monolith

**Context / requirement:** The read path is ~1000× the write path and has opposite consistency requirements.
An onsale spike must not degrade browsing for unrelated events.

**Chosen approach:** Separate Encore services — `catalog`, `search`, `booking`, `payments`, `organizer`,
`testsupport` — with `booking` as the sole inventory writer.

**Why it fits:** This is functional partitioning along the natural failure boundary. `catalog` may serve
stale data and stay up; `booking` fails closed. Keeping them separate prevents a heavy search query from
starving the booking connection pool.

**Alternatives considered:** Single service — simpler, but couples read and write scaling and makes the
"never cache inventory" rule a convention rather than a boundary.

**Tradeoffs and consequences:** Cross-service calls for composed responses. Encore makes these typed and
traced, so the cost is low.

**Status:** accepted

---

## D9 — Charge only when the hold's remaining TTL exceeds the payment budget

**Context / requirement:** If a charge succeeds but the hold expires before conversion, the user is charged
for a seat they do not receive. This is the system's worst failure mode.

**Chosen approach:** Before charging, verify `remaining_ttl > payment_timeout_budget`. If not, extend the
hold first (conditional on the hold token). If the charge still succeeds after conversion fails, the booking
enters `COMPENSATING`, a refund is issued, and the case is alerted.

**Why it fits:** Converts a likely race into a rare one preemptively, rather than relying solely on
compensation. Compensation is treated as fallible, not as an infallible rollback.

**Alternatives considered:** Compensation only — simpler but leaves a routinely-hit refund path.
Holding the seat indefinitely during payment — unbounded inventory lockup under abandoned checkouts.

**Tradeoffs and consequences:** Adds a pre-check and a hold-extension path. A small window remains, which is
why `COMPENSATING` and the reconcile job both exist.

**Status:** accepted

---

## D10 — Hold reaper as an invokable private endpoint, not an in-process timer

**Context / requirement:** Expired holds must return seats to inventory, and this behavior must be testable
deterministically.

**Chosen approach:** A `private` API endpoint performing a `SKIP LOCKED` fenced batch release, invoked by
Encore Cron in cloud environments and **called directly by tests** locally.

**Why it fits:** Verified from Encore's documentation that **cron jobs do not run locally or in preview
environments**, so an endpoint is required regardless. This is also strictly better for testing: combined
with the injected `Clock` (D11), hold expiry is testable with no `sleep` and no flakiness. Encore requires
cron endpoints to be idempotent, which the fenced query already is.

**Alternatives considered:** An in-process background goroutine — would run locally but is untestable
deterministically, and does not coordinate across instances.

**Tradeoffs and consequences:** Expiry is batch, not instant, so a seat may remain `HELD` slightly past its
expiry. Harmless: the claim path treats an expired hold as claimable anyway.

**Status:** accepted

---

## D11 — Injected `Clock`, no direct `time.Now()` in business logic

**Context / requirement:** Hold expiry, TTLs, and reaping are all time-dependent. Tests must control time.

**Chosen approach:** A `Clock` interface injected through Encore service structs. Business logic never calls
`time.Now()` directly. `testsupport` exposes a clock-advance endpoint in local/test environments.

**Why it fits:** Makes every expiry test deterministic and instant instead of sleep-based and flaky. Removes
the largest source of non-determinism in this system.

**Alternatives considered:** Real time plus short test TTLs — slow and flaky under CI load.

**Tradeoffs and consequences:** Every time-dependent component must accept a clock. Note that SQL predicates
like `hold_expires_at < now()` must take the clock's time as a **parameter** rather than using the database's
`now()`, or the test clock is bypassed.

**Status:** accepted

---

## D12 — Virtual waiting room designed, deferred

**Context / requirement:** A hot onsale concentrates ~1k claims/s and ~500k concurrent users onto one event.

**Chosen approach:** Document the Redis-backed admission-token design and leave a seam — booking endpoints
validate an admission claim that is a no-op in v1. Do not build it yet.

**Why it fits:** The waiting room depends on a correct inventory path existing first, and correctness is the
harder problem. Analysis also shows the claim path itself is not the bottleneck (concurrent claims hit
different rows); the pressure is connection exhaustion and retry storms, partly addressed by PgBouncer and
per-user rate limits.

**Alternatives considered:** Building it in the first pass — large work before the core booking path is
proven. Omitting it entirely — knowingly leaves the worst failure mode unaddressed.

**Tradeoffs and consequences:** The first real onsale spike is unprotected beyond rate limits.
**Named trigger:** first onsale where claim p99 breaches 300 ms.

**Status:** accepted

---

## D14 — Development-grade auth handler, hard-failed outside local/test

**Context / requirement:** `auth` endpoints need a caller identity, but building real identity management
is out of scope and would not exercise anything the design cares about.

**Chosen approach:** An Encore auth handler that treats the bearer token as the user id after validating it
against `^[A-Za-z0-9_-]{3,64}$`. It **returns `Unimplemented` in any environment that is not a unit test or a
locally-running app**, so it cannot be deployed accidentally.

**Why it fits:** It establishes the property the rest of the system depends on — identity always arrives via
the verified auth context and is never read from a request body — without building an identity provider. The
environment guard is unit-tested across the full matrix and **fails closed** on unknown environment types.

**Alternatives considered:** Real JWT verification — more work for no design insight at this stage.
No auth at all — would have let a user id leak into request bodies and normalised an unsafe pattern.

**Tradeoffs and consequences:** Not usable in any real deployment; must be replaced before one. The format
validation also keeps odd characters out of logs and cache keys.

**Note on environment detection:** `encore.EnvLocal` is deprecated and no longer returned by the runtime. A
locally-running app reports `EnvDevelopment` + `CloudLocal`. Guarding on `EnvLocal` would have rejected local
development entirely — this is unit-tested in `internal/clock/clock_test.go` and `identity/auth_test.go`.

**Status:** accepted

---

## D15 — Single shared database, not one per service

**Context / requirement:** Publishing an event must atomically mark it `ON_SALE` and materialise one ticket
per seat. Encore encourages a database per service.

**Chosen approach:** One `ticketing` database owned by a `store` package, accessed by other services via
`sqldb.Named("ticketing")`. Table ownership by service is enforced by convention and code review.

**Why it fits:** Events and tickets must be transactable together, which a database-per-service split makes
impossible without a distributed transaction — a large cost for no benefit at this scale. It also matches the
design's actual deployment target: a single Postgres leader with read replicas.

**Alternatives considered:** A database per service — more idiomatic for Encore, but would force publish to
become a saga purely as an artifact of the storage split.

**Tradeoffs and consequences:** Service isolation at the storage layer is conventional rather than enforced.
Accepted, and revisited if a service ever needs independent scaling of its store.

**Status:** accepted

---

## D16 — Search results ordered by start time, not relevance

**Context / requirement:** Search needs stable keyset pagination (H7: every result exactly once, no
duplicates, no gaps) and ideally relevance ranking. Decided during implementation.

**Chosen approach:** Order by `(starts_at, event_id)` ascending — soonest first. The `tsvector` already
carries `setweight` title/description weights, but they are not used for ordering yet.

**Why it fits:** Relevance ranking and stable keyset pagination pull in opposite directions. A relevance
score is neither stable across concurrent writes nor usable as an indexed sort key, so paging by it either
repeats or skips rows — exactly the failure H7 exists to catch. Start time is stable, indexed
(`events_onsale_keyset_idx`), and genuinely useful for a ticketing product, where "what's on soon" is the
common intent.

**Alternatives considered:** `ts_rank` ordering with offset pagination — would give better text relevance
but reintroduces the deep-offset scan that keyset pagination exists to avoid. Ranking with a
rank-and-id cursor — the rank is not stable across writes, so the cursor silently drifts.

**Tradeoffs and consequences:** A search for a broad term returns the soonest matching events rather than
the best matching ones. Accepted for now; it is the right moment to fix this when OpenSearch lands (D6),
since the engine handles ranked pagination properly. The stored weights mean no backfill will be needed.

**Status:** accepted

---

## D17 — Pagination cursors are opaque but unsigned

**Context / requirement:** The plan called for a "tamper-evident" cursor. Reconsidered during implementation.

**Chosen approach:** Cursors are base64url-encoded JSON of `(starts_at, event_id)`, strictly validated on
decode, with no HMAC. A malformed cursor is a 400.

**Why it fits:** Signing protects against tampering that gains something. Here a forged cursor can only
change which page of *public* search results the caller sees — no authorization decision depends on its
contents and it exposes no private data. An HMAC would add key management and rotation for no security
benefit. What actually matters is that a bad cursor is rejected loudly rather than silently restarting from
page one, which would repeat results; that is enforced and unit-tested.

**Alternatives considered:** HMAC-signed cursors — appropriate the moment a cursor ever encodes a
user-scoped or permission-scoped filter. **Revisit trigger:** the first cursor over non-public data, e.g.
`GET /v1/bookings` in Step 5.

**Tradeoffs and consequences:** A caller can hand-craft a cursor and jump to an arbitrary position in public
search results. Harmless, and equivalent to what they could achieve with date filters anyway.

**Status:** accepted

---

## D18 — A provider transport error must not release the seats

**Context / requirement:** A charge can fail two very different ways: a **decline** (the provider says no —
a definite outcome) or a **transport error** (no answer — the money may or may not have moved). Discovered
while building the purchase saga.

**Chosen approach:** A decline releases the seats immediately and records a `FAILED` booking. A transport
error releases **nothing**: the payment row stays `IN_PROGRESS`, the hold stays intact, and the caller gets
`Unavailable`.

**Why it fits:** Releasing seats while a charge may still land is how you sell one seat twice and refund
neither. Leaving the hold alone costs at most one abandoned lease, which the reaper cleans up; guessing
wrong costs real money and a double sale. `IN_PROGRESS` is therefore treated as genuinely ambiguous and
never as "not yet done".

**Alternatives considered:** Treating any charge failure as a decline — simpler and wrong. Retrying the
charge on transport error — cannot be done safely without first reconciling, since the original may have
succeeded.

**Tradeoffs and consequences:** Seats stay locked for up to the remaining TTL after an ambiguous failure,
and resolving the payment needs the reconcile job. Accepted: it fails in the direction that does not lose
money.

**Status:** accepted

---

## D19 — A test seam for the charged-but-seats-lost interleaving

**Context / requirement:** The system's worst money-losing failure — the charge succeeds, then the seats turn
out to be gone — is unreachable sequentially. Something must steal the seats *while* the payment is in
flight.

**Chosen approach:** An unexported package variable `duringPaymentHook func()` in the booking service, nil
in every real environment, invoked between the charge and the conversion. Tests set it to reassign the seats
mid-payment.

**Why it fits:** The alternative was leaving the compensation path untested, which for the one path that
loses customer money is not acceptable. The seam is unexported, nil-checked, zero-cost, and documented at
its definition.

**Alternatives considered:** Testing `convertToSold` and `compensate` in isolation — proves the pieces but
not that `purchase` actually composes them. Driving a real race with goroutines — inherently flaky, and it
would not reliably hit the window.

**Tradeoffs and consequences:** A test-only branch in production code. Bounded and explicit, and the payoff
is that `COMPENSATING` and the fallible-refund path are both genuinely exercised.

**Status:** accepted

---

## D20 — Test seeds derive time from the injected clock, never SQL `now()`

**Context / requirement:** Found by a failing test. The booking unit tests froze the clock at a fixed date,
while the test seed created events with `onsale_at = now() - interval '1 hour'` using SQL `now()`. The
service compared its frozen clock against a real-time `onsale_at` months away and refused every claim with
"tickets are not yet on sale".

**Chosen approach:** The seed helper takes the `*Service` and derives all timestamps from `svc.clock.Now()`,
so seed and service cannot disagree by construction.

**Why it fits:** This is the same class of bug D11 warns about, arriving from the test side. Passing the
service makes the mismatch impossible to reintroduce rather than relying on a reviewer noticing.

**Tradeoffs and consequences:** The seed helper is coupled to the service under test, which is precisely the
coupling that makes it correct.

**Status:** accepted

---

## D21 — Two caches: versioned event view, unversioned availability

**Context / requirement:** Event detail must be cacheable at 80k RPS, but it embeds an availability count
that changes on every sale.

**Chosen approach:** Two separate cache entries. The **event view** is keyed on `(event_id, version)` with a
30-minute TTL. **Availability** is keyed on `event_id` alone with a 5-second TTL.

**Why it fits:** `events.version` is incremented in the same transaction as any edit, so a versioned key
makes invalidation free — the old key simply becomes unreachable, with no purge and no delete-on-write race.
That only works for data which changes when the event changes. Availability changes without the event
changing, so a versioned key would never invalidate it. Combining the two into one entry would force either
a useless TTL on the event body or a permanently stale count.

**Alternatives considered:** One cache entry for the whole response — simpler, but the TTL would have to be
5s, discarding the benefit of a version-keyed body. No availability cache — correct but puts an aggregation
query on every read.

**Tradeoffs and consequences:** Two lookups per event read instead of one, plus one indexed primary-key read
for the version. Availability is up to 5s stale, which D4 already accepts.

**Status:** accepted

---

## D22 — Sales do not invalidate the availability cache

**Context / requirement:** A sale makes the cached availability count wrong. The obvious reaction is to
invalidate on write.

**Chosen approach:** Do not invalidate. Let the 5-second TTL expire, with single-flight collapsing the
rebuild.

**Why it fits:** At ~1k claims/s on a hot event, invalidate-on-write would delete and rebuild the same key
about a thousand times a second. That is a stampede, not a cache — and it degrades precisely when load is
highest, which is the opposite of what a cache is for. A TTL bounds rebuilds to once per interval regardless
of sale rate. Availability is advisory anyway (D4), so bounded staleness costs nothing real.

**Alternatives considered:** Delete-on-write — intuitive and wrong at this write rate. Write-through updates
from the booking service — couples the inventory writer to the read cache for a number that is advisory.

**Tradeoffs and consequences:** A buyer may see a count up to 5s old. They may therefore pick a seat that is
gone and get a `SEAT_UNAVAILABLE` conflict — already an expected, well-handled path.
A manual force-refresh endpoint exists for operator corrections.

**Status:** accepted

---

## D23 — Event detail is a raw endpoint, for HTTP validators

**Context / requirement:** At 80k RPS the cheapest possible response is one with no body. That needs ETag
and `If-None-Match` handling, including returning 304.

**Chosen approach:** `GET /v1/events/:eventID` is an Encore **raw** endpoint. Every other endpoint stays typed.

**Why it fits:** A typed Encore endpoint cannot return 304 — it maps a return value or an error to a status,
and 304 is neither. Revalidation is the entire point of this layer, so the one endpoint that needs it gets
raw treatment and the rest keep their typed contracts.

The ETag covers the **whole representation**, event version *and* availability numbers. Deriving it from the
version alone would be cheaper, but a 304 would then hide a sold-out section from a client revalidating a
ticketing page — the one thing they opened it to check.

**Alternatives considered:** Typed endpoint with ETag response header but no 304 — loses the bandwidth saving
that motivates the header. Version-only ETag — cheaper and wrong, as above.

**Tradeoffs and consequences:** Path parameters and error rendering are hand-written for this endpoint; the
error envelope is reproduced to match Encore's typed shape so clients see one format. Computing the ETag
requires building the body, so a 304 saves bandwidth rather than work — normal ETag semantics.

**Status:** accepted

---

## D24 — Cache failure sheds load rather than falling through freely

**Context / requirement:** If the cache tier is unavailable, every read wants to rebuild from Postgres.

**Chosen approach:** Cache misses and cache *errors* are counted separately. Rebuilds run through a
64-slot semaphore; requests that cannot get a slot are shed with `Unavailable` rather than queued.

**Why it fits:** An unrestricted fallback converts a cache outage into a database outage, which is strictly
worse — the cache is optional and Postgres is not. Shedding a fraction of reads keeps the booking path,
which shares that database, alive. Failing fast also beats queueing, which would collapse latency for
everyone.

**Alternatives considered:** Unbounded fallback — the default, and the mistake the read-scaling playbook
names explicitly. Serving stale-on-error — no stale copy exists once the cache is the thing that is down.

**Tradeoffs and consequences:** During a cache outage some catalog reads fail. Accepted: browse degrades,
booking survives.

**Status:** accepted

---

## D25 — The load proof fails if it observes no contention

**Context / requirement:** A concurrency test that happens to serialise proves nothing, but still passes.

**Chosen approach:** The load proof asserts `hold_conflicts > 0` alongside the invariant, and workers
deliberately target already-claimed seats ~35% of the time instead of only taking untouched batches.

**Why it fits:** Without the spoiler behaviour, a work queue hands each seat to exactly one worker and no
conflict can occur — the test would report success while exercising none of the contention machinery. The
observed run produces ~481 conflicts against 625 successful holds, so the conflict path is genuinely covered.

**Tradeoffs and consequences:** The conflict count varies between runs, so the assertion is "> 0" rather
than an exact figure. A sequential drain phase finishes any inventory the concurrent phase left, so the final
invariant check is deterministic rather than "almost sold out".

**Status:** accepted

---

## D13 — Build complete through Step 8

**Context / requirement:** The build ran in three passes: Steps 0–3 (catalog and search), Steps 4–6 (the
booking path), then Steps 7–8 (read path and load proof).

**Chosen approach:** All eight steps are built and green.

**Tradeoffs and consequences:** The zero-oversell invariant is proven at three levels: unit tests under
maximal single-row contention, E2E tests through the HTTP stack, and a 5,000-seat concurrent sellout load
proof that reports zero oversells and zero stranded seats with 481 genuine conflicts along the way.

**What remains deliberately unbuilt**, each with a named trigger recorded above: the virtual waiting room
(D12), OpenSearch (D6), Temporal for long-lived flows (D7), a separate `event_view` table (D26), read
replicas, a CDN, `tickets` sharding, and multi-region. The development auth handler (D14) and the
unconfigured payment provider must both be replaced before any real deployment; both fail closed.

**Status:** accepted

---

## D26 — No separate `event_view` table; the versioned cache subsumes it

**Context / requirement:** The plan for Step 7 listed a denormalized `event_view` table alongside the cache.
Reconsidered while building.

**Chosen approach:** Skip the table. The cached projection is assembled by two queries on a cache miss and
stored under the versioned key.

**Why it fits:** With a version-keyed cache the hit rate on a ~18GB catalog is very high, so a materialized
table would only reduce the cost of a *miss* — which is not the bottleneck. Against that, the table needs a
second write path kept correct on publish, on edit, and on any venue change, which is real ongoing risk for
no measured gain. The read-scaling ladder says to stop at the rung that meets the SLO.

**Alternatives considered:** Building the table as planned — more faithful to the plan, but adds an
invalidation surface the versioned cache was chosen specifically to avoid.

**Tradeoffs and consequences:** A cache miss costs two queries instead of one indexed read.
**Named trigger to revisit:** when cache-miss rebuild latency, rather than hit-path latency, appears in the
p99 for `GET /v1/events/:id`.

**Status:** accepted
