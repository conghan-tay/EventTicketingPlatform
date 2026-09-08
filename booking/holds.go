package booking

import (
	"context"
	"errors"
	"slices"
	"time"

	"encore.dev/beta/errs"
	"encore.dev/storage/sqldb"
	"encore.dev/storage/sqldb/sqlerr"
	"github.com/google/uuid"
)

type CreateHoldRequest struct {
	// TicketIDs are the specific inventory rows to claim, as returned by the seat map.
	TicketIDs []int64 `json:"ticket_ids"`
	// IdempotencyKey makes a retried request return the original hold instead of
	// claiming a second set of seats.
	IdempotencyKey string `header:"Idempotency-Key"`
}

type HeldSeat struct {
	TicketID   int64  `json:"ticket_id"`
	Section    string `json:"section"`
	RowLabel   string `json:"row_label"`
	SeatNumber int    `json:"seat_number"`
	PriceCents int64  `json:"price_cents"`
}

type Hold struct {
	HoldID  string `json:"hold_id"`
	EventID int64  `json:"event_id"`
	// HoldToken fences the eventual sale. It is returned on creation and on an
	// idempotent replay, because the client cannot complete a purchase without it.
	HoldToken        string     `json:"hold_token"`
	Status           string     `json:"status"`
	ExpiresAt        time.Time  `json:"expires_at"`
	SecondsRemaining float64    `json:"seconds_remaining"`
	Seats            []HeldSeat `json:"seats"`
	TotalCents       int64      `json:"total_cents"`
}

// CreateHold claims specific seats for the caller.
//
//encore:api auth method=POST path=/v1/events/:eventID/holds
func (s *Service) CreateHold(ctx context.Context, eventID int64, req *CreateHoldRequest) (*Hold, error) {
	userID, err := callerID()
	if err != nil {
		return nil, err
	}
	return s.createHold(ctx, userID, eventID, req)
}

