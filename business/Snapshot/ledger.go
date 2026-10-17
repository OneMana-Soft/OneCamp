package business

// The contributor ledger: who edited a resource between two of its versions.
//
// A version used to be credited with whoever was in the one save it was taken
// on, and the collaboration service cleared the list on every save: with a
// version every 10 minutes and a save every few seconds, history credited the
// wrong people. Every changed save now adds its people (and agents, each with
// the person who asked it) to a Redis set per resource, and the next version
// takes the whole set, atomically, so each edit is credited once.
//
// Idle marks: a resource edited and then left alone for two minutes gets a
// version (Notion's rule). A changed save marks the time in a sorted set; the
// idle sweep takes each resource whose mark is old enough, claiming it with a
// compare-and-remove so one instance takes it once.

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	redisInit "github.com/akashc777/OneCamp/initializers/redis"
	"github.com/redis/go-redis/v9"
)

// Ledger keeps who edited a resource since its last version.
type Ledger interface {
	Add(ctx context.Context, resourceKind, uuid string, who []Contributor)
	// Drain is everyone added since the last Drain, and clears them.
	Drain(ctx context.Context, resourceKind, uuid string) []Contributor
}

// Marks keeps when resources were last edited, for idle versions.
type Marks interface {
	Mark(ctx context.Context, resourceKind, uuid string, at time.Time)
	// Due is up to n resources last marked before `before`, with their marks.
	Due(ctx context.Context, resourceKind string, before time.Time, n int) []Mark
	// Claim removes a mark if it is still the one Due saw; true means this
	// caller takes the version.
	Claim(ctx context.Context, resourceKind string, m Mark) bool
}

// Mark is one resource's last edit.
type Mark struct {
	UUID string
	At   int64 // unix milliseconds
}

var (
	ledger Ledger = redisLedger{}
	marks  Marks  = redisMarks{}
)

// LedgerTTL: a ledger nobody versions for this long is dropped.
const ledgerTTL = 30 * 24 * time.Hour

func ledgerKey(kind, uuid string) string { return "versions:ledger:" + kind + ":" + uuid }
func marksKey(kind string) string        { return "versions:idle:" + kind }

type redisLedger struct{}

func (redisLedger) Add(ctx context.Context, kind, uuid string, who []Contributor) {
	if redisInit.RedisClient == nil || len(who) == 0 {
		return
	}
	members := make([]any, 0, len(who))
	for _, c := range who {
		if b, err := json.Marshal(c); err == nil && (c.UserUUID != "" || c.AgentUUID != "") {
			members = append(members, string(b))
		}
	}
	if len(members) == 0 {
		return
	}
	key := ledgerKey(kind, uuid)
	pipe := redisInit.RedisClient.TxPipeline()
	pipe.SAdd(ctx, key, members...)
	pipe.Expire(ctx, key, ledgerTTL)
	if _, err := pipe.Exec(ctx); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Snapshot ledger add %s failed: %v", key, err)
	}
}

func (redisLedger) Drain(ctx context.Context, kind, uuid string) []Contributor {
	if redisInit.RedisClient == nil {
		return nil
	}
	key := ledgerKey(kind, uuid)
	pipe := redisInit.RedisClient.TxPipeline()
	members := pipe.SMembers(ctx, key)
	pipe.Del(ctx, key)
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		helpers.LogErrorWithContext(ctx, "business/Snapshot ledger drain %s failed: %v", key, err)
		return nil
	}
	return decodeContributors(members.Val())
}

func decodeContributors(members []string) []Contributor {
	out := make([]Contributor, 0, len(members))
	for _, m := range members {
		var c Contributor
		if json.Unmarshal([]byte(m), &c) == nil {
			out = append(out, c)
		}
	}
	return sortContributors(out)
}

// sortContributors orders people before agents, each by uuid, deduplicated.
func sortContributors(in []Contributor) []Contributor {
	seen := map[Contributor]bool{}
	var out []Contributor
	for _, c := range in {
		if (c.UserUUID == "" && c.AgentUUID == "") || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if (a.AgentUUID == "") != (b.AgentUUID == "") {
			return a.AgentUUID == ""
		}
		if a.AgentUUID != b.AgentUUID {
			return a.AgentUUID < b.AgentUUID
		}
		if a.UserUUID != b.UserUUID {
			return a.UserUUID < b.UserUUID
		}
		return a.AskerUUID < b.AskerUUID
	})
	return out
}

type redisMarks struct{}

func (redisMarks) Mark(ctx context.Context, kind, uuid string, at time.Time) {
	if redisInit.RedisClient == nil {
		return
	}
	if err := redisInit.RedisClient.ZAdd(ctx, marksKey(kind), redis.Z{Score: float64(at.UnixMilli()), Member: uuid}).Err(); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Snapshot idle mark %s failed: %v", uuid, err)
	}
}

func (redisMarks) Due(ctx context.Context, kind string, before time.Time, n int) []Mark {
	if redisInit.RedisClient == nil {
		return nil
	}
	zs, err := redisInit.RedisClient.ZRangeByScoreWithScores(ctx, marksKey(kind), &redis.ZRangeBy{
		Min: "-inf", Max: strconv.FormatInt(before.UnixMilli(), 10), Count: int64(n),
	}).Result()
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Snapshot idle due failed: %v", err)
		return nil
	}
	out := make([]Mark, 0, len(zs))
	for _, z := range zs {
		if id, ok := z.Member.(string); ok {
			out = append(out, Mark{UUID: id, At: int64(z.Score)})
		}
	}
	return out
}

// claimScript removes a member only while its score is the one seen.
var claimScript = redis.NewScript(`
local s = redis.call('ZSCORE', KEYS[1], ARGV[1])
if s and tonumber(s) == tonumber(ARGV[2]) then
  return redis.call('ZREM', KEYS[1], ARGV[1])
end
return 0`)

func (redisMarks) Claim(ctx context.Context, kind string, m Mark) bool {
	if redisInit.RedisClient == nil {
		return false
	}
	n, err := claimScript.Run(ctx, redisInit.RedisClient, []string{marksKey(kind)}, m.UUID, m.At).Int()
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Snapshot idle claim %s failed: %v", m.UUID, err)
		return false
	}
	return n == 1
}
