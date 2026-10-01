//go:build integration
// +build integration

package integration

// Dgraph side of the harness.
//
// WHY THIS EXISTS. The permission half of agent-to-agent delegation
// (originCanAddressSurface) asks Dgraph whether the ORIGINATING PERSON is a member
// of the channel, or of the project owning the task. That is the check standing
// between a delegated chain and a surface the originator cannot see — the
// no-privilege-laundering invariant — and it had no test that ever reached a
// database.
//
// It could not have had one. The unit tests run with no Dgraph configured, so the
// membership lookup fails and the function returns false; they prove it DENIES and
// can say nothing about whether it correctly ALLOWS. A comment in the delegation
// test claimed the check was "exercised against a live graph in integration", but
// no such test existed and this harness was Postgres-only, so it could not be
// written. This closes that.
//
// NOTE ON GLOBALS: dgraphInit.ConnectDgraph assigns a package-level client, so
// tests using this share one graph. Seed with unique uuids per test rather than
// expecting isolation.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/dgraph-io/dgo/v230/protos/api"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// ensureLoggers points the package globals at discard sinks.
//
// Required, not cosmetic: ConnectDgraph logs on the success path, so with the
// globals nil it panics on a nil *log.Logger before returning — a test binary would
// crash rather than fail. loggerInit sets these up in the server; nothing does in a
// test. Mirrors the same guard in services/AI/main_test.go.
func ensureLoggers() {
	if helpers.Logger == nil {
		helpers.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	}
	if helpers.MessageLogs == nil {
		discard := log.New(io.Discard, "", 0)
		helpers.MessageLogs = &helpers.Message{InfoLog: discard, ErrorLog: discard}
	}
}

// DgraphEnv carries a live Dgraph for one test.
type DgraphEnv struct {
	container testcontainers.Container
	Endpoint  string
}

// SetupDgraph starts a single-node Dgraph, connects the project's client to it and
// applies the real schema (ConnectDgraph calls createSchema, so the predicates
// under test are the production ones, not a hand-written subset — a hand-written
// subset is how a test ends up passing against a shape production does not have).
func SetupDgraph(t *testing.T) *DgraphEnv {
	t.Helper()
	ensureLoggers()
	ctx := context.Background()

	req := testcontainers.ContainerRequest{
		// Same image production runs (see final-compose.yml), running zero and
		// alpha in one container because a test does not need them split. Using
		// the production image rather than dgraph/standalone keeps the version
		// under test the version that ships.
		Image:        "dgraph/dgraph:latest",
		ExposedPorts: []string{"9080/tcp", "8080/tcp"},
		Cmd: []string{"/bin/bash", "-c",
			"dgraph zero --my=localhost:5080 & " +
				"until curl -sf localhost:6080/state >/dev/null; do sleep 1; done; " +
				"dgraph alpha --my=localhost:7080 --zero=localhost:5080 --security whitelist=0.0.0.0/0"},
		// The HTTP health endpoint is the honest readiness signal; the gRPC port
		// accepts connections before Alter will succeed.
		WaitingFor: wait.ForHTTP("/health").
			WithPort("8080/tcp").
			WithStartupTimeout(180 * time.Second),
	}

	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("start dgraph container: %v", err)
	}

	host, err := c.Host(ctx)
	if err != nil {
		t.Fatalf("dgraph host: %v", err)
	}
	port, err := c.MappedPort(ctx, "9080")
	if err != nil {
		t.Fatalf("dgraph port: %v", err)
	}
	endpoint := fmt.Sprintf("%s:%s", host, port.Port())

	// Retry: Alter can still be rejected for a moment after /health is green.
	var connErr error
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		connErr, _ = dgraphInit.ConnectDgraph(ctx, &dgraphInit.DgraphConfig{Endpoint: endpoint})
		if connErr == nil && dgraphInit.DgraphClient != nil {
			break
		}
		time.Sleep(time.Second)
	}
	if connErr != nil {
		t.Fatalf("connect dgraph at %s: %v", endpoint, connErr)
	}

	env := &DgraphEnv{container: c, Endpoint: endpoint}
	t.Cleanup(env.Close)
	return env
}

// Close terminates the Dgraph container.
func (e *DgraphEnv) Close() {
	if e == nil || e.container == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = e.container.Terminate(ctx)
}

// Mutate applies a JSON set-mutation and returns the blank-node -> uid map, so a
// caller can seed a graph shape in one call and then reference the assigned uids.
func (e *DgraphEnv) Mutate(t *testing.T, setJSON any) map[string]string {
	t.Helper()
	raw, err := json.Marshal(setJSON)
	if err != nil {
		t.Fatalf("marshal mutation: %v", err)
	}
	resp, err := dgraphInit.DgraphClient.NewTxn().Mutate(context.Background(), &api.Mutation{
		SetJson:   raw,
		CommitNow: true,
	})
	if err != nil {
		t.Fatalf("dgraph mutate: %v\npayload: %s", err, raw)
	}
	return resp.GetUids()
}
