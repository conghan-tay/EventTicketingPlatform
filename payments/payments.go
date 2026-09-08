// Package payments wraps an external payment provider behind an idempotent effect
// record.
//
// The important property here is that a charge is an *irreversible external effect*.
// The process can die after the provider has taken the money but before we record it,
// so the record has three states and IN_PROGRESS is treated as genuinely ambiguous —
// never as "not yet done". Recovery reconciles with the provider; it never blindly
// retries a charge.
package payments

import (
	"context"
	"errors"
	"time"

	"encore.dev/beta/errs"
	"encore.dev/rlog"
	"encore.dev/storage/sqldb"
	"encore.dev/storage/sqldb/sqlerr"
	"github.com/google/uuid"

	"encore.app/internal/clock"
)

var db = sqldb.Named("ticketing")

//encore:service
type Service struct {
	provider Provider
	clock    clock.Clock
}

func initService() (*Service, error) {
	return &Service{provider: defaultProvider(), clock: clock.Default()}, nil
}

type ChargeRequest struct {
	// IdempotencyKey is derived from the booking attempt. The same key must never
	// result in two charges.
	IdempotencyKey string `json:"idempotency_key"`
	UserID         string `json:"user_id"`
	AmountCents    int64  `json:"amount_cents"`
	PaymentMethod  string `json:"payment_method"`
	// Description is passed to the provider for the customer's statement.
	Description string `json:"description"`
}

type Payment struct {
	PaymentID     string `json:"payment_id"`
	State         string `json:"state"`
	AmountCents   int64  `json:"amount_cents"`
	ProviderRef   string `json:"provider_ref"`
	FailureReason string `json:"failure_reason"`
}

// Charge takes money, at most once per idempotency key.
//
// Private: only the booking service may charge, and it does so as part of the
// purchase saga.
//
//encore:api private method=POST path=/internal/payments/charge
func (s *Service) Charge(ctx context.Context, req *ChargeRequest) (*Payment, error) {
	if req.IdempotencyKey == "" {
		return nil, &errs.Error{Code: errs.InvalidArgument, Message: "idempotency_key is required"}
	}
	if req.AmountCents < 0 {
		return nil, &errs.Error{Code: errs.InvalidArgument, Message: "amount_cents must not be negative"}
	}

	// Claim the key first. Inserting the record before calling the provider is what
	// makes the ambiguous case recoverable: if we crash mid-charge, an IN_PROGRESS row
	// exists to reconcile against, rather than a charge nobody knows about.
	paymentID := uuid.New()
	now := s.clock.Now()

	_, err := db.Exec(ctx, `
		INSERT INTO payments (payment_id, idempotency_key, user_id, amount_cents,
		                      state, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'IN_PROGRESS', $5, $5)
	`, paymentID, req.IdempotencyKey, req.UserID, req.AmountCents, now)

	if err != nil {
		if sqldb.ErrCode(err) != sqlerr.UniqueViolation {
			return nil, err
		}
		// The key has been seen before. Return the recorded outcome rather than
		// charging again — this is the whole point of the record.
		return s.replay(ctx, req)
	}

	result, chargeErr := s.provider.Charge(ctx, ProviderCharge{
		IdempotencyKey: req.IdempotencyKey,
		AmountCents:    req.AmountCents,
		PaymentMethod:  req.PaymentMethod,
		Description:    req.Description,
	})

	if chargeErr != nil {
		// A transport-level failure is ambiguous: the provider may or may not have
		// charged. The row stays IN_PROGRESS for the reconcile job to resolve, and we
		// surface it as unavailable rather than a clean decline.
		rlog.Error("payment provider call failed; leaving payment ambiguous",
			"payment_id", paymentID.String(), "err", chargeErr)
		return nil, &errs.Error{
			Code:    errs.Unavailable,
			Message: "payment provider unavailable; payment outcome is unresolved",
		}
	}

	state := "COMPLETED"
	if !result.Approved {
		state = "FAILED"
	}

	// A decline is a definite outcome, so recording it is safe.
	if _, err := db.Exec(ctx, `
		UPDATE payments
		   SET state = $2::payment_state, provider_ref = $3, failure_reason = $4, updated_at = $5
		 WHERE payment_id = $1
	`, paymentID, state, nullIfEmpty(result.ProviderRef), nullIfEmpty(result.DeclineReason), s.clock.Now()); err != nil {
		return nil, err
	}

	return &Payment{
		PaymentID:     paymentID.String(),
		State:         state,
		AmountCents:   req.AmountCents,
		ProviderRef:   result.ProviderRef,
		FailureReason: result.DeclineReason,
	}, nil
}

