//go:build integration
// +build integration

package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/akashc777/OneCamp/initializers/livekitInit"
)

// LiveKitEnv carries a live call server for one test.
type LiveKitEnv struct {
	container testcontainers.Container
	// URL is the server's HTTP address (the room service), WSURL the one
	// clients connect to.
	URL, WSURL  string
	Key, Secret string
}

// SetupLiveKit starts the call server the customer stack pins
// (livekit/livekit-server:v1.11.0) in dev mode, and points the project's
// LiveKit service at it, so the code under test talks to a real server: the
// one that refuses a publish a token does not allow, closes empty rooms, and
// disconnects whoever it is told to.
//
// Media runs over the container's own address (dev mode's single UDP port),
// which the host reaches directly on Linux.
func SetupLiveKit(t *testing.T) *LiveKitEnv {
	t.Helper()
	ensureLoggers()
	ctx := context.Background()

	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "livekit/livekit-server:v1.11.0",
			Cmd:          []string{"--dev", "--bind", "0.0.0.0"},
			ExposedPorts: []string{"7880/tcp"},
			WaitingFor: wait.ForHTTP("/").
				WithPort("7880/tcp").
				WithStartupTimeout(90 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start livekit container: %v", err)
	}
	host, err := c.Host(ctx)
	if err != nil {
		t.Fatalf("livekit host: %v", err)
	}
	port, err := c.MappedPort(ctx, "7880")
	if err != nil {
		t.Fatalf("livekit port: %v", err)
	}
	env := &LiveKitEnv{
		container: c,
		URL:       fmt.Sprintf("http://%s:%s", host, port.Port()),
		WSURL:     fmt.Sprintf("ws://%s:%s", host, port.Port()),
		// Dev mode's fixed pair.
		Key:    "devkey",
		Secret: "secret",
	}

	saved := livekitInit.LiveKitService
	livekitInit.LiveKitService = livekitInit.LiveKitServiceStruct{
		LiveKitClient: lksdk.NewRoomServiceClient(env.URL, env.Key, env.Secret),
		EgressClient:  lksdk.NewEgressClient(env.URL, env.Key, env.Secret),
		Config:        &livekitInit.LiveKitConfigStruct{HostURL: env.URL, ApiKey: env.Key, ApiSecret: env.Secret},
	}
	t.Cleanup(func() {
		livekitInit.LiveKitService = saved
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = c.Terminate(ctx)
	})
	settleBeforeTeardown(t)
	return env
}
