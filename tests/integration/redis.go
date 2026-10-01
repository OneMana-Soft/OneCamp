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
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	MQTT "github.com/eclipse/paho.mqtt.golang"

	mqttInit "github.com/akashc777/OneCamp/initializers/mqttInit"
	redisInit "github.com/akashc777/OneCamp/initializers/redis"
)

// RedisEnv carries the live Redis for a single test.
type RedisEnv struct {
	container testcontainers.Container
	Addr      string
}

// SetupRedis starts Redis and points the project's client at it, so the code
// under test reaches the same store production does.
func SetupRedis(t *testing.T) *RedisEnv {
	t.Helper()
	ensureLoggers()
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
		t.Fatalf("start redis container: %v", err)
	}

	host, err := c.Host(ctx)
	if err != nil {
		t.Fatalf("redis host: %v", err)
	}
	port, err := c.MappedPort(ctx, "6379")
	if err != nil {
		t.Fatalf("redis port: %v", err)
	}
	addr := host + ":" + port.Port()

	if err := redisInit.ConnectRedis(&redisInit.RedisConnConfig{Host: addr}); err != nil {
		t.Fatalf("connect redis at %s: %v", addr, err)
	}

	env := &RedisEnv{container: c, Addr: addr}
	t.Cleanup(env.Close)
	return env
}

// Close terminates the container and clears the global client, so a later test in
// the same binary cannot keep writing to a dead address and read it as a miss.
func (e *RedisEnv) Close() {
	if e == nil || e.container == nil {
		return
	}
	if redisInit.RedisClient != nil {
		_ = redisInit.RedisClient.Close()
		redisInit.RedisClient = nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = e.container.Terminate(ctx)
}

// StubMqttClient gives the MQTT global a client object without a broker.
//
// The code under test publishes a "this channel changed" nudge from a
// fire-and-forget goroutine, and with the global nil that goroutine dereferences
// nil and takes the whole test binary down -- a crash, not a failure, pointing at
// MQTT rather than at whatever the test was about.
//
// A broker container would be the wrong weight here. In production MqttClient is
// never nil: ConnectMqtt assigns it from MQTT.NewClient before connecting and
// returns nil even when the connect fails, so the only path that aborts boot is a
// JWT signing error. An unconnected client is therefore the production shape under
// a broker outage, and publishing on one returns a token carrying ErrNotConnected
// instead of panicking. That is exactly what these tests want: no nil deref, and
// no pretence of testing delivery.
func StubMqttClient(t *testing.T) {
	t.Helper()
	ensureLoggers()
	// Port 1 so nothing can accidentally connect; no Connect call, so nothing waits.
	mqttInit.MqttClient = MQTT.NewClient(MQTT.NewClientOptions().AddBroker("tcp://127.0.0.1:1"))
	t.Cleanup(func() { mqttInit.MqttClient = nil })
}
