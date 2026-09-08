//go:build e2e && loadproof

// This file is the load proof. It is behind its own build tag because it takes tens
// of seconds and drives thousands of requests, which does not belong in the normal
// feedback loop. Run it with `make loadproof`.
//
// It exists to validate the one property the entire design is built around:
//
//	tickets sold == capacity, zero oversells, zero seats stranded.
//
// The unit tests already prove correctness under maximal contention on a single row.
// This proves it holds across a whole event, through the real HTTP stack, with
// payments, expiry and the cache all in play at once.
package e2e

import (
	"math/rand"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// Capacity is the seat count for the onsale. 5,000 is the figure the plan commits
	// to and is a realistic mid-size arena.
	capacity = 5000

	// workers is the concurrency level. Well above the number of database connections,
	// so requests genuinely queue and compete.
	workers = 60

	// seatsPerPurchase is the party size, at the per-hold maximum.
	seatsPerPurchase = 8

	// spoilerRate is how often a worker deliberately targets seats another worker is
	// probably already working on, rather than taking the next free batch.
	//
	// Without this the queue hands each seat to exactly one worker and no conflict
	// ever occurs — the test would pass while proving nothing about contention.
	spoilerRate = 0.35

	loadProofBudget = 5 * time.Minute
)

type loadStats struct {
	holdsCreated   atomic.Int64
	holdConflicts  atomic.Int64
	quotaRejects   atomic.Int64
	purchasesOK    atomic.Int64
	purchaseFailed atomic.Int64
	otherErrors    atomic.Int64
}

func TestLoadProofSellOutUnderConcurrency(t *testing.T) {
	h := NewHarness(t)

	t.Logf("seeding a %d-seat event", capacity)
	eventID := h.SeedPublishedEvent(
		VenueOpts{Sections: []SectionSpec{
			// 50 rows x 100 seats = 5,000
			{Name: "FLOOR", Rows: 50, SeatsPerRow: 100},
		}},
		EventOpts{Tiers: []TierSpec{{Section: "FLOOR", PriceCents: 7500}}},
	)

	// Fetch the seat map once. Re-fetching it per iteration would make the harness
	// O(n^2) and measure our own polling rather than the booking path.
	allTickets := h.AvailableTicketIDs(eventID, capacity)
	require.Len(t, allTickets, capacity)

	// Confirm the starting state is what we think it is.
	start := h.Integrity(eventID)
	require.EqualValues(t, capacity, start.Total)
	require.EqualValues(t, capacity, start.Available)
	require.True(t, start.Consistent, "the event should start consistent: %+v", start)

	var (
		stats    loadStats
		nextIdx  atomic.Int64
		soldSeat sync.Map // ticketID -> bookingID, to detect a seat sold twice
		wg       sync.WaitGroup
	)

	deadline := time.Now().Add(loadProofBudget)
	began := time.Now()

	for w := range workers {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			// Per-worker RNG: sharing one would serialise workers on its mutex and
			// quietly reduce the concurrency this test is trying to create.
			rng := rand.New(rand.NewSource(int64(worker)*7919 + 13))
			user := "load-buyer-" + strconv.Itoa(worker)

			for time.Now().Before(deadline) {
				batch, done := nextBatch(&nextIdx, allTickets, rng)
				if done {
					return
				}
				attemptPurchase(t, h, eventID, user, batch, &stats, &soldSeat)
			}
		}(w)
	}
	wg.Wait()

	elapsed := time.Since(began)

	// Abandoned or lost holds may still be outstanding. Expire them and sweep, then
	// finish the job sequentially so the run ends deterministically at a full sellout
	// rather than "nearly".
	t.Log("draining remaining inventory")
	h.AdvanceClock(HoldTTLForTests + time.Minute)
	h.ReapHolds()
	drainRemaining(t, h, eventID, &stats, &soldSeat)

	// Final sweep so nothing is left HELD by an abandoned lease.
	h.AdvanceClock(HoldTTLForTests + time.Minute)
	h.ReapHolds()

	final := h.Integrity(eventID)

	t.Logf("load proof finished in %s", elapsed.Round(time.Millisecond))
	t.Logf("  holds created   : %d", stats.holdsCreated.Load())
	t.Logf("  hold conflicts  : %d", stats.holdConflicts.Load())
	t.Logf("  quota rejects   : %d", stats.quotaRejects.Load())
	t.Logf("  purchases ok    : %d", stats.purchasesOK.Load())
	t.Logf("  purchases failed: %d", stats.purchaseFailed.Load())
	t.Logf("  other errors    : %d", stats.otherErrors.Load())
	t.Logf("  integrity       : %+v", final)

	// The test is only meaningful if it actually created contention.
	assert.Positive(t, stats.holdConflicts.Load(),
		"no seat conflicts occurred, so this run did not exercise contention and proves nothing")

	// Nothing should have failed in an unexplained way.
	assert.Zero(t, stats.otherErrors.Load(), "unexpected errors during the onsale")

	// ---- The invariant ----

	assert.EqualValues(t, capacity, final.Total, "capacity must not change")
	assert.EqualValues(t, capacity, final.Sold,
		"every seat must be sold: sold=%d of %d", final.Sold, capacity)
	assert.EqualValues(t, 0, final.Available, "no seat may be left unsold")
	assert.EqualValues(t, 0, final.Held,
		"no seat may be stranded in HELD after the final reap")

	assert.EqualValues(t, 0, final.DuplicateSeats,
		"OVERSELL: a seat is represented by more than one ticket row")
	assert.EqualValues(t, 0, final.SoldWithoutBooking,
		"a sold seat with no booking means somebody's ticket has no owner record")
	assert.EqualValues(t, 0, final.HeldWithoutHold,
		"inventory held by nobody would never be released")
	assert.EqualValues(t, 0, final.UnresolvedCompensations,
		"money was taken without seats delivered and without a completed refund")

	assert.EqualValues(t, capacity, final.TicketsInConfirmedBookings,
		"every sold seat must belong to a confirmed booking")
	assert.True(t, final.Consistent, "final integrity check failed: %+v", final)

	// The in-process map is an independent witness: it is built from what the API
	// told each worker, not from the database, so agreement between the two means
	// no seat was reported sold to two different buyers.
	distinct := 0
	soldSeat.Range(func(_, _ any) bool {
		distinct++
		return true
	})
	assert.Equal(t, capacity, distinct,
		"the API reported %d distinct sold seats, expected %d", distinct, capacity)
}

