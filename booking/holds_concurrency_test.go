package booking

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"encore.dev/beta/errs"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"encore.app/internal/clock"
	"encore.app/internal/lockkeys"
	"encore.app/store"
)

// These tests are the reason the API handlers delegate to unexported methods taking a
// user id: Encore offers no way to inject auth data into a unit test, and the
// concurrency behaviour of the claim path is the single most important thing to prove.
//
// They run against a real Postgres provisioned by `encore test` and a real Redis
// (miniredis, which implements EVAL), because the whole point is the behaviour of the
// Lua acquire and the conditional write. A mock would test nothing that matters here.

// testEnv bundles the three things a booking test needs to control: the database, the
// lease store, and time.
type testEnv struct {
	svc   *Service
	mr    *miniredis.Miniredis
	clock *clock.Controllable
}

// newTestEnv gives each test its own lease store and a frozen clock.
func newTestEnv(t *testing.T) *testEnv {
	t.Helper()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	c := frozenClock()
	return &testEnv{
		svc:   &Service{clock: c, locks: NewRedisLocker(rdb)},
		mr:    mr,
		clock: c,
	}
}

// advance moves both clocks forward together.
//
// Two clocks exist because two things measure time. Redis expires a lock against its
// own clock, which miniredis.FastForward drives; the sorted-set score that answers
// "which seats are held" is written from the injected application clock. In production
// both are wall time and agree by construction. A test that moved only one would
// produce a state that cannot occur in production — a lock key gone while its index
// entry still looks live, or the reverse — so they are always advanced as a pair.
func (e *testEnv) advance(d time.Duration) {
	e.mr.FastForward(d)
	e.clock.Advance(d)
}

// freshDB empties the database before a test.
//
// Encore reuses one test database across a package, and the per-user hold quota counts
// a user's holds across all events, so a leftover from a previous test would change
// this one's outcome.
func freshDB(ctx context.Context, t *testing.T) {
	t.Helper()
	require.NoError(t, store.TruncateAll(ctx))
}

// frozenClock returns a clock stopped at a fixed instant.
//
// An untouched Controllable still tracks real time, so asserting on an exact expiry
// against one drifts by however long the test took. Freezing removes that.
func frozenClock() *clock.Controllable {
	c := clock.NewControllable()
	c.Set(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))
	return c
}

// seedEvent creates a venue, an on-sale event and materialised tickets, returning the
// event id and its ticket ids.
//
// It writes SQL directly rather than calling the organizer service, so these tests
// depend only on the schema.
//
// The event window is derived from svc's clock, not SQL now(). A frozen test clock
// sits months away from real time, so seeding with now() produced an event that was
// "not yet on sale" as far as the service was concerned. Taking the service makes
// that mismatch impossible to reintroduce.
func seedEvent(ctx context.Context, t *testing.T, svc *Service, seatCount int) (int64, []int64) {
	t.Helper()
	now := svc.clock.Now()

	// A unique venue name per test keeps concurrent tests from colliding, since
	// Encore reuses one test database by default.
	suffix := fmt.Sprintf("%s-%d", t.Name(), time.Now().UnixNano())

	var venueID int64
	require.NoError(t, db.QueryRow(ctx, `
		INSERT INTO venues (name, city, country, created_at)
		VALUES ($1, 'London', 'GB', $2)
		RETURNING venue_id
	`, "Test Venue "+suffix, now).Scan(&venueID))

	sections := make([]string, seatCount)
	rowLabels := make([]string, seatCount)
	numbers := make([]int32, seatCount)
	for i := range seatCount {
		sections[i] = "FLOOR"
		rowLabels[i] = "A"
		numbers[i] = int32(i + 1)
	}
	_, err := db.Exec(ctx, `
		INSERT INTO seats (venue_id, section, row_label, seat_number)
		SELECT $1, s.section, s.row_label, s.seat_number
		  FROM unnest($2::text[], $3::text[], $4::int[]) AS s(section, row_label, seat_number)
	`, venueID, sections, rowLabels, numbers)
	require.NoError(t, err)

	var eventID int64
	require.NoError(t, db.QueryRow(ctx, `
		INSERT INTO events (venue_id, organizer_id, title, category, status,
		                    starts_at, ends_at, onsale_at, created_at, updated_at)
		VALUES ($1, 'org-test', $2, 'MUSIC', 'ON_SALE', $3, $4, $5, $6, $6)
		RETURNING event_id
	`, venueID, "Test Event "+suffix,
		now.Add(30*24*time.Hour), now.Add(30*24*time.Hour+3*time.Hour),
		now.Add(-time.Hour), now).Scan(&eventID))

	var tierID int64
	require.NoError(t, db.QueryRow(ctx, `
		INSERT INTO price_tiers (event_id, section, price_cents)
		VALUES ($1, 'FLOOR', 5000)
		RETURNING price_tier_id
	`, eventID).Scan(&tierID))

	rows, err := db.Query(ctx, `
		WITH inserted AS (
			INSERT INTO tickets (event_id, seat_id, price_tier_id, status)
			SELECT $1, s.seat_id, $2, 'AVAILABLE'
			  FROM seats s WHERE s.venue_id = $3
			RETURNING ticket_id
		)
		SELECT ticket_id FROM inserted ORDER BY ticket_id
	`, eventID, tierID, venueID)
	require.NoError(t, err)
	defer rows.Close()

	var ticketIDs []int64
	for rows.Next() {
		var id int64
		require.NoError(t, rows.Scan(&id))
		ticketIDs = append(ticketIDs, id)
	}
	require.NoError(t, rows.Err())
	require.Len(t, ticketIDs, seatCount)

	return eventID, ticketIDs
}

