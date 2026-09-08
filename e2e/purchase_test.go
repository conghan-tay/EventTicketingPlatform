//go:build e2e

package e2e

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The happy path: hold, pay, own the seats.
func TestPurchaseConfirmsBooking(t *testing.T) {
	h := NewHarness(t)
	eventID := h.SeedSellableEvent(t, 3, 4, 4500) // 12 seats
	seats := h.AvailableTicketIDs(eventID, 2)

	hold := h.CreateHold(eventID, seats, HoldOpts{})
	booking := h.Purchase(hold, PurchaseOpts{})

	assert.NotEmpty(t, booking.BookingID)
	assert.Equal(t, "CONFIRMED", booking.Status)
	assert.Equal(t, eventID, booking.EventID)
	assert.Equal(t, hold.HoldID, booking.HoldID)
	assert.EqualValues(t, 9000, booking.TotalCents, "2 seats at 4500")

	// The seats are SOLD, and only those seats moved.
	statuses := h.TicketStatuses(eventID)
	for id, status := range statuses {
		if contains(seats, id) {
			assert.Equal(t, "SOLD", status, "ticket %d", id)
		} else {
			assert.Equal(t, "AVAILABLE", status, "ticket %d should be untouched", id)
		}
	}

	// Availability drops permanently.
	resp, err := h.Anonymous().Get(pathf("/v1/events/%d/availability", eventID), nil)
	require.NoError(t, err)
	var avail struct {
		Available int64 `json:"available"`
	}
	require.NoError(t, resp.DecodeInto(&avail))
	assert.EqualValues(t, 10, avail.Available)

	// The booking is readable, and the hold is spent.
	assert.Equal(t, "CONFIRMED", h.GetBooking(booking.BookingID, DefaultBuyer).Status)

	var spent Hold
	hr := h.GetHold(hold.HoldID, DefaultBuyer)
	require.NoError(t, hr.DecodeInto(&spent))
	assert.Equal(t, "CONVERTED", spent.Status)

	// Exactly one charge reached the provider.
	assert.Equal(t, 1, h.PaymentSummary().Charges)
	assert.Equal(t, 0, h.PaymentSummary().Refunds)
}

// A sold seat can never be claimed again. This is the invariant the whole design
// exists to protect.
func TestSoldSeatsCannotBeHeldAgain(t *testing.T) {
	h := NewHarness(t)
	eventID := h.SeedSellableEvent(t, 2, 2, 1000)
	seats := h.AvailableTicketIDs(eventID, 2)

	hold := h.CreateHold(eventID, seats, HoldOpts{})
	h.Purchase(hold, PurchaseOpts{})

	resp := h.TryHold(eventID, seats, HoldOpts{User: "late-buyer"})
	assert.Equal(t, http.StatusConflict, resp.Status, "body: %s", resp.Body)

	// Still SOLD, and still owned by the original booking.
	for _, id := range seats {
		assert.Equal(t, "SOLD", h.TicketStatuses(eventID)[id])
	}
}

// A declined payment must release the seats immediately rather than making the buyer
// and everyone else wait out the TTL.
func TestPurchaseDeclinedReleasesSeats(t *testing.T) {
	h := NewHarness(t)
	eventID := h.SeedSellableEvent(t, 2, 3, 2000)
	seats := h.AvailableTicketIDs(eventID, 2)

	h.SetPaymentBehaviour(PaymentDecline)

	hold := h.CreateHold(eventID, seats, HoldOpts{})
	resp := h.TryPurchase(hold, PurchaseOpts{})

	require.Equal(t, http.StatusConflict, resp.Status, "body: %s", resp.Body)
	assert.Contains(t, resp.Error().Message, "declined")

	// The failure is a durable, readable record, not just an error response.
	var body struct {
		Details struct {
			BookingID string `json:"booking_id"`
			Reason    string `json:"reason"`
		} `json:"details"`
	}
	require.NoError(t, resp.DecodeInto(&body))
	require.NotEmpty(t, body.Details.BookingID, "a declined attempt should still record a booking")

	booking := h.GetBooking(body.Details.BookingID, DefaultBuyer)
	assert.Equal(t, "FAILED", booking.Status)
	assert.Equal(t, "card_declined", booking.FailureReason)

	// Seats are back on sale and immediately claimable by someone else.
	for _, id := range seats {
		assert.Equal(t, "AVAILABLE", h.TicketStatuses(eventID)[id])
	}
	h.SetPaymentBehaviour(PaymentSucceed)
	h.Purchase(h.CreateHold(eventID, seats, HoldOpts{User: "buyer-b"}), PurchaseOpts{User: "buyer-b"})
}

