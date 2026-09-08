// Package booking owns ticket inventory: tickets, holds and bookings.
//
// It is the only writer of inventory in the system, and it never serves a read from
// a cache or a replica. Correctness here rests on one invariant:
//
//	a ticket has at most one owner, and a SOLD ticket is never released.
//
// That invariant is enforced by conditional writes on the individual ticket row,
// backed by UNIQUE (event_id, seat_id) and CHECK constraints in the schema as the
// final boundary. See docs/system-design.md §7.1.
//
// Each exported API resolves the caller from the verified auth context and then
// delegates to an unexported method taking the user id explicitly. That split is
// deliberate: Encore provides no way to inject auth data into a unit test, and the
// concurrency behaviour of the claim path is the single most important thing to test.
package booking

import (
	"time"

	"encore.dev/beta/auth"
	"encore.dev/beta/errs"
	"encore.dev/storage/sqldb"

	"encore.app/internal/clock"
)

var db = sqldb.Named("ticketing")

const (
	// HoldTTL is how long a claim survives without payment. Long enough for a real
	// checkout, short enough that abandoned carts do not strand inventory.
	HoldTTL = 10 * time.Minute

	// MaxTicketsPerHold caps a single request. Real ticketing systems cap party size
	// to blunt bulk buying.
	MaxTicketsPerHold = 8

	// MaxActiveHoldsPerUser is the anti-hoarding quota: a script cannot lock up
	// inventory by opening unlimited concurrent holds it never intends to buy.
	MaxActiveHoldsPerUser = 3
)

//encore:service
type Service struct {
	clock clock.Clock
}

func initService() (*Service, error) {
	return &Service{clock: clock.Default()}, nil
}

func callerID() (string, error) {
	uid, ok := auth.UserID()
	if !ok {
		return "", &errs.Error{Code: errs.Unauthenticated, Message: "authentication required"}
	}
	return string(uid), nil
}

func badRequest(msg string) error {
	return &errs.Error{Code: errs.InvalidArgument, Message: msg}
}

// notFound is used for both genuinely missing and not-owned resources, so a caller
// cannot probe for the existence of another user's hold or booking.
func notFound(msg string) error {
	return &errs.Error{Code: errs.NotFound, Message: msg}
}

// SeatConflict carries the specific tickets that were lost, so a client can re-render
// the seat map and let the user choose again rather than showing a bare error.
type SeatConflict struct {
	LostTicketIDs []int64 `json:"lost_ticket_ids"`
}

func (SeatConflict) ErrDetails() {}

func seatUnavailable(lost []int64) error {
	return &errs.Error{
		Code:    errs.Aborted,
		Message: "one or more requested seats are no longer available",
		Details: SeatConflict{LostTicketIDs: lost},
	}
}
