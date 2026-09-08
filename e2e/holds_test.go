//go:build e2e

package e2e

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Happy path: claim specific seats and receive a lease.
func TestCreateHold(t *testing.T) {
	h := NewHarness(t)
	eventID := h.SeedSellableEvent(t, 4, 5, 5000) // 20 seats
	seats := h.AvailableTicketIDs(eventID, 3)

	hold := h.CreateHold(eventID, seats, HoldOpts{})

	assert.Equal(t, eventID, hold.EventID)
	assert.Equal(t, "ACTIVE", hold.Status)
	assert.Len(t, hold.Seats, 3)
	assert.EqualValues(t, 15000, hold.TotalCents, "3 seats at 5000")
	assert.True(t, hold.ExpiresAt.After(h.ClockNow()), "hold must expire in the future")

	// The claimed seats are now HELD, and nothing else moved.
	statuses := h.TicketStatuses(eventID)
	held := 0
	for id, status := range statuses {
		if contains(seats, id) {
			assert.Equal(t, "HELD", status, "ticket %d", id)
			held++
		} else {
			assert.Equal(t, "AVAILABLE", status, "ticket %d should be untouched", id)
		}
	}
	assert.Equal(t, 3, held)

	// Availability reflects the hold: held seats are not available.
	resp, err := h.Anonymous().Get(pathf("/v1/events/%d/availability", eventID), nil)
	require.NoError(t, err)
	var avail struct {
		Total     int64 `json:"total"`
		Available int64 `json:"available"`
	}
	require.NoError(t, resp.DecodeInto(&avail))
	assert.EqualValues(t, 20, avail.Total)
	assert.EqualValues(t, 17, avail.Available)
}

func TestGetHold(t *testing.T) {
	h := NewHarness(t)
	eventID := h.SeedSellableEvent(t, 2, 2, 1000)
	hold := h.CreateHold(eventID, h.AvailableTicketIDs(eventID, 2), HoldOpts{})

	resp := h.GetHold(hold.HoldID, DefaultBuyer)
	require.Equal(t, http.StatusOK, resp.Status, "body: %s", resp.Body)

	var got Hold
	require.NoError(t, resp.DecodeInto(&got))
	assert.Equal(t, hold.HoldID, got.HoldID)
	assert.Equal(t, "ACTIVE", got.Status)
	assert.Len(t, got.Seats, 2)
	assert.Greater(t, got.SecondsRemaining, 0.0)

	// A hold is private to its owner.
	other := h.GetHold(hold.HoldID, "someone-else")
	assert.Equal(t, http.StatusNotFound, other.Status,
		"another user must not read this hold, got %d: %s", other.Status, other.Body)
}

// SEAT_UNAVAILABLE must name the seats that were lost, so a client can re-render the
// seat map and let the user pick again.
func TestHoldConflictNamesLostSeats(t *testing.T) {
	h := NewHarness(t)
	eventID := h.SeedSellableEvent(t, 2, 5, 1000) // 10 seats
	seats := h.AvailableTicketIDs(eventID, 4)

	first := h.CreateHold(eventID, seats[:2], HoldOpts{User: "buyer-a"})
	require.Len(t, first.Seats, 2)

	// Second buyer asks for one already-held seat plus two free ones.
	overlapping := []int64{seats[1], seats[2], seats[3]}
	resp := h.TryHold(eventID, overlapping, HoldOpts{User: "buyer-b"})

	require.Equal(t, http.StatusConflict, resp.Status, "body: %s", resp.Body)

	var body struct {
		Details struct {
			LostTicketIDs []int64 `json:"lost_ticket_ids"`
		} `json:"details"`
	}
	require.NoError(t, resp.DecodeInto(&body))
	assert.Equal(t, []int64{seats[1]}, body.Details.LostTicketIDs,
		"only the contested seat should be reported lost")

	// All-or-nothing: the two free seats were NOT claimed by the failed request.
	statuses := h.TicketStatuses(eventID)
	assert.Equal(t, "AVAILABLE", statuses[seats[2]])
	assert.Equal(t, "AVAILABLE", statuses[seats[3]])
	assert.Equal(t, "HELD", statuses[seats[0]])
	assert.Equal(t, "HELD", statuses[seats[1]])
}