// A provider transport failure is ambiguous — the charge may or may not have landed.
// The seats must NOT be released, because releasing them while a charge is in flight
// is how you sell one seat twice and refund neither.
func TestPurchaseProviderErrorLeavesHoldIntact(t *testing.T) {
	h := NewHarness(t)
	eventID := h.SeedSellableEvent(t, 2, 2, 3000)
	seats := h.AvailableTicketIDs(eventID, 1)

	h.SetPaymentBehaviour(PaymentError)

	hold := h.CreateHold(eventID, seats, HoldOpts{})
	resp := h.TryPurchase(hold, PurchaseOpts{})

	require.Equal(t, http.StatusServiceUnavailable, resp.Status,
		"an ambiguous provider failure must not be reported as a clean decline: %s", resp.Body)

	// The seat is still held by this buyer, not released and not sold.
	assert.Equal(t, "HELD", h.TicketStatuses(eventID)[seats[0]])

	var still Hold
	hr := h.GetHold(hold.HoldID, DefaultBuyer)
	require.NoError(t, hr.DecodeInto(&still))
	assert.Equal(t, "ACTIVE", still.Status)

	// Nobody else can take it.
	other := h.TryHold(eventID, seats, HoldOpts{User: "opportunist"})
	assert.Equal(t, http.StatusConflict, other.Status, "body: %s", other.Body)
}

// The fence: holding the hold id is not enough, the caller must present the token.
func TestPurchaseRequiresValidHoldToken(t *testing.T) {
	h := NewHarness(t)
	eventID := h.SeedSellableEvent(t, 2, 2, 1000)
	hold := h.CreateHold(eventID, h.AvailableTicketIDs(eventID, 1), HoldOpts{})

	t.Run("missing token", func(t *testing.T) {
		resp := h.TryPurchase(hold, PurchaseOpts{OmitToken: true})
		assert.Equal(t, http.StatusBadRequest, resp.Status, "body: %s", resp.Body)
	})

	t.Run("wrong token", func(t *testing.T) {
		resp := h.TryPurchase(hold, PurchaseOpts{Token: uuid.NewString()})
		assert.Equal(t, http.StatusForbidden, resp.Status, "body: %s", resp.Body)
	})

	// No charge was attempted for either rejected request.
	assert.Equal(t, 0, h.PaymentSummary().Charges)

	// And the hold is still usable with the right token.
	h.Purchase(hold, PurchaseOpts{})
}

func TestPurchaseRequiresOwnership(t *testing.T) {
	h := NewHarness(t)
	eventID := h.SeedSellableEvent(t, 2, 2, 1000)
	hold := h.CreateHold(eventID, h.AvailableTicketIDs(eventID, 1), HoldOpts{})

	// Another user with a stolen hold id AND token still cannot buy it.
	resp := h.TryPurchase(hold, PurchaseOpts{User: "thief"})
	assert.Equal(t, http.StatusNotFound, resp.Status, "body: %s", resp.Body)
	assert.Equal(t, 0, h.PaymentSummary().Charges)
}

func TestPurchaseRequiresIdempotencyKey(t *testing.T) {
	h := NewHarness(t)
	eventID := h.SeedSellableEvent(t, 2, 2, 1000)
	hold := h.CreateHold(eventID, h.AvailableTicketIDs(eventID, 1), HoldOpts{})

	resp, err := h.AsUser(DefaultBuyer).PostWithHeaders(
		pathf("/v1/holds/%s/purchase", hold.HoldID),
		map[string]any{"payment_method": "pm_test_card"},
		map[string]string{"X-Hold-Token": hold.HoldToken},
	)
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadRequest, resp.Status,
		"an unkeyed purchase cannot be made safe against retries: %s", resp.Body)
	assert.Equal(t, 0, h.PaymentSummary().Charges)
}

// The money-critical idempotency property: a retried purchase charges exactly once.
func TestPurchaseIdempotencyChargesOnce(t *testing.T) {
	h := NewHarness(t)
	eventID := h.SeedSellableEvent(t, 2, 3, 2500)
	seats := h.AvailableTicketIDs(eventID, 2)

	hold := h.CreateHold(eventID, seats, HoldOpts{})

	first := h.Purchase(hold, PurchaseOpts{IdempotencyKey: "purchase-1"})
	second := h.Purchase(hold, PurchaseOpts{IdempotencyKey: "purchase-1"})

	assert.Equal(t, first.BookingID, second.BookingID, "replay must return the original booking")
	assert.Equal(t, "CONFIRMED", second.Status)

	assert.Equal(t, 1, h.PaymentSummary().Charges, "the card must be charged exactly once")

	// Exactly two seats sold, not four.
	sold := 0
	for _, status := range h.TicketStatuses(eventID) {
		if status == "SOLD" {
			sold++
		}
	}
	assert.Equal(t, 2, sold)
}

