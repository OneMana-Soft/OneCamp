// Package store is the typed access layer for Redis. Every feature
// package goes through these helpers instead of touching
// redisInit.RedisClient directly so:
//  1. Keys are always built via the registry — no inline fmt.Sprintf
//     calls leaking into business logic.
//  2. TTLs are always the registered default unless the caller opts
//     in to a per-call override.
//  3. A nil Redis client (dev/test, hard outage) gracefully no-ops
//     where the call site can tolerate cache misses.
//  4. Sliding-window and counter-based rate limiters are implemented
//     once, not duplicated across five controllers.
//
// The store is intentionally thin. It does not introduce its own
// caching layer, schema validation, or retries. The official redis/v9
// library already handles connection pooling and timeouts; this layer
// only enforces *consistency* of usage.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/akashc777/OneCamp/helpers"
	redisInit "github.com/akashc777/OneCamp/initializers/redis"
	"github.com/akashc777/OneCamp/models/redis/registry"
)

// ErrNotConnected is returned by methods that cannot proceed without
// Redis (e.g. SSO state minting) when the global client is nil.
var ErrNotConnected = errors.New("redis store: not connected")

// IsAvailable reports whether the global Redis client is initialised.
// Most read paths should silently treat it as a cache miss; explicit
// callers (auth-state mints, distributed locks) should error out.
func IsAvailable() bool {
	return redisInit.RedisClient != nil
}

// =============================================================================
// JSON cache primitives — caches a value blob with the registry's TTL.
// =============================================================================

// SetJSON serialises value and stores it under the spec's key. Uses
// the spec's default TTL.
//
// A nil Redis client is a silent no-op so cache writes never break
// the request path.
func SetJSON(ctx context.Context, spec registry.Spec, args []string, value interface{}) error {
	return SetJSONWithTTL(ctx, spec, args, value, spec.TTL)
}

// SetJSONWithTTL is like SetJSON but allows a per-call TTL. Pass 0 to
// store without expiry (only valid for specs whose TTL is also 0).
func SetJSONWithTTL(ctx context.Context, spec registry.Spec, args []string, value interface{}, ttl time.Duration) error {
	if !IsAvailable() {
		return nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "redis store: SetJSON marshal failed key=%s err=%+v", spec.Namespace, err)
		return err
	}
	key := spec.Build(args...)
	if err := redisInit.RedisClient.Set(ctx, key, data, ttl).Err(); err != nil {
		helpers.LogErrorWithContext(ctx, "redis store: Set failed key=%s err=%+v", key, err)
		return err
	}
	return nil
}

// GetJSON loads the cached value into target. Returns (true, nil) on
// hit, (false, nil) on miss, (false, err) on transport failure.
//
// target must be a non-nil pointer — same contract as json.Unmarshal.
func GetJSON(ctx context.Context, spec registry.Spec, args []string, target interface{}) (bool, error) {
	if !IsAvailable() {
		return false, nil
	}
	key := spec.Build(args...)
	val, err := redisInit.RedisClient.Get(ctx, key).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return false, nil
		}
		helpers.LogErrorWithContext(ctx, "redis store: Get failed key=%s err=%+v", key, err)
		return false, err
	}
	if err := json.Unmarshal([]byte(val), target); err != nil {
		helpers.LogErrorWithContext(ctx, "redis store: Unmarshal failed key=%s err=%+v", key, err)
		return false, err
	}
	return true, nil
}

// SetString stores a plain string with the spec's default TTL. Used
// for refresh tokens and other non-JSON payloads.
func SetString(ctx context.Context, spec registry.Spec, args []string, value string) error {
	return SetStringWithTTL(ctx, spec, args, value, spec.TTL)
}

// SetStringWithTTL is the per-call-TTL variant.
func SetStringWithTTL(ctx context.Context, spec registry.Spec, args []string, value string, ttl time.Duration) error {
	if !IsAvailable() {
		return nil
	}
	key := spec.Build(args...)
	if err := redisInit.RedisClient.Set(ctx, key, value, ttl).Err(); err != nil {
		helpers.LogErrorWithContext(ctx, "redis store: SetString failed key=%s err=%+v", key, err)
		return err
	}
	return nil
}

