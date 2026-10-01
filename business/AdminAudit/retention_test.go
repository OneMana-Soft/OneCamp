package business

import "testing"

// RetentionDays decides whether somebody's audit history starts being erased.
// Its defaults matter more than its logic: the wrong answer here quietly
// destroys evidence on an install that never asked for retention.
func TestRetentionDays(t *testing.T) {
	t.Run("unset means keep everything", func(t *testing.T) {
		// The previous behaviour, and the default. An install that upgrades and
		// configures nothing must not start erasing on its own.
		t.Setenv("AUDIT_RETENTION_DAYS", "")
		if got := RetentionDays(); got != 0 {
			t.Errorf("RetentionDays() = %d with nothing set, want 0 (keep everything)", got)
		}
	})

	t.Run("nonsense means keep everything", func(t *testing.T) {
		// A typo in an env var must fail towards keeping evidence, never towards
		// deleting it.
		for _, raw := range []string{"soon", "-5", "0", "9e9e9", " "} {
			t.Setenv("AUDIT_RETENTION_DAYS", raw)
			if got := RetentionDays(); got != 0 {
				t.Errorf("RetentionDays() = %d for %q, want 0", got, raw)
			}
		}
	})

	t.Run("below the legal floor is raised to it, not honoured", func(t *testing.T) {
		// The AI Act wants at least six months. Accepting 30 would turn a
		// compliance control into a way to fail one by accident.
		t.Setenv("AUDIT_RETENTION_DAYS", "30")
		if got := RetentionDays(); got != minRetentionDays {
			t.Errorf("RetentionDays() = %d for 30, want the %d-day floor", got, minRetentionDays)
		}
	})

	t.Run("a real window is honoured", func(t *testing.T) {
		t.Setenv("AUDIT_RETENTION_DAYS", "365")
		if got := RetentionDays(); got != 365 {
			t.Errorf("RetentionDays() = %d, want 365", got)
		}
	})

	t.Run("the floor is at least six months", func(t *testing.T) {
		if minRetentionDays < 183 {
			t.Errorf("minRetentionDays = %d, below the six months the AI Act asks for", minRetentionDays)
		}
	})
}
