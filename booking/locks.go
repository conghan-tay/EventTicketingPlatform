package booking

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"encore.app/internal/lockkeys"
)

// The seat lease lives in Redis rather than in Postgres.
//
// A lock is `SET lock:{ticket_id} {user_id} PX {ttl}`: acquisition is a single atomic
// Redis operation, and expiry is the key's TTL, so there is no reaper. The Postgres
// ticket row knows only AVAILABLE or BOOKED.
//
// The TTL is authoritative for expiry (decision 4). Everything else here — the
// per-event sorted set, the per-user sorted set — is a secondary index, because Redis
// has no secondary indexes and each way of asking a question needs its own key.
//
// IMPORTANT: this layer is *not* what prevents an oversell. Redis can lose a key to an
// eviction, a failover or a restart, and two users can then hold the same seat. The
// invariant is held by the conditional write in convertToSold, which only converts a
// ticket that is still AVAILABLE. A lost lock costs a compensation, never a seat sold
// twice. See docs/additionalFeatures/RedisLock.md.

// HoldRecord is the lease, stored as JSON at hold:{hold_id}.
//
// It carries the ticket set because tickets.hold_id no longer exists — purchase has no
// other way to learn which seats a hold covers.
type HoldRecord struct {
	HoldID    string  `json:"hold_id"`
	UserID    string  `json:"user_id"`
	EventID   int64   `json:"event_id"`
	TicketIDs []int64 `json:"ticket_ids"`
	Token     string  `json:"token"`
	// ExpiresAt is unix milliseconds, mirroring the lock TTL so a client can be told
	// how long it has without a second round trip.
	ExpiresAt int64 `json:"expires_at"`
	// Status is the lease's outcome: ACTIVE, CONVERTED or RELEASED.
	//
	// The record deliberately outlives the locks it describes. If it expired with
	// them, a client polling GET /v1/holds/:id a second after its lease lapsed would
	// get a 404 and no explanation, and a client that had just bought its seats would
	// get a 404 for the hold that produced the booking. The record is cheap; keeping
	// it for a grace period is what lets the endpoint answer "EXPIRED" or "CONVERTED"
	// rather than "never heard of it".
	Status string `json:"status"`
}

// Lease statuses.
const (
	HoldActive    = "ACTIVE"
	HoldConverted = "CONVERTED"
	HoldReleased  = "RELEASED"
	HoldExpired   = "EXPIRED"
)

// holdRecordGrace is how long a lease record outlives its locks, so an expired or
// completed hold remains explicable to the client that owned it.
const holdRecordGrace = time.Hour

func (h *HoldRecord) expiresAtTime() time.Time {
	return time.UnixMilli(h.ExpiresAt).UTC()
}

// AcquireResult reports what the acquire script did.
type AcquireResult struct {
	OK bool
	// QuotaExceeded means the user already holds MaxActiveHoldsPerUser live leases.
	QuotaExceeded bool
	// Lost carries the tickets that were already locked. All-or-nothing: when this is
	// non-empty nothing at all was claimed.
	Lost []int64
}

// Locker is the seat-lease store. It exists as an interface so unit tests can run
// against miniredis without the booking service knowing the difference.
type Locker interface {
	Acquire(ctx context.Context, rec *HoldRecord, now time.Time, ttl time.Duration, maxHolds int) (*AcquireResult, error)
	Release(ctx context.Context, rec *HoldRecord) (int64, error)
	Extend(ctx context.Context, rec *HoldRecord, newExpiry time.Time, ttl time.Duration) (int64, error)
	LoadHold(ctx context.Context, holdID string) (*HoldRecord, error)
	LockedTickets(ctx context.Context, eventID int64, now time.Time) (map[int64]bool, error)
	ActiveHolds(ctx context.Context, userID string, now time.Time) (int64, error)
	TicketTTL(ctx context.Context, eventID, ticketID int64) (time.Duration, error)
	ClaimIdempotency(ctx context.Context, userID, key string, ttl time.Duration) (holdID string, claimed bool, err error)
	CommitIdempotency(ctx context.Context, userID, key, holdID string, ttl time.Duration) error
	ReleaseIdempotency(ctx context.Context, userID, key string) error
	FlushAll(ctx context.Context) error
}

// idemPending marks an idempotency key that has been claimed but whose hold has not
// been written yet. A concurrent replay that sees it must retry rather than guess.
const idemPending = "PENDING"

