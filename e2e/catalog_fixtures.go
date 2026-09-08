//go:build e2e

package e2e

import (
	"net/http"
	"time"

	"github.com/stretchr/testify/require"
)

// DefaultOrganizer is the identity fixtures act as unless told otherwise.
const DefaultOrganizer = "organizer-1"

type SectionSpec struct {
	Name        string `json:"name"`
	Rows        int    `json:"rows"`
	SeatsPerRow int    `json:"seats_per_row"`
}

type TierSpec struct {
	Section    string `json:"section"`
	PriceCents int64  `json:"price_cents"`
}

// VenueOpts describes a venue to create. Zero values get sensible defaults so tests
// only state what they care about.
type VenueOpts struct {
	Name      string
	City      string
	Country   string
	Latitude  float64
	Longitude float64
	Sections  []SectionSpec
}

// EventOpts describes an event to create.
type EventOpts struct {
	Title       string
	Description string
	Category    string
	StartsAt    time.Time
	EndsAt      time.Time
	OnsaleAt    time.Time
	Tiers       []TierSpec
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
	// Stale marks the availability figures as advisory (D4).
	Stale bool      `json:"stale"`
	AsOf  time.Time `json:"as_of"`
}

type PublishResult struct {
	EventID        int64  `json:"event_id"`
	Status         string `json:"status"`
	TicketsCreated int64  `json:"tickets_created"`
}

// CreateVenue creates a venue and its seats, returning the venue id.
func (h *Harness) CreateVenue(opts VenueOpts) int64 {
	h.t.Helper()

	if opts.Name == "" {
		opts.Name = "Test Arena"
	}
	if opts.City == "" {
		opts.City = "London"
	}
	if opts.Country == "" {
		opts.Country = "GB"
	}
	if opts.Sections == nil {
		opts.Sections = []SectionSpec{{Name: "FLOOR", Rows: 10, SeatsPerRow: 20}}
	}

	client := h.AsUser(DefaultOrganizer)
	resp, err := client.Post("/v1/venues", map[string]any{
		"name":      opts.Name,
		"city":      opts.City,
		"country":   opts.Country,
		"latitude":  opts.Latitude,
		"longitude": opts.Longitude,
	})
	require.NoError(h.t, err)
	require.Equal(h.t, http.StatusOK, resp.Status, "create venue: %s", resp.Body)

	var venue Venue
	require.NoError(h.t, resp.DecodeInto(&venue))
	require.NotZero(h.t, venue.VenueID)

	h.AddSeats(venue.VenueID, opts.Sections)
	return venue.VenueID
}

// AddSeats materialises a venue's seat topology.
func (h *Harness) AddSeats(venueID int64, sections []SectionSpec) int64 {
	h.t.Helper()

	resp, err := h.AsUser(DefaultOrganizer).Post(
		pathf("/v1/venues/%d/seats", venueID),
		map[string]any{"sections": sections},
	)
	require.NoError(h.t, err)
	require.Equal(h.t, http.StatusOK, resp.Status, "add seats: %s", resp.Body)

	var out struct {
		SeatsCreated int64 `json:"seats_created"`
	}
	require.NoError(h.t, resp.DecodeInto(&out))
	return out.SeatsCreated
}

// CreateEvent creates a DRAFT event at the venue.
func (h *Harness) CreateEvent(venueID int64, opts EventOpts) int64 {
	h.t.Helper()

	now := h.ClockNow()
	if opts.Title == "" {
		opts.Title = "Test Event"
	}
	if opts.Category == "" {
		opts.Category = "MUSIC"
	}
	if opts.StartsAt.IsZero() {
		opts.StartsAt = now.Add(30 * 24 * time.Hour)
	}
	if opts.EndsAt.IsZero() {
		opts.EndsAt = opts.StartsAt.Add(3 * time.Hour)
	}
	if opts.OnsaleAt.IsZero() {
		opts.OnsaleAt = now.Add(-time.Hour) // already on sale
	}
	if opts.Tiers == nil {
		opts.Tiers = []TierSpec{{Section: "FLOOR", PriceCents: 5000}}
	}

	resp, err := h.AsUser(DefaultOrganizer).Post("/v1/events", map[string]any{
		"venue_id":    venueID,
		"title":       opts.Title,
		"description": opts.Description,
		"category":    opts.Category,
		"starts_at":   opts.StartsAt,
		"ends_at":     opts.EndsAt,
		"onsale_at":   opts.OnsaleAt,
		"tiers":       opts.Tiers,
	})
	require.NoError(h.t, err)
	require.Equal(h.t, http.StatusOK, resp.Status, "create event: %s", resp.Body)

	var ev Event
	require.NoError(h.t, resp.DecodeInto(&ev))
	require.NotZero(h.t, ev.EventID)
	return ev.EventID
}

