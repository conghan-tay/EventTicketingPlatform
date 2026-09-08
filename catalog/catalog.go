// Package catalog serves public event reads.
//
// Everything here is derived, cacheable and may be stale. The catalog owns no
// authoritative state (D8), and availability figures it returns are explicitly
// advisory (D4) — only the conditional write on the ticket row decides who gets a
// seat. Clients must never treat a number from this service as a reservation.
//
// Read path, in the order the escalation ladder prescribes: indexed queries, a
// projection cached under a version-derived key, single-flight rebuilds, a bounded
// database fallback, and HTTP validators so a CDN and browsers can revalidate
// cheaply. See docs/system-design.md §7.2.
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
	// Stale marks the availability figures as advisory. Present in the payload so a
	// client cannot mistake them for a guarantee.
	Stale bool      `json:"stale"`
	AsOf  time.Time `json:"as_of"`
}

var errEventNotFound = &errs.Error{Code: errs.NotFound, Message: "event not found"}

// eventDetail composes the cached view with the cached availability snapshot.
func (s *Service) eventDetail(ctx context.Context, eventID int64) (*Event, error) {
	view, err := s.eventView(ctx, eventID)
	if err != nil {
		return nil, err
	}

	avail, err := s.availability(ctx, eventID)
	if err != nil {
		return nil, err
	}

	availBySection := make(map[string]int64, len(avail.Sections))
	for _, sec := range avail.Sections {
		availBySection[sec.Section] = sec.Available
	}

	ev := &Event{
		EventID:     view.EventID,
		Title:       view.Title,
		Description: view.Description,
		Category:    view.Category,
		Status:      view.Status,
		Version:     view.Version,
		StartsAt:    view.StartsAt,
		EndsAt:      view.EndsAt,
		OnsaleAt:    view.OnsaleAt,
		Venue:       view.Venue,
		TotalSeats:  view.TotalSeats,
		Available:   avail.Available,
		Tiers:       make([]Tier, 0, len(view.Tiers)),
		Stale:       true,
		AsOf:        avail.BuiltAt,
	}
	for _, t := range view.Tiers {
		ev.Tiers = append(ev.Tiers, Tier{
			PriceTierID: t.PriceTierID,
			Section:     t.Section,
			PriceCents:  t.PriceCents,
			TotalSeats:  t.TotalSeats,
			Available:   availBySection[t.Section],
		})
	}
	return ev, nil
}

