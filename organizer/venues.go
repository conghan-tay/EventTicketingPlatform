package organizer

import (
	"context"
	"errors"

	"encore.dev/beta/errs"

	"encore.app/internal/seatmap"
)

type CreateVenueRequest struct {
	Name    string `json:"name"`
	City    string `json:"city"`
	Country string `json:"country"`
	// Coordinates are optional, but must be supplied together. A venue without them
	// simply never matches a radius-filtered search.
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
}

type Venue struct {
	VenueID   int64   `json:"venue_id"`
	Name      string  `json:"name"`
	City      string  `json:"city"`
	Country   string  `json:"country"`
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
}

// CreateVenue registers a venue.
//
//encore:api auth method=POST path=/v1/venues
func (s *Service) CreateVenue(ctx context.Context, req *CreateVenueRequest) (*Venue, error) {
	if _, err := callerID(); err != nil {
		return nil, err
	}

	if trimmed(req.Name) == "" {
		return nil, badRequest("name is required")
	}
	if trimmed(req.City) == "" {
		return nil, badRequest("city is required")
	}
	if trimmed(req.Country) == "" {
		return nil, badRequest("country is required")
	}
	if req.Latitude < -90 || req.Latitude > 90 {
		return nil, badRequest("latitude must be between -90 and 90")
	}
	if req.Longitude < -180 || req.Longitude > 180 {
		return nil, badRequest("longitude must be between -180 and 180")
	}

	// The schema requires coordinates to be null together. Treating the zero pair as
	// "not supplied" keeps the API simple; (0,0) is in the Atlantic and not a venue.
	var lat, lon *float64
	if req.Latitude != 0 || req.Longitude != 0 {
		lat, lon = &req.Latitude, &req.Longitude
	}

	var v Venue
	err := db.QueryRow(ctx, `
		INSERT INTO venues (name, city, country, latitude, longitude, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING venue_id, name, city, country,
		          coalesce(latitude, 0), coalesce(longitude, 0)
	`, trimmed(req.Name), trimmed(req.City), trimmed(req.Country), lat, lon, s.clock.Now()).
		Scan(&v.VenueID, &v.Name, &v.City, &v.Country, &v.Latitude, &v.Longitude)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

type AddSeatsRequest struct {
	Sections []seatmap.SectionSpec `json:"sections"`
}

type AddSeatsResponse struct {
	VenueID      int64 `json:"venue_id"`
	SeatsCreated int64 `json:"seats_created"`
}

// AddSeats materialises a venue's seat topology from section specs.
//
//encore:api auth method=POST path=/v1/venues/:venueID/seats
func (s *Service) AddSeats(ctx context.Context, venueID int64, req *AddSeatsRequest) (*AddSeatsResponse, error) {
	if _, err := callerID(); err != nil {
		return nil, err
	}

	seats, err := seatmap.Generate(req.Sections)
	if err != nil {
		// Generation failures are all caller mistakes (no sections, bad counts,
		// duplicate section name, exceeding the venue cap).
		if errors.Is(err, seatmap.ErrTooManySeats) {
			return nil, &errs.Error{Code: errs.InvalidArgument, Message: err.Error()}
		}
		return nil, badRequest(err.Error())
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT true FROM venues WHERE venue_id = $1`, venueID).Scan(&exists); err != nil {
		return nil, &errs.Error{Code: errs.NotFound, Message: "venue not found"}
	}

	// Seats are immutable once an event has been published against the venue;
	// changing topology underneath materialised tickets would orphan inventory.
	var publishedEvents int64
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM events
		 WHERE venue_id = $1 AND status <> 'DRAFT'
	`, venueID).Scan(&publishedEvents); err != nil {
		return nil, err
	}
	if publishedEvents > 0 {
		return nil, &errs.Error{
			Code:    errs.FailedPrecondition,
			Message: "venue seat map cannot change once an event has been published against it",
		}
	}

	// Build parallel arrays and insert in one statement via unnest. A row-at-a-time
	// loop would be thousands of round trips for a large venue.
	sections := make([]string, len(seats))
	rowLabels := make([]string, len(seats))
	numbers := make([]int32, len(seats))
	for i, seat := range seats {
		sections[i] = seat.Section
		rowLabels[i] = seat.RowLabel
		numbers[i] = int32(seat.Number)
	}

	var created int64
	err = tx.QueryRow(ctx, `
		WITH inserted AS (
			INSERT INTO seats (venue_id, section, row_label, seat_number)
			SELECT $1, s.section, s.row_label, s.seat_number
			  FROM unnest($2::text[], $3::text[], $4::int[])
			       AS s(section, row_label, seat_number)
			ON CONFLICT (venue_id, section, row_label, seat_number) DO NOTHING
			RETURNING 1
		)
		SELECT count(*) FROM inserted
	`, venueID, sections, rowLabels, numbers).Scan(&created)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &AddSeatsResponse{VenueID: venueID, SeatsCreated: created}, nil
}