// GetString loads a plain string. Returns ("", false, nil) on miss.
func GetString(ctx context.Context, spec registry.Spec, args []string) (string, bool, error) {
	if !IsAvailable() {
		return "", false, nil
	}
	key := spec.Build(args...)
	val, err := redisInit.RedisClient.Get(ctx, key).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return "", false, nil
		}
		helpers.LogErrorWithContext(ctx, "redis store: GetString failed key=%s err=%+v", key, err)
		return "", false, err
	}
	return val, true, nil
}

// AddToSet adds members to a set and refreshes its TTL.
//
// The TTL is set on every add rather than only on creation, because a set that
// accumulates over the life of something (the participants in a call) must
// outlive its LAST write, not its first. Expiring from creation would drop the
// people who joined a long meeting late.
//
// A no-op when Redis is unavailable, like every other write here: the caller
// gets a set that reads back empty, which every caller must already handle.
func AddToSet(ctx context.Context, spec registry.Spec, args []string, members ...string) error {
	if !IsAvailable() || len(members) == 0 {
		return nil
	}
	key := spec.Build(args...)
	vals := make([]interface{}, 0, len(members))
	for _, m := range members {
		vals = append(vals, m)
	}
	pipe := redisInit.RedisClient.TxPipeline()
	pipe.SAdd(ctx, key, vals...)
	pipe.Expire(ctx, key, spec.TTL)
	if _, err := pipe.Exec(ctx); err != nil {
		helpers.LogErrorWithContext(ctx, "redis store: AddToSet failed key=%s err=%+v", key, err)
		return err
	}
	return nil
}

// GetSetMembers reads a set. Returns an empty slice on miss or when Redis is
// unavailable, never nil, so callers can range without a guard.
func GetSetMembers(ctx context.Context, spec registry.Spec, args []string) ([]string, error) {
	if !IsAvailable() {
		return []string{}, nil
	}
	key := spec.Build(args...)
	members, err := redisInit.RedisClient.SMembers(ctx, key).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return []string{}, nil
		}
		helpers.LogErrorWithContext(ctx, "redis store: GetSetMembers failed key=%s err=%+v", key, err)
		return []string{}, err
	}
	if members == nil {
		return []string{}, nil
	}
	return members, nil
}

// GetInt64 reads a counter value. Returns (0, false, nil) on miss.
func GetInt64(ctx context.Context, spec registry.Spec, args []string) (int64, bool, error) {
	if !IsAvailable() {
		return 0, false, nil
	}
	key := spec.Build(args...)
	val, err := redisInit.RedisClient.Get(ctx, key).Int64()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return 0, false, nil
		}
		helpers.LogErrorWithContext(ctx, "redis store: GetInt64 failed key=%s err=%+v", key, err)
		return 0, false, err
	}
	return val, true, nil
}

// GetDelString atomically fetches and deletes; used for one-shot
// nonces (SSO state). Returns redis.Nil-mapped (""/false) on miss.
func GetDelString(ctx context.Context, spec registry.Spec, args []string) (string, bool, error) {
	if !IsAvailable() {
		return "", false, ErrNotConnected
	}
	key := spec.Build(args...)
	val, err := redisInit.RedisClient.GetDel(ctx, key).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return "", false, nil
		}
		return "", false, err
	}
	return val, true, nil
}

// Delete drops a single key. No-op when Redis is unavailable.
func Delete(ctx context.Context, spec registry.Spec, args []string) error {
	if !IsAvailable() {
		return nil
	}
	key := spec.Build(args...)
	if err := redisInit.RedisClient.Del(ctx, key).Err(); err != nil {
		helpers.LogErrorWithContext(ctx, "redis store: Del failed key=%s err=%+v", key, err)
		return err
	}
	return nil
}