// ticketStatus reads the durable state. Postgres knows only AVAILABLE or BOOKED now —
// a held seat is still AVAILABLE here, and the lease lives in lockOwner.
func ticketStatus(ctx context.Context, t *testing.T, ticketID int64) string {
	t.Helper()
	var status string
	require.NoError(t, db.QueryRow(ctx, `
		SELECT status::text FROM tickets WHERE ticket_id = $1
	`, ticketID).Scan(&status))
	return status
}

// lockOwner reports which user holds a seat, if anyone does.
func (e *testEnv) lockOwner(t *testing.T, eventID, ticketID int64) (string, bool) {
	t.Helper()
	val, err := e.mr.Get(lockkeys.Lock(eventID, ticketID))
	if err != nil {
		return "", false
	}
	return val, true
}

// The central invariant: a seat has at most one owner. This is unreachable via an
// E2E test, which cannot drive genuinely simultaneous claims against one row.
func TestConcurrentClaimsOnOneSeatYieldExactlyOneWinner(t *testing.T) {
	ctx := context.Background()
	freshDB(ctx, t)
	env := newTestEnv(t)
	eventID, tickets := seedEvent(ctx, t, env.svc, 1)
	seat := tickets[0]

	const racers = 32

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners []string
		aborted int
		others  []error
	)

	start := make(chan struct{})
	for i := range racers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release everyone at once to maximise real contention

			hold, err := env.svc.createHold(ctx, fmt.Sprintf("racer-%d", i), eventID,
				&CreateHoldRequest{TicketIDs: []int64{seat}})

			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				winners = append(winners, hold.HoldID)
			case errs.Code(err) == errs.Aborted:
				aborted++
			default:
				others = append(others, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	assert.Empty(t, others, "every loser should be a clean seat conflict, not an unexpected error")
	require.Len(t, winners, 1, "exactly one claim may succeed")
	assert.Equal(t, racers-1, aborted, "all other racers must lose cleanly")

	// Exactly one lock exists, and the seat is still AVAILABLE in Postgres: a hold is
	// not a sale, and nothing durable has happened yet.
	_, locked := env.lockOwner(t, eventID, seat)
	assert.True(t, locked, "the winning claim must leave a lock behind")
	assert.Equal(t, "AVAILABLE", ticketStatus(ctx, t, seat))
}

