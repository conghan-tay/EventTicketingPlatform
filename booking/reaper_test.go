package booking

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The reaper's hard rule: a SOLD ticket is never released.
//
// This is a hard-to-reach state — a sold ticket whose old lease has long since
// lapsed — and the failure would be silent and severe: a seat taken back from
// somebody who paid for it.
func TestReaperLeavesSoldTicketSold(t *testing.T) {
	ctx := context.Background()
	freshDB(ctx, t)
	testClock := frozenClock()
	svc := &Service{clock: testClock}

	eventID, tickets := seedEvent(ctx, t, svc, 2)
	seat := tickets[0]

	hold, err := svc.createHold(ctx, "buyer", eventID, &CreateHoldRequest{TicketIDs: []int64{seat}})
	require.NoError(t, err)

	// Sell it, then plant a lapsed hold_expires_at on the sold row. This is the
	// dangerous shape: SOLD, but carrying a timestamp far in the past.
	bookingID := uuid.New()
	_, err = db.Exec(ctx, `
		INSERT INTO bookings (booking_id, event_id, hold_id, user_id, status,
		                      total_cents, created_at, updated_at)
		VALUES ($1, $2, $3, 'buyer', 'CONFIRMED', 5000, $4, $4)
	`, bookingID, eventID, mustParseUUID(t, hold.HoldID), testClock.Now())
	require.NoError(t, err)

	_, err = db.Exec(ctx, `
		UPDATE tickets
		   SET status = 'SOLD', booking_id = $2, hold_id = NULL,
		       hold_expires_at = $3
		 WHERE ticket_id = $1
	`, seat, bookingID, testClock.Now().Add(-24*time.Hour))
	require.NoError(t, err)

	testClock.Advance(48 * time.Hour)

	// Sweep repeatedly; the seat must survive every one.
	for range 3 {
		result, err := svc.reap(ctx)
		require.NoError(t, err)
		assert.EqualValues(t, 0, result.TicketsReleased,
			"the reaper released a sold ticket — a paying customer just lost their seat")
	}

	status, holdRef := ticketStatus(ctx, t, seat)
	assert.Equal(t, "SOLD", status)
	assert.Nil(t, holdRef)
}

// The reaper is idempotent, as Encore requires of cron endpoints.
func TestReaperIsIdempotent(t *testing.T) {
	ctx := context.Background()
	freshDB(ctx, t)
	testClock := frozenClock()
	svc := &Service{clock: testClock}

	eventID, tickets := seedEvent(ctx, t, svc, 4)
	_, err := svc.createHold(ctx, "abandoner", eventID, &CreateHoldRequest{TicketIDs: tickets})
	require.NoError(t, err)

	testClock.Advance(HoldTTL + time.Minute)

	first, err := svc.reap(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 4, first.TicketsReleased)
	assert.EqualValues(t, 1, first.HoldsExpired)

	second, err := svc.reap(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 0, second.TicketsReleased)
	assert.EqualValues(t, 0, second.HoldsExpired)
}

// A live lease must survive a sweep. Reaping early would yank seats out of an
// in-progress checkout.
func TestReaperDoesNotTouchLiveHolds(t *testing.T) {
	ctx := context.Background()
	freshDB(ctx, t)
	testClock := frozenClock()
	svc := &Service{clock: testClock}

	eventID, tickets := seedEvent(ctx, t, svc, 3)
	hold, err := svc.createHold(ctx, "shopper", eventID, &CreateHoldRequest{TicketIDs: tickets})
	require.NoError(t, err)

	// Advance to just before expiry.
	testClock.Advance(HoldTTL - time.Second)

	result, err := svc.reap(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 0, result.TicketsReleased)

	for _, id := range tickets {
		status, holdRef := ticketStatus(ctx, t, id)
		assert.Equal(t, "HELD", status)
		require.NotNil(t, holdRef)
		assert.Equal(t, hold.HoldID, *holdRef)
	}
}

// SKIP LOCKED means concurrent reapers cooperate instead of fighting: seats are
// released exactly once in total, and no sweep errors out on a lock conflict.
func TestConcurrentReapersReleaseEachSeatOnce(t *testing.T) {
	ctx := context.Background()
	freshDB(ctx, t)
	testClock := frozenClock()
	svc := &Service{clock: testClock}

	const seats = 30
	eventID, tickets := seedEvent(ctx, t, svc, seats)

	// Several holds so there is real work to divide.
	for i := 0; i < seats; i += 5 {
		_, err := svc.createHold(ctx, "abandoner-"+uuid.NewString(), eventID,
			&CreateHoldRequest{TicketIDs: tickets[i : i+5]})
		require.NoError(t, err)
	}

	testClock.Advance(HoldTTL + time.Minute)

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		total    int64
		sweepErr []error
	)
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := svc.reap(ctx)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				sweepErr = append(sweepErr, err)
				return
			}
			total += result.TicketsReleased
		}()
	}
	wg.Wait()

	assert.Empty(t, sweepErr, "concurrent reapers must not error; SKIP LOCKED avoids the conflict")
	assert.EqualValues(t, seats, total,
		"each seat must be counted as released exactly once across all sweeps")

	var available int64
	require.NoError(t, db.QueryRow(ctx, `
		SELECT count(*) FROM tickets WHERE event_id = $1 AND status = 'AVAILABLE'
	`, eventID).Scan(&available))
	assert.EqualValues(t, seats, available)
}
