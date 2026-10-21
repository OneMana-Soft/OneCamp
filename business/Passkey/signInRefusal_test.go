package business

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/go-webauthn/webauthn/protocol"
)

// A browser offering a passkey the server has no record of (one made for
// another install, or whose record went with the demo's nightly reset) is
// refused, and that isn't a failure: a WARN, where it was an ERROR 27 times a
// week. A record that couldn't be read still is one.
func TestAnUnknownPasskeyIsAWarningNotAnError(t *testing.T) {
	for _, c := range []struct {
		name  string
		err   error
		level string
	}{
		{"no record of it", protocol.ErrBadRequest.WithDetails("Failed to lookup Client-side Discoverable Credential: unknown passkey").WithError(errUnknownPasskey), "WARN"},
		{"its record couldn't be read", protocol.ErrBadRequest.WithDetails("Failed to lookup Client-side Discoverable Credential").WithError(errors.New("pq: connection refused")), "ERROR"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var logged bytes.Buffer
			prev := helpers.Logger
			helpers.Logger = slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
			t.Cleanup(func() { helpers.Logger = prev })

			logSignInRefusal(context.Background(), c.err)
			var line struct {
				Level string `json:"level"`
			}
			if err := json.Unmarshal(bytes.TrimSpace(logged.Bytes()), &line); err != nil {
				t.Fatalf("log %q: %v", logged.String(), err)
			}
			if line.Level != c.level {
				t.Errorf("logged at %s, want %s", line.Level, c.level)
			}
		})
	}
}
