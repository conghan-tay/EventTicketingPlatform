//go:build e2e

package e2e

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A repeat read must be served from cache. Asserted through the counters rather than
// inferred from latency, which would be flaky and prove nothing.
func TestEventDetailIsCachedOnRepeatRead(t *testing.T) {
	h := NewHarness(t)
	eventID := h.SeedSellableEvent(t, 4, 5, 3000)

	// First read populates both the versioned view and the availability snapshot.
	first := h.GetEvent(eventID)
	require.Equal(t, "Test Event", first.Title)

	afterFirst := h.CacheStats()
	assert.GreaterOrEqual(t, afterFirst.Misses, int64(2),
		"the first read should miss both the event view and the availability snapshot")

	// Second read of the same version should hit.
	second := h.GetEvent(eventID)
	assert.Equal(t, first.Version, second.Version)

	afterSecond := h.CacheStats()
	assert.Greater(t, afterSecond.Hits, afterFirst.Hits,
		"a repeat read must be served from cache, got hits %d -> %d",
		afterFirst.Hits, afterSecond.Hits)
	assert.Equal(t, afterFirst.Misses, afterSecond.Misses,
		"a repeat read must not miss again")
}

// The versioned-key property: an edit is visible on the very next read, with no
// explicit purge anywhere in the system.
func TestEventUpdateBumpsVersionAndIsImmediatelyVisible(t *testing.T) {
	h := NewHarness(t)
	eventID := h.SeedSellableEvent(t, 2, 3, 2000)

	before := h.GetEvent(eventID)
	require.Equal(t, "Test Event", before.Title)

	// Warm the cache so a stale entry exists for the old version.
	h.GetEvent(eventID)

	updated := h.UpdateEvent(eventID, UpdateEventOpts{
		Title:       StrPtr("Renamed Show"),
		Description: StrPtr("A brand new description"),
	})
	assert.Equal(t, before.Version+1, updated.Version,
		"an edit must bump the version in the same transaction")

	// No purge was issued. The next read is correct purely because the key changed.
	after := h.GetEvent(eventID)
	assert.Equal(t, "Renamed Show", after.Title)
	assert.Equal(t, "A brand new description", after.Description)
	assert.Equal(t, updated.Version, after.Version)

	// The event's capacity is untouched by a metadata edit.
	assert.Equal(t, before.TotalSeats, after.TotalSeats)
}

// Sales must be reflected without an edit, since availability is cached separately
// from the versioned event body. The event version stays put; only the count moves.
func TestAvailabilityChangesWithoutVersionBump(t *testing.T) {
	h := NewHarness(t)
	eventID := h.SeedSellableEvent(t, 2, 5, 1000) // 10 seats

	before := h.GetEvent(eventID)
	require.EqualValues(t, 10, before.Available)

	seats := h.AvailableTicketIDs(eventID, 3)
	h.Purchase(h.CreateHold(eventID, seats, HoldOpts{}), PurchaseOpts{})

	// Within the TTL the cached snapshot is still served. That is the design (D4):
	// availability is advisory and bounded-stale, not immediate.
	assert.EqualValues(t, 10, h.GetEvent(eventID).Available,
		"inside the TTL the advisory count may still be the old one")

	h.RefreshAvailability(eventID)

	after := h.GetEvent(eventID)
	assert.EqualValues(t, 7, after.Available, "three seats were sold")
	assert.Equal(t, before.Version, after.Version,
		"a sale must not bump the event version; only an edit does")
	assert.EqualValues(t, before.TotalSeats, after.TotalSeats)
	assert.True(t, after.Stale, "availability is advisory by design (D4)")
}

// HTTP validators: the cheapest possible response at 80k RPS is one with no body.
func TestEventDetailConditionalRequest(t *testing.T) {
	h := NewHarness(t)
	eventID := h.SeedSellableEvent(t, 2, 2, 1500)

	first := h.GetEventRaw(eventID, "")
	require.Equal(t, http.StatusOK, first.Status, "body: %s", first.Body)

	etag := first.Header.Get("ETag")
	require.NotEmpty(t, etag, "an ETag is required for revalidation")
	assert.True(t, len(etag) > 2 && etag[0] == 'W',
		"expected a weak validator, got %q", etag)

	cc := first.Header.Get("Cache-Control")
	assert.Contains(t, cc, "max-age=", "a CDN needs a freshness lifetime, got %q", cc)
	assert.Contains(t, cc, "public", "event detail is shared, not per-user")

	// Revalidating with the same tag returns 304 and no body.
	second := h.GetEventRaw(eventID, etag)
	assert.Equal(t, http.StatusNotModified, second.Status, "body: %s", second.Body)
	assert.Empty(t, second.Body, "a 304 must carry no body")

	// The wildcard also matches.
	assert.Equal(t, http.StatusNotModified, h.GetEventRaw(eventID, "*").Status)

	// A stale tag gets the full body back.
	third := h.GetEventRaw(eventID, `W/"0000000000000000"`)
	assert.Equal(t, http.StatusOK, third.Status)
	assert.NotEmpty(t, third.Body)
}

