package saml

import (
	"context"
	"errors"
	"testing"
	"time"

	crewjam "github.com/crewjam/saml"
	"github.com/redis/go-redis/v9"

	redisInit "github.com/akashc777/OneCamp/initializers/redis"
)

// When Redis can't be asked whether an assertion was used, the sign-in is
// refused: it couldn't start a session either, and a captured answer must
// never get through. tests/integration's TestASAMLAnswerSignsInOnce runs the
// replay against a real Redis.
func TestAnAssertionIsRefusedWhenItsUseCantBeChecked(t *testing.T) {
	down := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: time.Second})
	old := redisInit.RedisClient
	t.Cleanup(func() {
		redisInit.RedisClient = old
		_ = down.Close()
	})
	redisInit.RedisClient = down

	err := TakeAssertion(context.Background(), &crewjam.Assertion{ID: "id-1", IssueInstant: time.Now()})
	if err == nil || errors.Is(err, ErrAssertionUsed) {
		t.Fatalf("with Redis down: %v, want a refusal that it couldn't be checked", err)
	}
}