// DeletePattern walks all keys matching pattern and deletes them in
// chunks. Used for bulk invalidations (e.g. all "project:tasks:UUID:*"
// variants on task CRUD).
//
// Use sparingly — SCAN is O(N) over the entire keyspace.
func DeletePattern(ctx context.Context, pattern string) error {
	if !IsAvailable() {
		return nil
	}
	iter := redisInit.RedisClient.Scan(ctx, 0, pattern, 0).Iterator()
	for iter.Next(ctx) {
		if err := redisInit.RedisClient.Del(ctx, iter.Val()).Err(); err != nil {
			helpers.LogErrorWithContext(ctx, "redis store: DeletePattern Del failed key=%s err=%+v", iter.Val(), err)
		}
	}
	return iter.Err()
}

// =============================================================================
// Counter-based rate limit (window-locked)
// =============================================================================

// RateLimitResult captures the outcome of a counter-based rate limit
// check. Allowed=false also populates RetryAfter so callers can write
// the Retry-After header.
type RateLimitResult struct {
	Allowed    bool
	Count      int64
	Limit      int
	RetryAfter time.Duration
}

// AllowFixedWindow atomically increments a counter and pins its TTL
// on the first hit. Returns Allowed=true while count <= max.
//
// This is the canonical replacement for the four near-identical
// `checkRateLimit` helpers across controllers (archive, slack-import,
// import, AI). All of them previously did:
//
//	count, _ := redis.Incr(ctx, key).Result()
//	if count == 1 { redis.Expire(ctx, key, window) }
//	return count <= max
//
// which has a subtle race: if Expire fails or the client disconnects
// between INCR and Expire, the counter never expires and the bucket
// is permanently full. We use Pipeline + ExpireNX to make the TTL
// idempotent and atomic with the increment, fixing that race.
//
// On any Redis error we fail open — better to over-serve than to
// 5xx during a Redis outage. Callers that must fail closed should
// use a Lua-scripted variant instead.
// IncrByWithTTL atomically adds delta to a counter and pins its TTL on first
// write (the window comes from the spec). Returns the new counter value.
// Best-effort: when Redis is unavailable it is a no-op returning (0, nil), and
// on a Redis error it returns (0, err); callers should treat metering as
// fail-open so a Redis blip never blocks the originating action.
func IncrByWithTTL(ctx context.Context, spec registry.Spec, args []string, delta int64) (int64, error) {
	if !IsAvailable() {
		return 0, nil
	}
	if delta == 0 {
		v, _, _ := GetInt64(ctx, spec, args)
		return v, nil
	}
	key := spec.Build(args...)
	pipe := redisInit.RedisClient.Pipeline()
	incr := pipe.IncrBy(ctx, key, delta)
	pipe.ExpireNX(ctx, key, spec.TTL)
	if _, err := pipe.Exec(ctx); err != nil {
		helpers.LogErrorWithContext(ctx, "redis store: IncrByWithTTL failed key=%s err=%+v", key, err)
		return 0, err
	}
	return incr.Val(), nil
}

func AllowFixedWindow(ctx context.Context, spec registry.Spec, args []string, limit int) RateLimitResult {
	if !IsAvailable() {
		return RateLimitResult{Allowed: true, Limit: limit}
	}
	if limit <= 0 {
		limit = 1
	}
	key := spec.Build(args...)
	window := spec.TTL

	pipe := redisInit.RedisClient.Pipeline()
	incrCmd := pipe.Incr(ctx, key)
	pipe.ExpireNX(ctx, key, window)
	if _, err := pipe.Exec(ctx); err != nil {
		helpers.LogErrorWithContext(ctx, "redis store: AllowFixedWindow pipeline failed key=%s err=%+v", key, err)
		return RateLimitResult{Allowed: true, Limit: limit}
	}
	count := incrCmd.Val()

	if int(count) > limit {
		ttl, _ := redisInit.RedisClient.TTL(ctx, key).Result()
		retry := ttl
		if retry <= 0 {
			retry = window
		}
		return RateLimitResult{
			Allowed:    false,
			Count:      count,
			Limit:      limit,
			RetryAfter: retry,
		}
	}
	return RateLimitResult{
		Allowed: true,
		Count:   count,
		Limit:   limit,
	}
}

