package booking

import (
	"context"

	"encore.dev/metrics"
)

// Observability counters. These are the numbers that tell you whether the contention
// design is working, and the oversell gauge is the one that must never move.
// booking_tickets_reaped is gone with the reaper: Redis expires locks itself, so
// there is no sweep to count. The inventory tied up by abandoned carts is now visible
// as the size of the per-event lock set rather than as a reap rate.
var (
	mClaimConflicts = metrics.NewCounter[uint64]("booking_claim_conflicts", metrics.CounterConfig{})
	mHoldsCreated   = metrics.NewCounter[uint64]("booking_holds_created", metrics.CounterConfig{})
	mHoldsConverted = metrics.NewCounter[uint64]("booking_holds_converted", metrics.CounterConfig{})
	mHoldsReleased  = metrics.NewCounter[uint64]("booking_holds_released", metrics.CounterConfig{})
	mPaymentsFailed = metrics.NewCounter[uint64]("booking_payments_failed", metrics.CounterConfig{})
	mCompensations  = metrics.NewCounter[uint64]("booking_compensations", metrics.CounterConfig{})
)

// Integrity is a direct audit of one event's inventory, computed from the
// authoritative tables rather than from any counter or cache.
//
// This exists because "no oversell" should be checkable, not merely asserted. The
// schema already makes an oversell unrepresentable via UNIQUE (event_id, seat_id),
// so DuplicateSeats should always be zero — which is exactly why measuring it is
// worthwhile: if it is ever non-zero, a constraint has been dropped.
type Integrity struct {
	EventID   int64 `json:"event_id"`
	Total     int64 `json:"total"`
	Available int64 `json:"available"`
	Sold      int64 `json:"sold"`

	// DuplicateSeats counts seats represented by more than one ticket row for this
	// event. Non-zero means the same physical seat could be sold twice.
	//
	// This is the direct oversell check, and it is deliberately unchanged by the move
	// to Redis: it is computed from the authoritative tables and would catch a double
	// sale no matter how the lease was lost.
	DuplicateSeats int64 `json:"duplicate_seats"`
	// SoldWithoutBooking counts booked tickets with no booking attached. Non-zero means
	// somebody's seat has no record of who bought it.
	SoldWithoutBooking int64 `json:"sold_without_booking"`
	// ConfirmedBookings and TicketsInConfirmedBookings must agree with Sold.
	ConfirmedBookings          int64 `json:"confirmed_bookings"`
	TicketsInConfirmedBookings int64 `json:"tickets_in_confirmed_bookings"`
	// UnresolvedCompensations counts bookings stuck mid-refund — money taken with no
	// seats delivered and no refund confirmed.
	UnresolvedCompensations int64 `json:"unresolved_compensations"`

	// Consistent is the single summary an operator or a load test can assert on.
	Consistent bool `json:"consistent"`
}

// CheckIntegrity audits an event's inventory.
//
//encore:api private method=GET path=/internal/booking/integrity/:eventID
func CheckIntegrity(ctx context.Context, eventID int64) (*Integrity, error) {
	out := &Integrity{EventID: eventID}

	if err := db.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE status = 'AVAILABLE'),
		       count(*) FILTER (WHERE status = 'BOOKED'),
		       count(*) FILTER (WHERE status = 'BOOKED' AND booking_id IS NULL)
		  FROM tickets
		 WHERE event_id = $1
	`, eventID).Scan(&out.Total, &out.Available, &out.Sold,
		&out.SoldWithoutBooking); err != nil {
		return nil, err
	}

	// The oversell check: one physical seat must map to at most one ticket row per
	// event. UNIQUE (event_id, seat_id) enforces it; this verifies the enforcement.
	if err := db.QueryRow(ctx, `
		SELECT count(*) FROM (
			SELECT seat_id FROM tickets
			 WHERE event_id = $1
			 GROUP BY seat_id
			HAVING count(*) > 1
		) dupes
	`, eventID).Scan(&out.DuplicateSeats); err != nil {
		return nil, err
	}

	if err := db.QueryRow(ctx, `
		SELECT count(*),
		       coalesce(sum(seat_count), 0)
		  FROM (
			SELECT b.booking_id,
			       (SELECT count(*) FROM tickets t WHERE t.booking_id = b.booking_id) AS seat_count
			  FROM bookings b
			 WHERE b.event_id = $1 AND b.status = 'CONFIRMED'
		  ) confirmed
	`, eventID).Scan(&out.ConfirmedBookings, &out.TicketsInConfirmedBookings); err != nil {
		return nil, err
	}

	if err := db.QueryRow(ctx, `
		SELECT count(*) FROM bookings
		 WHERE event_id = $1 AND status = 'COMPENSATING'
	`, eventID).Scan(&out.UnresolvedCompensations); err != nil {
		return nil, err
	}

	// With HELD gone from the schema, every ticket is either available or booked, so
	// the accounting identity tightens: there is no third bucket for a row to hide in.
	out.Consistent = out.DuplicateSeats == 0 &&
		out.SoldWithoutBooking == 0 &&
		out.UnresolvedCompensations == 0 &&
		out.Sold == out.TicketsInConfirmedBookings &&
		out.Total == out.Available+out.Sold

	return out, nil
}
