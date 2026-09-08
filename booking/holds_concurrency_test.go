package booking

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"encore.dev/beta/errs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"encore.app/internal/clock"
	"encore.app/store"
)

// These tests are the reason the API handlers delegate to unexported methods taking a
// user id: Encore offers no way to inject auth data into a unit test, and the
// concurrency behaviour of the claim path is the single most important thing to prove.
//
// They run against a real Postgres provisioned by `encore test`, because the whole
// point is the behaviour of row locks and conditional writes. A mock would test
// nothing that matters here.

func newTestService() *Service {
	return &Service{clock: clock.Real{}}
}

// freshDB empties the database before a test.
//
// Encore reuses one test database across a package, and two things here are global by
// nature: the reaper sweeps every expired hold regardless of event, and the per-user
// hold quota counts a user's holds across all events. Without a clean slate, a test
// would see leftovers from its predecessors — which is exactly how the first run of
// these tests failed.
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

func ticketStatus(ctx context.Context, t *testing.T, ticketID int64) (string, *string) {
	t.Helper()
	var (
		status string
		holdID *string
	)
	require.NoError(t, db.QueryRow(ctx, `
		SELECT status::text, hold_id::text FROM tickets WHERE ticket_id = $1
	`, ticketID).Scan(&status, &holdID))
	return status, holdID
}

// The central invariant: a seat has at most one owner. This is unreachable via an
// E2E test, which cannot drive genuinely simultaneous claims against one row.
func TestConcurrentClaimsOnOneSeatYieldExactlyOneWinner(t *testing.T) {
	ctx := context.Background()
	freshDB(ctx, t)
	svc := newTestService()
	eventID, tickets := seedEvent(ctx, t, svc, 1)
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

			hold, err := svc.createHold(ctx, fmt.Sprintf("racer-%d", i), eventID,
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

	// The database agrees with exactly one winner.
	status, holdID := ticketStatus(ctx, t, seat)
	assert.Equal(t, "HELD", status)
	require.NotNil(t, holdID)
	assert.Equal(t, winners[0], *holdID)
}

// Overlapping multi-seat claims submitted in opposing orders are the classic deadlock
// shape. The ORDER BY in the locking read is what prevents it; without that, this test
// fails with "deadlock detected" rather than clean conflicts.
func TestOverlappingMultiSeatClaimsDoNotDeadlock(t *testing.T) {
	ctx := context.Background()
	freshDB(ctx, t)
	svc := newTestService()
	eventID, tickets := seedEvent(ctx, t, svc, 6)

	// Two overlapping sets, deliberately requested in opposite orders.
	ascending := []int64{tickets[0], tickets[1], tickets[2], tickets[3]}
	descending := []int64{tickets[3], tickets[2], tickets[1], tickets[0]}

	const rounds = 24
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		succeeded int
		conflicts int
		deadlocks []error
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

			_, err := svc.createHold(ctx, fmt.Sprintf("user-%d", i), eventID,
				&CreateHoldRequest{TicketIDs: set})

			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				succeeded++
			case errs.Code(err) == errs.Aborted, errs.Code(err) == errs.ResourceExhausted:
				conflicts++
			default:
				deadlocks = append(deadlocks, err)
			}
		}(i, set)
	}
	close(start)
	wg.Wait()

	assert.Empty(t, deadlocks,
		"a deterministic lock order must turn contention into clean conflicts, never a deadlock")
	assert.Equal(t, 1, succeeded, "the four contested seats can only be claimed once")
	assert.Equal(t, rounds-1, conflicts)
}

