package business

import (
	"os"
	"testing"
	"time"
)

// EmailEnabled is what tells a new workspace that it cannot send anything.
//
// Every freshly provisioned install starts with no key — make secrets clears
// RESEND_API_KEY on purpose — and nothing else reports it: /auth/forgot-password
// answers "check your email" either way, and an invitation that never arrives looks
// like a slow mail server.
func TestEmailEnabled(t *testing.T) {
	// The settings cache would otherwise carry a value across subtests and make the
	// second one read the first one's answer.
	reset := func() {
		settingsMu.Lock()
		settingsCache, settingsExpires = nil, time.Time{}
		settingsMu.Unlock()
	}

	orig, had := os.LookupEnv("RESEND_API_KEY")
	t.Cleanup(func() {
		if had {
			os.Setenv("RESEND_API_KEY", orig)
		} else {
			os.Unsetenv("RESEND_API_KEY")
		}
		reset()
	})

	t.Run("no key anywhere is off", func(t *testing.T) {
		os.Unsetenv("RESEND_API_KEY")
		reset()
		if EmailEnabled() {
			t.Fatal("a workspace with no sending key reported that it can send")
		}
	})

	t.Run("an empty key is still off", func(t *testing.T) {
		// make secrets writes RESEND_API_KEY= rather than removing the line, so the
		// variable is SET AND EMPTY on every managed install. Treating "set" as
		// "configured" would make the warning never appear on the one deployment
		// shape that always needs it.
		os.Setenv("RESEND_API_KEY", "")
		reset()
		if EmailEnabled() {
			t.Fatal("an empty key was read as configured")
		}
	})

	t.Run("a key in the environment is on", func(t *testing.T) {
		os.Setenv("RESEND_API_KEY", "re_live_abc123")
		reset()
		if !EmailEnabled() {
			t.Fatal("a configured key was not seen")
		}
	})
}
