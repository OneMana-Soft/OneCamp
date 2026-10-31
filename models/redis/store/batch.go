package store

// Several counters read, or several meters moved, in one round trip (backend
// audit #33: every AI call read four to eight counters and wrote as many,
// a round trip apiece).

import (
	"context"
	"errors"

	"github.com/akashc777/OneCamp/helpers"
	redisInit "github.com/akashc777/OneCamp/initializers/redis"
	"github.com/akashc777/OneCamp/models/redis/registry"
	"github.com/redis/go-redis/v9"
)

// CounterKey names one counter: its spec and the args that build its key.
type CounterKey struct {
	Spec registry.Spec
	Args []string
}

// GetInt64Many is GetInt64 for several counters in one MGET: each value,
// and whether it was there. All misses when Redis is unavailable.
func GetInt64Many(ctx context.Context, keys []CounterKey) ([]int64, []bool, error) {
	vals, found := make([]int64, len(keys)), make([]bool, len(keys))
	if !IsAvailable() || len(keys) == 0 {
		return vals, found, nil
	}
	built := make([]string, len(keys))
	for i, k := range keys {
		built[i] = k.Spec.Build(k.Args...)
	}
	res, err := redisInit.RedisClient.MGet(ctx, built...).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		helpers.LogErrorWithContext(ctx, "redis store: GetInt64Many failed err=%+v", err)
		return vals, found, err
	}
	for i, r := range res {
		if r == nil {
			continue
		}
		s, ok := r.(string)
		if !ok {
			continue
		}
		var n int64
		if err := redis.NewStringResult(s, nil).Scan(&n); err == nil {
			vals[i], found[i] = n, true
		}
	}
	return vals, found, nil
}

// CounterIncr is one IncrByWithTTL; ScoreIncr is one ZIncrBy.
type CounterIncr struct {
	Key   CounterKey
	Delta int64
}

type ScoreIncr struct {
	Key    CounterKey
	Member string
	Delta  float64
}

// IncrMany is IncrByWithTTL and ZIncrBy for several counters and sorted sets
// in one pipeline, with the same TTLs. Best-effort and fail-open, as they are.
func IncrMany(ctx context.Context, counters []CounterIncr, scores []ScoreIncr) {
	if !IsAvailable() || len(counters)+len(scores) == 0 {
		return
	}
	pipe := redisInit.RedisClient.Pipeline()
	for _, c := range counters {
		if c.Delta == 0 {
			continue
		}
		key := c.Key.Spec.Build(c.Key.Args...)
		pipe.IncrBy(ctx, key, c.Delta)
		pipe.ExpireNX(ctx, key, c.Key.Spec.TTL)
	}
	for _, s := range scores {
		if s.Delta == 0 || s.Member == "" {
			continue
		}
		key := s.Key.Spec.Build(s.Key.Args...)
		pipe.ZIncrBy(ctx, key, s.Delta, s.Member)
		if s.Key.Spec.TTL > 0 {
			pipe.Expire(ctx, key, s.Key.Spec.TTL)
		}
	}
	if _, err := pipe.Exec(ctx); err != nil {
		helpers.LogErrorWithContext(ctx, "redis store: IncrMany failed err=%+v", err)
	}
}

// StringSet is one SetString: the key its spec and args build, and its value.
type StringSet struct {
	Key   CounterKey
	Value string
}

// SetStringMany is SetString for several keys in one pipeline, each with its
// spec's TTL.
func SetStringMany(ctx context.Context, sets []StringSet) error {
	if !IsAvailable() || len(sets) == 0 {
		return nil
	}
	pipe := redisInit.RedisClient.Pipeline()
	for _, s := range sets {
		pipe.Set(ctx, s.Key.Spec.Build(s.Key.Args...), s.Value, s.Key.Spec.TTL)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		helpers.LogErrorWithContext(ctx, "redis store: SetStringMany failed err=%+v", err)
		return err
	}
	return nil
}
