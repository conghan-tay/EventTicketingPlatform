package organizer

import (
	"context"
	"errors"
	"time"

	"encore.dev/beta/errs"
	"encore.dev/storage/sqldb"
)

type TierSpec struct {
	Section    string `json:"section"`
	PriceCents int64  `json:"price_cents"`
}

type CreateEventRequest struct {
	VenueID     int64      `json:"venue_id"`
	Title       string     `json:"title"`
	Description string     `json:"description"`
	Category    string     `json:"category"`
	StartsAt    time.Time  `json:"starts_at"`
	EndsAt      time.Time  `json:"ends_at"`
	OnsaleAt    time.Time  `json:"onsale_at"`
	Tiers       []TierSpec `json:"tiers"`
}

type Event struct {
	EventID     int64     `json:"event_id"`
	VenueID     int64     `json:"venue_id"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Category    string    `json:"category"`
	Status      string    `json:"status"`
	Version     int       `json:"version"`
	StartsAt    time.Time `json:"starts_at"`
	EndsAt      time.Time `json:"ends_at"`
	OnsaleAt    time.Time `json:"onsale_at"`
}

// CreateEvent creates a DRAFT event with its price tiers. No inventory exists until
// the event is published.
//
//encore:api auth method=POST path=/v1/events
func (s *Service) CreateEvent(ctx context.Context, req *CreateEventRequest) (*Event, error) {
	organizerID, err := callerID()
	if err != nil {
		return nil, err
	}

	if req.VenueID <= 0 {
		return nil, badRequest("venue_id is required")
	}
	if trimmed(req.Title) == "" {
		return nil, badRequest("title is required")
	}
	if trimmed(req.Category) == "" {
		return nil, badRequest("category is required")
	}
	if req.StartsAt.IsZero() || req.EndsAt.IsZero() || req.OnsaleAt.IsZero() {
		return nil, badRequest("starts_at, ends_at and onsale_at are required")
	}
	if !req.EndsAt.After(req.StartsAt) {
		return nil, badRequest("ends_at must be after starts_at")
	}
	if req.OnsaleAt.After(req.StartsAt) {
		return nil, badRequest("onsale_at must not be after starts_at")
	}
	if len(req.Tiers) == 0 {
		return nil, badRequest("at least one price tier is required")
	}

	seenSection := make(map[string]struct{}, len(req.Tiers))
	for _, tier := range req.Tiers {
		if trimmed(tier.Section) == "" {
			return nil, badRequest("tier section is required")
		}
		if tier.PriceCents < 0 {
			return nil, badRequest("tier price_cents must not be negative")
		}
		if _, dup := seenSection[tier.Section]; dup {
			return nil, badRequest("duplicate tier for section " + tier.Section)
		}
		seenSection[tier.Section] = struct{}{}
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var venueExists bool
	if err := tx.QueryRow(ctx,
		`SELECT true FROM venues WHERE venue_id = $1`, req.VenueID).Scan(&venueExists); err != nil {
		if errors.Is(err, sqldb.ErrNoRows) {
			return nil, &errs.Error{Code: errs.NotFound, Message: "venue not found"}
		}
		return nil, err
	}

	// Reject a tier naming a section the venue does not have. Without this check the
	// mismatch would only surface at publish time, as seats with no price.
	for section := range seenSection {
		var sectionExists bool
		err := tx.QueryRow(ctx, `
			SELECT true FROM seats WHERE venue_id = $1 AND section = $2 LIMIT 1
		`, req.VenueID, section).Scan(&sectionExists)
		if errors.Is(err, sqldb.ErrNoRows) {
			return nil, badRequest("venue has no section named " + section)
		} else if err != nil {
			return nil, err
		}
	}

	now := s.clock.Now()
	var ev Event
	err = tx.QueryRow(ctx, `
		INSERT INTO events (venue_id, organizer_id, title, description, category,
		                    status, starts_at, ends_at, onsale_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, 'DRAFT', $6, $7, $8, $9, $9)
		RETURNING event_id, venue_id, title, description, category, status::text,
		          version, starts_at, ends_at, onsale_at
	`, req.VenueID, organizerID, trimmed(req.Title), req.Description, trimmed(req.Category),
		req.StartsAt, req.EndsAt, req.OnsaleAt, now).
		Scan(&ev.EventID, &ev.VenueID, &ev.Title, &ev.Description, &ev.Category,
			&ev.Status, &ev.Version, &ev.StartsAt, &ev.EndsAt, &ev.OnsaleAt)
	if err != nil {
		return nil, err
	}

	sections := make([]string, len(req.Tiers))
	prices := make([]int64, len(req.Tiers))
	for i, tier := range req.Tiers {
		sections[i] = trimmed(tier.Section)
		prices[i] = tier.PriceCents
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO price_tiers (event_id, section, price_cents)
		SELECT $1, t.section, t.price_cents
		  FROM unnest($2::text[], $3::bigint[]) AS t(section, price_cents)
	`, ev.EventID, sections, prices); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &ev, nil
}

