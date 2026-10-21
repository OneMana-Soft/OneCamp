//go:build integration

package ai

import (
	"context"
	"sync/atomic"
	"testing"

	redisInit "github.com/akashc777/OneCamp/initializers/redis"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/redis/go-redis/v9"
)

// countingHook counts the round trips a Redis client makes: a command, or a
// pipeline of them.
type countingHook struct{ n atomic.Int32 }

func (h *countingHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *countingHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error { h.n.Add(1); return next(ctx, cmd) }
}
func (h *countingHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error { h.n.Add(1); return next(ctx, cmds) }
}

// Backend audit #33: every AI call read each cap that applied (the
// person's, the agent's, the channel's, the workspace's) in a round trip of
// its own, and wrote each meter in one or two more. It reads them in one
// round trip and writes them in one, and still says which cap was reached.
// Run: go test -tags=integration ./services/AI/ -run TestAnAICallReadsAndMetersItsBudgetsInOneRoundTripEach -v
func TestAnAICallReadsAndMetersItsBudgetsInOneRoundTripEach(t *testing.T) {
	integration.SetupRedis(t)
	t.Setenv("AI_WORKSPACE_DAILY_TOKEN_BUDGET", "1000")
	t.Setenv("AI_USER_DAILY_TOKEN_BUDGET", "500")
	hook := &countingHook{}
	redisInit.RedisClient.AddHook(hook)

	ctx := WithChannelBudget(WithAgentBudget(WithActor(context.Background(), "u-maya"), "agent-1", 300), "chan-1", 400)
	hook.n.Store(0)
	if over, _ := TokenBudgetExceeded(ctx); over {
		t.Fatal("nothing spent yet")
	}
	if n := hook.n.Load(); n != 1 {
		t.Errorf("reading four caps took %d round trips", n)
	}
	hook.n.Store(0)
	RecordTokenSpend(ctx, 350)
	if n := hook.n.Load(); n != 1 {
		t.Errorf("metering a call took %d round trips", n)
	}
	// The agent's cap (300) is reached first among the more specific ones.
	if over, scope := TokenBudgetExceeded(ctx); !over || scope != BudgetScopeAgent {
		t.Errorf("after 350 tokens: over %v, scope %v; want the agent's", over, scope)
	}
	if used := AgentTokenUsageToday(context.Background(), "agent-1"); used != 350 {
		t.Errorf("the agent's meter reads %d", used)
	}
}
