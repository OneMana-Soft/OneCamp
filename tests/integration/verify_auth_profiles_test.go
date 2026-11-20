//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	redisInit "github.com/akashc777/OneCamp/initializers/redis"
	"github.com/akashc777/OneCamp/middleware"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/redis/registry"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/redis/go-redis/v9"
)

// redisCommands counts what reaches Redis, a pipeline's commands one each,
// by name.
type redisCommands struct {
	mu    sync.Mutex
	names []string
	// mgets are the keys of each MGET, in order.
	mgets [][]string
}

func (c *redisCommands) DialHook(next redis.DialHook) redis.DialHook { return next }

func (c *redisCommands) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		c.mu.Lock()
		c.names = append(c.names, cmd.Name())
		if cmd.Name() == "mget" {
			var keys []string
			for _, k := range cmd.Args()[1:] {
				keys = append(keys, fmt.Sprint(k))
			}
			c.mgets = append(c.mgets, keys)
		}
		c.mu.Unlock()
		return next(ctx, cmd)
	}
}

func (c *redisCommands) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		c.mu.Lock()
		for _, cmd := range cmds {
			c.names = append(c.names, cmd.Name())
		}
		c.mu.Unlock()
		return next(ctx, cmds)
	}
}

// take returns the commands counted so far and starts again, less a new
// connection's handshake (HELLO, CLIENT SETINFO), which no request makes.
func (c *redisCommands) take() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, n := range c.names {
		if n != "hello" && n != "client" {
			out = append(out, n)
		}
	}
	c.names = nil
	return out
}

// countRedis swaps in a client to the same Redis that counts every command,
// for the rest of the test.
func countRedis(t *testing.T) *redisCommands {
	t.Helper()
	saved := redisInit.RedisClient
	client := redis.NewClient(saved.Options())
	counter := &redisCommands{}
	client.AddHook(counter)
	redisInit.RedisClient = client
	t.Cleanup(func() {
		redisInit.RedisClient = saved
		_ = client.Close()
	})
	return counter
}

// verifyAuthMember stands up a member and returns a request that passes
// VerifyAuth for them, and their id.
func verifyAuthMember(t *testing.T, email string) (func() int, string) {
	t.Helper()
	t.Setenv("JWT_SECRET", "verify-auth-profiles-integration-secret")
	t.Setenv("ALLOWED_USERS", "")
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	integration.SetupDgraph(t)
	id, _, err := userBusiness.CreateUserWithMethod(ctx, email, "Verify Auth", nil, userModels.AuthMethodEmail, false)
	if err != nil {
		t.Fatal(err)
	}
	session, err := helpers.SignSessionToken(id.String(), helpers.TokenTypeAccess, time.Now().Add(10*time.Minute).Unix())
	if err != nil {
		t.Fatal(err)
	}
	h := middleware.VerifyAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Context().Value(helpers.UserInfoContextKey) == nil {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	call := func() int {
		req := httptest.NewRequest(http.MethodGet, "/user/profile", nil)
		req.AddCookie(&http.Cookie{Name: "Authorization", Value: session})
		req.AddCookie(&http.Cookie{Name: "DeviceId", Value: "device-1"})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	return call, id.String()
}

// A1-01: with both profiles cached, VerifyAuth reads them in ONE round trip
// (one MGET), not a GET apiece.
func TestVerifyAuthReadsBothWarmProfilesInOneRedisCommand(t *testing.T) {
	call, id := verifyAuthMember(t, "warm-profiles@example.test")
	if code := call(); code != http.StatusOK {
		t.Fatalf("first request (fills the cache): %d", code)
	}
	counter := countRedis(t)
	if code := call(); code != http.StatusOK {
		t.Fatalf("warm request: %d", code)
	}
	if got := counter.take(); len(got) != 1 || got[0] != "mget" {
		t.Fatalf("a warm VerifyAuth makes one MGET, got %d commands %v", len(got), got)
	}
	want := fmt.Sprint([]string{registry.UserProfile.Build(id), registry.UserDgraphProfile.Build(id), registry.TaskReadGeneration.Build()})
	if got := fmt.Sprint(counter.mgets); got != "["+want+"]" {
		t.Fatalf("the MGET reads %s, want both profiles and the task-read generation %s", got, want)
	}
}

// A1-01: a profile that isn't cached is loaded by its own loader and put in
// the cache, whichever of the two is missing; the next request is warm. The
// MGET's miss is the only read of its key: a cold profile costs one SET more
// than a warm request, never a second GET (CR-377 F2).
func TestVerifyAuthLoadsAColdProfileAndFillsTheCache(t *testing.T) {
	call, id := verifyAuthMember(t, "cold-profiles@example.test")
	ctx := context.Background()
	keys := map[string]string{
		"account": registry.UserProfile.Build(id),
		"graph":   registry.UserDgraphProfile.Build(id),
	}
	counter := countRedis(t)
	for _, cold := range [][]string{{"account", "graph"}, {"account"}, {"graph"}} {
		for _, k := range cold {
			if err := redisInit.RedisClient.Del(ctx, keys[k]).Err(); err != nil {
				t.Fatal(err)
			}
		}
		counter.take()
		if code := call(); code != http.StatusOK {
			t.Fatalf("cold %v: %d", cold, code)
		}
		want := []string{"mget"}
		for range cold {
			want = append(want, "set")
		}
		if got := counter.take(); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("cold %v: VerifyAuth made %d Redis commands %v, want %d %v", cold, len(got), got, len(want), want)
		}
		for name, key := range keys {
			if n := redisInit.RedisClient.Exists(ctx, key).Val(); n != 1 {
				t.Fatalf("cold %v: the %s profile was not put back in the cache", cold, name)
			}
		}
	}
}
