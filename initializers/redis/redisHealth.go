package redisInit

// Is the cache answering?
//
// WHAT DEPENDS ON IT. Sessions, unread counts and presence. Redis being down
// does not stop the server: requests get slower and some of them get wrong, and
// the symptom a user reports is "it logged me out" or "the badge is stuck",
// neither of which points at Redis. Boot pinged it once; this asks now.

import (
	"context"
	"fmt"

	"github.com/akashc777/OneCamp/helpers"
)

func init() {
	helpers.RegisterSystemCheck(helpers.SystemCheck{
		Name: "cache",
		Kind: helpers.CheckKindDependency,
		Describe: "Redis answers a ping, so sessions, unread counts and presence have somewhere to live. " +
			"It does not prove any particular key is correct.",
		Probe: func(ctx context.Context) error {
			if RedisClient == nil {
				return fmt.Errorf("no Redis client: the cache was never initialised, so sessions and unread counts have nowhere to live")
			}
			if err := RedisClient.Ping(ctx).Err(); err != nil {
				return fmt.Errorf("Redis did not answer a ping: %w", err)
			}
			return nil
		},
	})
}
