package helpers

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The registry exists so a shared package can sweep records it must not import.
// What has to hold: what registers is what gets swept, in a stable order, and an
// edition that links nothing registers nothing.
func TestRetentionSweepers(t *testing.T) {
	reset := func() {
		retentionMu.Lock()
		retentionSweepers = map[string]RetentionSweeper{}
		retentionMu.Unlock()
	}
	reset()
	t.Cleanup(reset)

	// The AI-free edition links no package that registers, so the sweep finds
	// nothing to do. Not an error condition: it is the carve working.
	if got := RetentionSweepers(); len(got) != 0 {
		t.Fatalf("empty registry returned %d sweeper(s)", len(got))
	}

	var sweptWith time.Time
	RegisterRetentionSweeper("agent run", "rows kept", func(_ context.Context, cutoff time.Time) (int64, error) {
		sweptWith = cutoff
		return 3, nil
	})
	RegisterRetentionSweeper("archive", "rows kept", func(context.Context, time.Time) (int64, error) {
		return 0, errors.New("nope")
	})

	// Nothing registered under an empty name or a nil func: a registration that
	// silently records a crash for later is worse than one that does not happen.
	RegisterRetentionSweeper("", "", func(context.Context, time.Time) (int64, error) { return 0, nil })
	RegisterRetentionSweeper("nil", "", nil)

	got := RetentionSweepers()
	if len(got) != 2 {
		t.Fatalf("expected 2 sweepers, got %d", len(got))
	}
	// Sorted, so the operator's log reads the same way on every pass.
	if got[0].Name != "agent run" || got[1].Name != "archive" {
		t.Fatalf("unstable order: %q, %q", got[0].Name, got[1].Name)
	}
	// The note travels with the sweeper: it is what the log line uses to say
	// what survived, and a store that redacts without saying so reads as
	// tampering.
	if got[0].Note == "" {
		t.Fatal("sweeper lost its note")
	}

	cutoff := time.Now().AddDate(0, 0, -190)
	n, err := got[0].Sweep(context.Background(), cutoff)
	if err != nil || n != 3 {
		t.Fatalf("sweep returned (%d, %v), want (3, nil)", n, err)
	}
	if !sweptWith.Equal(cutoff) {
		t.Fatalf("sweeper got cutoff %v, want %v", sweptWith, cutoff)
	}
	// A failing store reports its error rather than aborting the pass, so one
	// broken store cannot leave every other one unswept for another day.
	if _, err := got[1].Sweep(context.Background(), cutoff); err == nil {
		t.Fatal("expected the failing sweeper to return its error")
	}

	// Re-registering replaces, so a package that registers twice cannot sweep
	// twice.
	RegisterRetentionSweeper("agent run", "rows kept", func(context.Context, time.Time) (int64, error) { return 9, nil })
	if got = RetentionSweepers(); len(got) != 2 {
		t.Fatalf("re-registration added a sweeper: %d", len(got))
	}
	if n, _ = got[0].Sweep(context.Background(), cutoff); n != 9 {
		t.Fatalf("re-registration did not replace: got %d", n)
	}
}
