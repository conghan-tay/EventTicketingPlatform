package booking

import (
	"context"
	"errors"
	"slices"
	"time"

	"encore.dev/beta/errs"
	"encore.dev/storage/sqldb"
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

// createHold acquires the Redis lease.
//
// There is no database transaction here at all. The claim is one atomic Lua script,
// which is what replaces `SELECT ... ORDER BY ticket_id FOR UPDATE`: a script runs to
// completion without interleaving, so the check-then-set across the whole seat set is
// indivisible and there is no lock ordering to get right.
//
// Postgres is still consulted for whether the event is on sale, because that is
// durable state Redis knows nothing about.
func (s *Service) createHold(ctx context.Context, userID string, eventID int64, req *CreateHoldRequest) (*Hold, error) {
	ticketIDs, err := validateTicketIDs(req.TicketIDs)
	if err != nil {
		return nil, err
	}

	now := s.clock.Now()
	if err := s.assertOnSale(ctx, eventID, now); err != nil {
		return nil, err
	}

	// Every requested ticket must belong to this event and still be sellable. Redis
	// holds no inventory, so without this check a caller could lock ticket ids that do
	// not exist, belong to another event, or were sold long ago.
	if err := s.assertTicketsClaimable(ctx, eventID, ticketIDs); err != nil {
		return nil, err
	}

	// The idempotency key is claimed before any seat is touched.
	//
	// In Postgres this ordering was free: the hold INSERT preceded the ticket UPDATE
	// inside one transaction, so a concurrent replay lost on the unique index and
	// claimed nothing. Redis has no cross-key transaction, so claiming the key first
	// is what stops two identical requests from acquiring two different sets of seats.
	var claimedIdem bool
	if req.IdempotencyKey != "" {
		existing, claimed, err := s.locks.ClaimIdempotency(ctx, userID, req.IdempotencyKey, HoldTTL())
		if errors.Is(err, errIdemInFlight) {
			return nil, &errs.Error{
				Code:    errs.Unavailable,
				Message: "a request with this idempotency key is in flight; retry shortly",
			}
		} else if err != nil {
			return nil, err
		}
		if !claimed {
			// Already resolved: return the original hold, inventory untouched.
			return s.loadHold(ctx, existing, userID)
		}
		claimedIdem = true
	}

	// From here on, any failure must free the idempotency key, or a retry would be
	// permanently answered with a failure that no longer applies.
	releaseIdem := func() {
		if claimedIdem {
			_ = s.locks.ReleaseIdempotency(ctx, userID, req.IdempotencyKey)
		}
	}

	ttl := HoldTTL()
	rec := &HoldRecord{
		HoldID:    uuid.NewString(),
		UserID:    userID,
		EventID:   eventID,
		TicketIDs: ticketIDs,
		Token:     uuid.NewString(),
		ExpiresAt: now.Add(ttl).UnixMilli(),
		Status:    HoldActive,
	}

	result, err := s.locks.Acquire(ctx, rec, now, ttl, MaxActiveHoldsPerUser)
	if err != nil {
		releaseIdem()
		return nil, err
	}

	switch {
	case result.QuotaExceeded:
		releaseIdem()
		return nil, &errs.Error{
			Code:    errs.ResourceExhausted,
			Message: "too many active holds; complete or release an existing hold first",
		}
	case len(result.Lost) > 0:
		releaseIdem()
		// All-or-nothing: nothing was claimed, so the seats in this request that were
		// free remain free for whoever asks next.
		//
		// A rising conflict rate is the signal that an onsale is genuinely contended,
		// and is what would justify turning on the waiting room (D12).
		mClaimConflicts.Increment()
		return nil, seatUnavailable(result.Lost)
	}

	if claimedIdem {
		if err := s.locks.CommitIdempotency(ctx, userID, req.IdempotencyKey, rec.HoldID, ttl); err != nil {
			// The seats are held and the hold is real; only the replay shortcut is
			// missing. Failing the request would be worse than a retry re-claiming.
			return nil, err
		}
	}

	mHoldsCreated.Increment()
	return s.hydrate(ctx, rec, now)
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
	rec, err := s.locks.LoadHold(ctx, holdID)
	if err != nil {
		return nil, err
	}
	if rec == nil || rec.UserID != userID {
		// Not-found for both genuinely missing and not-owned, so a caller cannot probe
		// for the existence of another user's hold.
		return nil, notFound("hold not found")
	}

	if rec.Status != HoldActive {
		return nil, &errs.Error{
			Code:    errs.FailedPrecondition,
			Message: "hold is not active (status " + rec.Status + ")",
		}
	}

	// Compare-and-delete inside the script: only locks this user still owns are freed.
	// A seat already taken over by somebody else is not ours to give back, and a
	// BOOKED ticket has no lock at all, so neither can be released here.
	rec.Status = HoldReleased
	released, err := s.locks.Release(ctx, rec)
	if err != nil {
		return nil, err
	}

	mHoldsReleased.Increment()
	return &ReleaseResponse{HoldID: holdID, TicketsReleased: released}, nil
}

// loadHold reads a hold and prices its seats.
func (s *Service) loadHold(ctx context.Context, holdID, userID string) (*Hold, error) {
	rec, err := s.locks.LoadHold(ctx, holdID)
	if err != nil {
		return nil, err
	}
	if rec == nil || rec.UserID != userID {
		return nil, notFound("hold not found")
	}
	return s.hydrate(ctx, rec, s.clock.Now())
}

// hydrate turns a lease record into the API shape, joining Postgres for seat detail
// and pricing.
//
// Seats are reported only while the lock is still live. A lapsed lease owns nothing,
// so it reports an empty seat list and a zero total rather than describing seats that
// are already back on sale.
func (s *Service) hydrate(ctx context.Context, rec *HoldRecord, now time.Time) (*Hold, error) {
	out := &Hold{
		HoldID:    rec.HoldID,
		EventID:   rec.EventID,
		HoldToken: rec.Token,
		ExpiresAt: rec.expiresAtTime(),
		Status:    rec.Status,
		Seats:     []HeldSeat{},
	}

	// A terminal lease owns nothing and has no time left to report.
	if rec.Status != HoldActive {
		return out, nil
	}

	// An ACTIVE record whose lease has lapsed is reported as EXPIRED. The record
	// outlives its locks by design, so the stored status alone would lie here.
	if remaining := out.ExpiresAt.Sub(now).Seconds(); remaining > 0 {
		out.SecondsRemaining = remaining
	} else {
		out.Status = HoldExpired
		return out, nil
	}

	// Which of this hold's tickets are still genuinely locked. A seat taken over by
	// another claim after a lapse must not still appear here.
	locked, err := s.locks.LockedTickets(ctx, rec.EventID, now)
	if err != nil {
		return nil, err
	}

	live := make([]int64, 0, len(rec.TicketIDs))
	for _, id := range rec.TicketIDs {
		if locked[id] {
			live = append(live, id)
		}
	}
	if len(live) == 0 {
		out.Status = HoldExpired
		return out, nil
	}

	seats, total, err := s.priceTickets(ctx, rec.EventID, live)
	if err != nil {
		return nil, err
	}
	out.Seats = seats
	out.TotalCents = total
	return out, nil
}

// priceTickets joins seat detail and tier price for a ticket set.
func (s *Service) priceTickets(ctx context.Context, eventID int64, ticketIDs []int64) ([]HeldSeat, int64, error) {
	rows, err := db.Query(ctx, `
		SELECT t.ticket_id, s.section, s.row_label, s.seat_number, pt.price_cents
		  FROM tickets t
		  JOIN seats s        ON s.seat_id = t.seat_id
		  JOIN price_tiers pt ON pt.price_tier_id = t.price_tier_id
		 WHERE t.event_id = $1 AND t.ticket_id = ANY($2)
		 ORDER BY t.ticket_id
	`, eventID, ticketIDs)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var (
		seats []HeldSeat
		total int64
	)
	for rows.Next() {
		var seat HeldSeat
		if err := rows.Scan(&seat.TicketID, &seat.Section, &seat.RowLabel,
			&seat.SeatNumber, &seat.PriceCents); err != nil {
			return nil, 0, err
		}
		total += seat.PriceCents
		seats = append(seats, seat)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return seats, total, nil
}

// assertOnSale rejects a claim against an event that is not selling.
//
// The onsale window is compared against the injected clock, not SQL now(), or a test
// could never exercise the pre-onsale path deterministically. Note that D11 still
// applies here: it is hold *expiry* that moved to real time, not event scheduling.
func (s *Service) assertOnSale(ctx context.Context, eventID int64, now time.Time) error {
	var (
		status   string
		onsaleAt time.Time
	)
	err := db.QueryRow(ctx, `
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

// assertTicketsClaimable rejects ticket ids that are not part of this event, and those
// already sold.
//
// The sold check matters more than it looks. Redis knows nothing about bookings, so an
// acquire will happily lock a seat that was sold an hour ago; the conditional write
// would then refuse the conversion and the buyer would be compensated for a seat they
// never had a chance at. Catching it here turns that into an immediate, honest 409.
//
// A ticket id that does not belong to this event is a client error, and is reported as
// not-found rather than as a conflict — nothing is contended.
func (s *Service) assertTicketsClaimable(ctx context.Context, eventID int64, ticketIDs []int64) error {
	rows, err := db.Query(ctx, `
		SELECT ticket_id, status = 'BOOKED' FROM tickets
		 WHERE event_id = $1 AND ticket_id = ANY($2)
	`, eventID, ticketIDs)
	if err != nil {
		return err
	}
	defer rows.Close()

	var found int64
	var booked []int64
	for rows.Next() {
		var (
			id       int64
			isBooked bool
		)
		if err := rows.Scan(&id, &isBooked); err != nil {
			return err
		}
		found++
		if isBooked {
			booked = append(booked, id)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	if found != int64(len(ticketIDs)) {
		return notFound("one or more ticket ids do not exist for this event")
	}
	if len(booked) > 0 {
		return seatUnavailable(booked)
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

	// Sorting keeps the lock order stable regardless of the order the client sent.
	// Under the Lua acquire this is no longer load-bearing for deadlock avoidance —
	// the script is indivisible — but a deterministic order keeps the conflict set
	// reported back to the client stable across retries.
	out := slices.Clone(ids)
	slices.Sort(out)
	return out, nil
}
