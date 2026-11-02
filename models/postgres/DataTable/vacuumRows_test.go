package models

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/models/pgexec"
)

// A flush that fails still has the VACUUM and the ANALYZE tried, and says
// so with their own errors: the count after a fill never does less than the
// ANALYZE it always ran before V-150 (V-155).
func TestAFailedFlushStillVacuumsAndAnalyzes(t *testing.T) {
	was, wasFlush := postgresInit.DBConn, flushCommits
	t.Cleanup(func() { postgresInit.DBConn, flushCommits = was, wasFlush })
	postgresInit.DBConn = &postgresInit.DB{} // no pool: each statement tried says ErrNoDatabase
	stuck := errors.New("the log isn't on disk yet")
	flushCommits = func(context.Context, time.Duration) error { return stuck }

	err := VacuumRows(context.Background())
	if !errors.Is(err, stuck) {
		t.Fatalf("VacuumRows = %v, want the flush's error", err)
	}
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		t.Fatalf("VacuumRows = %v: only the flush's error, so neither the VACUUM nor the ANALYZE was tried", err)
	}
	tried := 0
	for _, e := range joined.Unwrap() {
		if errors.Is(e, pgexec.ErrNoDatabase) {
			tried++
		}
	}
	if tried != 2 {
		t.Fatalf("VacuumRows = %v: %d statements tried, want the VACUUM and the ANALYZE", err, tried)
	}
}
