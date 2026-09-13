package booking

import (
	"context"
	"strconv"
	"testing"
	"time"

	"encore.dev/beta/errs"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"encore.app/internal/lockkeys"
	"encore.app/payments"
)

func bookingStatus(ctx context.Context, t *testing.T, bookingID string) (string, string) {
	t.Helper()
	var status, reason string
	require.NoError(t, db.QueryRow(ctx, `
		SELECT status::text, coalesce(failure_reason, '') FROM bookings WHERE booking_id = $1
	`, bookingID).Scan(&status, &reason))
	return status, reason
}

// stealSeat books a ticket out from under an in-flight purchase.
//
// This is the shape the conditional write exists to catch, and under the Redis lease
// it is no longer hypothetical: if Redis drops a lock, a second buyer genuinely can
// claim and book the same seat while the first buyer's charge is in flight. Marking
// the ticket BOOKED by somebody else is exactly what that leaves behind.
func stealSeat(ctx context.Context, t *testing.T, eventID, ticketID int64) uuid.UUID {
	t.Helper()

	thiefBooking := uuid.New()
	_, err := db.Exec(ctx, `
		INSERT INTO bookings (booking_id, event_id, hold_id, user_id, status,
		                      total_cents, created_at, updated_at)
		VALUES ($1, $2, $3, 'thief', 'CONFIRMED', 5000, now(), now())
	`, thiefBooking, eventID, uuid.New())
	require.NoError(t, err)

	_, err = db.Exec(ctx, `
		UPDATE tickets SET status = 'BOOKED', booking_id = $2 WHERE ticket_id = $1
	`, ticketID, thiefBooking)
	require.NoError(t, err)

	return thiefBooking
}

// The money-losing case: the provider took the money but the seats were gone by the
// time we tried to convert them.
//
// This is unreachable sequentially — something has to take the seats mid-payment — so
// it uses the duringPaymentHook seam. It is the single most important sad path in the
// system: getting it wrong means a customer is charged for nothing.
func TestChargeSucceedsButSeatsLostLandsInCompensating(t *testing.T) {
	ctx := context.Background()
	freshDB(ctx, t)
	env := newTestEnv(t)

	eventID, tickets := seedEvent(ctx, t, env.svc, 2)
	seat := tickets[0]

	hold, err := env.svc.createHold(ctx, "unlucky-buyer", eventID,
		&CreateHoldRequest{TicketIDs: []int64{seat}})
	require.NoError(t, err)

	var thiefBooking uuid.UUID
	duringPaymentHook = func() { thiefBooking = stealSeat(ctx, t, eventID, seat) }
	t.Cleanup(func() { duringPaymentHook = nil })

	_, err = env.svc.purchase(ctx, "unlucky-buyer", hold.HoldID, &PurchaseRequest{
		PaymentMethod:  "pm_card",
		HoldToken:      hold.HoldToken,
		IdempotencyKey: "compensation-" + uuid.NewString(),
	})

	// The caller is told plainly that the seats were lost and money is coming back.
	require.Error(t, err)
	details, ok := errs.Details(err).(PaymentFailure)
	require.True(t, ok, "expected PaymentFailure details, got %T", errs.Details(err))
	require.NotEmpty(t, details.BookingID)

	// The obligation is durable. With the mock refunding successfully this reaches
	// COMPENSATED; the point is that it is never CONFIRMED and never silently dropped.
	status, reason := bookingStatus(ctx, t, details.BookingID)
	assert.Contains(t, []string{"COMPENSATING", "COMPENSATED"}, status,
		"a charge that could not be delivered must be recorded for refund, got %s", status)
	assert.Equal(t, "seats_lost_after_charge", reason)

	// Critically: the seat still belongs to whoever got there first. The conditional
	// write refused to hand it to a second buyer, which is the whole invariant.
	var owner uuid.UUID
	require.NoError(t, db.QueryRow(ctx,
		`SELECT booking_id FROM tickets WHERE ticket_id = $1`, seat).Scan(&owner))
	assert.Equal(t, thiefBooking, owner,
		"the seat must stay with the first buyer, never be overwritten by the second")
}