// Overlapping multi-seat claims submitted in opposing orders used to be the classic
// deadlock shape, prevented by ORDER BY in the locking read.
//
// Under the Lua acquire there is no deadlock to avoid — a script runs to completion
// without interleaving, so there are no two lock-holders to cycle. What still has to
// hold is the useful half of the old guarantee: contention resolves to exactly one
// winner and clean conflicts for everyone else, rather than a livelock in which
// several claimants each grab part of the set and all back out.
func TestOverlappingMultiSeatClaimsResolveToOneWinner(t *testing.T) {
	ctx := context.Background()
	freshDB(ctx, t)
	env := newTestEnv(t)
	eventID, tickets := seedEvent(ctx, t, env.svc, 6)

	// Two overlapping sets, deliberately requested in opposite orders.
	ascending := []int64{tickets[0], tickets[1], tickets[2], tickets[3]}
	descending := []int64{tickets[3], tickets[2], tickets[1], tickets[0]}

	const rounds = 24
	var (
		wg         sync.WaitGroup
		mu         sync.Mutex
		succeeded  int
		conflicts  int
		unexpected []error
	)

	start := make(chan struct{})
	for i := range rounds {
		set := ascending
		if i%2 == 1 {
			set = descending
		}
		wg.Add(1)
		go func(i int, set []int64) {
			defer wg.Done()
			<-start

			_, err := env.svc.createHold(ctx, fmt.Sprintf("user-%d", i), eventID,
				&CreateHoldRequest{TicketIDs: set})

			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				succeeded++
			case errs.Code(err) == errs.Aborted, errs.Code(err) == errs.ResourceExhausted:
				conflicts++
			default:
				unexpected = append(unexpected, err)
			}
		}(i, set)
	}
	close(start)
	wg.Wait()

	assert.Empty(t, unexpected,
		"contention must resolve as clean conflicts, never as an unexpected error")
	assert.Equal(t, 1, succeeded, "the four contested seats can only be claimed once")
	assert.Equal(t, rounds-1, conflicts)

	// No seat may be left locked by a claim that reported failure — that is the
	// partial-acquisition failure the all-or-nothing script exists to prevent.
	locked := 0
	for _, id := range ascending {
		if _, ok := env.lockOwner(t, eventID, id); ok {
			locked++
		}
	}
	assert.Equal(t, len(ascending), locked,
		"the single winner must hold all four seats, with no partial residue")
}

// All-or-nothing. A partial claim would strand the free seats: held by a hold the
// client was told does not exist.
func TestPartialAvailabilityRollsBackCompletely(t *testing.T) {
	ctx := context.Background()
	freshDB(ctx, t)
	env := newTestEnv(t)
	eventID, tickets := seedEvent(ctx, t, env.svc, 5)

	// Take one seat out of circulation.
	_, err := env.svc.createHold(ctx, "first-buyer", eventID,
		&CreateHoldRequest{TicketIDs: []int64{tickets[2]}})
	require.NoError(t, err)

	// Now ask for a set that includes it.
	requested := []int64{tickets[0], tickets[1], tickets[2], tickets[3]}
	_, err = env.svc.createHold(ctx, "second-buyer", eventID,
		&CreateHoldRequest{TicketIDs: requested})

	require.Error(t, err)
	assert.Equal(t, errs.Aborted, errs.Code(err))

	// The conflict names exactly the contested seat.
	details, ok := errs.Details(err).(SeatConflict)
	require.True(t, ok, "expected SeatConflict details, got %T", errs.Details(err))
	assert.Equal(t, []int64{tickets[2]}, details.LostTicketIDs)

	// Critically: the other three seats are untouched and still claimable.
	for _, id := range []int64{tickets[0], tickets[1], tickets[3]} {
		_, locked := env.lockOwner(t, eventID, id)
		assert.False(t, locked, "ticket %d must carry no lock after a failed claim", id)
	}

	// And the first buyer still owns theirs.
	owner, locked := env.lockOwner(t, eventID, tickets[2])
	require.True(t, locked)
	assert.Equal(t, "first-buyer", owner)

	// A retry avoiding the contested seat now succeeds.
	_, err = env.svc.createHold(ctx, "second-buyer", eventID,
		&CreateHoldRequest{TicketIDs: []int64{tickets[0], tickets[1], tickets[3]}})
	assert.NoError(t, err)
}

