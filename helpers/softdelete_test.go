package helpers

import (
	"testing"
	"time"
)

// The three "alive" encodings are the whole point of IsSoftDeleted. Each case here
// corresponds to a write path that actually exists in this tree, so a regression
// names the path it broke rather than just failing.
func TestIsSoftDeletedAcceptsEveryEncodingOfAlive(t *testing.T) {
	epoch := time.Unix(0, 0).UTC()
	goZero := time.Time{}
	reactivated := time.Time{}.UTC() // exactly what ActivateUser writes

	cases := []struct {
		name    string
		in      *time.Time
		deleted bool
		why     string
	}{
		{"never deleted", nil, false, "a nil timestamp is the common case for a live row"},
		{"reactivated user", &reactivated, false, "ActivateUser writes time.Time{}.UTC(), not NULL; a `!= nil` check would lock them out"},
		{"go zero", &goZero, false, "the Go zero time is not a deletion"},
		{"dgraph epoch", &epoch, false, "epoch is Dgraph's unset datetime, and the graph filters treat it as present"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsSoftDeleted(c.in); got != c.deleted {
				t.Fatalf("IsSoftDeleted = %v, want %v: %s", got, c.deleted, c.why)
			}
		})
	}
}

func TestIsSoftDeletedDetectsARealDeletion(t *testing.T) {
	// A real deletion is any instant after the epoch boundary the graph compares
	// against. One second past epoch must already count, or a clock-skewed or
	// backdated write would silently resurrect a removed record.
	justAfterEpoch := time.Unix(1, 0).UTC()
	now := time.Now()

	for _, c := range []struct {
		name string
		in   *time.Time
	}{
		{"one second after epoch", &justAfterEpoch},
		{"deleted just now", &now},
	} {
		t.Run(c.name, func(t *testing.T) {
			if !IsSoftDeleted(c.in) {
				t.Fatalf("IsSoftDeleted(%v) = false, want true; a deleted record must not read as live", c.in)
			}
		})
	}
}