// errIdemInFlight is returned while a concurrent request holds the key but has not yet
// finished claiming seats.
var errIdemInFlight = errors.New("idempotent request in flight")

// --- Lua ---------------------------------------------------------------------

// acquireScript claims every ticket or none.
//
// This is what replaces `SELECT ... ORDER BY ticket_id FOR UPDATE`. A Lua script runs
// to completion without interleaving, so the check-then-set across the whole seat set
// is atomic and needs no lock ordering — there is no deadlock to avoid because there
// is only ever one script running.
//
// KEYS[1] event zset, KEYS[2] user zset, KEYS[3] hold key, KEYS[4..] one lock per ticket
// ARGV[1] userID, [2] holdID, [3] now ms, [4] expiry ms, [5] ttl ms,
// ARGV[6] maxHolds, [7] hold JSON, [8] ticket count, [9] record ttl ms, [10..] ticket ids
var acquireScript = redis.NewScript(`
local eventZ, userZ, holdK = KEYS[1], KEYS[2], KEYS[3]
local userID, holdID = ARGV[1], ARGV[2]
local now      = tonumber(ARGV[3])
local expires  = tonumber(ARGV[4])
local ttlMs    = tonumber(ARGV[5])
local maxHolds = tonumber(ARGV[6])
local holdJSON = ARGV[7]
local n        = tonumber(ARGV[8])
local recordTtlMs = tonumber(ARGV[9])

-- Sorted-set members are not removed when a lock key expires: a TTL applies to the
-- whole key, not to a member of another key. Without this sweep a hot event's set
-- would grow without bound.
redis.call('ZREMRANGEBYSCORE', eventZ, '-inf', now)
redis.call('ZREMRANGEBYSCORE', userZ,  '-inf', now)

if redis.call('ZCARD', userZ) >= maxHolds then
  return {'QUOTA'}
end

-- All-or-nothing: check everything before writing anything.
local lost = {}
for i = 1, n do
  if redis.call('EXISTS', KEYS[3 + i]) == 1 then
    lost[#lost + 1] = ARGV[9 + i]
  end
end
if #lost > 0 then
  table.insert(lost, 1, 'LOST')
  return lost
end

for i = 1, n do
  redis.call('SET', KEYS[3 + i], userID, 'PX', ttlMs)
  redis.call('ZADD', eventZ, expires, ARGV[9 + i])
end
redis.call('ZADD', userZ, expires, holdID)
-- The record outlives the locks so an expired hold stays explicable.
redis.call('SET', holdK, holdJSON, 'PX', recordTtlMs)
return {'OK'}
`)

// releaseScript is a compare-and-delete: it frees only the locks this user still owns.
//
// The comparison is the whole point. A blind DEL would let a user whose lease already
// lapsed — and whose seats were taken over by somebody else — delete the new holder's
// lock. This is the operation Encore's cache package could not express, and the reason
// this layer uses a raw client.
//
// KEYS[1] event zset, KEYS[2] user zset, KEYS[3] hold key, KEYS[4..] locks
// ARGV[1] userID, [2] holdID, [3] ticket count, [4] hold JSON, [5] record ttl ms, [6..] ticket ids
var releaseScript = redis.NewScript(`
local eventZ, userZ, holdK = KEYS[1], KEYS[2], KEYS[3]
local userID, holdID = ARGV[1], ARGV[2]
local n = tonumber(ARGV[3])
local holdJSON = ARGV[4]
local recordTtlMs = tonumber(ARGV[5])

local released = 0
for i = 1, n do
  if redis.call('GET', KEYS[3 + i]) == userID then
    redis.call('DEL', KEYS[3 + i])
    redis.call('ZREM', eventZ, ARGV[5 + i])
    released = released + 1
  end
end
redis.call('ZREM', userZ, holdID)
-- Keep the record, with its outcome, rather than deleting it: a client that just
-- bought its seats still needs GET /v1/holds/:id to answer.
redis.call('SET', holdK, holdJSON, 'PX', recordTtlMs)
return released
`)

