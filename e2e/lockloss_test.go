//go:build e2e

package e2e

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Losing the lease store must cost money, never a seat.
//
// Moving the hold out of Postgres bought fast acquisition and free expiry, and it
// introduced a failure the old design could not have: Redis is not durable, so an
// eviction, a failover or a restart can drop a lock while a purchase is in flight.
// Two buyers then both believe they hold the same seat.
//
// The design's answer is that the lease is only an optimistic gate. The authoritative
// step is still the conditional UPDATE in convertToSold, which converts a ticket only
// while it is AVAILABLE. These tests are the evidence that the answer holds.

// FlushLocks drops every lease, simulating a total Redis loss.
func (h *Harness) FlushLocks() {
	h.t.Helper()
	resp, err := h.Post("/_test/locks/flush", nil)
	require.NoError(h.t, err)
	require.Equal(h.t, http.StatusOK, resp.Status, "flush locks: %s", resp.Body)
}

// EvictSeatLocks drops seat locks but keeps lease records, as allkeys-lru would.
func (h *Harness) EvictSeatLocks() {
	h.t.Helper()
	resp, err := h.Post("/_test/locks/evict-seat-locks", nil)
	require.NoError(h.t, err)
	require.Equal(h.t, http.StatusOK, resp.Status, "evict seat locks: %s", resp.Body)
}

// Total loss of the lease store is the benign case: a buyer whose lease record is gone
// is refused outright, before the provider is ever called.
func TestTotalLockLossRefusesBeforeCharging(t *testing.T) {
	h := NewHarness(t)

	eventID := h.SeedSellableEvent(t, 2, 2, 2500)
	seats := h.AvailableTicketIDs(eventID, 1)

	hold := h.CreateHold(eventID, seats, HoldOpts{User: "buyer-a"})
	before := h.PaymentSummary().Charges

	h.FlushLocks()

	resp := h.TryPurchase(hold, PurchaseOpts{User: "buyer-a"})
	assert.NotEqual(t, http.StatusOK, resp.Status,
		"a purchase against a vanished lease must not succeed: %s", resp.Body)
	assert.Equal(t, before, h.PaymentSummary().Charges,
		"a buyer whose lease is gone must never reach the provider")
	assert.Equal(t, "AVAILABLE", h.TicketStatuses(eventID)[seats[0]])
}

// The dangerous case: the lock is evicted while the lease record survives, so two
// buyers hold live leases on one seat and both try to buy it.
//
// The seat must be sold exactly once. In practice the loser is also refused before the
// provider is called — losing a seat removes it from the lease index, and prepareLease
// prices from the index, so the purchase dies at validation. That is a better outcome
// than a compensation and worth pinning down, because it is not obvious from the code
// that the checks compose that way.
//
// The genuinely money-losing interleaving — lease valid at prepareLease, seat gone by
// convertToSold — cannot be driven from here, because it needs the seat to be taken
// during the charge itself. That is what the duringPaymentHook seam in
// booking/purchase_test.go exists for.
func TestEvictedLockCannotCauseAnOversell(t *testing.T) {
	h := NewHarness(t)

	eventID := h.SeedSellableEvent(t, 2, 2, 2500)
	seats := h.AvailableTicketIDs(eventID, 1)

	// Buyer A takes the seat legitimately.
	holdA := h.CreateHold(eventID, seats, HoldOpts{User: "buyer-a"})

	// Redis evicts the lock. A's lease record survives, so A still believes it holds
	// the seat — which is precisely the corruption being induced.
	h.EvictSeatLocks()

	// Buyer B can now claim and buy the same seat, because nothing remembers A's lock.
	holdB := h.CreateHold(eventID, seats, HoldOpts{User: "buyer-b"})
	require.NotEqual(t, holdA.HoldID, holdB.HoldID,
		"the point of this test is two live leases on one seat")

	chargesBefore := h.PaymentSummary().Charges

	bookingB := h.Purchase(holdB, PurchaseOpts{User: "buyer-b"})
	require.Equal(t, "CONFIRMED", bookingB.Status)

	// Now A tries to buy a seat that is already gone.
	respA := h.TryPurchase(holdA, PurchaseOpts{User: "buyer-a"})

	assert.NotEqual(t, http.StatusOK, respA.Status,
		"the second buyer must not also be sold the seat: %s", respA.Body)

	// Only B's charge reached the provider. A was refused at validation, so there is no
	// money to refund — strictly better than compensating, and the reason this test
	// does not look for a booking id on A's response.
	assert.Equal(t, chargesBefore+1, h.PaymentSummary().Charges,
		"only the buyer who got the seat should have been charged")

	// The seat is sold once, to B.
	assert.Equal(t, "BOOKED", h.TicketStatuses(eventID)[seats[0]])

	// The audit is the assertion that actually matters: a duplicate here would mean one
	// physical seat represented twice.
	integrity := h.Integrity(eventID)
	assert.EqualValues(t, 0, integrity.DuplicateSeats,
		"OVERSELL: the same seat is represented by more than one ticket row")
	assert.EqualValues(t, 1, integrity.Sold, "exactly one seat may be sold")
	assert.EqualValues(t, integrity.Sold, integrity.TicketsInConfirmedBookings,
		"every sold seat must belong to exactly one confirmed booking")
	assert.True(t, integrity.Consistent,
		"an unresolved compensation means money was taken with no seat and no refund: %+v",
		integrity)
}