// A whole event selling out under concurrency must claim every seat exactly once:
// no double claim, and no seat stranded unclaimed.
func TestConcurrentSelloutClaimsEverySeatExactlyOnce(t *testing.T) {
	ctx := context.Background()
	freshDB(ctx, t)
	env := newTestEnv(t)

	const seats = 40
	eventID, tickets := seedEvent(ctx, t, env.svc, seats)

	var (
		wg sync.WaitGroup
		mu sync.Mutex
		// Each seat maps to the set of holds that claimed it. Any entry with more
		// than one hold is a double claim.
		claimedBy = map[int64][]string{}
	)

	start := make(chan struct{})
	// More racers than seats, each targeting one seat, several racers per seat.
	for i := range seats * 3 {
		seat := tickets[i%seats]
		wg.Add(1)
		go func(i int, seat int64) {
			defer wg.Done()
			<-start

			hold, err := env.svc.createHold(ctx, fmt.Sprintf("buyer-%d", i), eventID,
				&CreateHoldRequest{TicketIDs: []int64{seat}})
			if err != nil {
				return
			}
			mu.Lock()
			claimedBy[seat] = append(claimedBy[seat], hold.HoldID)
			mu.Unlock()
		}(i, seat)
	}
	close(start)
	wg.Wait()

	for seat, holds := range claimedBy {
		assert.Len(t, holds, 1, "seat %d was claimed by %d holds — that is a double claim", seat, len(holds))
	}
	assert.Len(t, claimedBy, seats, "every seat should have been claimed exactly once")

	// Redis is the final word on the lease: every seat carries exactly one lock.
	for _, id := range tickets {
		_, locked := env.lockOwner(t, eventID, id)
		assert.True(t, locked, "seat %d should be locked after a full sellout of holds", id)
	}
}

// An expired lease must be claimable immediately, and only one racer may take it over.
func TestConcurrentTakeoverOfExpiredHold(t *testing.T) {
	ctx := context.Background()
	freshDB(ctx, t)
	env := newTestEnv(t)

	eventID, tickets := seedEvent(ctx, t, env.svc, 1)
	seat := tickets[0]

	_, err := env.svc.createHold(ctx, "abandoner", eventID, &CreateHoldRequest{TicketIDs: []int64{seat}})
	require.NoError(t, err)

	// Walk past the TTL. There is no reaper to run: the lock key expires on its own,
	// which is the whole point of moving the lease to Redis.
	env.advance(HoldTTL() + time.Minute)

	_, stillLocked := env.lockOwner(t, eventID, seat)
	require.False(t, stillLocked, "the lock must expire on its own, with nothing sweeping it")

	const racers = 16
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners []string
	)
	start := make(chan struct{})
	for i := range racers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			hold, err := env.svc.createHold(ctx, fmt.Sprintf("taker-%d", i), eventID,
				&CreateHoldRequest{TicketIDs: []int64{seat}})
			if err == nil {
				mu.Lock()
				winners = append(winners, hold.HoldID)
				mu.Unlock()
			}
		}(i)
	}
	close(start)
	wg.Wait()

	require.Len(t, winners, 1, "an expired seat may be taken over by exactly one claimant")

	_, locked := env.lockOwner(t, eventID, seat)
	assert.True(t, locked, "the taking-over claim must leave a lock behind")
	assert.Equal(t, "AVAILABLE", ticketStatus(ctx, t, seat))
}

// Quota is restored when a lease expires, so an abandoned checkout does not
// permanently consume one of a user's three slots.
//
// This preserves the intent of the deleted TestReaperRestoresHoldQuota: the quota is
// now a ZCOUNT over live sorted-set members rather than a count of ACTIVE hold rows,
// and nothing sweeps it, so it has to fall away on its own.
func TestExpiredHoldRestoresQuota(t *testing.T) {
	ctx := context.Background()
	freshDB(ctx, t)
	env := newTestEnv(t)

	eventID, tickets := seedEvent(ctx, t, env.svc, 8)

	for i := range MaxActiveHoldsPerUser {
		_, err := env.svc.createHold(ctx, "serial-abandoner", eventID,
			&CreateHoldRequest{TicketIDs: []int64{tickets[i]}})
		require.NoError(t, err)
	}

	_, err := env.svc.createHold(ctx, "serial-abandoner", eventID,
		&CreateHoldRequest{TicketIDs: []int64{tickets[5]}})
	require.Error(t, err)
	assert.Equal(t, errs.ResourceExhausted, errs.Code(err), "the fourth hold must be refused")

	env.advance(HoldTTL() + time.Minute)

	active, err := env.svc.locks.ActiveHolds(ctx, "serial-abandoner", env.clock.Now())
	require.NoError(t, err)
	assert.EqualValues(t, 0, active, "expired leases must not count against the quota")

	_, err = env.svc.createHold(ctx, "serial-abandoner", eventID,
		&CreateHoldRequest{TicketIDs: []int64{tickets[5]}})
	assert.NoError(t, err, "quota should be free once the leases have lapsed")
}
