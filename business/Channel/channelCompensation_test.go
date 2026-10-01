package business

import (
	"testing"
	"time"
)

// Restoring a channel must distinguish "was never archived" from "was archived at time T".
//
// The Postgres column is nullable, but the model carries a plain time.Time, so a channel that
// was never archived arrives as the ZERO time rather than as an absence. Writing that straight
// back sets deleted_at to year 1 — which archives a live channel while attempting to undo a
// failed rename, turning a recoverable failure into a channel that disappears from every
// listing. That is a worse outcome than the divergence the compensation exists to fix, so the
// mapping is pinned here rather than trusted.
func TestChannelDeletedAtOrNil(t *testing.T) {
	archivedAt := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name     string
		in       time.Time
		wantNil  bool
		wantSame time.Time
	}{
		{
			name:    "never archived maps to NULL, not to the zero time",
			in:      time.Time{},
			wantNil: true,
		},
		{
			name:     "an archived channel keeps its exact timestamp",
			in:       archivedAt,
			wantNil:  false,
			wantSame: archivedAt,
		},
	}

	for _, c := range cases {
		got := channelDeletedAtOrNil(c.in)
		if c.wantNil {
			if got != nil {
				t.Errorf("%s: got %v, want nil — a non-nil zero time would archive a live channel",
					c.name, *got)
			}
			continue
		}
		if got == nil {
			t.Errorf("%s: got nil, want %v — a previously archived channel would be un-archived",
				c.name, c.wantSame)
			continue
		}
		if !got.Equal(c.wantSame) {
			t.Errorf("%s: got %v, want %v", c.name, *got, c.wantSame)
		}
	}
}

// The returned pointer must not alias a shared variable: two channels restored in the same
// request must not end up with the same timestamp.
func TestChannelDeletedAtOrNilDoesNotAliasAcrossCalls(t *testing.T) {
	first := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	second := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)

	a := channelDeletedAtOrNil(first)
	b := channelDeletedAtOrNil(second)

	if a == nil || b == nil {
		t.Fatal("both inputs were non-zero; neither result should be nil")
	}
	if a == b {
		t.Fatal("both calls returned the same pointer; one channel's timestamp would overwrite the other")
	}
	if !a.Equal(first) || !b.Equal(second) {
		t.Errorf("values crossed over: got %v and %v, want %v and %v", *a, *b, first, second)
	}
}
