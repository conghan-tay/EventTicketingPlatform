//go:build e2e

package e2e

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// H3: the core catalog journey — create venue, create event, publish, read it back.
func TestCreateAndViewEvent(t *testing.T) {
	h := NewHarness(t)

	venueID := h.CreateVenue(VenueOpts{
		Name:      "Royal Albert Hall",
		City:      "London",
		Country:   "GB",
		Latitude:  51.5010,
		Longitude: -0.1774,
		Sections: []SectionSpec{
			{Name: "STALLS", Rows: 10, SeatsPerRow: 20}, // 200
			{Name: "BALCONY", Rows: 5, SeatsPerRow: 10}, // 50
		},
	})

	eventID := h.CreateEvent(venueID, EventOpts{
		Title:       "Prom Night",
		Description: "An evening of orchestral favourites",
		Category:    "CLASSICAL",
		Tiers: []TierSpec{
			{Section: "STALLS", PriceCents: 9500},
			{Section: "BALCONY", PriceCents: 4500},
		},
	})

	// Before publishing, the event is a draft and not publicly viewable.
	resp, err := h.Anonymous().Get(pathf("/v1/events/%d", eventID), nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, resp.Status,
		"a DRAFT event must not be publicly viewable, got %d: %s", resp.Status, resp.Body)

	published := h.PublishEvent(eventID)
	assert.Equal(t, "ON_SALE", published.Status)
	assert.EqualValues(t, 250, published.TicketsCreated, "200 stalls + 50 balcony")

	ev := h.GetEvent(eventID)
	assert.Equal(t, eventID, ev.EventID)
	assert.Equal(t, "Prom Night", ev.Title)
	assert.Equal(t, "An evening of orchestral favourites", ev.Description)
	assert.Equal(t, "CLASSICAL", ev.Category)
	assert.Equal(t, "ON_SALE", ev.Status)

	assert.Equal(t, "Royal Albert Hall", ev.Venue.Name)
	assert.Equal(t, "London", ev.Venue.City)
	assert.InDelta(t, 51.5010, ev.Venue.Latitude, 0.0001)

	assert.EqualValues(t, 250, ev.TotalSeats)
	assert.EqualValues(t, 250, ev.Available, "nothing is sold yet")

	// Tiers carry both price and per-section capacity.
	require.Len(t, ev.Tiers, 2)
	byName := map[string]Tier{}
	for _, tier := range ev.Tiers {
		byName[tier.Section] = tier
	}

	stalls := byName["STALLS"]
	assert.EqualValues(t, 9500, stalls.PriceCents)
	assert.EqualValues(t, 200, stalls.TotalSeats)
	assert.EqualValues(t, 200, stalls.Available)

	balcony := byName["BALCONY"]
	assert.EqualValues(t, 4500, balcony.PriceCents)
	assert.EqualValues(t, 50, balcony.TotalSeats)
	assert.EqualValues(t, 50, balcony.Available)
}

// H4: publishing materialises exactly one ticket per seat — no more, no fewer.
// A miscount here would either strand inventory or, far worse, create phantom seats.
func TestPublishMaterialisesExactlyOneTicketPerSeat(t *testing.T) {
	h := NewHarness(t)

	venueID := h.CreateVenue(VenueOpts{
		Sections: []SectionSpec{{Name: "FLOOR", Rows: 10, SeatsPerRow: 20}},
	})
	eventID := h.CreateEvent(venueID, EventOpts{
		Tiers: []TierSpec{{Section: "FLOOR", PriceCents: 5000}},
	})

	result := h.PublishEvent(eventID)
	require.EqualValues(t, 200, result.TicketsCreated)

	counts := h.TableCounts()
	require.Contains(t, counts, "tickets", "stats should report the tickets table")
	assert.EqualValues(t, 200, counts["tickets"])
	assert.EqualValues(t, 200, counts["seats"])
	assert.EqualValues(t, 1, counts["events"])
	assert.EqualValues(t, 1, counts["venues"])

	// And every ticket is available via the public seat map.
	resp, err := h.Anonymous().Get(pathf("/v1/events/%d/seats", eventID), Query(map[string]string{
		"section": "FLOOR",
	}))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.Status, "body: %s", resp.Body)

	var seatMap struct {
		Seats []struct {
			TicketID int64  `json:"ticket_id"`
			Section  string `json:"section"`
			RowLabel string `json:"row_label"`
			Number   int    `json:"seat_number"`
			Status   string `json:"status"`
		} `json:"seats"`
	}
	require.NoError(t, resp.DecodeInto(&seatMap))
	require.Len(t, seatMap.Seats, 200)

	seen := map[string]bool{}
	for _, s := range seatMap.Seats {
		assert.Equal(t, "AVAILABLE", s.Status)
		key := s.RowLabel + "-" + strconv.Itoa(s.Number)
		require.False(t, seen[key], "duplicate seat %s", key)
		seen[key] = true
	}
	assert.Len(t, seen, 200)
}

