package business

// TestMain gives this package's code a logger, so a unit test that reaches a
// log line (the purge pass reports what it did) fails on its assertion rather
// than panicking on a nil logger. Mirrors business/AIAgent's bootstrap; output
// is discarded to keep test logs readable.

import (
	"io"
	"log"
	"log/slog"
	"os"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
)

func TestMain(m *testing.M) {
	if helpers.Logger == nil {
		helpers.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	}
	if helpers.MessageLogs == nil {
		discard := log.New(io.Discard, "", 0)
		helpers.MessageLogs = &helpers.Message{InfoLog: discard, ErrorLog: discard}
	}
	os.Exit(m.Run())
}
