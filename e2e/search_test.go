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

type searchHit struct {
	EventID    int64     `json:"event_id"`
	Title      string    `json:"title"`
	Category   string    `json:"category"`
	StartsAt   time.Time `json:"starts_at"`
	VenueName  string    `json:"venue_name"`
	City       string    `json:"city"`
	DistanceKM *float64  `json:"distance_km"`
	Available  int64     `json:"available"`
}

type searchPage struct {
	Results    []searchHit `json:"results"`
	NextCursor string      `json:"next_cursor"`
	HasMore    bool        `json:"has_more"`
}

func (h *Harness) search(t *testing.T, params map[string]string) searchPage {
	t.Helper()
	resp, err := h.Anonymous().Get("/v1/search", Query(params))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.Status, "search failed: %s", resp.Body)

	var page searchPage
	require.NoError(t, resp.DecodeInto(&page))
	return page
}

func (p searchPage) ids() []int64 {
	out := make([]int64, 0, len(p.Results))
	for _, r := range p.Results {
		out = append(out, r.EventID)
	}
	return out
}

// H5: free-text search finds a published event.
func TestSearchByText(t *testing.T) {
	h := NewHarness(t)

	venueID := h.CreateVenue(VenueOpts{Name: "Wembley Stadium", City: "London"})
	jazz := h.CreateEvent(venueID, EventOpts{
		Title:       "Autumn Jazz Festival",
		Description: "Three days of contemporary jazz",
		Category:    "MUSIC",
	})
	h.PublishEvent(jazz)

	opera := h.CreateEvent(venueID, EventOpts{
		Title:    "La Traviata",
		Category: "OPERA",
	})
	h.PublishEvent(opera)

	// Matches on the title.
	assert.Equal(t, []int64{jazz}, h.search(t, map[string]string{"q": "jazz"}).ids())

	// Matches on the description too.
	assert.Equal(t, []int64{jazz}, h.search(t, map[string]string{"q": "contemporary"}).ids())

	// Stemming: "festivals" should still find "Festival".
	assert.Equal(t, []int64{jazz}, h.search(t, map[string]string{"q": "festivals"}).ids())

	// Case-insensitive.
	assert.Equal(t, []int64{jazz}, h.search(t, map[string]string{"q": "JAZZ"}).ids())

	// A term matching nothing returns an empty list, not an error.
	page := h.search(t, map[string]string{"q": "nonexistentzzz"})
	assert.Empty(t, page.Results)
	assert.False(t, page.HasMore)

	// No query at all lists everything on sale.
	assert.ElementsMatch(t, []int64{jazz, opera}, h.search(t, nil).ids())
}

// Search must tolerate whatever a user types. These inputs would break a naive
// to_tsquery call, which errors on unbalanced syntax.
func TestSearchToleratesHostileQueryText(t *testing.T) {
	h := NewHarness(t)
	venueID := h.CreateVenue(VenueOpts{})
	h.PublishEvent(h.CreateEvent(venueID, EventOpts{Title: "Normal Event"}))

	for _, q := range []string{"&&", "|", "!", "(", ")", "a & | b", "'", `"unclosed`, ":*", "\\", "a:::b"} {
		resp, err := h.Anonymous().Get("/v1/search", Query(map[string]string{"q": q}))
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, resp.Status,
			"query %q should not error, got %d: %s", q, resp.Status, resp.Body)
	}
}

