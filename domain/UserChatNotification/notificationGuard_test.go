package domain

import (
	"context"
	"testing"
	"time"
)

// Inconsistent input must be reported, not swallowed.
//
// This function used to return nil both when there was nothing to do AND when the two
// parallel lists disagreed. Row i pairs recipient i with grouping id i, so a disagreement
// means a caller's bookkeeping drifted — and the old behaviour was to create notifications
// for NOBODY in the batch, with no error returned and nothing logged. Silence is the worst
// possible response to that, because the notifications simply never appear and there is no
// trace explaining why.
//
// Both cases below return before any database work, so this runs without Postgres.
func TestBulkDMNotificationsRejectsMismatchedInput(t *testing.T) {
	ctx := context.Background()
	now := time.Now()

	cases := []struct {
		name    string
		to      []string
		grp     []string
		wantErr bool
	}{
		{"fewer grouping ids than recipients", []string{"u1", "u2"}, []string{"g1"}, true},
		{"more grouping ids than recipients", []string{"u1"}, []string{"g1", "g2"}, true},
		{"no recipients at all is genuinely a no-op", nil, nil, false},
		{"no recipients but stray grouping ids is still a no-op", nil, []string{"g1"}, false},
	}

	for _, c := range cases {
		err := BulkCreateDMNotificationsIfNotExists(ctx, "sender", c.to, c.grp, "all", now)
		if c.wantErr && err == nil {
			t.Errorf("%s: got nil, want an error — a whole batch of notifications would "+
				"vanish with no explanation", c.name)
		}
		if !c.wantErr && err != nil {
			t.Errorf("%s: got %v, want nil", c.name, err)
		}
	}
}