// extendScript is a compare-and-PEXPIRE, backing the D9 guarantee that a charge never
// starts with less lease left than the payment budget.
//
// It returns how many locks it extended. Fewer than expected means a seat was lost,
// and the caller must not proceed to charge for it.
//
// KEYS[1] event zset, KEYS[2] user zset, KEYS[3] hold key, KEYS[4..] locks
// ARGV[1] userID, [2] holdID, [3] new expiry ms, [4] ttl ms, [5] hold JSON,
// ARGV[6] ticket count, [7] record ttl ms, [8..] ticket ids
var extendScript = redis.NewScript(`
local eventZ, userZ, holdK = KEYS[1], KEYS[2], KEYS[3]
local userID, holdID = ARGV[1], ARGV[2]
local expires  = tonumber(ARGV[3])
local ttlMs    = tonumber(ARGV[4])
local holdJSON = ARGV[5]
local n        = tonumber(ARGV[6])
local recordTtlMs = tonumber(ARGV[7])

local extended = 0
for i = 1, n do
  if redis.call('GET', KEYS[3 + i]) == userID then
    redis.call('PEXPIRE', KEYS[3 + i], ttlMs)
    redis.call('ZADD', eventZ, expires, ARGV[7 + i])
    extended = extended + 1
  end
end

-- Only refresh the lease record if the whole set survived. A partial extension must
-- not look like a healthy hold.
if extended == n then
  redis.call('ZADD', userZ, expires, holdID)
  redis.call('SET', holdK, holdJSON, 'PX', recordTtlMs)
end
return extended
`)

// --- implementation ----------------------------------------------------------

// RedisLocker is the production Locker.
type RedisLocker struct {
	rdb *redis.Client
}

func NewRedisLocker(rdb *redis.Client) *RedisLocker { return &RedisLocker{rdb: rdb} }

// keysFor builds the key slice every script expects: the two sorted sets, the hold
// record, then one lock key per ticket in the record's order.
func keysFor(rec *HoldRecord) []string {
	keys := make([]string, 0, 3+len(rec.TicketIDs))
	keys = append(keys,
		lockkeys.EventLocks(rec.EventID),
		lockkeys.UserHolds(rec.UserID),
		lockkeys.Hold(rec.HoldID),
	)
	for _, id := range rec.TicketIDs {
		keys = append(keys, lockkeys.Lock(rec.EventID, id))
	}
	return keys
}

func ticketArgs(ids []int64) []any {
	out := make([]any, 0, len(ids))
	for _, id := range ids {
		out = append(out, strconv.FormatInt(id, 10))
	}
	return out
}

func (l *RedisLocker) Acquire(
	ctx context.Context, rec *HoldRecord, now time.Time, ttl time.Duration, maxHolds int,
) (*AcquireResult, error) {
	payload, err := json.Marshal(rec)
	if err != nil {
		return nil, err
	}

	args := []any{
		rec.UserID,
		rec.HoldID,
		now.UnixMilli(),
		rec.ExpiresAt,
		ttl.Milliseconds(),
		maxHolds,
		string(payload),
		len(rec.TicketIDs),
		(ttl + holdRecordGrace).Milliseconds(),
	}
	args = append(args, ticketArgs(rec.TicketIDs)...)

	raw, err := acquireScript.Run(ctx, l.rdb, keysFor(rec), args...).Slice()
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, errors.New("acquire script returned nothing")
	}

	switch raw[0] {
	case "OK":
		return &AcquireResult{OK: true}, nil
	case "QUOTA":
		return &AcquireResult{QuotaExceeded: true}, nil
	case "LOST":
		lost := make([]int64, 0, len(raw)-1)
		for _, v := range raw[1:] {
			s, ok := v.(string)
			if !ok {
				continue
			}
			id, convErr := strconv.ParseInt(s, 10, 64)
			if convErr != nil {
				continue
			}
			lost = append(lost, id)
		}
		return &AcquireResult{Lost: lost}, nil
	default:
		return nil, errors.New("unexpected acquire result")
	}
}

// Release frees the locks and records the lease's outcome.
//
// rec.Status is what the record is left saying, so the caller decides whether this was
// a conversion or a release. The record survives; only the locks go.
func (l *RedisLocker) Release(ctx context.Context, rec *HoldRecord) (int64, error) {
	payload, err := json.Marshal(rec)
	if err != nil {
		return 0, err
	}

	args := []any{
		rec.UserID,
		rec.HoldID,
		len(rec.TicketIDs),
		string(payload),
		holdRecordGrace.Milliseconds(),
	}
	args = append(args, ticketArgs(rec.TicketIDs)...)
	return releaseScript.Run(ctx, l.rdb, keysFor(rec), args...).Int64()
}

