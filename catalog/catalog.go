// Package catalog serves public event reads.
//
// Everything here is derived, cacheable and may be stale. The catalog owns no
// authoritative state (D8), and availability figures it returns are explicitly
// advisory (D4) — only the conditional write on the ticket row decides who gets a
// seat. Clients must never treat a number from this service as a reservation.
package catalog

import (
	"context"
	"errors"
	"time"

	"encore.dev/beta/errs"
	"encore.dev/storage/sqldb"

	"encore.app/internal/clock"
)

var db = sqldb.Named("ticketing")

//encore:service
type Service struct {
	clock clock.Clock
}

func initService() (*Service, error) {
	return &Service{clock: clock.Default()}, nil
}

type Venue struct {
	VenueID   int64   `json:"venue_id"`
	Name      string  `json:"name"`
	City      string  `json:"city"`
	Country   string  `json:"country"`
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
}

type Tier struct {
	PriceTierID int64  `json:"price_tier_id"`
	Section     string `json:"section"`
	PriceCents  int64  `json:"price_cents"`
	TotalSeats  int64  `json:"total_seats"`
	Available   int64  `json:"available"`
}

type Event struct {
	EventID     int64     `json:"event_id"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Category    string    `json:"category"`
	Status      string    `json:"status"`
	Version     int       `json:"version"`
	StartsAt    time.Time `json:"starts_at"`
	EndsAt      time.Time `json:"ends_at"`
	OnsaleAt    time.Time `json:"onsale_at"`
	Venue       Venue     `json:"venue"`
	Tiers       []Tier    `json:"tiers"`
	TotalSeats  int64     `json:"total_seats"`
	Available   int64     `json:"available"`
}

var errEventNotFound = &errs.Error{Code: errs.NotFound, Message: "event not found"}

// GetEvent returns an event's public detail.
//
// Only ON_SALE and CLOSED events are visible. A DRAFT event returns NotFound rather
// than a distinct status, so an unpublished lineup cannot be discovered by probing ids.
//
//encore:api public method=GET path=/v1/events/:eventID
func (s *Service) GetEvent(ctx context.Context, eventID int64) (*Event, error) {
	var ev Event
	err := db.QueryRow(ctx, `
		SELECT e.event_id, e.title, e.description, e.category, e.status::text, e.version,
		       e.starts_at, e.ends_at, e.onsale_at,
		       v.venue_id, v.name, v.city, v.country,
		       coalesce(v.latitude, 0), coalesce(v.longitude, 0)
		  FROM events e
		  JOIN venues v ON v.venue_id = e.venue_id
		 WHERE e.event_id = $1
		   AND e.status IN ('ON_SALE', 'CLOSED')
	`, eventID).Scan(
		&ev.EventID, &ev.Title, &ev.Description, &ev.Category, &ev.Status, &ev.Version,
		&ev.StartsAt, &ev.EndsAt, &ev.OnsaleAt,
		&ev.Venue.VenueID, &ev.Venue.Name, &ev.Venue.City, &ev.Venue.Country,
		&ev.Venue.Latitude, &ev.Venue.Longitude,
	)
	if errors.Is(err, sqldb.ErrNoRows) {
		return nil, errEventNotFound
	} else if err != nil {
		return nil, err
	}

	tiers, total, available, err := s.tierBreakdown(ctx, eventID)
	if err != nil {
		return nil, err
	}
	ev.Tiers = tiers
	ev.TotalSeats = total
	ev.Available = available
	return &ev, nil
}

// tierBreakdown aggregates capacity and availability per price tier.
//
// This is the aggregation that Step 7 replaces with a cached read model; it is a
// single grouped scan of a partial index today, which is well within budget at the
// current stage.
func (s *Service) tierBreakdown(ctx context.Context, eventID int64) ([]Tier, int64, int64, error) {
	rows, err := db.Query(ctx, `
		SELECT pt.price_tier_id, pt.section, pt.price_cents,
		       count(t.ticket_id) AS total_seats,
		       count(t.ticket_id) FILTER (WHERE t.status = 'AVAILABLE') AS available
		  FROM price_tiers pt
		  LEFT JOIN tickets t
		    ON t.event_id = pt.event_id AND t.price_tier_id = pt.price_tier_id
		 WHERE pt.event_id = $1
		 GROUP BY pt.price_tier_id, pt.section, pt.price_cents
		 ORDER BY pt.price_cents DESC, pt.section
	`, eventID)
	if err != nil {
		return nil, 0, 0, err
	}
	defer rows.Close()

	var (
		tiers            []Tier
		total, available int64
	)
	for rows.Next() {
		var t Tier
		if err := rows.Scan(&t.PriceTierID, &t.Section, &t.PriceCents, &t.TotalSeats, &t.Available); err != nil {
			return nil, 0, 0, err
		}
		total += t.TotalSeats
		available += t.Available
		tiers = append(tiers, t)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, 0, err
	}
	if tiers == nil {
		tiers = []Tier{}
	}
	return tiers, total, available, nil
}

type SectionAvailability struct {
	Section   string `json:"section"`
	Total     int64  `json:"total"`
	Available int64  `json:"available"`
}

type Availability struct {
	EventID   int64                 `json:"event_id"`
	Total     int64                 `json:"total"`
	Available int64                 `json:"available"`
	Sections  []SectionAvailability `json:"sections"`
	AsOf      time.Time             `json:"as_of"`
	// Stale is always true: this figure is advisory by design (D4). It is surfaced in
	// the payload so a client cannot mistake it for a guarantee.
	Stale bool `json:"stale"`
}

// GetAvailability returns per-section availability.
//
//encore:api public method=GET path=/v1/events/:eventID/availability
func (s *Service) GetAvailability(ctx context.Context, eventID int64) (*Availability, error) {
	if err := s.requireVisible(ctx, eventID); err != nil {
		return nil, err
	}

	rows, err := db.Query(ctx, `
		SELECT pt.section,
		       count(t.ticket_id) AS total,
		       count(t.ticket_id) FILTER (WHERE t.status = 'AVAILABLE') AS available
		  FROM price_tiers pt
		  LEFT JOIN tickets t
		    ON t.event_id = pt.event_id AND t.price_tier_id = pt.price_tier_id
		 WHERE pt.event_id = $1
		 GROUP BY pt.section
		 ORDER BY pt.section
	`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := &Availability{
		EventID:  eventID,
		Sections: []SectionAvailability{},
		AsOf:     s.clock.Now(),
		Stale:    true,
	}
	for rows.Next() {
		var sec SectionAvailability
		if err := rows.Scan(&sec.Section, &sec.Total, &sec.Available); err != nil {
			return nil, err
		}
		out.Total += sec.Total
		out.Available += sec.Available
		out.Sections = append(out.Sections, sec)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

type Seat struct {
	TicketID   int64  `json:"ticket_id"`
	Section    string `json:"section"`
	RowLabel   string `json:"row_label"`
	SeatNumber int    `json:"seat_number"`
	Status     string `json:"status"`
	PriceCents int64  `json:"price_cents"`
}

type SeatMapParams struct {
	// Section optionally narrows the map. Large venues have ~100k seats, so a client
	// rendering a seat picker should request one section at a time.
	Section string `query:"section"`
}

type SeatMap struct {
	EventID int64     `json:"event_id"`
	Section string    `json:"section"`
	Seats   []Seat    `json:"seats"`
	AsOf    time.Time `json:"as_of"`
	// As with Availability, the seat map is advisory. A seat shown AVAILABLE here may
	// already be gone; only the claim is authoritative.
	Stale bool `json:"stale"`
}

// GetSeatMap returns seat-level status for an event.
//
//encore:api public method=GET path=/v1/events/:eventID/seats
func (s *Service) GetSeatMap(ctx context.Context, eventID int64, p *SeatMapParams) (*SeatMap, error) {
	if err := s.requireVisible(ctx, eventID); err != nil {
		return nil, err
	}

	// An empty section means "all sections". The query uses a NULL sentinel rather
	// than string concatenation so there is one plan and no injection surface.
	var section *string
	if p != nil && p.Section != "" {
		section = &p.Section
	}

	rows, err := db.Query(ctx, `
		SELECT t.ticket_id, s.section, s.row_label, s.seat_number,
		       t.status::text, pt.price_cents
		  FROM tickets t
		  JOIN seats s       ON s.seat_id = t.seat_id
		  JOIN price_tiers pt ON pt.price_tier_id = t.price_tier_id
		 WHERE t.event_id = $1
		   AND ($2::text IS NULL OR s.section = $2)
		 ORDER BY s.section, s.row_label, s.seat_number
	`, eventID, section)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := &SeatMap{EventID: eventID, Seats: []Seat{}, AsOf: s.clock.Now(), Stale: true}
	if section != nil {
		out.Section = *section
	}
	for rows.Next() {
		var seat Seat
		if err := rows.Scan(&seat.TicketID, &seat.Section, &seat.RowLabel,
			&seat.SeatNumber, &seat.Status, &seat.PriceCents); err != nil {
			return nil, err
		}
		out.Seats = append(out.Seats, seat)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// requireVisible returns NotFound unless the event is publicly viewable, so the
// availability and seat-map endpoints cannot be used to probe unpublished events.
func (s *Service) requireVisible(ctx context.Context, eventID int64) error {
	var visible bool
	err := db.QueryRow(ctx, `
		SELECT true FROM events
		 WHERE event_id = $1 AND status IN ('ON_SALE', 'CLOSED')
	`, eventID).Scan(&visible)
	if errors.Is(err, sqldb.ErrNoRows) {
		return errEventNotFound
	}
	return err
}
