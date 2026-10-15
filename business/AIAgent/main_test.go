package business

// TestMain initializes the logging globals this package's code paths use, so a
// unit test that touches code which LOGS (the concurrent lookup pre-pass, the
// steering drain, compaction) fails on its assertion rather than panicking on a
// nil logger. Mirrors services/AI's test bootstrap; output is discarded to keep
// test logs readable.

import (
	"context"
	"io"
	"log"
	"log/slog"
	"os"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/google/uuid"
)

func TestMain(m *testing.M) {
	if helpers.Logger == nil {
		helpers.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	}
	if helpers.MessageLogs == nil {
		discard := log.New(io.Discard, "", 0)
		helpers.MessageLogs = &helpers.Message{InfoLog: discard, ErrorLog: discard}
	}
	// Remote-agent tests answer from httptest servers on loopback, which only
	// an agent an admin added may reach: these tests' agents are an admin's
	// unless a test says otherwise.
	creatorIsAdmin = func(context.Context, *uuid.UUID) bool { return true }
	os.Exit(m.Run())
}