// PublishEvent publishes an event, materialising one ticket per seat.
func (h *Harness) PublishEvent(eventID int64) PublishResult {
	h.t.Helper()

	resp, err := h.AsUser(DefaultOrganizer).Post(pathf("/v1/events/%d/publish", eventID), nil)
	require.NoError(h.t, err)
	require.Equal(h.t, http.StatusOK, resp.Status, "publish: %s", resp.Body)

	var out PublishResult
	require.NoError(h.t, resp.DecodeInto(&out))
	return out
}

// GetEvent fetches an event's public detail.
func (h *Harness) GetEvent(eventID int64) Event {
	h.t.Helper()

	resp, err := h.Anonymous().Get(pathf("/v1/events/%d", eventID), nil)
	require.NoError(h.t, err)
	require.Equal(h.t, http.StatusOK, resp.Status, "get event: %s", resp.Body)

	var ev Event
	require.NoError(h.t, resp.DecodeInto(&ev))
	return ev
}

// CacheStats reports catalog cache counters, so a test can prove a read was served
// from cache rather than assuming it.
type CacheStats struct {
	Hits     int64 `json:"hits"`
	Misses   int64 `json:"misses"`
	Errors   int64 `json:"errors"`
	Rebuilds int64 `json:"rebuilds"`
	Shed     int64 `json:"shed"`
}

func (h *Harness) CacheStats() CacheStats {
	h.t.Helper()
	resp, err := h.Get("/_test/cache-stats", nil)
	require.NoError(h.t, err)
	require.Equal(h.t, http.StatusOK, resp.Status, "cache stats: %s", resp.Body)

	var out CacheStats
	require.NoError(h.t, resp.DecodeInto(&out))
	return out
}

// GetEventRaw fetches an event with optional conditional-request headers, so ETag and
// 304 behaviour can be asserted.
func (h *Harness) GetEventRaw(eventID int64, ifNoneMatch string) *Response {
	h.t.Helper()
	headers := map[string]string{}
	if ifNoneMatch != "" {
		headers["If-None-Match"] = ifNoneMatch
	}
	resp, err := h.Anonymous().GetWithHeaders(pathf("/v1/events/%d", eventID), nil, headers)
	require.NoError(h.t, err)
	return resp
}

// UpdateEventOpts is a partial event edit; nil fields are left unchanged.
type UpdateEventOpts struct {
	Title       *string
	Description *string
	Category    *string
	User        string
}

// TryUpdateEvent edits an event and returns the raw response.
func (h *Harness) TryUpdateEvent(eventID int64, opts UpdateEventOpts) *Response {
	h.t.Helper()
	if opts.User == "" {
		opts.User = DefaultOrganizer
	}

	body := map[string]any{}
	if opts.Title != nil {
		body["title"] = *opts.Title
	}
	if opts.Description != nil {
		body["description"] = *opts.Description
	}
	if opts.Category != nil {
		body["category"] = *opts.Category
	}

	resp, err := h.AsUser(opts.User).Patch(pathf("/v1/events/%d", eventID), body)
	require.NoError(h.t, err)
	return resp
}

// UpdateEvent edits an event and requires success.
func (h *Harness) UpdateEvent(eventID int64, opts UpdateEventOpts) Event {
	h.t.Helper()
	resp := h.TryUpdateEvent(eventID, opts)
	require.Equal(h.t, http.StatusOK, resp.Status, "update event: %s", resp.Body)

	var ev Event
	require.NoError(h.t, resp.DecodeInto(&ev))
	return ev
}

func StrPtr(s string) *string { return &s }

// RefreshAvailability drops an event's cached availability snapshot.
//
// Encore cache TTLs are real Redis expiries, so the injected clock cannot age them
// out. Dropping the key is how a test observes a sale without sleeping through the
// 5s TTL — the TTL itself is deliberately not shortened for tests.
func (h *Harness) RefreshAvailability(eventID int64) {
	h.t.Helper()
	resp, err := h.Post(pathf("/_test/refresh-availability/%d", eventID), nil)
	require.NoError(h.t, err)
	require.Equal(h.t, http.StatusOK, resp.Status, "refresh availability: %s", resp.Body)
}

// SeedPublishedEvent is the common setup: a venue, an on-sale event, and tickets.
func (h *Harness) SeedPublishedEvent(v VenueOpts, e EventOpts) int64 {
	h.t.Helper()
	venueID := h.CreateVenue(v)
	eventID := h.CreateEvent(venueID, e)
	h.PublishEvent(eventID)
	return eventID
}