// =============================================================================
// Sliding-window rate limit (sorted-set, by score = unix timestamp)
// =============================================================================

// AllowSlidingWindow uses a sorted set keyed by unix-second score so
// that arbitrary windows (not just full minutes) are honoured. Used
// for incoming webhook rate limits where a fair "30 per rolling 60
// seconds" matters more than a wall-clock-aligned bucket.
//
// The two-phase implementation (count-only → if-under add) avoids the
// classic "INCR-style overshoot by one" where the Nth+1 caller is
// rejected but their slot is still added.
//
// We pre-trim entries older than `windowSec` on every call so the
// sorted set never grows unbounded. Memory cost: O(limit) per key.
func AllowSlidingWindow(ctx context.Context, spec registry.Spec, args []string, limit int, windowSec int64) RateLimitResult {
	if !IsAvailable() {
		return RateLimitResult{Allowed: true, Limit: limit}
	}
	if limit <= 0 {
		limit = 1
	}
	key := spec.Build(args...)
	now := time.Now().Unix()
	windowStart := now - windowSec
	// Member is "<unix-second>-<unique-suffix>" so two requests in
	// the same second don't collide on the sorted-set member.
	member := strconv.FormatInt(now, 10) + "-" + strconv.FormatInt(time.Now().UnixNano(), 16)

	// Phase 1: trim + count
	pipe := redisInit.RedisClient.TxPipeline()
	pipe.ZRemRangeByScore(ctx, key, "-inf", strconv.FormatInt(windowStart, 10))
	countCmd := pipe.ZCard(ctx, key)
	if _, err := pipe.Exec(ctx); err != nil {
		helpers.LogErrorWithContext(ctx, "redis store: AllowSlidingWindow phase1 failed key=%s err=%+v", key, err)
		return RateLimitResult{Allowed: true, Limit: limit}
	}
	if countCmd.Val() >= int64(limit) {
		return RateLimitResult{
			Allowed:    false,
			Count:      countCmd.Val(),
			Limit:      limit,
			RetryAfter: time.Duration(windowSec) * time.Second,
		}
	}

	// Phase 2: under cap — add this request and pin TTL.
	pipe2 := redisInit.RedisClient.TxPipeline()
	pipe2.ZAdd(ctx, key, redis.Z{Score: float64(now), Member: member})
	// TTL = window + slack so a user idle for the window doesn't
	// keep the key around forever.
	pipe2.Expire(ctx, key, time.Duration(windowSec*2)*time.Second)
	if _, err := pipe2.Exec(ctx); err != nil {
		helpers.LogErrorWithContext(ctx, "redis store: AllowSlidingWindow phase2 failed key=%s err=%+v", key, err)
	}
	return RateLimitResult{
		Allowed: true,
		Count:   countCmd.Val() + 1,
		Limit:   limit,
	}
}

// =============================================================================
// Convenience: TTL inspection
// =============================================================================

// =============================================================================
// Helpers
// =============================================================================

// AppendRetryAfter formats RetryAfter as the integer seconds value
// expected by the HTTP Retry-After header (RFC 7231).
func (r RateLimitResult) RetryAfterSeconds() int {
	if r.RetryAfter <= 0 {
		return 0
	}
	secs := int(r.RetryAfter.Round(time.Second).Seconds())
	if secs < 1 {
		secs = 1
	}
	return secs
}