func TestAvailabilityEndpoint(t *testing.T) {
	h := NewHarness(t)

	eventID := h.SeedPublishedEvent(
		VenueOpts{Sections: []SectionSpec{
			{Name: "FLOOR", Rows: 4, SeatsPerRow: 5},
			{Name: "REAR", Rows: 2, SeatsPerRow: 5},
		}},
		EventOpts{Tiers: []TierSpec{
			{Section: "FLOOR", PriceCents: 8000},
			{Section: "REAR", PriceCents: 3000},
		}},
	)

	resp, err := h.Anonymous().Get(pathf("/v1/events/%d/availability", eventID), nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.Status, "body: %s", resp.Body)

	var avail struct {
		EventID  int64 `json:"event_id"`
		Total    int64 `json:"total"`
		Availabl int64 `json:"available"`
		Stale    bool  `json:"stale"`
		Sections []struct {
			Section   string `json:"section"`
			Total     int64  `json:"total"`
			Available int64  `json:"available"`
		} `json:"sections"`
	}
	require.NoError(t, resp.DecodeInto(&avail))

	assert.EqualValues(t, 30, avail.Total)
	assert.EqualValues(t, 30, avail.Availabl)
	assert.True(t, avail.Stale,
		"availability is advisory by design (D4) and must say so, so clients never treat it as authoritative")
	require.Len(t, avail.Sections, 2)
}

// S2: unknown event id.
func TestGetUnknownEventReturns404(t *testing.T) {
	h := NewHarness(t)

	resp, err := h.Anonymous().Get("/v1/events/999999", nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, resp.Status, "body: %s", resp.Body)
	assert.Equal(t, "not_found", resp.Error().Code)
}

// S3: validation of the event time window.
func TestCreateEventRejectsInvalidTimeWindow(t *testing.T) {
	h := NewHarness(t)
	venueID := h.CreateVenue(VenueOpts{})
	now := h.ClockNow()

	cases := []struct {
		name   string
		starts time.Time
		ends   time.Time
		onsale time.Time
	}{
		{
			name:   "ends before starts",
			starts: now.Add(48 * time.Hour),
			ends:   now.Add(24 * time.Hour),
			onsale: now,
		},
		{
			name:   "ends equals starts",
			starts: now.Add(48 * time.Hour),
			ends:   now.Add(48 * time.Hour),
			onsale: now,
		},
		{
			name:   "onsale after the event has started",
			starts: now.Add(24 * time.Hour),
			ends:   now.Add(26 * time.Hour),
			onsale: now.Add(25 * time.Hour),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := h.AsUser(DefaultOrganizer).Post("/v1/events", map[string]any{
				"venue_id":  venueID,
				"title":     "Bad Window",
				"category":  "MUSIC",
				"starts_at": tc.starts,
				"ends_at":   tc.ends,
				"onsale_at": tc.onsale,
				"tiers":     []TierSpec{{Section: "FLOOR", PriceCents: 1000}},
			})
			require.NoError(t, err)
			assert.Equal(t, http.StatusBadRequest, resp.Status, "body: %s", resp.Body)
		})
	}
}