func TestHoldRejectsUnknownAndForeignTickets(t *testing.T) {
	h := NewHarness(t)
	eventA := h.SeedSellableEvent(t, 2, 2, 1000)
	eventB := h.SeedSellableEvent(t, 2, 2, 1000)

	// A ticket id that does not exist.
	resp := h.TryHold(eventA, []int64{999999}, HoldOpts{})
	assert.Equal(t, http.StatusNotFound, resp.Status, "body: %s", resp.Body)

	// A ticket belonging to a different event must not be claimable via eventA.
	foreign := h.AvailableTicketIDs(eventB, 1)
	resp = h.TryHold(eventA, foreign, HoldOpts{})
	assert.Equal(t, http.StatusNotFound, resp.Status,
		"a ticket from another event must not be holdable here, got %d: %s", resp.Status, resp.Body)

	// eventB's inventory is untouched.
	assert.Equal(t, "AVAILABLE", h.TicketStatuses(eventB)[foreign[0]])
}

func TestHoldValidatesRequest(t *testing.T) {
	h := NewHarness(t)
	eventID := h.SeedSellableEvent(t, 4, 5, 1000)
	seats := h.AvailableTicketIDs(eventID, 2)

	cases := map[string][]int64{
		"empty":     {},
		"duplicate": {seats[0], seats[0]},
	}
	for name, ids := range cases {
		t.Run(name, func(t *testing.T) {
			resp := h.TryHold(eventID, ids, HoldOpts{})
			assert.Equal(t, http.StatusBadRequest, resp.Status, "body: %s", resp.Body)
		})
	}

	t.Run("too many seats in one hold", func(t *testing.T) {
		many := h.AvailableTicketIDs(eventID, 12)
		resp := h.TryHold(eventID, many, HoldOpts{})
		assert.Equal(t, http.StatusBadRequest, resp.Status, "body: %s", resp.Body)

		// Nothing was claimed.
		for _, id := range many {
			assert.Equal(t, "AVAILABLE", h.TicketStatuses(eventID)[id])
		}
	})
}

// HOLD_LIMIT_EXCEEDED: the per-user active-hold quota, which is the cheap defence
// against a script hoarding inventory it never intends to buy.
func TestHoldLimitExceeded(t *testing.T) {
	h := NewHarness(t)
	eventID := h.SeedSellableEvent(t, 10, 10, 1000) // 100 seats
	seats := h.AvailableTicketIDs(eventID, 20)

	// Consume the quota with separate holds.
	for i := range 3 {
		h.CreateHold(eventID, seats[i:i+1], HoldOpts{User: "hoarder"})
	}

	resp := h.TryHold(eventID, seats[10:11], HoldOpts{User: "hoarder"})
	assert.Equal(t, http.StatusTooManyRequests, resp.Status,
		"a fourth concurrent hold should be refused, got %d: %s", resp.Status, resp.Body)

	// The quota is per user, so a different buyer is unaffected.
	h.CreateHold(eventID, seats[11:12], HoldOpts{User: "other-buyer"})
}

func TestHoldRejectsEventNotOnSale(t *testing.T) {
	h := NewHarness(t)

	t.Run("draft event", func(t *testing.T) {
		venueID := h.CreateVenue(VenueOpts{})
		draft := h.CreateEvent(venueID, EventOpts{})
		// No published tickets exist, so this must be refused cleanly, not 500.
		resp := h.TryHold(draft, []int64{1}, HoldOpts{})
		assert.Equal(t, http.StatusBadRequest, resp.Status, "body: %s", resp.Body)
		assert.Equal(t, "failed_precondition", resp.Error().Code)
		assert.Contains(t, resp.Error().Message, "not on sale")
	})

	t.Run("onsale in the future", func(t *testing.T) {
		now := h.SetClock(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))
		venueID := h.CreateVenue(VenueOpts{Sections: []SectionSpec{{Name: "FLOOR", Rows: 2, SeatsPerRow: 2}}})
		eventID := h.CreateEvent(venueID, EventOpts{
			StartsAt: now.Add(30 * 24 * time.Hour),
			OnsaleAt: now.Add(7 * 24 * time.Hour), // not yet on sale
			Tiers:    []TierSpec{{Section: "FLOOR", PriceCents: 1000}},
		})
		h.PublishEvent(eventID)

		seats := h.AvailableTicketIDs(eventID, 1)
		resp := h.TryHold(eventID, seats, HoldOpts{})
		assert.Equal(t, http.StatusBadRequest, resp.Status,
			"holding before onsale should be refused, got %d: %s", resp.Status, resp.Body)
		assert.Contains(t, resp.Error().Message, "not yet on sale")

		// Once the onsale time arrives, the same request succeeds.
		h.AdvanceClock(8 * 24 * time.Hour)
		h.CreateHold(eventID, seats, HoldOpts{})
	})
}

func TestHoldRequiresAuth(t *testing.T) {
	h := NewHarness(t)
	eventID := h.SeedSellableEvent(t, 2, 2, 1000)
	seats := h.AvailableTicketIDs(eventID, 1)

	resp, err := h.Anonymous().Post(pathf("/v1/events/%d/holds", eventID),
		map[string]any{"ticket_ids": seats})
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized, resp.Status, "body: %s", resp.Body)
}