// All-or-nothing. A partial claim would strand the free seats: held by a hold the
// client was told does not exist.
func TestPartialAvailabilityRollsBackCompletely(t *testing.T) {
	ctx := context.Background()
	freshDB(ctx, t)
	svc := newTestService()
	eventID, tickets := seedEvent(ctx, t, svc, 5)

	// Take one seat out of circulation.
	taken, err := svc.createHold(ctx, "first-buyer", eventID,
		&CreateHoldRequest{TicketIDs: []int64{tickets[2]}})
	require.NoError(t, err)

	// Now ask for a set that includes it.
	requested := []int64{tickets[0], tickets[1], tickets[2], tickets[3]}
	_, err = svc.createHold(ctx, "second-buyer", eventID,
		&CreateHoldRequest{TicketIDs: requested})

	require.Error(t, err)
	assert.Equal(t, errs.Aborted, errs.Code(err))

	// The conflict names exactly the contested seat.
	details, ok := errs.Details(err).(SeatConflict)
	require.True(t, ok, "expected SeatConflict details, got %T", errs.Details(err))
	assert.Equal(t, []int64{tickets[2]}, details.LostTicketIDs)

	// Critically: the other three seats are untouched and still claimable.
	for _, id := range []int64{tickets[0], tickets[1], tickets[3]} {
		status, holdID := ticketStatus(ctx, t, id)
		assert.Equal(t, "AVAILABLE", status, "ticket %d must be rolled back", id)
		assert.Nil(t, holdID, "ticket %d must carry no hold", id)
	}

	// And the first buyer still owns theirs.
	status, holdID := ticketStatus(ctx, t, tickets[2])
	assert.Equal(t, "HELD", status)
	require.NotNil(t, holdID)
	assert.Equal(t, taken.HoldID, *holdID)

	// A retry avoiding the contested seat now succeeds.
	_, err = svc.createHold(ctx, "second-buyer", eventID,
		&CreateHoldRequest{TicketIDs: []int64{tickets[0], tickets[1], tickets[3]}})
	assert.NoError(t, err)
}

// A whole event selling out under concurrency must sell every seat exactly once:
// no oversell, and no seat stranded unclaimed.
func TestConcurrentSelloutClaimsEverySeatExactlyOnce(t *testing.T) {
	ctx := context.Background()
	freshDB(ctx, t)
	svc := newTestService()

	const seats = 40
	eventID, tickets := seedEvent(ctx, t, svc, seats)

	var (
		wg sync.WaitGroup
		mu sync.Mutex
		// Each seat maps to the set of holds that claimed it. Any entry with more
		// than one hold is an oversell.
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

			hold, err := svc.createHold(ctx, fmt.Sprintf("buyer-%d", i), eventID,
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
		assert.Len(t, holds, 1, "seat %d was claimed by %d holds — that is an oversell", seat, len(holds))
	}
	assert.Len(t, claimedBy, seats, "every seat should have been claimed exactly once")

	// The database is the final word: exactly `seats` rows are HELD, none AVAILABLE.
	var held, available int64
	require.NoError(t, db.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE status = 'HELD'),
		       count(*) FILTER (WHERE status = 'AVAILABLE')
		  FROM tickets WHERE event_id = $1
	`, eventID).Scan(&held, &available))

	assert.EqualValues(t, seats, held)
	assert.EqualValues(t, 0, available, "a sold-out event must leave no seat stranded")
}

// An expired lease must be claimable immediately, and only one racer may take it over.
func TestConcurrentTakeoverOfExpiredHold(t *testing.T) {
	ctx := context.Background()
	freshDB(ctx, t)
	testClock := frozenClock()
	svc := &Service{clock: testClock}

	eventID, tickets := seedEvent(ctx, t, svc, 1)
	seat := tickets[0]

	_, err := svc.createHold(ctx, "abandoner", eventID, &CreateHoldRequest{TicketIDs: []int64{seat}})
	require.NoError(t, err)

	// Walk past the TTL without running the reaper.
	testClock.Advance(HoldTTL + time.Minute)

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
			hold, err := svc.createHold(ctx, fmt.Sprintf("taker-%d", i), eventID,
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

	status, holdID := ticketStatus(ctx, t, seat)
	assert.Equal(t, "HELD", status)
	require.NotNil(t, holdID)
	assert.Equal(t, winners[0], *holdID)
}
