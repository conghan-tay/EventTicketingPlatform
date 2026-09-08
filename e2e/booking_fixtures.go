//go:build e2e

package e2e

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

const DefaultBuyer = "buyer-1"

type HeldSeat struct {
	TicketID   int64  `json:"ticket_id"`
	Section    string `json:"section"`
	RowLabel   string `json:"row_label"`
	SeatNumber int    `json:"seat_number"`
	PriceCents int64  `json:"price_cents"`
}

type Hold struct {
	HoldID           string     `json:"hold_id"`
	HoldToken        string     `json:"hold_token"`
	EventID          int64      `json:"event_id"`
	Status           string     `json:"status"`
	ExpiresAt        time.Time  `json:"expires_at"`
	SecondsRemaining float64    `json:"seconds_remaining"`
	Seats            []HeldSeat `json:"seats"`
	TotalCents       int64      `json:"total_cents"`
}

type Booking struct {
	BookingID     string `json:"booking_id"`
	EventID       int64  `json:"event_id"`
	HoldID        string `json:"hold_id"`
	Status        string `json:"status"`
	TotalCents    int64  `json:"total_cents"`
	FailureReason string `json:"failure_reason"`
}

// AvailableTicketIDs returns ticket ids the buyer could claim, in seat-map order.
func (h *Harness) AvailableTicketIDs(eventID int64, n int) []int64 {
	h.t.Helper()

	resp, err := h.Anonymous().Get(pathf("/v1/events/%d/seats", eventID), nil)
	require.NoError(h.t, err)
	require.Equal(h.t, http.StatusOK, resp.Status, "seat map: %s", resp.Body)

	var seatMap struct {
		Seats []struct {
			TicketID int64  `json:"ticket_id"`
			Status   string `json:"status"`
		} `json:"seats"`
	}
	require.NoError(h.t, resp.DecodeInto(&seatMap))

	out := make([]int64, 0, n)
	for _, s := range seatMap.Seats {
		if s.Status == "AVAILABLE" {
			out = append(out, s.TicketID)
			if len(out) == n {
				break
			}
		}
	}
	require.Len(h.t, out, n, "event %d does not have %d available seats", eventID, n)
	return out
}

// TicketStatuses maps ticket id to status, for asserting inventory state directly.
func (h *Harness) TicketStatuses(eventID int64) map[int64]string {
	h.t.Helper()

	resp, err := h.Anonymous().Get(pathf("/v1/events/%d/seats", eventID), nil)
	require.NoError(h.t, err)
	require.Equal(h.t, http.StatusOK, resp.Status, "seat map: %s", resp.Body)

	var seatMap struct {
		Seats []struct {
			TicketID int64  `json:"ticket_id"`
			Status   string `json:"status"`
		} `json:"seats"`
	}
	require.NoError(h.t, resp.DecodeInto(&seatMap))

	out := make(map[int64]string, len(seatMap.Seats))
	for _, s := range seatMap.Seats {
		out[s.TicketID] = s.Status
	}
	return out
}

// HoldOpts controls a hold request. Zero values get defaults.
type HoldOpts struct {
	User           string
	IdempotencyKey string
}

// TryHold attempts a hold and returns the raw response, for asserting failures.
func (h *Harness) TryHold(eventID int64, ticketIDs []int64, opts HoldOpts) *Response {
	h.t.Helper()

	if opts.User == "" {
		opts.User = DefaultBuyer
	}
	headers := map[string]string{}
	if opts.IdempotencyKey != "" {
		headers["Idempotency-Key"] = opts.IdempotencyKey
	}

	resp, err := h.AsUser(opts.User).PostWithHeaders(
		pathf("/v1/events/%d/holds", eventID),
		map[string]any{"ticket_ids": ticketIDs},
		headers,
	)
	require.NoError(h.t, err)
	return resp
}

// CreateHold claims seats and requires success.
func (h *Harness) CreateHold(eventID int64, ticketIDs []int64, opts HoldOpts) Hold {
	h.t.Helper()

	resp := h.TryHold(eventID, ticketIDs, opts)
	require.Equal(h.t, http.StatusOK, resp.Status, "create hold: %s", resp.Body)

	var hold Hold
	require.NoError(h.t, resp.DecodeInto(&hold))
	require.NotEmpty(h.t, hold.HoldID)
	require.NotEmpty(h.t, hold.HoldToken)
	return hold
}

func (h *Harness) GetHold(holdID string, user string) *Response {
	h.t.Helper()
	if user == "" {
		user = DefaultBuyer
	}
	resp, err := h.AsUser(user).Get(pathf("/v1/holds/%s", holdID), nil)
	require.NoError(h.t, err)
	return resp
}

func (h *Harness) ReleaseHold(holdID string, user string) *Response {
	h.t.Helper()
	if user == "" {
		user = DefaultBuyer
	}
	resp, err := h.AsUser(user).Delete(pathf("/v1/holds/%s", holdID))
	require.NoError(h.t, err)
	return resp
}

// PurchaseOpts controls a purchase request.
type PurchaseOpts struct {
	User           string
	Token          string
	IdempotencyKey string
	// OmitToken sends no hold token at all, to prove the fence is enforced.
	OmitToken bool
}

