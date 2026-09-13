// Package booking owns ticket inventory: tickets and bookings, with the seat lease
// held in Redis.
//
// It is the only writer of inventory in the system, and it never serves a read from
// a cache or a replica. Correctness here rests on one invariant:
//
//	a ticket has at most one owner, and a BOOKED ticket is never released.
//
// That invariant is enforced by conditional writes on the individual ticket row,
// backed by UNIQUE (event_id, seat_id) and CHECK constraints in the schema as the
// final boundary. See docs/system-design.md §7.1.
//
// The lease itself lives in Redis (docs/additionalFeatures/RedisLock.md). That makes
// acquisition fast and expiry free, but Redis is not durable: it can lose a lock to an
// eviction or a failover. The lock is therefore an optimistic gate, and the
// authoritative step remains the conditional UPDATE in convertToSold. A lost lock
// costs a compensation, never a seat sold twice.
//
// Each exported API resolves the caller from the verified auth context and then
// delegates to an unexported method taking the user id explicitly. That split is
// deliberate: Encore provides no way to inject auth data into a unit test, and the
// concurrency behaviour of the claim path is the single most important thing to test.
package booking

import (
	"errors"
	"os"
	"time"

	"encore.dev/beta/auth"
	"encore.dev/beta/errs"
	"encore.dev/rlog"
	"encore.dev/storage/sqldb"
	"encore.dev/storage/sqldb/sqlerr"

	"encore.app/internal/clock"
	"encore.app/internal/lockkeys"
)

var db = sqldb.Named("ticketing")

const (
	// defaultHoldTTL is how long a claim survives without payment. Long enough for a
	// real checkout, short enough that abandoned carts do not strand inventory.
	defaultHoldTTL = 10 * time.Minute

	// MaxTicketsPerHold caps a single request. Real ticketing systems cap party size
	// to blunt bulk buying.
	MaxTicketsPerHold = 8

	// MaxActiveHoldsPerUser is the anti-hoarding quota: a script cannot lock up
	// inventory by opening unlimited concurrent holds it never intends to buy.
	MaxActiveHoldsPerUser = 3
)

// HoldTTL is the lease duration, overridable via HOLD_TTL.
//
// It became configurable when expiry moved to Redis. The TTL is now real wall-clock
// time that no injected clock can advance, so an end-to-end test of expiry has to
// genuinely wait — and waiting ten minutes is not a test. Environments that exercise
// expiry set this to a couple of seconds.
func HoldTTL() time.Duration {
	if raw := os.Getenv("HOLD_TTL"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	return defaultHoldTTL
}

//encore:service
type Service struct {
	clock clock.Clock
	locks Locker
}

func initService() (*Service, error) {
	// Fail closed on a missing lease-store address.
	//
	// The address falls back to localhost so `encore run` and the test suite work
	// without ceremony, but that fallback is actively dangerous anywhere else: a
	// deployment with LOCK_REDIS_ADDR unset would start cleanly and then fail on the
	// first booking. A configuration error should surface at boot, not at the till.
	//
	// This mirrors identity.ProductionLike, which refuses the development auth handler
	// outside local and test for the same reason.
	if !clock.IsTimeControllableEnv() && !lockkeys.Configured() {
		return nil, errors.New(
			"LOCK_REDIS_ADDR must be set outside local and test environments: " +
				"the seat lease has no durable home without it")
	}

	rlog.Info("booking service using seat-lease store", "addr", lockkeys.Addr())

	return &Service{
		clock: clock.Default(),
		locks: NewRedisLocker(lockkeys.NewClient()),
	}, nil
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

// isUniqueViolation reports whether err is a unique-constraint violation, using
// Encore's typed SQL error codes rather than matching on a raw SQLSTATE string.
//
// Still needed on the purchase path: bookings keep their idempotency index in
// Postgres, since the bookings table is durable state and not a lease.
func isUniqueViolation(err error) bool {
	return sqldb.ErrCode(err) == sqlerr.UniqueViolation
}