type PublishResponse struct {
	EventID        int64  `json:"event_id"`
	Status         string `json:"status"`
	TicketsCreated int64  `json:"tickets_created"`
}

// PublishEvent moves an event to ON_SALE and materialises one ticket per venue seat.
//
// Both happen in one transaction, which is the reason events and tickets share a
// database (D15): a partially published event would either sell seats that do not
// exist or hide seats that do.
//
//encore:api auth method=POST path=/v1/events/:eventID/publish
func (s *Service) PublishEvent(ctx context.Context, eventID int64) (*PublishResponse, error) {
	organizerID, err := callerID()
	if err != nil {
		return nil, err
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Lock the event row so two concurrent publish calls cannot both materialise
	// inventory. The status check below then rejects the loser.
	var status string
	var venueID int64
	err = tx.QueryRow(ctx, `
		SELECT status::text, venue_id
		  FROM events
		 WHERE event_id = $1 AND organizer_id = $2
		   FOR UPDATE
	`, eventID, organizerID).Scan(&status, &venueID)
	if errors.Is(err, sqldb.ErrNoRows) {
		// Deliberately NotFound rather than PermissionDenied: a non-owner should not
		// be able to discover that an event id exists.
		return nil, &errs.Error{Code: errs.NotFound, Message: "event not found"}
	} else if err != nil {
		return nil, err
	}

	if status != "DRAFT" {
		return nil, &errs.Error{
			Code:    errs.AlreadyExists,
			Message: "event is already published (status " + status + ")",
		}
	}

	// Every venue section must be priced, or publishing would create unpriceable
	// seats. Checked before any insert so publish is all-or-nothing.
	var unpricedSection string
	err = tx.QueryRow(ctx, `
		SELECT DISTINCT s.section
		  FROM seats s
		 WHERE s.venue_id = $1
		   AND NOT EXISTS (
		       SELECT 1 FROM price_tiers pt
		        WHERE pt.event_id = $2 AND pt.section = s.section
		   )
		 LIMIT 1
	`, venueID, eventID).Scan(&unpricedSection)
	if err == nil {
		return nil, badRequest("venue section " + unpricedSection + " has no price tier")
	} else if !errors.Is(err, sqldb.ErrNoRows) {
		return nil, err
	}

	var seatCount int64
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM seats WHERE venue_id = $1`, venueID).Scan(&seatCount); err != nil {
		return nil, err
	}
	if seatCount == 0 {
		return nil, badRequest("venue has no seats; add seats before publishing")
	}

	// One INSERT ... SELECT rather than a row-at-a-time loop: a large venue is
	// 100k seats, and this keeps it to a single statement.
	//
	// ON CONFLICT DO NOTHING makes the statement idempotent against the
	// UNIQUE (event_id, seat_id) constraint, so a retry cannot duplicate inventory
	// even if the status check were somehow bypassed.
	var created int64
	err = tx.QueryRow(ctx, `
		WITH inserted AS (
			INSERT INTO tickets (event_id, seat_id, price_tier_id, status)
			SELECT $1, s.seat_id, pt.price_tier_id, 'AVAILABLE'
			  FROM seats s
			  JOIN price_tiers pt
			    ON pt.event_id = $1 AND pt.section = s.section
			 WHERE s.venue_id = $2
			ON CONFLICT (event_id, seat_id) DO NOTHING
			RETURNING 1
		)
		SELECT count(*) FROM inserted
	`, eventID, venueID).Scan(&created)
	if err != nil {
		return nil, err
	}

	if created != seatCount {
		// Defensive: the unpriced-section check above should make this unreachable.
		// Returning an error rolls the transaction back rather than publishing an
		// event with incomplete inventory.
		return nil, &errs.Error{
			Code:    errs.Internal,
			Message: "inventory materialisation incomplete; publish aborted",
		}
	}

	// version is bumped in the same transaction as the change, so any cached
	// representation keyed on it becomes unreachable without an explicit purge (D4).
	if _, err := tx.Exec(ctx, `
		UPDATE events
		   SET status = 'ON_SALE', version = version + 1, updated_at = $2
		 WHERE event_id = $1
	`, eventID, s.clock.Now()); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &PublishResponse{EventID: eventID, Status: "ON_SALE", TicketsCreated: created}, nil
}