func (s *Service) createHold(ctx context.Context, userID string, eventID int64, req *CreateHoldRequest) (*Hold, error) {
	ticketIDs, err := validateTicketIDs(req.TicketIDs)
	if err != nil {
		return nil, err
	}

	// An idempotent replay must return the original hold without touching inventory.
	if req.IdempotencyKey != "" {
		existing, err := s.findHoldByIdempotencyKey(ctx, userID, req.IdempotencyKey)
		if err != nil {
			return nil, err
		}
		if existing != nil {
			return existing, nil
		}
	}

	now := s.clock.Now()
	holdID := uuid.New()
	token := uuid.NewString()
	expiresAt := now.Add(HoldTTL)

	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if err := s.assertOnSale(ctx, tx, eventID, now); err != nil {
		return nil, err
	}

	if err := s.assertHoldQuota(ctx, tx, userID, now); err != nil {
		return nil, err
	}

	// Lock the requested rows in a deterministic order.
	//
	// ORDER BY ticket_id is load-bearing. Two users claiming overlapping seat sets in
	// opposite orders would otherwise deadlock; a consistent lock order turns that
	// into one winner and one clean conflict.
	//
	// A ticket is claimable if it is AVAILABLE, or if it is HELD by a lease that has
	// already lapsed. The latter matters because expiry is a batch process: a seat
	// whose hold expired a second ago must be sellable immediately, not whenever the
	// reaper next runs.
	rows, err := tx.Query(ctx, `
		SELECT ticket_id, status = 'AVAILABLE'
		            OR (status = 'HELD' AND hold_expires_at <= $3) AS claimable,
		       hold_id
		  FROM tickets
		 WHERE event_id = $1 AND ticket_id = ANY($2)
		 ORDER BY ticket_id
		   FOR UPDATE
	`, eventID, ticketIDs, now)
	if err != nil {
		return nil, err
	}

	var (
		found         []int64
		lost          []int64
		displacedHold []uuid.UUID
	)
	for rows.Next() {
		var (
			id        int64
			claimable bool
			holdRef   *uuid.UUID
		)
		if err := rows.Scan(&id, &claimable, &holdRef); err != nil {
			rows.Close()
			return nil, err
		}
		found = append(found, id)
		if !claimable {
			lost = append(lost, id)
		} else if holdRef != nil {
			displacedHold = append(displacedHold, *holdRef)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// A ticket id that does not belong to this event is a client error, and is
	// reported as not-found rather than as a conflict — nothing is contended.
	if len(found) != len(ticketIDs) {
		return nil, notFound("one or more ticket ids do not exist for this event")
	}
	if len(lost) > 0 {
		// All-or-nothing: returning here rolls back, so the claimable seats in this
		// request are left untouched for whoever asks next.
		//
		// A rising conflict rate is the signal that an onsale is genuinely contended,
		// and is what would justify turning on the waiting room (D12).
		mClaimConflicts.Increment()
		return nil, seatUnavailable(lost)
	}

	// Taking over a lapsed lease invalidates it. Without this the previous holder's
	// record would still read ACTIVE while owning no seats.
	if len(displacedHold) > 0 {
		if _, err := tx.Exec(ctx, `
			UPDATE holds SET status = 'EXPIRED'
			 WHERE hold_id = ANY($1) AND status = 'ACTIVE'
		`, displacedHold); err != nil {
			return nil, err
		}
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO holds (hold_id, event_id, user_id, hold_token, status,
		                   expires_at, created_at, idempotency_key)
		VALUES ($1, $2, $3, $4, 'ACTIVE', $5, $6, $7)
	`, holdID, eventID, userID, token, expiresAt, now, nullIfEmpty(req.IdempotencyKey)); err != nil {
		// Two concurrent replays of the same key: the unique index rejects the loser,
		// which then reads back the winner's hold.
		if isUniqueViolation(err) && req.IdempotencyKey != "" {
			existing, findErr := s.findHoldByIdempotencyKey(ctx, userID, req.IdempotencyKey)
			if findErr == nil && existing != nil {
				return existing, nil
			}
		}
		return nil, err
	}

	// The conditional write. The predicate repeats the claimable test so the update
	// itself is guarded, not just the earlier read.
	res, err := tx.Exec(ctx, `
		UPDATE tickets
		   SET status = 'HELD', hold_id = $3, hold_expires_at = $4, booking_id = NULL
		 WHERE event_id = $1
		   AND ticket_id = ANY($2)
		   AND (status = 'AVAILABLE' OR (status = 'HELD' AND hold_expires_at <= $5))
	`, eventID, ticketIDs, holdID, expiresAt, now)
	if err != nil {
		return nil, err
	}
	if res.RowsAffected() != int64(len(ticketIDs)) {
		// Defensive: the rows are locked, so this should be unreachable. Rolling back
		// is the only safe response — a partial claim would strand inventory.
		return nil, seatUnavailable(ticketIDs)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	mHoldsCreated.Increment()

	return s.loadHold(ctx, holdID.String(), userID)
}

// GetHold returns the caller's hold.
//
//encore:api auth method=GET path=/v1/holds/:holdID
func (s *Service) GetHold(ctx context.Context, holdID string) (*Hold, error) {
	userID, err := callerID()
	if err != nil {
		return nil, err
	}
	return s.loadHold(ctx, holdID, userID)
}

type ReleaseResponse struct {
	HoldID          string `json:"hold_id"`
	TicketsReleased int64  `json:"tickets_released"`
}

// ReleaseHold gives seats back before the lease expires.
//
//encore:api auth method=DELETE path=/v1/holds/:holdID
func (s *Service) ReleaseHold(ctx context.Context, holdID string) (*ReleaseResponse, error) {
	userID, err := callerID()
	if err != nil {
		return nil, err
	}
	return s.releaseHold(ctx, userID, holdID)
}

func (s *Service) releaseHold(ctx context.Context, userID, holdID string) (*ReleaseResponse, error) {
	id, err := uuid.Parse(holdID)
	if err != nil {
		return nil, notFound("hold not found")
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var status string
	err = tx.QueryRow(ctx, `
		SELECT status::text FROM holds
		 WHERE hold_id = $1 AND user_id = $2
		   FOR UPDATE
	`, id, userID).Scan(&status)
	if errors.Is(err, sqldb.ErrNoRows) {
		return nil, notFound("hold not found")
	} else if err != nil {
		return nil, err
	}

	if status != "ACTIVE" {
		return nil, &errs.Error{
			Code:    errs.FailedPrecondition,
			Message: "hold is not active (status " + status + ")",
		}
	}

	// Fenced on hold_id and status='HELD'. A SOLD ticket can never be released here,
	// and a seat already taken over by another claim is not ours to give back.
	res, err := tx.Exec(ctx, `
		UPDATE tickets
		   SET status = 'AVAILABLE', hold_id = NULL, hold_expires_at = NULL
		 WHERE hold_id = $1 AND status = 'HELD'
	`, id)
	if err != nil {
		return nil, err
	}

	if _, err := tx.Exec(ctx,
		`UPDATE holds SET status = 'RELEASED' WHERE hold_id = $1`, id); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	mHoldsReleased.Increment()
	return &ReleaseResponse{HoldID: holdID, TicketsReleased: res.RowsAffected()}, nil
}

// loadHold reads a hold with its seats. Status is reported as EXPIRED once the lease
// has lapsed, even if the stored row still says ACTIVE, because the reaper is a batch
// process and a client must never see a lapsed lease described as active.
func (s *Service) loadHold(ctx context.Context, holdID, userID string) (*Hold, error) {
	id, err := uuid.Parse(holdID)
	if err != nil {
		return nil, notFound("hold not found")
	}

	var (
		h      Hold
		status string
	)
	err = db.QueryRow(ctx, `
		SELECT hold_id::text, event_id, hold_token, status::text, expires_at
		  FROM holds
		 WHERE hold_id = $1 AND user_id = $2
	`, id, userID).Scan(&h.HoldID, &h.EventID, &h.HoldToken, &status, &h.ExpiresAt)
	if errors.Is(err, sqldb.ErrNoRows) {
		return nil, notFound("hold not found")
	} else if err != nil {
		return nil, err
	}

	now := s.clock.Now()
	if status == "ACTIVE" && !h.ExpiresAt.After(now) {
		status = "EXPIRED"
	}
	h.Status = status
	if remaining := h.ExpiresAt.Sub(now).Seconds(); remaining > 0 {
		h.SecondsRemaining = remaining
	}

	// Seats are read through tickets.hold_id, so a seat taken over by another claim
	// correctly disappears from this hold.
	rows, err := db.Query(ctx, `
		SELECT t.ticket_id, s.section, s.row_label, s.seat_number, pt.price_cents
		  FROM tickets t
		  JOIN seats s        ON s.seat_id = t.seat_id
		  JOIN price_tiers pt ON pt.price_tier_id = t.price_tier_id
		 WHERE t.hold_id = $1
		 ORDER BY t.ticket_id
	`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	h.Seats = []HeldSeat{}
	for rows.Next() {
		var seat HeldSeat
		if err := rows.Scan(&seat.TicketID, &seat.Section, &seat.RowLabel,
			&seat.SeatNumber, &seat.PriceCents); err != nil {
			return nil, err
		}
		h.TotalCents += seat.PriceCents
		h.Seats = append(h.Seats, seat)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &h, nil
}

func (s *Service) findHoldByIdempotencyKey(ctx context.Context, userID, key string) (*Hold, error) {
	var holdID string
	err := db.QueryRow(ctx, `
		SELECT hold_id::text FROM holds
		 WHERE user_id = $1 AND idempotency_key = $2
	`, userID, key).Scan(&holdID)
	if errors.Is(err, sqldb.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	return s.loadHold(ctx, holdID, userID)
}

// assertOnSale rejects a claim against an event that is not selling.
//
// The onsale window is compared against the injected clock, not SQL now(), or a test
// could never exercise the pre-onsale path deterministically.
func (s *Service) assertOnSale(ctx context.Context, tx *sqldb.Tx, eventID int64, now time.Time) error {
	var (
		status   string
		onsaleAt time.Time
	)
	err := tx.QueryRow(ctx, `
		SELECT status::text, onsale_at FROM events WHERE event_id = $1
	`, eventID).Scan(&status, &onsaleAt)
	if errors.Is(err, sqldb.ErrNoRows) {
		return notFound("event not found")
	} else if err != nil {
		return err
	}

	if status != "ON_SALE" {
		return &errs.Error{
			Code:    errs.FailedPrecondition,
			Message: "event is not on sale (status " + status + ")",
		}
	}
	if onsaleAt.After(now) {
		return &errs.Error{
			Code:    errs.FailedPrecondition,
			Message: "tickets are not yet on sale for this event",
		}
	}
	return nil
}

// assertHoldQuota enforces the per-user active-hold limit. Lapsed holds do not count,
// so the quota throttles hoarding rather than punishing an abandoned checkout.
func (s *Service) assertHoldQuota(ctx context.Context, tx *sqldb.Tx, userID string, now time.Time) error {
	var active int64
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM holds
		 WHERE user_id = $1 AND status = 'ACTIVE' AND expires_at > $2
	`, userID, now).Scan(&active); err != nil {
		return err
	}
	if active >= MaxActiveHoldsPerUser {
		return &errs.Error{
			Code:    errs.ResourceExhausted,
			Message: "too many active holds; complete or release an existing hold first",
		}
	}
	return nil
}

func validateTicketIDs(ids []int64) ([]int64, error) {
	if len(ids) == 0 {
		return nil, badRequest("ticket_ids must not be empty")
	}
	if len(ids) > MaxTicketsPerHold {
		return nil, badRequest("at most 8 tickets may be held in one request")
	}

	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if id <= 0 {
			return nil, badRequest("ticket ids must be positive")
		}
		if _, dup := seen[id]; dup {
			return nil, badRequest("ticket_ids must not contain duplicates")
		}
		seen[id] = struct{}{}
	}

	// Sorting here means the lock order in SQL matches the caller's set regardless of
	// the order they sent, which keeps the deadlock-avoidance property independent of
	// client behaviour.
	out := slices.Clone(ids)
	slices.Sort(out)
	return out, nil
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// isUniqueViolation reports whether err is a unique-constraint violation, using
// Encore's typed SQL error codes rather than matching on a raw SQLSTATE string.
func isUniqueViolation(err error) bool {
	return sqldb.ErrCode(err) == sqlerr.UniqueViolation
}