// UpdateJSONAtomic performs a read-modify-write on a JSON value under optimistic
// concurrency (WATCH/MULTI/EXEC), retrying on concurrent modification. It is the
// safe primitive for hot, multi-writer interactive state — e.g. tallying poll
// votes where two users clicking at the same instant would otherwise lose a vote
// under a naive load → mutate → save.
//
// `mutate` receives the current value (already JSON-unmarshalled into a fresh T;
// the zero value when the key is absent) and the `found` flag, and returns the
// new value to persist. It may be called more than once if the key changes
// mid-transaction, so it MUST be a pure function of its input (no side effects).
// The value is written back with the provided TTL.
func UpdateJSONAtomic[T any](
	ctx context.Context,
	spec registry.Spec,
	args []string,
	ttl time.Duration,
	mutate func(cur T, found bool) T,
) error {
	if !IsAvailable() {
		return ErrNotConnected
	}
	key := spec.Build(args...)
	client := redisInit.RedisClient

	const maxRetries = 8
	txf := func(tx *redis.Tx) error {
		var cur T
		found := false
		raw, err := tx.Get(ctx, key).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			return err
		}
		if err == nil {
			if uerr := json.Unmarshal([]byte(raw), &cur); uerr != nil {
				// Corrupt value — treat as absent so the mutate can rebuild it.
				cur = *new(T)
			} else {
				found = true
			}
		}

		next := mutate(cur, found)
		data, merr := json.Marshal(next)
		if merr != nil {
			return merr
		}
		// Only commit if the watched key is unchanged since the Get above.
		_, perr := tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.Set(ctx, key, data, ttl)
			return nil
		})
		return perr
	}

	for i := 0; i < maxRetries; i++ {
		err := client.Watch(ctx, txf, key)
		if err == nil {
			return nil
		}
		if errors.Is(err, redis.TxFailedErr) {
			continue // key changed concurrently — retry the read-modify-write
		}
		helpers.LogErrorWithContext(ctx, "redis store: UpdateJSONAtomic failed key=%s err=%+v", key, err)
		return err
	}
	return fmt.Errorf("redis store: UpdateJSONAtomic exhausted retries for key=%s", key)
}

// =============================================================================
// Sorted-set leaderboard helpers (ZINCRBY / ZREVRANGE)
// =============================================================================

// ScoredMember is one entry of a sorted set: its member id and accumulated
// score (e.g. a user id and their token spend for the day).
type ScoredMember struct {
	Member string
	Score  float64
}

// ZIncrBy adds delta to member's score in a registered sorted set and pins the
// spec's TTL so the set rolls over with its day bucket. Best-effort and
// fail-open: a nil client or Redis error is swallowed so metering never blocks
// the originating action.
func ZIncrBy(ctx context.Context, spec registry.Spec, args []string, member string, delta float64) {
	if !IsAvailable() || delta == 0 || member == "" {
		return
	}
	key := spec.Build(args...)
	pipe := redisInit.RedisClient.Pipeline()
	pipe.ZIncrBy(ctx, key, delta, member)
	if spec.TTL > 0 {
		pipe.Expire(ctx, key, spec.TTL)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		helpers.LogErrorWithContext(ctx, "redis store: ZIncrBy failed key=%s err=%+v", key, err)
	}
}

// ZRevRangeWithScores returns the top `limit` members of a registered sorted
// set, highest score first. Returns an empty slice on miss or when Redis is
// unavailable (read-only, best-effort).
func ZRevRangeWithScores(ctx context.Context, spec registry.Spec, args []string, limit int) []ScoredMember {
	if !IsAvailable() {
		return nil
	}
	if limit <= 0 {
		limit = 20
	}
	key := spec.Build(args...)
	res, err := redisInit.RedisClient.ZRevRangeWithScores(ctx, key, 0, int64(limit-1)).Result()
	if err != nil {
		if err != redis.Nil {
			helpers.LogErrorWithContext(ctx, "redis store: ZRevRangeWithScores failed key=%s err=%+v", key, err)
		}
		return nil
	}
	out := make([]ScoredMember, 0, len(res))
	for _, z := range res {
		member, _ := z.Member.(string)
		out = append(out, ScoredMember{Member: member, Score: z.Score})
	}
	return out
}
