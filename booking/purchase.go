package booking

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"encore.dev/beta/errs"
	"encore.dev/rlog"
	"encore.dev/storage/sqldb"
	"github.com/google/uuid"

	"encore.app/payments"
)

// PaymentBudget is how long a charge is allowed to take.
//
// A purchase refuses to start unless the hold has at least this much life left, and
// extends the lease if it does not (D9). This converts the money-losing race — charge
// succeeds, hold expires, seats gone — from something that happens routinely into
// something that needs a genuinely unlucky coincidence.
const PaymentBudget = 60 * time.Second

// duringPaymentHook runs between the charge and the conversion.
//
// It is nil in every real environment and is set only by tests. It exists because the
// charge-succeeded-but-seats-lost interleaving is the one failure in this system that
// loses money, and it is unreachable sequentially: something has to steal the seats
// while the payment is in flight. A test seam is the honest way to prove the
// compensation path works, rather than leaving it untested and hoping.
var duringPaymentHook func()

type PurchaseRequest struct {
	PaymentMethod string `json:"payment_method"`
	// HoldToken fences the sale. Possession of the hold id is not enough: the token
	// proves the caller still holds the lease it was granted.
	HoldToken string `header:"X-Hold-Token"`
	// IdempotencyKey makes a retried purchase return the original booking rather than
	// charging the card a second time.
	IdempotencyKey string `header:"Idempotency-Key"`
}

type Booking struct {
	BookingID     string `json:"booking_id"`
	EventID       int64  `json:"event_id"`
	HoldID        string `json:"hold_id"`
	Status        string `json:"status"`
	TotalCents    int64  `json:"total_cents"`
	PaymentID     string `json:"payment_id"`
	FailureReason string `json:"failure_reason"`
}

// Purchase converts a hold into a confirmed booking.
//
//encore:api auth method=POST path=/v1/holds/:holdID/purchase
func (s *Service) Purchase(ctx context.Context, holdID string, req *PurchaseRequest) (*Booking, error) {
	userID, err := callerID()
	if err != nil {
		return nil, err
	}
	return s.purchase(ctx, userID, holdID, req)
}

