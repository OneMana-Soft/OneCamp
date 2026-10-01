package ai

import (
	"io"
	"log"
	"log/slog"
	"os"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
)

// TestMain initializes the minimal helpers logging globals so unit tests that
// exercise code paths which log (e.g. reindex buffer teardown) don't panic on
// a nil logger. Output is discarded to keep test logs clean.
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
