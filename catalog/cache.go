package catalog

import (
	"context"
	"errors"
	"strconv"
	"sync/atomic"
	"time"

	"encore.dev/metrics"
	"encore.dev/rlog"
	"encore.dev/storage/cache"
	"golang.org/x/sync/singleflight"
)

var cluster = cache.NewCluster("catalog-cache", cache.ClusterConfig{
	// Every entry here is a derived projection that can be rebuilt from Postgres, so
	// evicting the least-recently-used key under memory pressure is always safe.
	EvictionPolicy: cache.AllKeysLRU,
})

// eventViewKey carries the event version, which is what makes invalidation free.
//
// events.version is incremented in the same transaction as any change to the event,
// so an update makes every previously cached key unreachable. There is no purge, no
// delete-on-write race, and no window where a stale entry can be served — the old key
// is simply never asked for again and expires on its own.
type eventViewKey struct {
	EventID int64
	Version int
}

// eventViewTTL is long because a versioned key is never stale: it only bounds how
// long an orphaned entry lingers after an update.
const eventViewTTL = 30 * time.Minute

var eventViewCache = cache.NewStructKeyspace[eventViewKey, EventView](cluster, cache.KeyspaceConfig{
	KeyPattern:    "event-view/:EventID/v/:Version",
	DefaultExpiry: cache.ExpireIn(eventViewTTL),
})

// availabilityKey is deliberately NOT versioned. Availability changes on every sale
// without the event itself changing, so a versioned key would never invalidate.
type availabilityKey struct {
	EventID int64
}

// availabilityTTL bounds staleness of an advisory number (D4). Five seconds is the
// figure the design commits to; the seat map is a hint, and the conditional write on
// the ticket row is the only authority.
const availabilityTTL = 5 * time.Second

var availabilityCache = cache.NewStructKeyspace[availabilityKey, AvailabilitySnapshot](cluster, cache.KeyspaceConfig{
	KeyPattern:    "availability/:EventID",
	DefaultExpiry: cache.ExpireIn(availabilityTTL),
})

// EventView is the cacheable projection of an event: everything that changes only
// when the event itself is edited.
//
// Availability is excluded on purpose. Mixing a value that changes every second into
// a key that only changes on edit would force either a useless TTL or a stale count.
type EventView struct {
	EventID     int64       `json:"event_id"`
	Title       string      `json:"title"`
	Description string      `json:"description"`
	Category    string      `json:"category"`
	Status      string      `json:"status"`
	Version     int         `json:"version"`
	StartsAt    time.Time   `json:"starts_at"`
	EndsAt      time.Time   `json:"ends_at"`
	OnsaleAt    time.Time   `json:"onsale_at"`
	Venue       Venue       `json:"venue"`
	Tiers       []TierPrice `json:"tiers"`
	TotalSeats  int64       `json:"total_seats"`
}

// TierPrice holds a tier's price and capacity. Capacity is fixed at publish time, so
// it belongs in the long-lived cached view; the available count does not.
type TierPrice struct {
	PriceTierID int64  `json:"price_tier_id"`
	Section     string `json:"section"`
	PriceCents  int64  `json:"price_cents"`
	TotalSeats  int64  `json:"total_seats"`
}

// AvailabilitySnapshot is the short-lived availability projection.
type AvailabilitySnapshot struct {
	EventID   int64                 `json:"event_id"`
	Total     int64                 `json:"total"`
	Available int64                 `json:"available"`
	Sections  []SectionAvailability `json:"sections"`
	BuiltAt   time.Time             `json:"built_at"`
}

// Metrics. The in-process atomics exist so the test suite can assert that a repeat
// read was actually served from cache; the Encore counters are what an operator would
// watch in a real deployment.
var (
	cacheHits   atomic.Int64
	cacheMisses atomic.Int64
	cacheErrors atomic.Int64
	dbRebuilds  atomic.Int64
	shedLoad    atomic.Int64

	mCacheHits = metrics.NewCounter[uint64]("catalog_cache_hits", metrics.CounterConfig{})
	mCacheMiss = metrics.NewCounter[uint64]("catalog_cache_misses", metrics.CounterConfig{})
	mCacheErrs = metrics.NewCounter[uint64]("catalog_cache_errors", metrics.CounterConfig{})
	mShedLoad  = metrics.NewCounter[uint64]("catalog_load_shed", metrics.CounterConfig{})
)

func recordHit() {
	cacheHits.Add(1)
	mCacheHits.Increment()
}

func recordMiss() {
	cacheMisses.Add(1)
	mCacheMiss.Increment()
}