// After an edit the old validator must no longer match, or a revalidating client
// would keep showing the previous title indefinitely.
func TestETagChangesAfterUpdate(t *testing.T) {
	h := NewHarness(t)
	eventID := h.SeedSellableEvent(t, 2, 2, 1500)

	first := h.GetEventRaw(eventID, "")
	oldETag := first.Header.Get("ETag")
	require.NotEmpty(t, oldETag)

	h.UpdateEvent(eventID, UpdateEventOpts{Title: StrPtr("Different Title")})

	revalidated := h.GetEventRaw(eventID, oldETag)
	assert.Equal(t, http.StatusOK, revalidated.Status,
		"a stale validator must not produce a 304 after an edit")

	newETag := revalidated.Header.Get("ETag")
	assert.NotEqual(t, oldETag, newETag, "the validator must change when the body does")
}

// A sale changes the body, so the validator must change too — otherwise a client
// revalidating a ticketing page would never see a section sell out.
func TestETagChangesWhenAvailabilityChanges(t *testing.T) {
	h := NewHarness(t)
	eventID := h.SeedSellableEvent(t, 2, 3, 1000)

	first := h.GetEventRaw(eventID, "")
	oldETag := first.Header.Get("ETag")
	require.NotEmpty(t, oldETag)

	seats := h.AvailableTicketIDs(eventID, 2)
	h.Purchase(h.CreateHold(eventID, seats, HoldOpts{}), PurchaseOpts{})

	h.RefreshAvailability(eventID)

	after := h.GetEventRaw(eventID, oldETag)
	assert.Equal(t, http.StatusOK, after.Status,
		"selling seats changes the representation, so the old validator must not match")
	assert.NotEqual(t, oldETag, after.Header.Get("ETag"))
}

func TestUpdateEventValidation(t *testing.T) {
	h := NewHarness(t)
	eventID := h.SeedSellableEvent(t, 2, 2, 1000)

	t.Run("empty payload", func(t *testing.T) {
		resp, err := h.AsUser(DefaultOrganizer).Patch(pathf("/v1/events/%d", eventID), map[string]any{})
		require.NoError(t, err)
		assert.Equal(t, http.StatusBadRequest, resp.Status, "body: %s", resp.Body)
	})

	t.Run("blank title", func(t *testing.T) {
		resp := h.TryUpdateEvent(eventID, UpdateEventOpts{Title: StrPtr("   ")})
		assert.Equal(t, http.StatusBadRequest, resp.Status, "body: %s", resp.Body)
	})

	t.Run("requires auth", func(t *testing.T) {
		resp, err := h.Anonymous().Patch(pathf("/v1/events/%d", eventID),
			map[string]any{"title": "Hijacked"})
		require.NoError(t, err)
		assert.Equal(t, http.StatusUnauthorized, resp.Status, "body: %s", resp.Body)
	})

	t.Run("requires ownership", func(t *testing.T) {
		resp := h.TryUpdateEvent(eventID, UpdateEventOpts{
			Title: StrPtr("Hijacked"), User: "not-the-organizer",
		})
		assert.Equal(t, http.StatusNotFound, resp.Status, "body: %s", resp.Body)

		// And the title is unchanged.
		assert.Equal(t, "Test Event", h.GetEvent(eventID).Title)
	})
}

// A partial update must not blank the fields it omits.
func TestUpdateEventIsPartial(t *testing.T) {
	h := NewHarness(t)
	venueID := h.CreateVenue(VenueOpts{})
	eventID := h.CreateEvent(venueID, EventOpts{
		Title:       "Original Title",
		Description: "Original description",
		Category:    "MUSIC",
	})
	h.PublishEvent(eventID)

	h.UpdateEvent(eventID, UpdateEventOpts{Title: StrPtr("New Title")})

	ev := h.GetEvent(eventID)
	assert.Equal(t, "New Title", ev.Title)
	assert.Equal(t, "Original description", ev.Description, "description must be preserved")
	assert.Equal(t, "MUSIC", ev.Category, "category must be preserved")
}

// An unknown event must not populate a cache entry or leak existence.
func TestUnknownEventIsNotCached(t *testing.T) {
	h := NewHarness(t)

	for range 3 {
		resp := h.GetEventRaw(999999, "")
		assert.Equal(t, http.StatusNotFound, resp.Status, "body: %s", resp.Body)
	}

	stats := h.CacheStats()
	assert.Zero(t, stats.Hits, "a missing event must never be served from cache")
}

// A DRAFT event stays invisible through the cached read path too.
func TestDraftEventIsNotVisibleThroughCache(t *testing.T) {
	h := NewHarness(t)
	venueID := h.CreateVenue(VenueOpts{})
	draft := h.CreateEvent(venueID, EventOpts{})

	for range 2 {
		assert.Equal(t, http.StatusNotFound, h.GetEventRaw(draft, "").Status)
	}

	// Publishing makes it visible immediately.
	h.PublishEvent(draft)
	assert.Equal(t, http.StatusOK, h.GetEventRaw(draft, "").Status)
}