func TestCreateEventRejectsTierForUnknownSection(t *testing.T) {
	h := NewHarness(t)
	venueID := h.CreateVenue(VenueOpts{
		Sections: []SectionSpec{{Name: "FLOOR", Rows: 2, SeatsPerRow: 2}},
	})
	now := h.ClockNow()

	// A tier naming a section the venue does not have would silently produce seats
	// with no price, so it must be rejected up front.
	resp, err := h.AsUser(DefaultOrganizer).Post("/v1/events", map[string]any{
		"venue_id":  venueID,
		"title":     "Mismatched Tier",
		"category":  "MUSIC",
		"starts_at": now.Add(24 * time.Hour),
		"ends_at":   now.Add(26 * time.Hour),
		"onsale_at": now,
		"tiers":     []TierSpec{{Section: "NO_SUCH_SECTION", PriceCents: 1000}},
	})
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadRequest, resp.Status, "body: %s", resp.Body)
}

// A section with no tier would produce unpriceable seats, so publish must refuse.
func TestPublishRejectsSectionWithoutTier(t *testing.T) {
	h := NewHarness(t)
	venueID := h.CreateVenue(VenueOpts{
		Sections: []SectionSpec{
			{Name: "FLOOR", Rows: 2, SeatsPerRow: 2},
			{Name: "UNPRICED", Rows: 2, SeatsPerRow: 2},
		},
	})
	eventID := h.CreateEvent(venueID, EventOpts{
		Tiers: []TierSpec{{Section: "FLOOR", PriceCents: 1000}},
	})

	resp, err := h.AsUser(DefaultOrganizer).Post(pathf("/v1/events/%d/publish", eventID), nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadRequest, resp.Status, "body: %s", resp.Body)

	// And nothing was materialised — publish is all-or-nothing.
	assert.EqualValues(t, 0, h.TableCounts()["tickets"])
}

// S4: publishing twice. Asserted explicitly rather than left ambiguous: the second
// call is rejected, and critically it must not duplicate inventory.
func TestPublishTwiceIsRejectedAndDoesNotDuplicateTickets(t *testing.T) {
	h := NewHarness(t)

	venueID := h.CreateVenue(VenueOpts{
		Sections: []SectionSpec{{Name: "FLOOR", Rows: 5, SeatsPerRow: 4}},
	})
	eventID := h.CreateEvent(venueID, EventOpts{
		Tiers: []TierSpec{{Section: "FLOOR", PriceCents: 5000}},
	})

	first := h.PublishEvent(eventID)
	require.EqualValues(t, 20, first.TicketsCreated)

	resp, err := h.AsUser(DefaultOrganizer).Post(pathf("/v1/events/%d/publish", eventID), nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusConflict, resp.Status,
		"republishing should conflict, got %d: %s", resp.Status, resp.Body)

	assert.EqualValues(t, 20, h.TableCounts()["tickets"], "inventory must not be duplicated")
	assert.EqualValues(t, 20, h.GetEvent(eventID).TotalSeats)
}

// S5: organizer endpoints are not public.
func TestOrganizerEndpointsRequireAuth(t *testing.T) {
	h := NewHarness(t)
	anon := h.Anonymous()

	calls := []struct {
		name string
		do   func() (*Response, error)
	}{
		{"create venue", func() (*Response, error) {
			return anon.Post("/v1/venues", map[string]any{"name": "X", "city": "Y", "country": "GB"})
		}},
		{"add seats", func() (*Response, error) {
			return anon.Post("/v1/venues/1/seats", map[string]any{
				"sections": []SectionSpec{{Name: "FLOOR", Rows: 1, SeatsPerRow: 1}},
			})
		}},
		{"create event", func() (*Response, error) {
			return anon.Post("/v1/events", map[string]any{"venue_id": 1, "title": "X"})
		}},
		{"publish", func() (*Response, error) {
			return anon.Post("/v1/events/1/publish", nil)
		}},
	}

	for _, c := range calls {
		t.Run(c.name, func(t *testing.T) {
			resp, err := c.do()
			require.NoError(t, err)
			assert.Equal(t, http.StatusUnauthorized, resp.Status,
				"%s should require auth, got %d: %s", c.name, resp.Status, resp.Body)
		})
	}
}

// An organizer must not be able to publish another organizer's event.
func TestPublishRequiresOwnership(t *testing.T) {
	h := NewHarness(t)

	venueID := h.CreateVenue(VenueOpts{})
	eventID := h.CreateEvent(venueID, EventOpts{})

	resp, err := h.AsUser("someone-else").Post(pathf("/v1/events/%d/publish", eventID), nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, resp.Status,
		"a non-owner should not be able to publish, got %d: %s", resp.Status, resp.Body)
}
