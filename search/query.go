package search

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"encore.app/internal/cursor"
)

const (
	DefaultLimit = 20
	MaxLimit     = 100
	// MaxRadiusKM is a little over half Earth's circumference, past which a radius
	// filter is meaningless.
	MaxRadiusKM = 20_000
	maxQueryLen = 200
)

// Params is the raw HTTP query string.
//
// Every field is a string, taken verbatim, for two reasons. Encore does not support
// pointer types in query strings, and an absent filter must be distinguishable from a
// zero one — `lat=0` is a valid location on the Gulf of Guinea, not "no location".
// Parsing here also keeps the entire validation matrix unit-testable without a
// running app.
type Params struct {
	Q        string `query:"q"`
	Category string `query:"category"`
	From     string `query:"from"`
	To       string `query:"to"`
	Lat      string `query:"lat"`
	Lng      string `query:"lng"`
	RadiusKM string `query:"radius_km"`
	Cursor   string `query:"cursor"`
	Limit    string `query:"limit"`
}

// Location is a validated radius filter.
type Location struct {
	Lat      float64
	Lng      float64
	RadiusKM float64
}

// Query is a validated search request. Constructing one is the only way to reach the
// index, so the index never has to re-validate.
type Query struct {
	Text     string
	Category string
	From     *time.Time
	To       *time.Time
	Location *Location
	After    *cursor.Key
	Limit    int
}

// parse validates and normalises raw parameters.
//
// Kept as a pure method on Params, separate from any database access, so the whole
// validation matrix is unit-testable without a running app.
func (p *Params) parse() (Query, error) {
	if p == nil {
		return Query{Limit: DefaultLimit}, nil
	}

	q := Query{
		Text:     strings.TrimSpace(p.Q),
		Category: strings.TrimSpace(p.Category),
	}

	if len(q.Text) > maxQueryLen {
		return Query{}, fmt.Errorf("q must be at most %d characters", maxQueryLen)
	}

	limit, err := parseLimit(p.Limit)
	if err != nil {
		return Query{}, err
	}
	q.Limit = limit

	if q.From, err = parseTime("from", p.From); err != nil {
		return Query{}, err
	}
	if q.To, err = parseTime("to", p.To); err != nil {
		return Query{}, err
	}
	if q.From != nil && q.To != nil && q.To.Before(*q.From) {
		return Query{}, errors.New("to must not be before from")
	}

	lat, err := parseFloat("lat", p.Lat)
	if err != nil {
		return Query{}, err
	}
	lng, err := parseFloat("lng", p.Lng)
	if err != nil {
		return Query{}, err
	}
	radius, err := parseFloat("radius_km", p.RadiusKM)
	if err != nil {
		return Query{}, err
	}
	if q.Location, err = parseLocation(lat, lng, radius); err != nil {
		return Query{}, err
	}

	if p.Cursor != "" {
		key, err := cursor.Decode(p.Cursor)
		if err != nil {
			// Surfaced as a client error rather than silently restarting from the
			// first page, which would repeat results the caller has already seen.
			return Query{}, fmt.Errorf("invalid cursor: %w", err)
		}
		q.After = &key
	}

	return q, nil
}

// parseLimit clamps rather than rejects a page size.
//
// A client asking for too much gets the maximum, and one asking for zero or a
// negative size gets the default. Honouring a huge limit would let a single request
// pull the entire catalogue. A value that is not a number at all is a client error,
// since silently substituting the default would hide a broken caller.
func parseLimit(raw string) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return DefaultLimit, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, errors.New("limit must be an integer")
	}
	switch {
	case n <= 0:
		return DefaultLimit, nil
	case n > MaxLimit:
		return MaxLimit, nil
	default:
		return n, nil
	}
}

func parseTime(name, raw string) (*time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, fmt.Errorf("%s must be an RFC3339 timestamp", name)
	}
	utc := t.UTC()
	return &utc, nil
}

func parseFloat(name, raw string) (*float64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return nil, fmt.Errorf("%s must be a number", name)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, fmt.Errorf("%s must be a finite number", name)
	}
	return &f, nil
}

// parseLocation validates that a radius filter is complete and in range. All three
// parts are required together: two of the three describes nothing usable.
func parseLocation(lat, lng, radius *float64) (*Location, error) {
	present := 0
	for _, v := range []*float64{lat, lng, radius} {
		if v != nil {
			present++
		}
	}
	if present == 0 {
		return nil, nil
	}
	if present != 3 {
		return nil, errors.New("lat, lng and radius_km must be supplied together")
	}

	if *lat < -90 || *lat > 90 {
		return nil, errors.New("lat must be between -90 and 90")
	}
	if *lng < -180 || *lng > 180 {
		return nil, errors.New("lng must be between -180 and 180")
	}
	if *radius <= 0 {
		return nil, errors.New("radius_km must be positive")
	}
	if *radius > MaxRadiusKM {
		return nil, fmt.Errorf("radius_km must be at most %d", MaxRadiusKM)
	}

	return &Location{Lat: *lat, Lng: *lng, RadiusKM: *radius}, nil
}

type Hit struct {
	EventID     int64     `json:"event_id"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Category    string    `json:"category"`
	StartsAt    time.Time `json:"starts_at"`
	EndsAt      time.Time `json:"ends_at"`
	VenueID     int64     `json:"venue_id"`
	VenueName   string    `json:"venue_name"`
	City        string    `json:"city"`
	Country     string    `json:"country"`
	// DistanceKM is set only when a radius filter was applied.
	DistanceKM    *float64 `json:"distance_km"`
	MinPriceCents int64    `json:"min_price_cents"`
	Available     int64    `json:"available"`
}

type Results struct {
	Results []Hit `json:"results"`
	// NextCursor is empty on the last page.
	NextCursor string `json:"next_cursor"`
	HasMore    bool   `json:"has_more"`
	// Stale marks these results as derived and eventually consistent (D4).
	Stale bool `json:"stale"`
}

// SearchIndex is the seam for swapping Postgres FTS for OpenSearch (D6) when search
// p95 exceeds 500ms or the active-event count passes roughly 2M.
type SearchIndex interface {
	Search(ctx context.Context, q Query) (*Results, error)
}