func (l *RedisLocker) Extend(
	ctx context.Context, rec *HoldRecord, newExpiry time.Time, ttl time.Duration,
) (int64, error) {
	extended := *rec
	extended.ExpiresAt = newExpiry.UnixMilli()
	payload, err := json.Marshal(&extended)
	if err != nil {
		return 0, err
	}

	args := []any{
		rec.UserID,
		rec.HoldID,
		newExpiry.UnixMilli(),
		ttl.Milliseconds(),
		string(payload),
		len(rec.TicketIDs),
		(ttl + holdRecordGrace).Milliseconds(),
	}
	args = append(args, ticketArgs(rec.TicketIDs)...)

	return extendScript.Run(ctx, l.rdb, keysFor(rec), args...).Int64()
}

func (l *RedisLocker) LoadHold(ctx context.Context, holdID string) (*HoldRecord, error) {
	raw, err := l.rdb.Get(ctx, lockkeys.Hold(holdID)).Bytes()
	if errors.Is(err, redis.Nil) {
		// The record expired with its TTL. Indistinguishable from never existing, and
		// both mean the same thing to a caller: there is no live lease here.
		return nil, nil
	} else if err != nil {
		return nil, err
	}

	var rec HoldRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

// LockedTickets returns the tickets in an event with a live lock. One round trip per
// read rather than one per seat, which is why the per-event sorted set exists.
func (l *RedisLocker) LockedTickets(
	ctx context.Context, eventID int64, now time.Time,
) (map[int64]bool, error) {
	return lockkeys.LockedTickets(ctx, l.rdb, eventID, now)
}

func (l *RedisLocker) ActiveHolds(ctx context.Context, userID string, now time.Time) (int64, error) {
	return l.rdb.ZCount(ctx, lockkeys.UserHolds(userID),
		"("+strconv.FormatInt(now.UnixMilli(), 10), "+inf").Result()
}

// TicketTTL reports the remaining life of one lock. Tests use it to assert the D9
// extension actually happened, replacing the old read of tickets.hold_expires_at.
func (l *RedisLocker) TicketTTL(ctx context.Context, eventID, ticketID int64) (time.Duration, error) {
	return l.rdb.PTTL(ctx, lockkeys.Lock(eventID, ticketID)).Result()
}

// ClaimIdempotency reserves the key before any seat is touched.
//
// Ordering matters and is the whole reason this is separate from Acquire. In Postgres
// the hold INSERT preceded the ticket UPDATE inside one transaction, so a concurrent
// replay lost on the unique index and claimed nothing. Redis has no cross-key
// transaction, so the key must be claimed first — otherwise two identical requests
// both miss, both acquire seats under different hold ids, and the user burns quota on
// a duplicate they never asked for.
//
// Returns (holdID, false) when the key was already resolved, ("", true) when this
// caller now owns it, and errIdemInFlight while another caller is mid-claim.
func (l *RedisLocker) ClaimIdempotency(
	ctx context.Context, userID, key string, ttl time.Duration,
) (string, bool, error) {
	k := lockkeys.Idem(userID, key)

	ok, err := l.rdb.SetNX(ctx, k, idemPending, ttl).Result()
	if err != nil {
		return "", false, err
	}
	if ok {
		return "", true, nil
	}

	existing, err := l.rdb.Get(ctx, k).Result()
	if errors.Is(err, redis.Nil) {
		// Expired between the SetNX and the Get. Treat as in-flight rather than
		// racing again; the client retries and wins cleanly.
		return "", false, errIdemInFlight
	} else if err != nil {
		return "", false, err
	}
	if existing == idemPending {
		return "", false, errIdemInFlight
	}
	return existing, false, nil
}

func (l *RedisLocker) CommitIdempotency(
	ctx context.Context, userID, key, holdID string, ttl time.Duration,
) error {
	return l.rdb.Set(ctx, lockkeys.Idem(userID, key), holdID, ttl).Err()
}

// ReleaseIdempotency frees a key whose claim failed, so a retry is not permanently
// answered with a failure that no longer applies.
func (l *RedisLocker) ReleaseIdempotency(ctx context.Context, userID, key string) error {
	return l.rdb.Del(ctx, lockkeys.Idem(userID, key)).Err()
}

func (l *RedisLocker) FlushAll(ctx context.Context) error {
	return l.rdb.FlushAll(ctx).Err()
}