// H6: date, location and category filters.
func TestSearchFilters(t *testing.T) {
	h := NewHarness(t)
	now := h.SetClock(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))

	london := h.CreateVenue(VenueOpts{
		Name: "O2 Arena", City: "London", Latitude: 51.5030, Longitude: 0.0032,
	})
	edinburgh := h.CreateVenue(VenueOpts{
		Name: "Usher Hall", City: "Edinburgh", Latitude: 55.9465, Longitude: -3.2050,
	})

	soon := h.CreateEvent(london, EventOpts{
		Title: "Soon In London", Category: "MUSIC",
		StartsAt: now.Add(7 * 24 * time.Hour),
	})
	later := h.CreateEvent(london, EventOpts{
		Title: "Later In London", Category: "COMEDY",
		StartsAt: now.Add(60 * 24 * time.Hour),
	})
	north := h.CreateEvent(edinburgh, EventOpts{
		Title: "Soon In Edinburgh", Category: "MUSIC",
		StartsAt: now.Add(10 * 24 * time.Hour),
	})
	for _, id := range []int64{soon, later, north} {
		h.PublishEvent(id)
	}

	t.Run("category", func(t *testing.T) {
		assert.ElementsMatch(t, []int64{soon, north},
			h.search(t, map[string]string{"category": "MUSIC"}).ids())
		assert.Equal(t, []int64{later},
			h.search(t, map[string]string{"category": "COMEDY"}).ids())
	})

	t.Run("date range", func(t *testing.T) {
		// Window covering only the first month.
		got := h.search(t, map[string]string{
			"from": now.Format(time.RFC3339),
			"to":   now.Add(30 * 24 * time.Hour).Format(time.RFC3339),
		}).ids()
		assert.ElementsMatch(t, []int64{soon, north}, got)

		// Window starting after everything.
		assert.Empty(t, h.search(t, map[string]string{
			"from": now.Add(365 * 24 * time.Hour).Format(time.RFC3339),
		}).ids())
	})

	t.Run("location radius", func(t *testing.T) {
		// 50km of London reaches the O2 but not Edinburgh (~530km away).
		got := h.search(t, map[string]string{
			"lat": "51.5074", "lng": "-0.1278", "radius_km": "50",
		})
		assert.ElementsMatch(t, []int64{soon, later}, got.ids())

		// Distance is reported and plausible for central London.
		for _, hit := range got.Results {
			require.NotNil(t, hit.DistanceKM, "distance should be reported when filtering by radius")
			assert.Less(t, *hit.DistanceKM, 50.0)
		}

		// A wide enough radius reaches Edinburgh.
		assert.ElementsMatch(t, []int64{soon, later, north}, h.search(t, map[string]string{
			"lat": "51.5074", "lng": "-0.1278", "radius_km": "1000",
		}).ids())

		// A tight radius around Edinburgh excludes London.
		assert.Equal(t, []int64{north}, h.search(t, map[string]string{
			"lat": "55.9533", "lng": "-3.1883", "radius_km": "25",
		}).ids())
	})

	t.Run("combined filters", func(t *testing.T) {
		assert.Equal(t, []int64{soon}, h.search(t, map[string]string{
			"q": "soon", "category": "MUSIC",
			"lat": "51.5074", "lng": "-0.1278", "radius_km": "50",
		}).ids())
	})
}

// A venue with no coordinates must not match a radius filter, and must not error.
func TestSearchExcludesVenuesWithoutCoordinates(t *testing.T) {
	h := NewHarness(t)

	noCoords := h.CreateVenue(VenueOpts{Name: "Mystery Venue", City: "Nowhere"})
	eventID := h.CreateEvent(noCoords, EventOpts{Title: "Unlocatable"})
	h.PublishEvent(eventID)

	// Visible without a location filter.
	assert.Equal(t, []int64{eventID}, h.search(t, nil).ids())

	// Absent once a radius is applied.
	assert.Empty(t, h.search(t, map[string]string{
		"lat": "51.5074", "lng": "-0.1278", "radius_km": "10000",
	}).ids())
}

// H7: cursor pagination walks every result exactly once.
func TestSearchCursorPagination(t *testing.T) {
	h := NewHarness(t)
	now := h.SetClock(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))
	venueID := h.CreateVenue(VenueOpts{Sections: []SectionSpec{{Name: "FLOOR", Rows: 1, SeatsPerRow: 2}}})

	const total = 25
	want := make([]int64, 0, total)
	for i := range total {
		id := h.CreateEvent(venueID, EventOpts{
			Title:    "Paged Event " + strconv.Itoa(i),
			StartsAt: now.Add(time.Duration(i+1) * 24 * time.Hour),
			Tiers:    []TierSpec{{Section: "FLOOR", PriceCents: 1000}},
		})
		h.PublishEvent(id)
		want = append(want, id)
	}

	var got []int64
	cursor := ""
	pages := 0
	for {
		params := map[string]string{"limit": "10"}
		if cursor != "" {
			params["cursor"] = cursor
		}
		page := h.search(t, params)
		pages++
		require.LessOrEqual(t, pages, 10, "pagination did not terminate")

		got = append(got, page.ids()...)
		if !page.HasMore {
			assert.Empty(t, page.NextCursor, "no cursor should be returned on the last page")
			break
		}
		require.NotEmpty(t, page.NextCursor, "has_more is true so a cursor is required")
		cursor = page.NextCursor
	}

	assert.Equal(t, 3, pages, "25 results at 10 per page")
	// Ordered by start time, and every event appears exactly once.
	assert.Equal(t, want, got, "pagination must yield every result once, in order")
}

// Events sharing a start time are the case a single-column cursor gets wrong: it
// would either skip them or repeat them at the page boundary.
func TestSearchPaginationWithIdenticalStartTimes(t *testing.T) {
	h := NewHarness(t)
	now := h.SetClock(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))
	venueID := h.CreateVenue(VenueOpts{})

	sameStart := now.Add(30 * 24 * time.Hour)
	want := make([]int64, 0, 9)
	for i := range 9 {
		id := h.CreateEvent(venueID, EventOpts{
			Title:    "Simultaneous " + strconv.Itoa(i),
			StartsAt: sameStart,
		})
		h.PublishEvent(id)
		want = append(want, id)
	}

	var got []int64
	cursor := ""
	for range 10 {
		params := map[string]string{"limit": "2"}
		if cursor != "" {
			params["cursor"] = cursor
		}
		page := h.search(t, params)
		got = append(got, page.ids()...)
		if !page.HasMore {
			break
		}
		cursor = page.NextCursor
	}

	assert.Equal(t, want, got, "identical start times must still paginate exactly once each")
}