// eventView returns the cached projection, rebuilding it on a miss.
//
// The version is read from Postgres first. That single indexed primary-key lookup is
// the price of never serving a stale event body: it makes the cache key reflect the
// current state, so an edit is visible on the very next read with no invalidation.
func (s *Service) eventView(ctx context.Context, eventID int64) (*EventView, error) {
	version, err := s.currentVersion(ctx, eventID)
	if err != nil {
		return nil, err
	}

	key := eventViewKey{EventID: eventID, Version: version}

	view, err := eventViewCache.Get(ctx, key)
	switch {
	case err == nil:
		recordHit()
		return &view, nil
	case isMiss(err):
		recordMiss()
	default:
		// The cache tier is unhealthy, not merely cold.
		recordCacheError(err)
	}

	// Collapse concurrent rebuilds of the same key into one query.
	built, err, _ := rebuildGroup.Do(cacheKeyString(key), func() (any, error) {
		var out *EventView
		ran, err := withDBSlot(ctx, func() error {
			v, buildErr := s.buildEventView(ctx, eventID)
			if buildErr != nil {
				return buildErr
			}
			out = v
			return nil
		})
		if err != nil {
			return nil, err
		}
		if !ran {
			// Load shed rather than pile onto the database.
			return nil, &errs.Error{
				Code:    errs.Unavailable,
				Message: "catalog is busy; please retry",
			}
		}

		// Populate best-effort. A cache write failure must not fail the read.
		if setErr := eventViewCache.Set(ctx, key, *out); setErr != nil {
			recordCacheError(setErr)
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	return built.(*EventView), nil
}

// currentVersion also enforces visibility: a DRAFT event reports not-found rather
// than a distinct status, so an unpublished lineup cannot be discovered by probing.
func (s *Service) currentVersion(ctx context.Context, eventID int64) (int, error) {
	var version int
	err := db.QueryRow(ctx, `
		SELECT version FROM events
		 WHERE event_id = $1 AND status IN ('ON_SALE', 'CLOSED')
	`, eventID).Scan(&version)
	if errors.Is(err, sqldb.ErrNoRows) {
		return 0, errEventNotFound
	} else if err != nil {
		return 0, err
	}
	return version, nil
}

// buildEventView assembles the projection in one round trip.
//
// Tier capacity is counted here rather than stored, because it is fixed once the
// event is published and this runs only on a cache miss.
func (s *Service) buildEventView(ctx context.Context, eventID int64) (*EventView, error) {
	var v EventView
	err := db.QueryRow(ctx, `
		SELECT e.event_id, e.title, e.description, e.category, e.status::text, e.version,
		       e.starts_at, e.ends_at, e.onsale_at,
		       vn.venue_id, vn.name, vn.city, vn.country,
		       coalesce(vn.latitude, 0), coalesce(vn.longitude, 0)
		  FROM events e
		  JOIN venues vn ON vn.venue_id = e.venue_id
		 WHERE e.event_id = $1
		   AND e.status IN ('ON_SALE', 'CLOSED')
	`, eventID).Scan(
		&v.EventID, &v.Title, &v.Description, &v.Category, &v.Status, &v.Version,
		&v.StartsAt, &v.EndsAt, &v.OnsaleAt,
		&v.Venue.VenueID, &v.Venue.Name, &v.Venue.City, &v.Venue.Country,
		&v.Venue.Latitude, &v.Venue.Longitude,
	)
	if errors.Is(err, sqldb.ErrNoRows) {
		return nil, errEventNotFound
	} else if err != nil {
		return nil, err
	}

	rows, err := db.Query(ctx, `
		SELECT pt.price_tier_id, pt.section, pt.price_cents, count(t.ticket_id)
		  FROM price_tiers pt
		  LEFT JOIN tickets t
		    ON t.event_id = pt.event_id AND t.price_tier_id = pt.price_tier_id
		 WHERE pt.event_id = $1
		 GROUP BY pt.price_tier_id, pt.section, pt.price_cents
		 ORDER BY pt.price_cents DESC, pt.section
	`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	v.Tiers = []TierPrice{}
	for rows.Next() {
		var t TierPrice
		if err := rows.Scan(&t.PriceTierID, &t.Section, &t.PriceCents, &t.TotalSeats); err != nil {
			return nil, err
		}
		v.TotalSeats += t.TotalSeats
		v.Tiers = append(v.Tiers, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &v, nil
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
	// Stale is always true: this figure is advisory by design (D4).
	Stale bool `json:"stale"`
}

// GetAvailability returns per-section availability.
//
//encore:api public method=GET path=/v1/events/:eventID/availability
func (s *Service) GetAvailability(ctx context.Context, eventID int64) (*Availability, error) {
	if _, err := s.currentVersion(ctx, eventID); err != nil {
		return nil, err
	}
	snap, err := s.availability(ctx, eventID)
	if err != nil {
		return nil, err
	}
	return &Availability{
		EventID:   snap.EventID,
		Total:     snap.Total,
		Available: snap.Available,
		Sections:  snap.Sections,
		AsOf:      snap.BuiltAt,
		Stale:     true,
	}, nil
}

// availability returns the short-TTL snapshot, rebuilding on a miss.
func (s *Service) availability(ctx context.Context, eventID int64) (*AvailabilitySnapshot, error) {
	key := availabilityKey{EventID: eventID}

	snap, err := availabilityCache.Get(ctx, key)
	switch {
	case err == nil:
		recordHit()
		return &snap, nil
	case isMiss(err):
		recordMiss()
	default:
		recordCacheError(err)
	}

	built, err, _ := rebuildGroup.Do("availability/"+itoa(eventID), func() (any, error) {
		var out *AvailabilitySnapshot
		ran, err := withDBSlot(ctx, func() error {
			s, buildErr := s.buildAvailability(ctx, eventID)
			if buildErr != nil {
				return buildErr
			}
			out = s
			return nil
		})
		if err != nil {
			return nil, err
		}
		if !ran {
			return nil, &errs.Error{Code: errs.Unavailable, Message: "catalog is busy; please retry"}
		}
		if setErr := availabilityCache.Set(ctx, key, *out); setErr != nil {
			recordCacheError(setErr)
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	return built.(*AvailabilitySnapshot), nil
}

func (s *Service) buildAvailability(ctx context.Context, eventID int64) (*AvailabilitySnapshot, error) {
	rows, err := db.Query(ctx, `
		SELECT pt.section,
		       count(t.ticket_id),
		       count(t.ticket_id) FILTER (WHERE t.status = 'AVAILABLE')
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

	out := &AvailabilitySnapshot{
		EventID:  eventID,
		Sections: []SectionAvailability{},
		BuiltAt:  s.clock.Now(),
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
// Deliberately not cached. It is per-seat, changes constantly during an onsale, and
// is the read most likely to be immediately acted upon — caching it would widen the
// window in which a client picks a seat that is already gone.
//
//encore:api public method=GET path=/v1/events/:eventID/seats
func (s *Service) GetSeatMap(ctx context.Context, eventID int64, p *SeatMapParams) (*SeatMap, error) {
	if _, err := s.currentVersion(ctx, eventID); err != nil {
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
		  JOIN seats s        ON s.seat_id = t.seat_id
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