func TestPurchaseRejectsExpiredHold(t *testing.T) {
	h := NewHarness(t)
	h.SetClock(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))

	eventID := h.SeedSellableEvent(t, 2, 2, 1000)
	hold := h.CreateHold(eventID, h.AvailableTicketIDs(eventID, 1), HoldOpts{})

	h.AdvanceClock(31 * time.Minute)

	resp := h.TryPurchase(hold, PurchaseOpts{})
	assert.Equal(t, http.StatusBadRequest, resp.Status, "body: %s", resp.Body)
	assert.Contains(t, resp.Error().Message, "expired")
	assert.Equal(t, 0, h.PaymentSummary().Charges, "an expired hold must never reach the provider")
}

// D9: a purchase started with very little TTL left must extend the lease before
// charging, rather than racing the expiry and risking a charge with no seats.
func TestPurchaseExtendsHoldWhenTTLIsShort(t *testing.T) {
	h := NewHarness(t)
	h.SetClock(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))

	eventID := h.SeedSellableEvent(t, 2, 2, 1000)
	seats := h.AvailableTicketIDs(eventID, 1)
	hold := h.CreateHold(eventID, seats, HoldOpts{})

	// Leave less than the payment budget on the clock (TTL 10m, budget 60s).
	h.AdvanceClock(9*time.Minute + 30*time.Second)

	booking := h.Purchase(hold, PurchaseOpts{})
	assert.Equal(t, "CONFIRMED", booking.Status,
		"a purchase inside the danger window should extend the lease and succeed, not fail")
	assert.Equal(t, "SOLD", h.TicketStatuses(eventID)[seats[0]])
}

func TestListBookings(t *testing.T) {
	h := NewHarness(t)
	eventID := h.SeedSellableEvent(t, 4, 4, 1500)

	var want []string
	for range 2 {
		hold := h.CreateHold(eventID, h.AvailableTicketIDs(eventID, 1), HoldOpts{})
		want = append(want, h.Purchase(hold, PurchaseOpts{}).BookingID)
	}

	resp, err := h.AsUser(DefaultBuyer).Get("/v1/bookings", nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.Status, "body: %s", resp.Body)

	var list struct {
		Bookings []Booking `json:"bookings"`
	}
	require.NoError(t, resp.DecodeInto(&list))
	require.Len(t, list.Bookings, 2)

	got := []string{list.Bookings[0].BookingID, list.Bookings[1].BookingID}
	assert.ElementsMatch(t, want, got)

	// Another user sees none of them.
	resp, err = h.AsUser("nosey").Get("/v1/bookings", nil)
	require.NoError(t, err)
	var theirs struct {
		Bookings []Booking `json:"bookings"`
	}
	require.NoError(t, resp.DecodeInto(&theirs))
	assert.Empty(t, theirs.Bookings)
}

func TestGetBookingRequiresOwnership(t *testing.T) {
	h := NewHarness(t)
	eventID := h.SeedSellableEvent(t, 2, 2, 1000)
	hold := h.CreateHold(eventID, h.AvailableTicketIDs(eventID, 1), HoldOpts{})
	booking := h.Purchase(hold, PurchaseOpts{})

	resp, err := h.AsUser("not-the-buyer").Get(pathf("/v1/bookings/%s", booking.BookingID), nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, resp.Status, "body: %s", resp.Body)
}

// Selling out an event through the real HTTP path must sell every seat exactly once.
func TestSellOutEventEndToEnd(t *testing.T) {
	h := NewHarness(t)
	eventID := h.SeedSellableEvent(t, 3, 4, 1000) // 12 seats

	soldTotal := 0
	for i := range 6 {
		user := "buyer-" + string(rune('a'+i))
		seats := h.AvailableTicketIDs(eventID, 2)
		hold := h.CreateHold(eventID, seats, HoldOpts{User: user})
		booking := h.Purchase(hold, PurchaseOpts{User: user})
		require.Equal(t, "CONFIRMED", booking.Status)
		soldTotal += 2
	}
	require.Equal(t, 12, soldTotal)

	// Every seat is sold; none stranded, none oversold.
	statuses := h.TicketStatuses(eventID)
	require.Len(t, statuses, 12)
	for id, status := range statuses {
		assert.Equal(t, "SOLD", status, "ticket %d", id)
	}

	resp, err := h.Anonymous().Get(pathf("/v1/events/%d/availability", eventID), nil)
	require.NoError(t, err)
	var avail struct {
		Total     int64 `json:"total"`
		Available int64 `json:"available"`
	}
	require.NoError(t, resp.DecodeInto(&avail))
	assert.EqualValues(t, 12, avail.Total)
	assert.EqualValues(t, 0, avail.Available)

	// And a further hold attempt finds nothing.
	assert.Equal(t, 6, h.PaymentSummary().Charges)
}