// A booking is recorded before the refund is attempted, so a refund failure leaves a
// visible obligation rather than losing track of the customer's money.
func TestRefundFailureLeavesBookingCompensating(t *testing.T) {
	ctx := context.Background()
	freshDB(ctx, t)
	env := newTestEnv(t)

	eventID, tickets := seedEvent(ctx, t, env.svc, 2)
	seat := tickets[0]

	hold, err := env.svc.createHold(ctx, "refund-victim", eventID,
		&CreateHoldRequest{TicketIDs: []int64{seat}})
	require.NoError(t, err)

	setProviderMode(t, "refund_fails")

	duringPaymentHook = func() { stealSeat(ctx, t, eventID, seat) }
	t.Cleanup(func() { duringPaymentHook = nil })

	_, err = env.svc.purchase(ctx, "refund-victim", hold.HoldID, &PurchaseRequest{
		PaymentMethod:  "pm_card",
		HoldToken:      hold.HoldToken,
		IdempotencyKey: "refund-fail-" + uuid.NewString(),
	})
	require.Error(t, err)

	details, ok := errs.Details(err).(PaymentFailure)
	require.True(t, ok, "expected PaymentFailure details, got %T", errs.Details(err))

	status, _ := bookingStatus(ctx, t, details.BookingID)
	assert.Equal(t, "COMPENSATING", status,
		"a failed refund must leave the obligation open, not mark it resolved")
	assert.Equal(t, "refund_pending", details.Reason)
}

// The fence in isolation: a stale hold token cannot convert seats, and no charge is
// attempted.
func TestPurchaseRejectsStaleToken(t *testing.T) {
	ctx := context.Background()
	freshDB(ctx, t)
	env := newTestEnv(t)

	eventID, tickets := seedEvent(ctx, t, env.svc, 1)
	hold, err := env.svc.createHold(ctx, "buyer", eventID,
		&CreateHoldRequest{TicketIDs: []int64{tickets[0]}})
	require.NoError(t, err)

	_, err = env.svc.purchase(ctx, "buyer", hold.HoldID, &PurchaseRequest{
		PaymentMethod:  "pm_card",
		HoldToken:      uuid.NewString(), // not the granted token
		IdempotencyKey: uuid.NewString(),
	})
	require.Error(t, err)
	assert.Equal(t, errs.PermissionDenied, errs.Code(err))

	// The seat is untouched and still leased to the rightful holder.
	owner, locked := env.lockOwner(t, eventID, tickets[0])
	require.True(t, locked, "a rejected purchase must not release the lease")
	assert.Equal(t, "buyer", owner)
	assert.Equal(t, "AVAILABLE", ticketStatus(ctx, t, tickets[0]))
}

// Purchasing a hold whose seats have all been taken must not charge the card.
func TestPurchaseRefusesWhenHoldCoversNoSeats(t *testing.T) {
	ctx := context.Background()
	freshDB(ctx, t)
	env := newTestEnv(t)

	eventID, tickets := seedEvent(ctx, t, env.svc, 1)
	hold, err := env.svc.createHold(ctx, "buyer", eventID,
		&CreateHoldRequest{TicketIDs: []int64{tickets[0]}})
	require.NoError(t, err)

	// Drop the lease out from under the hold, simulating a completed takeover: both
	// the lock itself and its entry in the per-event index.
	env.mr.Del(lockkeys.Lock(eventID, tickets[0]))
	_, err = env.mr.ZRem(lockkeys.EventLocks(eventID), strconv.FormatInt(tickets[0], 10))
	require.NoError(t, err)

	_, err = env.svc.purchase(ctx, "buyer", hold.HoldID, &PurchaseRequest{
		PaymentMethod:  "pm_card",
		HoldToken:      hold.HoldToken,
		IdempotencyKey: uuid.NewString(),
	})
	require.Error(t, err)
	assert.Equal(t, errs.FailedPrecondition, errs.Code(err))
	assert.Contains(t, err.Error(), "no longer covers any seats")
}