// recordCacheError distinguishes a cache being *down* from a key being absent. A miss
// is normal; an error means the cache tier is unhealthy and the database is about to
// absorb traffic it was not sized for.
func recordCacheError(err error) {
	cacheErrors.Add(1)
	mCacheErrs.Increment()
	rlog.Warn("catalog cache error; falling back to the database", "err", err)
}

// rebuildGroup collapses concurrent rebuilds of the same key into one database query.
//
// Without this, a popular event expiring — or being updated during an onsale — sends
// every in-flight request to Postgres simultaneously. One rebuild serves them all.
var rebuildGroup singleflight.Group

// dbFallbackLimit caps how many requests may rebuild from Postgres at once.
//
// This is the cache-outage policy. An unrestricted fallback turns a cache outage into
// a database outage, which is strictly worse: the cache is optional, Postgres is not.
// Requests beyond the limit are shed rather than queued, so latency fails fast
// instead of collapsing everything.
const dbFallbackLimit = 64

var dbFallbackSlots = make(chan struct{}, dbFallbackLimit)

// withDBSlot runs fn if a rebuild slot is free, and reports whether it ran.
func withDBSlot(ctx context.Context, fn func() error) (ran bool, err error) {
	select {
	case dbFallbackSlots <- struct{}{}:
		defer func() { <-dbFallbackSlots }()
		dbRebuilds.Add(1)
		return true, fn()
	case <-ctx.Done():
		return false, ctx.Err()
	default:
		shedLoad.Add(1)
		mShedLoad.Increment()
		return false, nil
	}
}

// isMiss reports whether err is an ordinary cache miss rather than a cache failure.
func isMiss(err error) bool {
	return errors.Is(err, cache.Miss)
}

// cacheKeyString renders a key for single-flight deduplication. It must include the
// version, or concurrent rebuilds spanning an update would share one result.
func cacheKeyString(k eventViewKey) string {
	return "event-view/" + strconv.FormatInt(k.EventID, 10) + "/v/" + strconv.Itoa(k.Version)
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

type RefreshResponse struct {
	EventID int64 `json:"event_id"`
	Dropped int   `json:"dropped"`
}

// RefreshAvailability drops an event's availability snapshot so the next read rebuilds it.
//
// Note that the event *view* needs no equivalent: its key carries the version, so an
// edit already makes old entries unreachable. Only availability, which is keyed
// without a version and refreshed on a TTL, can benefit from being dropped early.
//
// This is a genuine operator tool — force a refresh after a manual inventory
// correction — and it is what lets tests assert on a sale being reflected without
// waiting out the TTL. The TTL is deliberately *not* shortened for tests, and sales
// deliberately do not invalidate: during a hot onsale that would delete and rebuild
// the same key ~1000 times a second, which is a stampede, not a cache. Bounding
// rebuilds to once per TTL is the point.
//
//encore:api private method=POST path=/internal/catalog/availability/:eventID/refresh
func RefreshAvailability(ctx context.Context, eventID int64) (*RefreshResponse, error) {
	dropped, err := availabilityCache.Delete(ctx, availabilityKey{EventID: eventID})
	if err != nil {
		// A cache that cannot be cleared is not a request failure: the entry expires
		// on its own within the TTL.
		recordCacheError(err)
		return &RefreshResponse{EventID: eventID, Dropped: 0}, nil
	}
	return &RefreshResponse{EventID: eventID, Dropped: dropped}, nil
}

type CacheStats struct {
	Hits     int64 `json:"hits"`
	Misses   int64 `json:"misses"`
	Errors   int64 `json:"errors"`
	Rebuilds int64 `json:"rebuilds"`
	Shed     int64 `json:"shed"`
}

// GetCacheStats reports in-process cache counters.
//
// Private, and used by the test suite to prove a repeat read was a cache hit rather
// than trusting that it was.
//
//encore:api private method=GET path=/internal/catalog/cache-stats
func GetCacheStats(ctx context.Context) (*CacheStats, error) {
	return &CacheStats{
		Hits:     cacheHits.Load(),
		Misses:   cacheMisses.Load(),
		Errors:   cacheErrors.Load(),
		Rebuilds: dbRebuilds.Load(),
		Shed:     shedLoad.Load(),
	}, nil
}

// ResetCacheStats zeroes the counters between tests.
//
//encore:api private method=POST path=/internal/catalog/cache-stats/reset
func ResetCacheStats(ctx context.Context) (*CacheStats, error) {
	cacheHits.Store(0)
	cacheMisses.Store(0)
	cacheErrors.Store(0)
	dbRebuilds.Store(0)
	shedLoad.Store(0)
	return &CacheStats{}, nil
}
