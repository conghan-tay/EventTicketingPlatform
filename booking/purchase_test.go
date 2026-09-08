package booking

import (
	"context"
	"testing"
	"time"

	"encore.dev/beta/errs"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"encore.app/internal/clock"
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

// The money-losing case: the provider took the money but the seats were gone by the
// time we tried to convert them.
//
// This is unreachable sequentially — something has to steal the seats mid-payment —
// so it uses the duringPaymentHook seam. It is the single most important sad path in
// the system: getting it wrong means a customer is charged for nothing.
func TestChargeSucceedsButSeatsLostLandsInCompensating(t *testing.T) {
	ctx := context.Background()
	freshDB(ctx, t)
	testClock := frozenClock()
	svc := &Service{clock: testClock}

	eventID, tickets := seedEvent(ctx, t, svc, 2)
	seat := tickets[0]

	hold, err := svc.createHold(ctx, "unlucky-buyer", eventID,
		&CreateHoldRequest{TicketIDs: []int64{seat}})
	require.NoError(t, err)

	// While the payment is in flight, forcibly reassign the seat to a different hold,
	// exactly as a takeover after expiry would.
	thiefHoldID := uuid.New()
	duringPaymentHook = func() {
		_, err := db.Exec(ctx, `
			INSERT INTO holds (hold_id, event_id, user_id, hold_token, status, expires_at, created_at)
			VALUES ($1, $2, 'thief', 'thief-token', 'ACTIVE', $3, $4)
		`, thiefHoldID, eventID, testClock.Now().Add(HoldTTL), testClock.Now())
		require.NoError(t, err)

		_, err = db.Exec(ctx, `
			UPDATE tickets SET hold_id = $2 WHERE ticket_id = $1
		`, seat, thiefHoldID)
		require.NoError(t, err)
	}
	t.Cleanup(func() { duringPaymentHook = nil })

	_, err = svc.purchase(ctx, "unlucky-buyer", hold.HoldID, &PurchaseRequest{
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

	// Critically: the seat was NOT sold to the buyer who could not get it.
	ticketState, holdRef := ticketStatus(ctx, t, seat)
	assert.NotEqual(t, "SOLD", ticketState,
		"a seat must never be sold to a buyer whose conversion failed")
	require.NotNil(t, holdRef)
	assert.Equal(t, thiefHoldID.String(), *holdRef)
}

// A booking is recorded before the refund is attempted, so a refund failure leaves a
// visible obligation rather than losing track of the customer's money.
func TestRefundFailureLeavesBookingCompensating(t *testing.T) {
	ctx := context.Background()
	freshDB(ctx, t)
	testClock := frozenClock()
	svc := &Service{clock: testClock}

	eventID, tickets := seedEvent(ctx, t, svc, 2)
	seat := tickets[0]

	hold, err := svc.createHold(ctx, "refund-victim", eventID,
		&CreateHoldRequest{TicketIDs: []int64{seat}})
	require.NoError(t, err)

	setProviderMode(t, "refund_fails")

	thiefHoldID := uuid.New()
	duringPaymentHook = func() {
		_, err := db.Exec(ctx, `
			INSERT INTO holds (hold_id, event_id, user_id, hold_token, status, expires_at, created_at)
			VALUES ($1, $2, 'thief', 'thief-token', 'ACTIVE', $3, $4)
		`, thiefHoldID, eventID, testClock.Now().Add(HoldTTL), testClock.Now())
		require.NoError(t, err)
		_, err = db.Exec(ctx, `UPDATE tickets SET hold_id = $2 WHERE ticket_id = $1`, seat, thiefHoldID)
		require.NoError(t, err)
	}
	t.Cleanup(func() { duringPaymentHook = nil })

	_, err = svc.purchase(ctx, "refund-victim", hold.HoldID, &PurchaseRequest{
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
	svc := &Service{clock: clock.Real{}}

	eventID, tickets := seedEvent(ctx, t, svc, 1)
	hold, err := svc.createHold(ctx, "buyer", eventID,
		&CreateHoldRequest{TicketIDs: []int64{tickets[0]}})
	require.NoError(t, err)

	_, err = svc.purchase(ctx, "buyer", hold.HoldID, &PurchaseRequest{
		PaymentMethod:  "pm_card",
		HoldToken:      uuid.NewString(), // not the granted token
		IdempotencyKey: uuid.NewString(),
	})
	require.Error(t, err)
	assert.Equal(t, errs.PermissionDenied, errs.Code(err))

	// The seat is untouched and still held.
	status, _ := ticketStatus(ctx, t, tickets[0])
	assert.Equal(t, "HELD", status)
}

// Purchasing a hold whose seats have all been taken must not charge the card.
func TestPurchaseRefusesWhenHoldCoversNoSeats(t *testing.T) {
	ctx := context.Background()
	freshDB(ctx, t)
	svc := &Service{clock: clock.Real{}}

	eventID, tickets := seedEvent(ctx, t, svc, 1)
	hold, err := svc.createHold(ctx, "buyer", eventID,
		&CreateHoldRequest{TicketIDs: []int64{tickets[0]}})
	require.NoError(t, err)

	// Strip the hold off the ticket, simulating a completed takeover.
	_, err = db.Exec(ctx, `
		UPDATE tickets SET status = 'AVAILABLE', hold_id = NULL, hold_expires_at = NULL
		 WHERE ticket_id = $1
	`, tickets[0])
	require.NoError(t, err)

	_, err = svc.purchase(ctx, "buyer", hold.HoldID, &PurchaseRequest{
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
	testClock := frozenClock()
	svc := &Service{clock: testClock}

	eventID, tickets := seedEvent(ctx, t, svc, 1)
	hold, err := svc.createHold(ctx, "buyer", eventID,
		&CreateHoldRequest{TicketIDs: []int64{tickets[0]}})
	require.NoError(t, err)

	// Leave less than PaymentBudget on the lease.
	testClock.Advance(HoldTTL - 10*time.Second)

	lease, err := svc.prepareLease(ctx, "buyer", mustParseUUID(t, hold.HoldID), hold.HoldToken)
	require.NoError(t, err)

	assert.True(t, lease.ExpiresAt.Sub(testClock.Now()) >= PaymentBudget,
		"the lease must be extended to cover the payment budget, got %s remaining",
		lease.ExpiresAt.Sub(testClock.Now()))

	// The extension is reflected on the ticket rows, not just in memory.
	var expiresAt time.Time
	require.NoError(t, db.QueryRow(ctx,
		`SELECT hold_expires_at FROM tickets WHERE ticket_id = $1`, tickets[0]).Scan(&expiresAt))
	assert.True(t, expiresAt.Sub(testClock.Now()) >= PaymentBudget)
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