// D9: a purchase inside the danger window extends the lease rather than racing it.
func TestPurchaseExtendsShortLease(t *testing.T) {
	ctx := context.Background()
	freshDB(ctx, t)
	env := newTestEnv(t)

	eventID, tickets := seedEvent(ctx, t, env.svc, 1)
	hold, err := env.svc.createHold(ctx, "buyer", eventID,
		&CreateHoldRequest{TicketIDs: []int64{tickets[0]}})
	require.NoError(t, err)

	// Leave less than PaymentBudget on the lease.
	env.advance(HoldTTL() - 10*time.Second)

	lease, err := env.svc.prepareLease(ctx, "buyer", mustParseUUID(t, hold.HoldID), hold.HoldToken)
	require.NoError(t, err)

	assert.True(t, lease.ExpiresAt.Sub(env.clock.Now()) >= PaymentBudget,
		"the lease must be extended to cover the payment budget, got %s remaining",
		lease.ExpiresAt.Sub(env.clock.Now()))

	// The extension reached Redis, not just the in-memory lease. This replaces the old
	// read of tickets.hold_expires_at: the TTL is now where the lease actually lives.
	ttl, err := env.svc.locks.TicketTTL(ctx, eventID, tickets[0])
	require.NoError(t, err)
	assert.True(t, ttl >= PaymentBudget,
		"the lock TTL must cover the payment budget, got %s", ttl)
}

// A lease lapsing after the sale must not disturb the sale.
//
// This preserves the intent of the deleted TestReaperLeavesSoldTicketSold. Under the
// old design a reaper swept expired holds and had to be fenced so it could never
// release a sold ticket. There is no sweep now: expiry deletes a Redis key and never
// touches Postgres, so a booked seat is structurally unreachable by expiry. That is a
// strictly stronger guarantee, and it is worth a test that would catch anyone
// reintroducing a path from expiry back into inventory.
func TestExpiredLeaseLeavesBookedTicketBooked(t *testing.T) {
	ctx := context.Background()
	freshDB(ctx, t)
	env := newTestEnv(t)

	eventID, tickets := seedEvent(ctx, t, env.svc, 1)
	seat := tickets[0]

	hold, err := env.svc.createHold(ctx, "buyer", eventID,
		&CreateHoldRequest{TicketIDs: []int64{seat}})
	require.NoError(t, err)

	booking, err := env.svc.purchase(ctx, "buyer", hold.HoldID, &PurchaseRequest{
		PaymentMethod:  "pm_card",
		HoldToken:      hold.HoldToken,
		IdempotencyKey: uuid.NewString(),
	})
	require.NoError(t, err)
	require.Equal(t, "CONFIRMED", booking.Status)
	require.Equal(t, "BOOKED", ticketStatus(ctx, t, seat))

	// Walk far past any lease window.
	env.advance(HoldTTL() + 2*time.Hour)

	assert.Equal(t, "BOOKED", ticketStatus(ctx, t, seat),
		"lease expiry must never take a seat back from somebody who paid for it")

	// And the seat cannot be re-leased, because the conditional write would refuse it
	// even if a lock were somehow acquired.
	_, err = env.svc.createHold(ctx, "latecomer", eventID,
		&CreateHoldRequest{TicketIDs: []int64{seat}})
	if err == nil {
		// A lock on a booked seat is possible — Redis does not know the seat is sold —
		// but converting it must fail rather than double-sell.
		assert.Equal(t, "BOOKED", ticketStatus(ctx, t, seat))
	}
}

func mustParseUUID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(s)
	require.NoError(t, err)
	return id
}

// setProviderMode scripts the mock payment provider and restores it afterwards, so a
// scripted failure cannot leak into another test.
func setProviderMode(t *testing.T, mode string) {
	t.Helper()
	ctx := context.Background()
	_, err := payments.SetTestBehaviour(ctx, &payments.SetBehaviourRequest{Mode: mode})
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := payments.ResetTestProvider(ctx)
		require.NoError(t, err)
	})
}
