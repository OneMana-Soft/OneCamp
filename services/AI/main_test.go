package ai

import (
	"io"
	"log"
	"log/slog"
	"os"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/safefetch"
	"github.com/akashc777/OneCamp/tests/integration"
)

// TestMain initializes the minimal helpers logging globals so unit tests that
// exercise code paths which log (e.g. reindex buffer teardown) don't panic on
// a nil logger. Output is discarded to keep test logs clean. The tests run
// through integration.Main, which stops the harness's servers once an
// integration run's tests are done (a unit run starts none).
func TestMain(m *testing.M) {
	if helpers.Logger == nil {
		helpers.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	}
	if helpers.MessageLogs == nil {
		discard := log.New(io.Discard, "", 0)
		helpers.MessageLogs = &helpers.Message{InfoLog: discard, ErrorLog: discard}
	}
	os.Exit(integration.Main(m))
}

// adminAdded is the address rule for a remote agent an admin added, which may
// be on this server's own network: the tests' remote agents answer from
// httptest servers on loopback.
var adminAdded = safefetch.EndpointPolicy(true)