// A replayed request must return the original hold, not claim more seats. Without
// this, a client retrying on a flaky network silently hoards inventory.
func TestHoldIdempotency(t *testing.T) {
	h := NewHarness(t)
	eventID := h.SeedSellableEvent(t, 4, 5, 2000)
	seats := h.AvailableTicketIDs(eventID, 2)

	first := h.CreateHold(eventID, seats, HoldOpts{IdempotencyKey: "idem-1"})
	second := h.CreateHold(eventID, seats, HoldOpts{IdempotencyKey: "idem-1"})

	assert.Equal(t, first.HoldID, second.HoldID, "replay must return the original hold")
	assert.Equal(t, first.HoldToken, second.HoldToken, "the client needs the same token back")

	// Exactly two seats are held in total, not four.
	heldCount := 0
	for _, status := range h.TicketStatuses(eventID) {
		if status == "HELD" {
			heldCount++
		}
	}
	assert.Equal(t, 2, heldCount)

	// A different key on the same seats is a genuine new request, and conflicts.
	resp := h.TryHold(eventID, seats, HoldOpts{IdempotencyKey: "idem-2"})
	assert.Equal(t, http.StatusConflict, resp.Status, "body: %s", resp.Body)
}

// The idempotency key is scoped per user, so two users can reuse the same key value.
func TestHoldIdempotencyIsPerUser(t *testing.T) {
	h := NewHarness(t)
	eventID := h.SeedSellableEvent(t, 4, 5, 2000)
	seats := h.AvailableTicketIDs(eventID, 2)

	a := h.CreateHold(eventID, seats[:1], HoldOpts{User: "buyer-a", IdempotencyKey: "shared"})
	b := h.CreateHold(eventID, seats[1:], HoldOpts{User: "buyer-b", IdempotencyKey: "shared"})

	assert.NotEqual(t, a.HoldID, b.HoldID)
}

func TestReleaseHold(t *testing.T) {
	h := NewHarness(t)
	eventID := h.SeedSellableEvent(t, 2, 3, 1000)
	seats := h.AvailableTicketIDs(eventID, 2)

	hold := h.CreateHold(eventID, seats, HoldOpts{})
	require.Equal(t, http.StatusOK, h.ReleaseHold(hold.HoldID, DefaultBuyer).Status)

	// Seats are immediately available again and re-claimable.
	statuses := h.TicketStatuses(eventID)
	for _, id := range seats {
		assert.Equal(t, "AVAILABLE", statuses[id])
	}
	h.CreateHold(eventID, seats, HoldOpts{User: "buyer-b"})
}

func TestReleaseHoldRequiresOwnership(t *testing.T) {
	h := NewHarness(t)
	eventID := h.SeedSellableEvent(t, 2, 2, 1000)
	hold := h.CreateHold(eventID, h.AvailableTicketIDs(eventID, 1), HoldOpts{})

	resp := h.ReleaseHold(hold.HoldID, "not-the-owner")
	assert.Equal(t, http.StatusNotFound, resp.Status, "body: %s", resp.Body)

	// Still held by the rightful owner.
	assert.Equal(t, "ACTIVE", func() string {
		var got Hold
		r := h.GetHold(hold.HoldID, DefaultBuyer)
		require.NoError(t, r.DecodeInto(&got))
		return got.Status
	}())
}

// An expired hold must not block a new claim even before the reaper has run. Expiry
// is a batch process, so the claim path has to treat a lapsed lease as claimable.
func TestExpiredHoldIsImmediatelyReclaimable(t *testing.T) {
	h := NewHarness(t)
	h.SetClock(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))

	eventID := h.SeedSellableEvent(t, 2, 2, 1000)
	seats := h.AvailableTicketIDs(eventID, 2)

	first := h.CreateHold(eventID, seats, HoldOpts{User: "abandoner"})

	// Walk past the TTL without running the reaper.
	h.AdvanceClock(31 * time.Minute)

	second := h.CreateHold(eventID, seats, HoldOpts{User: "buyer-b"})
	assert.NotEqual(t, first.HoldID, second.HoldID)

	// The abandoned hold is now reported as expired, not still active.
	var stale Hold
	resp := h.GetHold(first.HoldID, "abandoner")
	require.Equal(t, http.StatusOK, resp.Status, "body: %s", resp.Body)
	require.NoError(t, resp.DecodeInto(&stale))
	assert.Equal(t, "EXPIRED", stale.Status,
		"a hold whose seats were taken over must not still claim to be ACTIVE")
}

func contains(haystack []int64, needle int64) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}
