//go:build e2e

package e2e

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The Step 6 definition of done: hold seats, let the lease lapse, reap, and confirm
// another buyer can take them.
func TestReaperReleasesExpiredHolds(t *testing.T) {
	h := NewHarness(t)
	h.SetClock(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))

	eventID := h.SeedSellableEvent(t, 2, 3, 2000)
	seats := h.AvailableTicketIDs(eventID, 3)

	hold := h.CreateHold(eventID, seats, HoldOpts{User: "abandoner"})
	for _, id := range seats {
		require.Equal(t, "HELD", h.TicketStatuses(eventID)[id])
	}

	// Nothing to reap while the lease is live.
	assert.EqualValues(t, 0, h.ReapHolds().TicketsReleased,
		"a live hold must not be reaped")

	h.AdvanceClock(11 * time.Minute)

	result := h.ReapHolds()
	assert.EqualValues(t, 3, result.TicketsReleased)
	assert.EqualValues(t, 1, result.HoldsExpired)

	for _, id := range seats {
		assert.Equal(t, "AVAILABLE", h.TicketStatuses(eventID)[id])
	}

	var reaped Hold
	resp := h.GetHold(hold.HoldID, "abandoner")
	require.NoError(t, resp.DecodeInto(&reaped))
	assert.Equal(t, "EXPIRED", reaped.Status)
	assert.Empty(t, reaped.Seats, "an expired hold owns no seats")

	// The seats are genuinely sellable again, all the way through payment.
	newHold := h.CreateHold(eventID, seats, HoldOpts{User: "buyer-b"})
	booking := h.Purchase(newHold, PurchaseOpts{User: "buyer-b"})
	assert.Equal(t, "CONFIRMED", booking.Status)
}

// Reaping must be idempotent, since Encore may invoke a cron endpoint more than once.
func TestReaperIsIdempotent(t *testing.T) {
	h := NewHarness(t)
	h.SetClock(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))

	eventID := h.SeedSellableEvent(t, 2, 2, 1000)
	seats := h.AvailableTicketIDs(eventID, 2)
	h.CreateHold(eventID, seats, HoldOpts{})

	h.AdvanceClock(11 * time.Minute)

	first := h.ReapHolds()
	assert.EqualValues(t, 2, first.TicketsReleased)

	second := h.ReapHolds()
	assert.EqualValues(t, 0, second.TicketsReleased, "a second sweep must release nothing extra")
	assert.EqualValues(t, 0, second.HoldsExpired)

	third := h.ReapHolds()
	assert.EqualValues(t, 0, third.TicketsReleased)

	for _, id := range seats {
		assert.Equal(t, "AVAILABLE", h.TicketStatuses(eventID)[id])
	}
}

// The reaper must never touch sold inventory. A bug here would take a seat away from
// somebody who paid for it — the worst outcome in the system after an oversell.
func TestReaperNeverReleasesSoldSeats(t *testing.T) {
	h := NewHarness(t)
	h.SetClock(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))

	eventID := h.SeedSellableEvent(t, 2, 3, 1500)
	sold := h.AvailableTicketIDs(eventID, 2)

	hold := h.CreateHold(eventID, sold, HoldOpts{})
	booking := h.Purchase(hold, PurchaseOpts{})
	require.Equal(t, "CONFIRMED", booking.Status)

	// Move well past the original lease window and sweep repeatedly.
	h.AdvanceClock(2 * time.Hour)
	for range 3 {
		assert.EqualValues(t, 0, h.ReapHolds().TicketsReleased,
			"sold seats must never be released by the reaper")
	}

	for _, id := range sold {
		assert.Equal(t, "SOLD", h.TicketStatuses(eventID)[id])
	}
	assert.Equal(t, "CONFIRMED", h.GetBooking(booking.BookingID, DefaultBuyer).Status)
}

// A released hold leaves nothing for the reaper to do.
func TestReaperIgnoresReleasedHolds(t *testing.T) {
	h := NewHarness(t)
	h.SetClock(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))

	eventID := h.SeedSellableEvent(t, 2, 2, 1000)
	seats := h.AvailableTicketIDs(eventID, 1)
	hold := h.CreateHold(eventID, seats, HoldOpts{})

	require.Equal(t, http.StatusOK, h.ReleaseHold(hold.HoldID, DefaultBuyer).Status)
	h.AdvanceClock(11 * time.Minute)

	result := h.ReapHolds()
	assert.EqualValues(t, 0, result.TicketsReleased)
	assert.EqualValues(t, 0, result.HoldsExpired,
		"an already-released hold is not an expired one")
}

// Expiry frees quota, so an abandoned checkout does not permanently consume a slot.
func TestReaperRestoresHoldQuota(t *testing.T) {
	h := NewHarness(t)
	h.SetClock(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))

	eventID := h.SeedSellableEvent(t, 10, 10, 1000)
	seats := h.AvailableTicketIDs(eventID, 8)

	for i := range 3 {
		h.CreateHold(eventID, seats[i:i+1], HoldOpts{User: "serial-abandoner"})
	}
	blocked := h.TryHold(eventID, seats[5:6], HoldOpts{User: "serial-abandoner"})
	require.Equal(t, http.StatusTooManyRequests, blocked.Status)

	h.AdvanceClock(11 * time.Minute)
	h.ReapHolds()

	// Quota is free again.
	h.CreateHold(eventID, seats[5:6], HoldOpts{User: "serial-abandoner"})
}
