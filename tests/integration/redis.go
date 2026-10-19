//go:build integration
// +build integration

package integration

// A real Redis, because the bug this exists to catch lives in the cache.
//
// models/redis/store gates every operation on IsAvailable(), which is "is the
// client non-nil". With no Redis the store is a silent no-op, so a test asserting
// that a permission change invalidates a cached answer PASSES WITHOUT THE FIX --
// there was no cached answer to begin with. That is the worst kind of green: the
// test names the right invariant and proves nothing about it.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	redisInit "github.com/akashc777/OneCamp/initializers/redis"
)

// RedisEnv carries the live Redis for a single test.
type RedisEnv struct {
	Addr string
}

// SetupRedis gives the test an empty Redis and points the project's client at
// it, so the code under test reaches the same store production does. The
// server is the binary's one (shared.go), emptied for the test.
func SetupRedis(t *testing.T) *RedisEnv {
	t.Helper()
	guardTestState(t)
	ensureLoggers()

	srv, err := theRedis.prepared(startRedis, func(s *redisServer) error {
		if err := s.empty(); err != nil {
			return fmt.Errorf("empty redis at %s: %w", s.addr, err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("%v", err)
	}

	if err := redisInit.ConnectRedis(&redisInit.RedisConnConfig{Host: srv.addr}); err != nil {
		t.Fatalf("connect redis at %s: %v", srv.addr, err)
	}

	env := &RedisEnv{Addr: srv.addr}
	t.Cleanup(env.Close)
	settleBeforeTeardown(t)
	return env
}

// Close clears the global client, so a later test in the same binary cannot
// keep writing to the store and read it as a miss.
func (e *RedisEnv) Close() {
	if e == nil {
		return
	}
	if redisInit.RedisClient != nil {
		_ = redisInit.RedisClient.Close()
		redisInit.RedisClient = nil
	}
}

// redisServer is the binary's Redis, and its CONFIG as it started
// (teststate.go).
type redisServer struct {
	addr     string
	baseline map[string]string
}

var theRedis shared[redisServer]

// startRedis starts the binary's Redis.
func startRedis() (*redisServer, func(), error) {
	ctx := context.Background()
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			// The image production runs (see final-compose.yml).
			Image:        "redis:7-alpine",
			ExposedPorts: []string{"6379/tcp"},
			WaitingFor:   wait.ForLog("Ready to accept connections").WithStartupTimeout(60 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("start redis container: %w", err)
	}
	host, err := c.Host(ctx)
	if err != nil {
		terminate(c)
		return nil, nil, fmt.Errorf("redis host: %w", err)
	}
	port, err := c.MappedPort(ctx, "6379")
	if err != nil {
		terminate(c)
		return nil, nil, fmt.Errorf("redis port: %w", err)
	}
	s := &redisServer{addr: host + ":" + port.Port()}
	if s.baseline, err = s.redisSettings(); err != nil {
		terminate(c)
		return nil, nil, fmt.Errorf("read redis's settings: %w", err)
	}
	return s, func() { terminate(c) }, nil
}

// empty deletes every key in every database, and every script and function
// loaded, as a new server holds none.
func (s *redisServer) empty() error {
	c := redis.NewClient(&redis.Options{Addr: s.addr})
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.FlushAll(ctx).Err(); err != nil {
		return err
	}
	if err := c.ScriptFlush(ctx).Err(); err != nil {
		return err
	}
	return c.FunctionFlush(ctx).Err()
}