// HoldTTLForTests mirrors booking.HoldTTL. Duplicated rather than imported because the
// E2E suite deliberately speaks only HTTP and does not link the services.
const HoldTTLForTests = 10 * time.Minute

// nextBatch returns the next set of ticket ids to attempt.
//
// Most of the time it takes an untouched batch, which is what makes the sellout
// progress. The rest of the time it picks a random offset, which is what makes
// workers collide and exercises the conflict path.
func nextBatch(nextIdx *atomic.Int64, all []int64, rng *rand.Rand) (batch []int64, exhausted bool) {
	if rng.Float64() < spoilerRate {
		start := rng.Intn(len(all))
		end := min(start+seatsPerPurchase, len(all))
		return all[start:end], false
	}

	start := int(nextIdx.Add(seatsPerPurchase)) - seatsPerPurchase
	if start >= len(all) {
		return nil, true
	}
	end := min(start+seatsPerPurchase, len(all))
	return all[start:end], false
}

// attemptPurchase runs one hold-then-pay cycle, classifying every outcome.
//
// Conflicts and quota rejections are expected and are counted, not failed: under
// contention they are the correct response, and the point of the test is that they
// happen cleanly.
func attemptPurchase(
	t *testing.T, h *Harness, eventID int64, user string,
	batch []int64, stats *loadStats, soldSeat *sync.Map,
) {
	t.Helper()

	resp := h.TryHold(eventID, batch, HoldOpts{User: user})
	switch resp.Status {
	case http.StatusOK:
		stats.holdsCreated.Add(1)
	case http.StatusConflict:
		stats.holdConflicts.Add(1)
		return
	case http.StatusTooManyRequests:
		stats.quotaRejects.Add(1)
		return
	case http.StatusBadRequest, http.StatusNotFound:
		// An empty batch or an id outside this event; neither should happen, but it
		// is a client-side mistake rather than a system failure.
		stats.otherErrors.Add(1)
		return
	default:
		stats.otherErrors.Add(1)
		t.Logf("unexpected hold status %d: %s", resp.Status, resp.Body)
		return
	}

	var hold Hold
	if err := resp.DecodeInto(&hold); err != nil {
		stats.otherErrors.Add(1)
		return
	}

	pres := h.TryPurchase(hold, PurchaseOpts{User: user})
	switch pres.Status {
	case http.StatusOK:
		var booking Booking
		if err := pres.DecodeInto(&booking); err != nil {
			stats.otherErrors.Add(1)
			return
		}
		stats.purchasesOK.Add(1)

		// Record which booking claims each seat. A second booking for the same seat
		// is an oversell, detected here independently of the database.
		for _, seat := range hold.Seats {
			if prior, loaded := soldSeat.LoadOrStore(seat.TicketID, booking.BookingID); loaded {
				t.Errorf("OVERSELL: ticket %d sold to booking %v and booking %s",
					seat.TicketID, prior, booking.BookingID)
			}
		}
	case http.StatusConflict, http.StatusBadRequest:
		// The lease lapsed or the seats were taken mid-payment. Give the seats back
		// promptly instead of waiting out the TTL.
		stats.purchaseFailed.Add(1)
		h.ReleaseHold(hold.HoldID, user)
	default:
		stats.otherErrors.Add(1)
		t.Logf("unexpected purchase status %d: %s", pres.Status, pres.Body)
		h.ReleaseHold(hold.HoldID, user)
	}
}

// drainRemaining sells whatever the concurrent phase left behind, so the run ends at
// a definite full sellout instead of a flaky "almost".
//
// This is cleanup, not the measurement: the contention assertions above are what
// prove the design, and this only removes run-to-run nondeterminism from the final
// invariant check.
func drainRemaining(t *testing.T, h *Harness, eventID int64, stats *loadStats, soldSeat *sync.Map) {
	t.Helper()

	for round := range 400 {
		state := h.Integrity(eventID)
		if state.Available == 0 && state.Held == 0 {
			return
		}
		if state.Held > 0 && state.Available == 0 {
			// Only lapsed leases remain; expire them and try again.
			h.AdvanceClock(HoldTTLForTests + time.Minute)
			h.ReapHolds()
			continue
		}

		want := int(min(state.Available, int64(seatsPerPurchase)))
		batch := h.AvailableTicketIDs(eventID, want)
		attemptPurchase(t, h, eventID, "drainer-"+strconv.Itoa(round%20), batch, stats, soldSeat)
	}
	t.Fatal("could not drain remaining inventory within the round limit")
}
