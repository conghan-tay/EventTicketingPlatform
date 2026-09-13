// Package lockkeys owns the Redis key layout for the seat lease, and the client that
// talks to the instance holding it.
//
// It exists so the format has exactly one definition. `booking` writes these keys and
// `catalog` reads one of them to render the seat map; if the two disagreed about the
// format, the seat map would quietly stop showing held seats with nothing failing.
//
// The lease itself is documented in docs/additionalFeatures/RedisLock.md.
package lockkeys

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// EventTag is the hash tag shared by every key an acquire touches for one event.
//
// On the single instance this runs against it makes no difference. It matters if this
// ever moves to Redis Cluster: a Lua script whose keys span slots is rejected
// outright, so retrofitting the tag later would be a data migration rather than a
// config change.
func EventTag(eventID int64) string {
	return "{event:" + strconv.FormatInt(eventID, 10) + "}"
}

// Lock is the lease on a single seat. Its TTL is what expires the hold.
func Lock(eventID, ticketID int64) string {
	return "lock:" + EventTag(eventID) + ":" + strconv.FormatInt(ticketID, 10)
}

// EventLocks is the per-event sorted set of locked tickets, scored by expiry.
//
// Redis has no secondary indexes, so this is how "which seats in this event are
// locked" is answered in one round trip rather than one per seat.
func EventLocks(eventID int64) string { return "locks:" + EventTag(eventID) }

// Hold is the lease record. Purchase reads it to learn which seats a hold covers,
// which tickets.hold_id used to answer.
func Hold(holdID string) string { return "hold:" + holdID }

// UserHolds is the per-user sorted set of live holds, scored by expiry. ZCOUNT over it
// is the anti-hoarding quota.
func UserHolds(userID string) string { return "user:" + userID + ":holds" }

// Idem maps an idempotency key to the hold it produced.
//
// The user id is embedded raw because identity.userIDPattern restricts it to
// [A-Za-z0-9_-], so it cannot contain the delimiter or shift it. The client-supplied
// Idempotency-Key has no such validation, so it is hashed to fixed-width hex rather
// than concatenated: an unvalidated client string should not become an unbounded
// Redis key name.
func Idem(userID, key string) string {
	sum := sha256.Sum256([]byte(key))
	return "idem:hold:" + userID + ":" + hex.EncodeToString(sum[:16])
}

// DefaultAddr is the local-development fallback, matching scripts/dev.sh.
const DefaultAddr = "127.0.0.1:6399"

// Configured reports whether an address was supplied explicitly.
//
// Callers use this to fail closed outside local development. An unset address is a
// configuration error, not a transient one: silently falling back to localhost means a
// misconfigured deployment starts cleanly and then fails on the first booking, which is
// the worst possible time to find out.
func Configured() bool { return os.Getenv("LOCK_REDIS_ADDR") != "" }

// Addr resolves the lock instance's address.
//
// This is deliberately a different Redis from Encore's managed cache cluster. That one
// runs allkeys-lru and may evict anything under memory pressure, which is precisely
// the wrong policy for a lock; this one should run noeviction.
//
// It is self-managed rather than Encore-provisioned, and that is not for want of
// trying. Encore does provision Redis, and its address is even discoverable from
// outside the process — but the Go application binary runs with a scrubbed
// environment and no `ENCORE_*` variables at all, and `encore.Meta()` exposes no
// infrastructure. There is no route from application code to an Encore-provisioned
// cache's address, so a lock store that needs a raw client has to be our own.
//
// In a cloud deployment this is the seam where an Encore secret belongs.
func Addr() string {
	if addr := os.Getenv("LOCK_REDIS_ADDR"); addr != "" {
		return addr
	}
	return DefaultAddr
}

// NewClient dials the lock instance.
func NewClient() *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr: Addr(),
		// The claim path is latency-critical and fails closed: a slow Redis must
		// surface as a fast error, not as a request that hangs holding a connection.
		DialTimeout:  2 * time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
		PoolSize:     64,
	})
}

// LockedTickets returns the tickets in an event whose lock is still live.
//
// Shared because both services need it: booking to decide what a hold still owns,
// catalog to overlay HELD onto the seat map.
func LockedTickets(
	ctx context.Context, rdb *redis.Client, eventID int64, now time.Time,
) (map[int64]bool, error) {
	// Exclusive lower bound: a lock whose expiry is exactly now has already lapsed.
	members, err := rdb.ZRangeByScore(ctx, EventLocks(eventID), &redis.ZRangeBy{
		Min: "(" + strconv.FormatInt(now.UnixMilli(), 10),
		Max: "+inf",
	}).Result()
	if err != nil {
		return nil, err
	}

	out := make(map[int64]bool, len(members))
	for _, m := range members {
		id, convErr := strconv.ParseInt(m, 10, 64)
		if convErr != nil {
			continue
		}
		out[id] = true
	}
	return out, nil
}