// replay returns the outcome already recorded for an idempotency key.
func (s *Service) replay(ctx context.Context, req *ChargeRequest) (*Payment, error) {
	var (
		p             Payment
		providerRef   *string
		failureReason *string
	)
	err := db.QueryRow(ctx, `
		SELECT payment_id::text, state::text, amount_cents, provider_ref, failure_reason
		  FROM payments WHERE idempotency_key = $1
	`, req.IdempotencyKey).Scan(&p.PaymentID, &p.State, &p.AmountCents, &providerRef, &failureReason)
	if errors.Is(err, sqldb.ErrNoRows) {
		// Lost a race with a concurrent insert that has not committed yet.
		return nil, &errs.Error{
			Code:    errs.Unavailable,
			Message: "a payment with this idempotency key is in flight; retry shortly",
		}
	} else if err != nil {
		return nil, err
	}

	// Reusing a key for a different amount is a caller bug, and silently returning
	// the old charge would hide it.
	if p.AmountCents != req.AmountCents {
		return nil, &errs.Error{
			Code:    errs.InvalidArgument,
			Message: "idempotency key already used for a different amount",
		}
	}

	if providerRef != nil {
		p.ProviderRef = *providerRef
	}
	if failureReason != nil {
		p.FailureReason = *failureReason
	}

	if p.State == "IN_PROGRESS" {
		// Ambiguous, not retryable. Reporting success or failure here would be a
		// guess, and one of those guesses loses money.
		return nil, &errs.Error{
			Code:    errs.Unavailable,
			Message: "payment outcome is unresolved; awaiting reconciliation",
		}
	}
	return &p, nil
}

type RefundRequest struct {
	PaymentID string `json:"payment_id"`
	Reason    string `json:"reason"`
}

type RefundResponse struct {
	PaymentID  string    `json:"payment_id"`
	Refunded   bool      `json:"refunded"`
	RefundedAt time.Time `json:"refunded_at"`
}

// Refund compensates a completed charge.
//
// It is idempotent on payment id: a payment already refunded reports success rather
// than refunding twice.
//
//encore:api private method=POST path=/internal/payments/refund
func (s *Service) Refund(ctx context.Context, req *RefundRequest) (*RefundResponse, error) {
	id, err := uuid.Parse(req.PaymentID)
	if err != nil {
		return nil, &errs.Error{Code: errs.InvalidArgument, Message: "invalid payment_id"}
	}

	var (
		state       string
		providerRef *string
		refundedAt  *time.Time
	)
	err = db.QueryRow(ctx, `
		SELECT state::text, provider_ref, refunded_at FROM payments WHERE payment_id = $1
	`, id).Scan(&state, &providerRef, &refundedAt)
	if errors.Is(err, sqldb.ErrNoRows) {
		return nil, &errs.Error{Code: errs.NotFound, Message: "payment not found"}
	} else if err != nil {
		return nil, err
	}

	if refundedAt != nil {
		return &RefundResponse{PaymentID: req.PaymentID, Refunded: true, RefundedAt: *refundedAt}, nil
	}
	if state != "COMPLETED" || providerRef == nil {
		return nil, &errs.Error{
			Code:    errs.FailedPrecondition,
			Message: "only a completed payment can be refunded (state " + state + ")",
		}
	}

	if err := s.provider.Refund(ctx, ProviderRefund{
		ProviderRef: *providerRef,
		Reason:      req.Reason,
	}); err != nil {
		// Compensation is fallible. Surfacing the failure keeps the booking in
		// COMPENSATING so an operator or the reconcile job can finish the job,
		// rather than marking it resolved when the money is still with us.
		rlog.Error("refund failed; compensation incomplete",
			"payment_id", req.PaymentID, "err", err)
		return nil, &errs.Error{Code: errs.Unavailable, Message: "refund failed"}
	}

	at := s.clock.Now()
	if _, err := db.Exec(ctx, `
		UPDATE payments SET refunded_at = $2, updated_at = $2 WHERE payment_id = $1
	`, id, at); err != nil {
		return nil, err
	}
	return &RefundResponse{PaymentID: req.PaymentID, Refunded: true, RefundedAt: at}, nil
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