// purchase runs the saga: charge, then convert. Two local commits with a
// compensation, which is why this is a saga and not a workflow engine (D7).
func (s *Service) purchase(ctx context.Context, userID, holdID string, req *PurchaseRequest) (*Booking, error) {
	id, err := uuid.Parse(holdID)
	if err != nil {
		return nil, notFound("hold not found")
	}
	if req.HoldToken == "" {
		return nil, badRequest("X-Hold-Token header is required")
	}
	if req.IdempotencyKey == "" {
		return nil, badRequest("Idempotency-Key header is required")
	}

	// A replayed purchase must never charge twice.
	if existing, err := s.findBookingByIdempotencyKey(ctx, userID, req.IdempotencyKey); err != nil {
		return nil, err
	} else if existing != nil {
		return existing, nil
	}

	// Step 1: validate the lease and price it, extending if the payment would not
	// comfortably fit inside the remaining TTL.
	lease, err := s.prepareLease(ctx, userID, id, req.HoldToken)
	if err != nil {
		return nil, err
	}

	// Step 2: charge. Deliberately outside every transaction — holding row locks
	// across an external call is how a hot event grinds to a halt.
	payment, chargeErr := payments.Charge(ctx, &payments.ChargeRequest{
		IdempotencyKey: "booking:" + req.IdempotencyKey,
		UserID:         userID,
		AmountCents:    lease.TotalCents,
		PaymentMethod:  req.PaymentMethod,
		Description:    fmt.Sprintf("Event %d — %d ticket(s)", lease.EventID, len(lease.TicketIDs)),
	})

	if chargeErr != nil {
		// Ambiguous or unavailable. No money is known to have moved and no seats were
		// converted, so the safe action is to leave the hold alone and let it expire
		// or be retried. Releasing here would be wrong: the charge may yet land.
		return nil, chargeErr
	}

	if payment.State != "COMPLETED" {
		// A definite decline. Release the seats immediately so they go back on sale
		// rather than waiting out the TTL.
		return s.recordDeclined(ctx, userID, lease, payment, req.IdempotencyKey)
	}

	if duringPaymentHook != nil {
		duringPaymentHook()
	}

	// Step 3: convert HELD -> SOLD, fenced by the hold id, in the same transaction as
	// the booking insert and the outbox row.
	booking, converted, err := s.convertToSold(ctx, userID, lease, payment, req.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	if converted {
		return booking, nil
	}

	// The money-losing case: the charge succeeded but the seats were gone. Compensate.
	return s.compensate(ctx, userID, lease, payment, req.IdempotencyKey)
}

// lease is a validated, priced hold ready to be paid for.
type lease struct {
	HoldID     uuid.UUID
	EventID    int64
	TicketIDs  []int64
	TotalCents int64
	ExpiresAt  time.Time
	// rec is the underlying Redis lease, carried so the conversion and any
	// compensation can address the same locks.
	rec *HoldRecord
}

// prepareLease verifies ownership and the fence token, prices the seats whose locks
// this hold still owns, and applies the D9 TTL guarantee.
func (s *Service) prepareLease(ctx context.Context, userID string, holdID uuid.UUID, token string) (*lease, error) {
	now := s.clock.Now()

	rec, err := s.locks.LoadHold(ctx, holdID.String())
	if err != nil {
		return nil, err
	}
	// A lease whose TTL lapsed is simply gone from Redis; there is no tombstone to
	// distinguish "expired" from "never existed".
	if rec == nil {
		return nil, &errs.Error{Code: errs.FailedPrecondition, Message: "hold has expired"}
	}
	if rec.UserID != userID {
		return nil, notFound("hold not found")
	}

	// Compare the fence before anything else. A caller with a stale token is not
	// entitled to this lease even if the hold is otherwise fine.
	if rec.Token != token {
		return nil, &errs.Error{Code: errs.PermissionDenied, Message: "invalid hold token"}
	}
	if rec.Status != HoldActive {
		return nil, &errs.Error{
			Code:    errs.FailedPrecondition,
			Message: "hold is no longer active (status " + rec.Status + ")",
		}
	}
	if !rec.expiresAtTime().After(now) {
		return nil, &errs.Error{Code: errs.FailedPrecondition, Message: "hold has expired"}
	}

	// Which locks are genuinely still ours. This replaces the old read through
	// tickets.hold_id and serves the same purpose: a seat taken over after a lapse
	// must not be charged for.
	locked, err := s.locks.LockedTickets(ctx, rec.EventID, now)
	if err != nil {
		return nil, err
	}

	l := lease{
		HoldID:    holdID,
		EventID:   rec.EventID,
		ExpiresAt: rec.expiresAtTime(),
		rec:       rec,
	}
	for _, id := range rec.TicketIDs {
		if locked[id] {
			l.TicketIDs = append(l.TicketIDs, id)
		}
	}
	if len(l.TicketIDs) == 0 {
		return nil, &errs.Error{
			Code:    errs.FailedPrecondition,
			Message: "hold no longer covers any seats",
		}
	}

	// D9: do not start a charge that could outlive the lease.
	//
	// The extension is a compare-and-PEXPIRE: it only touches locks whose value is
	// still this user, so a seat lost between the read and the extend is not silently
	// re-acquired. Fewer extensions than seats means the set changed underneath us.
	if l.ExpiresAt.Sub(now) < PaymentBudget {
		newExpiry := now.Add(PaymentBudget)

		extendRec := *rec
		extendRec.TicketIDs = l.TicketIDs

		extended, err := s.locks.Extend(ctx, &extendRec, newExpiry, PaymentBudget)
		if err != nil {
			return nil, err
		}
		if extended != int64(len(l.TicketIDs)) {
			return nil, &errs.Error{
				Code:    errs.Aborted,
				Message: "hold changed while preparing payment; please retry",
			}
		}
		l.ExpiresAt = newExpiry
		l.rec = &extendRec
	}

	prices, err := s.ticketPrices(ctx, l.EventID, l.TicketIDs)
	if err != nil {
		return nil, err
	}
	if len(prices) != len(l.TicketIDs) {
		return nil, &errs.Error{
			Code:    errs.FailedPrecondition,
			Message: "hold no longer covers any seats",
		}
	}
	for _, cents := range prices {
		l.TotalCents += cents
	}

	return &l, nil
}

// ticketPrices returns price_cents per ticket for a set of ids.
func (s *Service) ticketPrices(ctx context.Context, eventID int64, ticketIDs []int64) (map[int64]int64, error) {
	rows, err := db.Query(ctx, `
		SELECT t.ticket_id, pt.price_cents
		  FROM tickets t
		  JOIN price_tiers pt ON pt.price_tier_id = t.price_tier_id
		 WHERE t.event_id = $1 AND t.ticket_id = ANY($2)
	`, eventID, ticketIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[int64]int64, len(ticketIDs))
	for rows.Next() {
		var id, cents int64
		if err := rows.Scan(&id, &cents); err != nil {
			return nil, err
		}
		out[id] = cents
	}
	return out, rows.Err()
}

// convertToSold performs the fenced conversion. The bool reports whether the seats
// were still ours; false means the charge needs compensating.
func (s *Service) convertToSold(
	ctx context.Context, userID string, l *lease, payment *payments.Payment, idemKey string,
) (*Booking, bool, error) {
	now := s.clock.Now()
	bookingID := uuid.New()
	paymentID, err := uuid.Parse(payment.PaymentID)
	if err != nil {
		return nil, false, err
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()

	// The booking row must exist before tickets can reference it (tickets_booking_fk).
	if _, err := tx.Exec(ctx, `
		INSERT INTO bookings (booking_id, event_id, hold_id, user_id, status,
		                      total_cents, payment_id, idempotency_key, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'CONFIRMED', $5, $6, $7, $8, $8)
	`, bookingID, l.EventID, l.HoldID, userID, l.TotalCents, paymentID, idemKey, now); err != nil {
		if isUniqueViolation(err) {
			// Concurrent replay of the same purchase; the winner's booking stands.
			existing, findErr := s.findBookingByIdempotencyKey(ctx, userID, idemKey)
			if findErr == nil && existing != nil {
				return existing, true, nil
			}
		}
		return nil, false, err
	}

	// THE INVARIANT LIVES HERE.
	//
	// `status = 'AVAILABLE'` is what makes an oversell unrepresentable, and it is
	// deliberately a property of Postgres rather than of the Redis lock. Redis can
	// lose a lock to an eviction or a failover and hand the same seat to two buyers;
	// both then arrive here, and the second one converts zero rows and is compensated.
	// Without this predicate that second buyer would overwrite the first — the same
	// seat, sold twice, with neither buyer refunded.
	res, err := tx.Exec(ctx, `
		UPDATE tickets
		   SET status = 'BOOKED', booking_id = $1
		 WHERE event_id = $2
		   AND ticket_id = ANY($3)
		   AND status = 'AVAILABLE'
	`, bookingID, l.EventID, l.TicketIDs)
	if err != nil {
		return nil, false, err
	}
	if res.RowsAffected() != int64(len(l.TicketIDs)) {
		// Roll back rather than sell a subset. The caller compensates the charge.
		rlog.Warn("seats lost during payment; converting to compensation",
			"hold_id", l.HoldID.String(), "expected", len(l.TicketIDs), "converted", res.RowsAffected())
		return nil, false, nil
	}

	// Outbox row in the same transaction as the business state, so a crash cannot
	// leave a confirmed booking with no notification, nor a notification for a
	// booking that rolled back.
	if err := insertOutbox(ctx, tx, now, "booking.confirmed", map[string]any{
		"booking_id":  bookingID.String(),
		"event_id":    l.EventID,
		"user_id":     userID,
		"ticket_ids":  l.TicketIDs,
		"total_cents": l.TotalCents,
	}); err != nil {
		return nil, false, err
	}

	if err := tx.Commit(); err != nil {
		return nil, false, err
	}

	// The sale is durable now, so the locks have no further job. Dropping them early
	// is cosmetic rather than load-bearing — they would expire on their own, and the
	// seat map reads BOOKED from Postgres regardless — but leaving a sold seat looking
	// held for the rest of the TTL is needless confusion.
	l.rec.Status = HoldConverted
	if _, err := s.locks.Release(ctx, l.rec); err != nil {
		rlog.Warn("could not release locks after a confirmed sale; they will expire",
			"hold_id", l.HoldID.String(), "err", err)
	}

	mHoldsConverted.Increment()

	return &Booking{
		BookingID:  bookingID.String(),
		EventID:    l.EventID,
		HoldID:     l.HoldID.String(),
		Status:     "CONFIRMED",
		TotalCents: l.TotalCents,
		PaymentID:  payment.PaymentID,
	}, true, nil
}

// recordDeclined releases the seats after a definite decline and records the failure.
func (s *Service) recordDeclined(
	ctx context.Context, userID string, l *lease, payment *payments.Payment, idemKey string,
) (*Booking, error) {
	now := s.clock.Now()
	bookingID := uuid.New()
	paymentID, err := uuid.Parse(payment.PaymentID)
	if err != nil {
		return nil, err
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	reason := payment.FailureReason
	if reason == "" {
		reason = "payment_declined"
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO bookings (booking_id, event_id, hold_id, user_id, status,
		                      total_cents, payment_id, idempotency_key,
		                      failure_reason, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'FAILED', $5, $6, $7, $8, $9, $9)
	`, bookingID, l.EventID, l.HoldID, userID, l.TotalCents, paymentID, idemKey, reason, now); err != nil {
		if isUniqueViolation(err) {
			existing, findErr := s.findBookingByIdempotencyKey(ctx, userID, idemKey)
			if findErr == nil && existing != nil {
				return existing, nil
			}
		}
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	// A decline is a definite "no", so the seats go straight back on sale rather than
	// waiting out the TTL. The release is a compare-and-delete: a seat already taken
	// over by somebody else is not ours to free, and a BOOKED ticket holds no lock at
	// all, so neither can be released here.
	//
	// This runs after the commit rather than inside it. Redis is not transactional
	// with Postgres, and of the two orderings this is the safe one: a failure here
	// leaves seats locked until the TTL lapses, whereas releasing first and then
	// failing to record the booking would put seats back on sale while a charge may
	// still be in play.
	l.rec.Status = HoldReleased
	if _, err := s.locks.Release(ctx, l.rec); err != nil {
		rlog.Warn("could not release locks after a decline; they will expire",
			"hold_id", l.HoldID.String(), "err", err)
	}

	mPaymentsFailed.Increment()

	// The booking is a real, readable record of the failure, so the caller gets a
	// business outcome rather than a bare error.
	return nil, &errs.Error{
		Code:    errs.Aborted,
		Message: "payment was declined: " + reason,
		Details: PaymentFailure{BookingID: bookingID.String(), Reason: reason},
	}
}

// PaymentFailure lets a client link a declined attempt to its booking record.
type PaymentFailure struct {
	BookingID string `json:"booking_id"`
	Reason    string `json:"reason"`
}

func (PaymentFailure) ErrDetails() {}

// compensate handles the case that must never silently succeed: money was taken but
// the seats could not be delivered.
//
// The booking is recorded as COMPENSATING *before* the refund is attempted, so the
// obligation is durable even if the refund call or this process fails. Compensation
// is fallible and is treated as such.
func (s *Service) compensate(
	ctx context.Context, userID string, l *lease, payment *payments.Payment, idemKey string,
) (*Booking, error) {
	now := s.clock.Now()
	bookingID := uuid.New()
	paymentID, perr := uuid.Parse(payment.PaymentID)
	if perr != nil {
		return nil, perr
	}

	if _, err := db.Exec(ctx, `
		INSERT INTO bookings (booking_id, event_id, hold_id, user_id, status,
		                      total_cents, payment_id, idempotency_key,
		                      failure_reason, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'COMPENSATING', $5, $6, $7,
		        'seats_lost_after_charge', $8, $8)
	`, bookingID, l.EventID, l.HoldID, userID, l.TotalCents, paymentID, idemKey, now); err != nil {
		if isUniqueViolation(err) {
			existing, findErr := s.findBookingByIdempotencyKey(ctx, userID, idemKey)
			if findErr == nil && existing != nil {
				return existing, nil
			}
		}
		return nil, err
	}

	mCompensations.Increment()
	rlog.Error("charged but could not deliver seats; refunding",
		"booking_id", bookingID.String(), "payment_id", payment.PaymentID,
		"hold_id", l.HoldID.String(), "amount_cents", l.TotalCents)

	if _, err := payments.Refund(ctx, &payments.RefundRequest{
		PaymentID: payment.PaymentID,
		Reason:    "seats no longer available after charge",
	}); err != nil {
		// Leave the booking in COMPENSATING. The obligation is recorded and visible
		// to the reconcile job and to operators; marking it resolved would be a lie.
		rlog.Error("refund failed; booking left COMPENSATING for reconciliation",
			"booking_id", bookingID.String(), "err", err)
		return nil, &errs.Error{
			Code:    errs.Internal,
			Message: "your seats were taken during payment and a refund is being processed",
			Details: PaymentFailure{BookingID: bookingID.String(), Reason: "refund_pending"},
		}
	}

	if _, err := db.Exec(ctx, `
		UPDATE bookings SET status = 'COMPENSATED', updated_at = $2 WHERE booking_id = $1
	`, bookingID, s.clock.Now()); err != nil {
		return nil, err
	}

	return nil, &errs.Error{
		Code:    errs.Aborted,
		Message: "your seats were taken during payment and your card has been refunded",
		Details: PaymentFailure{BookingID: bookingID.String(), Reason: "seats_lost_after_charge"},
	}
}

// GetBooking returns the caller's booking.
//
//encore:api auth method=GET path=/v1/bookings/:bookingID
func (s *Service) GetBooking(ctx context.Context, bookingID string) (*Booking, error) {
	userID, err := callerID()
	if err != nil {
		return nil, err
	}

	id, err := uuid.Parse(bookingID)
	if err != nil {
		return nil, notFound("booking not found")
	}

	b, err := s.loadBooking(ctx, userID, "booking_id = $2", id)
	if err != nil {
		return nil, err
	}
	if b == nil {
		return nil, notFound("booking not found")
	}
	return b, nil
}

type BookingList struct {
	Bookings []Booking `json:"bookings"`
}

// ListBookings returns the caller's bookings, newest first.
//
//encore:api auth method=GET path=/v1/bookings
func (s *Service) ListBookings(ctx context.Context) (*BookingList, error) {
	userID, err := callerID()
	if err != nil {
		return nil, err
	}

	rows, err := db.Query(ctx, `
		SELECT booking_id::text, event_id, hold_id::text, status::text,
		       total_cents, coalesce(payment_id::text, ''), coalesce(failure_reason, '')
		  FROM bookings
		 WHERE user_id = $1
		 ORDER BY created_at DESC, booking_id
		 LIMIT 100
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := &BookingList{Bookings: []Booking{}}
	for rows.Next() {
		var b Booking
		if err := rows.Scan(&b.BookingID, &b.EventID, &b.HoldID, &b.Status,
			&b.TotalCents, &b.PaymentID, &b.FailureReason); err != nil {
			return nil, err
		}
		out.Bookings = append(out.Bookings, b)
	}
	return out, rows.Err()
}

func (s *Service) findBookingByIdempotencyKey(ctx context.Context, userID, key string) (*Booking, error) {
	return s.loadBooking(ctx, userID, "idempotency_key = $2", key)
}

// loadBooking reads one booking scoped to the owner. Returns nil, nil when absent, so
// callers can distinguish "no such booking" from a query error.
func (s *Service) loadBooking(ctx context.Context, userID, predicate string, arg any) (*Booking, error) {
	var b Booking
	// predicate is a compile-time constant chosen by the caller, never user input.
	err := db.QueryRow(ctx, `
		SELECT booking_id::text, event_id, hold_id::text, status::text,
		       total_cents, coalesce(payment_id::text, ''), coalesce(failure_reason, '')
		  FROM bookings
		 WHERE user_id = $1 AND `+predicate,
		userID, arg).Scan(&b.BookingID, &b.EventID, &b.HoldID, &b.Status,
		&b.TotalCents, &b.PaymentID, &b.FailureReason)
	if errors.Is(err, sqldb.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	return &b, nil
}

func insertOutbox(ctx context.Context, tx *sqldb.Tx, now time.Time, topic string, payload map[string]any) error {
	buf, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO outbox (topic, payload, created_at) VALUES ($1, $2, $3)
	`, topic, buf, now)
	return err
}