// TryPurchase attempts a purchase and returns the raw response.
func (h *Harness) TryPurchase(hold Hold, opts PurchaseOpts) *Response {
	h.t.Helper()

	if opts.User == "" {
		opts.User = DefaultBuyer
	}
	token := hold.HoldToken
	if opts.Token != "" {
		token = opts.Token
	}

	headers := map[string]string{}
	if !opts.OmitToken {
		headers["X-Hold-Token"] = token
	}
	if opts.IdempotencyKey == "" {
		opts.IdempotencyKey = uuid.NewString()
	}
	headers["Idempotency-Key"] = opts.IdempotencyKey

	resp, err := h.AsUser(opts.User).PostWithHeaders(
		pathf("/v1/holds/%s/purchase", hold.HoldID),
		map[string]any{"payment_method": "pm_test_card"},
		headers,
	)
	require.NoError(h.t, err)
	return resp
}

// Purchase completes a purchase and requires success.
func (h *Harness) Purchase(hold Hold, opts PurchaseOpts) Booking {
	h.t.Helper()

	resp := h.TryPurchase(hold, opts)
	require.Equal(h.t, http.StatusOK, resp.Status, "purchase: %s", resp.Body)

	var b Booking
	require.NoError(h.t, resp.DecodeInto(&b))
	return b
}

func (h *Harness) GetBooking(bookingID, user string) Booking {
	h.t.Helper()
	if user == "" {
		user = DefaultBuyer
	}
	resp, err := h.AsUser(user).Get(pathf("/v1/bookings/%s", bookingID), nil)
	require.NoError(h.t, err)
	require.Equal(h.t, http.StatusOK, resp.Status, "get booking: %s", resp.Body)

	var b Booking
	require.NoError(h.t, resp.DecodeInto(&b))
	return b
}

// Payment provider behaviour modes, used to drive the sad paths deterministically.
const (
	PaymentSucceed = "succeed"
	PaymentDecline = "decline"
	PaymentError   = "error"
)

// SetPaymentBehaviour scripts the mock payment provider.
func (h *Harness) SetPaymentBehaviour(mode string) {
	h.t.Helper()
	resp, err := h.Post("/_test/payments/behaviour", map[string]any{"mode": mode})
	require.NoError(h.t, err)
	require.Equal(h.t, http.StatusOK, resp.Status, "set payment behaviour: %s", resp.Body)
}

type ReapResult struct {
	TicketsReleased int64 `json:"tickets_released"`
	HoldsExpired    int64 `json:"holds_expired"`
}

// ReapHolds runs the expiry reaper. Cron does not fire locally, so tests invoke it
// directly through testsupport (D10).
func (h *Harness) ReapHolds() ReapResult {
	h.t.Helper()
	resp, err := h.Post("/_test/reap-holds", nil)
	require.NoError(h.t, err)
	require.Equal(h.t, http.StatusOK, resp.Status, "reap: %s", resp.Body)

	var out ReapResult
	require.NoError(h.t, resp.DecodeInto(&out))
	return out
}

// PaymentSummary reports provider call counts, so a test can prove a charge happened
// exactly once across a retry.
type PaymentSummary struct {
	Charges int `json:"charges"`
	Refunds int `json:"refunds"`
}

func (h *Harness) PaymentSummary() PaymentSummary {
	h.t.Helper()
	resp, err := h.Get("/_test/payments/summary", nil)
	require.NoError(h.t, err)
	require.Equal(h.t, http.StatusOK, resp.Status, "payment summary: %s", resp.Body)

	var out PaymentSummary
	require.NoError(h.t, resp.DecodeInto(&out))
	return out
}

// IntegrityReport is a direct audit of an event's inventory, computed from the
// authoritative tables rather than from any counter or cache.
type IntegrityReport struct {
	EventID   int64 `json:"event_id"`
	Total     int64 `json:"total"`
	Available int64 `json:"available"`
	Held      int64 `json:"held"`
	Sold      int64 `json:"sold"`

	DuplicateSeats             int64 `json:"duplicate_seats"`
	SoldWithoutBooking         int64 `json:"sold_without_booking"`
	HeldWithoutHold            int64 `json:"held_without_hold"`
	ConfirmedBookings          int64 `json:"confirmed_bookings"`
	TicketsInConfirmedBookings int64 `json:"tickets_in_confirmed_bookings"`
	UnresolvedCompensations    int64 `json:"unresolved_compensations"`

	Consistent bool `json:"consistent"`
}

// Integrity audits an event's inventory.
func (h *Harness) Integrity(eventID int64) IntegrityReport {
	h.t.Helper()
	resp, err := h.Get(pathf("/_test/integrity/%d", eventID), nil)
	require.NoError(h.t, err)
	require.Equal(h.t, http.StatusOK, resp.Status, "integrity: %s", resp.Body)

	var out IntegrityReport
	require.NoError(h.t, resp.DecodeInto(&out))
	return out
}

// SeedSellableEvent is the standard setup for booking tests: an on-sale event with a
// small, fully available seat map.
func (h *Harness) SeedSellableEvent(t *testing.T, rows, seatsPerRow int, priceCents int64) int64 {
	t.Helper()
	return h.SeedPublishedEvent(
		VenueOpts{Sections: []SectionSpec{{Name: "FLOOR", Rows: rows, SeatsPerRow: seatsPerRow}}},
		EventOpts{Tiers: []TierSpec{{Section: "FLOOR", PriceCents: priceCents}}},
	)
}
