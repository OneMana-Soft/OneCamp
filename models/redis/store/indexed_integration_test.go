//go:build integration

package store_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	redisInit "github.com/akashc777/OneCamp/initializers/redis"
	"github.com/akashc777/OneCamp/models/redis/registry"
	"github.com/akashc777/OneCamp/models/redis/store"
	"github.com/akashc777/OneCamp/tests/integration"
)

// A channel's cached entries are dropped by name, every one of them and only
// that channel's: no SCAN of the keyspace, which every membership change ran.
func TestIndexedEntriesAreDroppedByName(t *testing.T) {
	ctx := context.Background()
	integration.SetupRedis(t)
	for _, u := range []string{"0x1", "0x2", "0x3"} {
		if err := store.SetJSONIndexed(ctx, registry.ChannelBasicInfo, registry.ChannelBasicInfoKeys, []string{"ch-a", u}, map[string]string{"u": u}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SetJSONIndexed(ctx, registry.ChannelBasicInfo, registry.ChannelBasicInfoKeys, []string{"ch-b", "0x1"}, map[string]string{"u": "0x1"}); err != nil {
		t.Fatal(err)
	}
	var v map[string]string
	if found, _ := store.GetJSON(ctx, registry.ChannelBasicInfo, []string{"ch-a", "0x2"}, &v); !found || v["u"] != "0x2" {
		t.Fatalf("written and read back: %v %v", found, v)
	}
	if ttl := redisInit.RedisClient.TTL(ctx, registry.ChannelBasicInfoKeys.Build("ch-a")).Val(); ttl <= 0 {
		t.Fatalf("the index expires: ttl %v", ttl)
	}
	if err := store.DeleteIndexed(ctx, registry.ChannelBasicInfo, registry.ChannelBasicInfoKeys, "ch-a"); err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"0x1", "0x2", "0x3"} {
		if found, _ := store.GetJSON(ctx, registry.ChannelBasicInfo, []string{"ch-a", u}, &v); found {
			t.Errorf("ch-a's entry for %s outlived the drop", u)
		}
	}
	if n := redisInit.RedisClient.Exists(ctx, registry.ChannelBasicInfoKeys.Build("ch-a")).Val(); n != 0 {
		t.Error("the index outlived the drop")
	}
	if found, _ := store.GetJSON(ctx, registry.ChannelBasicInfo, []string{"ch-b", "0x1"}, &v); !found {
		t.Error("another channel's entry went with it")
	}
}

// Many entries written and dropped at once leave none behind: each writer's
// own drop, made after its write, removes its entry. A drop that read the
// index and then deleted it in a second step lost an entry another writer
// recorded in between; that entry then outlived its writer's own drop (a
// member who had joined a channel read as not in it).
func TestConcurrentIndexedDropsLeaveNothingBehind(t *testing.T) {
	ctx := context.Background()
	integration.SetupRedis(t)
	const writers = 40
	for round := 0; round < 5; round++ {
		var wg sync.WaitGroup
		for i := 0; i < writers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				u := fmt.Sprintf("0x%d-%d", round, i)
				_ = store.SetJSONIndexed(ctx, registry.ChannelBasicInfo, registry.ChannelBasicInfoKeys, []string{"busy", u}, map[string]int{"is_member": 0})
				_ = store.DeleteIndexed(ctx, registry.ChannelBasicInfo, registry.ChannelBasicInfoKeys, "busy")
			}(i)
		}
		wg.Wait()
		var v map[string]int
		for i := 0; i < writers; i++ {
			u := fmt.Sprintf("0x%d-%d", round, i)
			if found, _ := store.GetJSON(ctx, registry.ChannelBasicInfo, []string{"busy", u}, &v); found {
				t.Fatalf("round %d: %s's entry outlived its own drop", round, u)
			}
		}
	}
}
