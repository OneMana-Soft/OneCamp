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
	"sync"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/dgraph-io/dgo/v230"
	"github.com/dgraph-io/dgo/v230/protos/api"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
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
	conn     *grpc.ClientConn // the project's client's, closed when the test ends
	Endpoint string
}

// SetupDgraph gives the test a single-node Dgraph holding nothing, connects the
// project's client to it and applies the real schema (ConnectDgraph calls
// createSchema, so the predicates under test are the production ones, not a
// hand-written subset — a hand-written subset is how a test ends up passing
// against a shape production does not have).
//
// The server is the binary's one (shared.go). What the tests before wrote is
// dropped first, data and schema alike, so the graph the test starts from is
// a new server's.
func SetupDgraph(t *testing.T) *DgraphEnv {
	t.Helper()
	guardTestState(t)
	ensureLoggers()
	ctx := context.Background()

	var conn *grpc.ClientConn
	srv, err := theDgraph.prepared(startDgraph, func(s *dgraphServer) error {
		// Retry: Alter can still be rejected for a moment after /health is green.
		var connErr error
		deadline := time.Now().Add(90 * time.Second)
		for time.Now().Before(deadline) {
			connErr = s.admin.Alter(ctx, &api.Operation{DropAll: true})
			if connErr == nil {
				connErr, conn = dgraphInit.ConnectDgraph(ctx, &dgraphInit.DgraphConfig{Endpoint: s.endpoint})
				if connErr == nil && dgraphInit.DgraphClient != nil {
					return nil
				}
				if conn != nil {
					_ = conn.Close()
					conn = nil
				}
			}
			time.Sleep(time.Second)
		}
		return fmt.Errorf("connect dgraph at %s: %w", s.endpoint, connErr)
	})
	if err != nil {
		t.Fatalf("%v", err)
	}

	env := &DgraphEnv{conn: conn, Endpoint: srv.endpoint}
	t.Cleanup(env.Close)
	settleBeforeTeardown(t)
	return env
}

// Close leaves the project's graph client reaching nothing, as the test's own
// server going used to: a later test that never set a graph up fails to reach
// one rather than reading another test's.
func (e *DgraphEnv) Close() {
	if e == nil || e.conn == nil {
		return
	}
	dgraphInit.DgraphClient = unreachableDgraph()
	_ = e.conn.Close()
	e.conn = nil
}

// dgraphServer is the binary's Dgraph, and a client of the harness's own for
// dropping what a test leaves.
type dgraphServer struct {
	endpoint string
	admin    *dgo.Dgraph
}

var theDgraph shared[dgraphServer]

// startDgraph starts the binary's Dgraph.
func startDgraph() (*dgraphServer, func(), error) {
	ctx := context.Background()
	req := testcontainers.ContainerRequest{
		// Same image production runs (see final-compose.yml), running zero and
		// alpha in one container because a test does not need them split. Using
		// the production image rather than dgraph/standalone keeps the version
		// under test the version that ships. Pinned, not "latest": latest named
		// whatever a machine last pulled (v25.4.1 here on 18 Oct, while the stacks
		// ran v25.3.3). helpers/dgraphStackGuard_test.go holds it to the compose
		// files' version.
		Image:        "dgraph/dgraph:v25.5.0",
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
		return nil, nil, fmt.Errorf("start dgraph container: %w", err)
	}
	host, err := c.Host(ctx)
	if err != nil {
		terminate(c)
		return nil, nil, fmt.Errorf("dgraph host: %w", err)
	}
	port, err := c.MappedPort(ctx, "9080")
	if err != nil {
		terminate(c)
		return nil, nil, fmt.Errorf("dgraph port: %w", err)
	}
	endpoint := fmt.Sprintf("%s:%s", host, port.Port())
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		terminate(c)
		return nil, nil, fmt.Errorf("dgraph client: %w", err)
	}
	srv := &dgraphServer{endpoint: endpoint, admin: dgo.NewDgraphClient(api.NewDgraphClient(conn))}
	return srv, func() {
		_ = conn.Close()
		terminate(c)
	}, nil
}

var nowhere struct {
	once   sync.Once
	client *dgo.Dgraph
}

// The server's graph client in the integration binary is never nil either
// (mqtt.go says why): before the first test sets a graph up it reaches
// nothing, as it does after every test (Close).
func init() {
	if dgraphInit.DgraphClient == nil {
		dgraphInit.DgraphClient = unreachableDgraph()
	}
}

// unreachableDgraph is a graph client with nowhere to go: every call fails
// as a call to a server that went away does.
func unreachableDgraph() *dgo.Dgraph {
	nowhere.once.Do(func() {
		conn, err := grpc.NewClient("127.0.0.1:1", grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err == nil {
			nowhere.client = dgo.NewDgraphClient(api.NewDgraphClient(conn))
		}
	})
	return nowhere.client
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

// Break points the project's graph client at an address that takes no
// connections until t ends, so a test can see what a handler answers when a
// read fails rather than finds nothing. The live client comes back after.
func (e *DgraphEnv) Break(t *testing.T) {
	t.Helper()
	conn, err := grpc.NewClient("127.0.0.1:1", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("a graph client with nowhere to go: %v", err)
	}
	live := dgraphInit.DgraphClient
	dgraphInit.DgraphClient = dgo.NewDgraphClient(api.NewDgraphClient(conn))
	t.Cleanup(func() {
		dgraphInit.DgraphClient = live
		_ = conn.Close()
	})
}

// Query runs a read-only query and returns its JSON, so a test can read back
// what the code under test wrote.
func (e *DgraphEnv) Query(t *testing.T, q string) []byte {
	t.Helper()
	resp, err := dgraphInit.DgraphClient.NewReadOnlyTxn().Query(context.Background(), q)
	if err != nil {
		t.Fatalf("dgraph query: %v\nquery: %s", err, q)
	}
	return resp.GetJson()
}