// S6: a DRAFT event is never discoverable.
func TestSearchExcludesUnpublishedEvents(t *testing.T) {
	h := NewHarness(t)
	venueID := h.CreateVenue(VenueOpts{})

	draft := h.CreateEvent(venueID, EventOpts{Title: "Secret Lineup Reveal"})
	published := h.CreateEvent(venueID, EventOpts{Title: "Secret Public Show"})
	h.PublishEvent(published)

	got := h.search(t, map[string]string{"q": "secret"}).ids()
	assert.Equal(t, []int64{published}, got)
	assert.NotContains(t, got, draft, "a DRAFT event must never appear in search")
}

// S7: a malformed cursor is a client error, not a 500 and not a silent restart.
func TestSearchRejectsMalformedCursor(t *testing.T) {
	h := NewHarness(t)

	for _, bad := range []string{"!!!", "eyJzIjoi", "Z2FyYmFnZQ", "e30", "bm90LWpzb24"} {
		resp, err := h.Anonymous().Get("/v1/search", Query(map[string]string{"cursor": bad}))
		require.NoError(t, err)
		assert.Equal(t, http.StatusBadRequest, resp.Status,
			"cursor %q should be a 400, got %d: %s", bad, resp.Status, resp.Body)
	}
}

// S8: limit is clamped rather than honoured blindly, so one request cannot ask for
// the entire catalogue.
func TestSearchClampsLimit(t *testing.T) {
	h := NewHarness(t)
	venueID := h.CreateVenue(VenueOpts{})
	now := h.ClockNow()

	for i := range 5 {
		id := h.CreateEvent(venueID, EventOpts{
			Title:    "Limit Test " + strconv.Itoa(i),
			StartsAt: now.Add(time.Duration(i+1) * 24 * time.Hour),
		})
		h.PublishEvent(id)
	}

	// Absurdly large limit is clamped, and the request still succeeds.
	page := h.search(t, map[string]string{"limit": "100000"})
	assert.Len(t, page.Results, 5)

	// Zero and negative fall back to the default rather than returning nothing.
	assert.Len(t, h.search(t, map[string]string{"limit": "0"}).Results, 5)
	assert.Len(t, h.search(t, map[string]string{"limit": "-3"}).Results, 5)

	// A non-numeric limit is a client error.
	resp, err := h.Anonymous().Get("/v1/search", Query(map[string]string{"limit": "abc"}))
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadRequest, resp.Status, "body: %s", resp.Body)
}

func TestSearchRejectsIncompleteLocationFilter(t *testing.T) {
	h := NewHarness(t)

	cases := []map[string]string{
		{"lat": "51.5"},     // no lng
		{"lng": "-0.12"},    // no lat
		{"radius_km": "10"}, // no coordinates
		{"lat": "51.5", "lng": "-0.12", "radius_km": "0"},
		{"lat": "91", "lng": "0", "radius_km": "10"}, // out of range
		{"lat": "0", "lng": "181", "radius_km": "10"},
	}

	for _, params := range cases {
		resp, err := h.Anonymous().Get("/v1/search", Query(params))
		require.NoError(t, err)
		assert.Equal(t, http.StatusBadRequest, resp.Status,
			"params %v should be a 400, got %d: %s", params, resp.Status, resp.Body)
	}
}

func TestSearchRejectsInvertedDateRange(t *testing.T) {
	h := NewHarness(t)
	now := h.ClockNow()

	resp, err := h.Anonymous().Get("/v1/search", Query(map[string]string{
		"from": now.Add(48 * time.Hour).Format(time.RFC3339),
		"to":   now.Format(time.RFC3339),
	}))
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadRequest, resp.Status, "body: %s", resp.Body)
}

// Search results carry availability so a listing can show "sold out" without a
// second request per event.
func TestSearchReportsAvailability(t *testing.T) {
	h := NewHarness(t)

	eventID := h.SeedPublishedEvent(
		VenueOpts{Sections: []SectionSpec{{Name: "FLOOR", Rows: 3, SeatsPerRow: 4}}},
		EventOpts{Tiers: []TierSpec{{Section: "FLOOR", PriceCents: 2500}}},
	)

	page := h.search(t, map[string]string{"q": "test"})
	require.Len(t, page.Results, 1)
	assert.Equal(t, eventID, page.Results[0].EventID)
	assert.EqualValues(t, 12, page.Results[0].Available)
	assert.Equal(t, "Test Arena", page.Results[0].VenueName)
}
