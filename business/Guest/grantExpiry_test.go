package business

import (
	"testing"
	"time"

	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
)

func at(d time.Duration) *time.Time {
	t := time.Now().Add(d)
	return &t
}

// grantExpired is one of two places that decide whether a link still opens; the
// other is the SQL predicate. They have to agree, and the cost of disagreeing is
// a link that works after it should have stopped.
func TestGrantExpiry(t *testing.T) {
	cases := []struct {
		name    string
		expires *time.Time
		want    bool
	}{
		// The reason the column became nullable. A grant with no expiry lasts
		// until somebody revokes it, so it is never expired.
		{"no expiry set", nil, false},
		{"expires in an hour", at(time.Hour), false},
		{"expired an hour ago", at(-time.Hour), true},
		// Exactly at the boundary counts as expired: the check is After(now), so
		// a grant whose moment has arrived no longer opens.
		{"expired a moment ago", at(-time.Millisecond), true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := grantExpired(&guestModel.GuestGrant{ExpiresAt: c.expires})
			if got != c.want {
				t.Errorf("grantExpired(%v) = %v, want %v", c.expires, got, c.want)
			}
		})
	}
}

// The ceiling still binds every link that has an expiry. Permanence has to be
// asked for explicitly, so a caller that merely passes a huge ttl gets the cap
// rather than a link that never dies.
func TestTTLCeilingStillApplies(t *testing.T) {
	if maxResourceGrantTTL >= 365*24*time.Hour {
		t.Errorf("maxResourceGrantTTL = %v, expected a bounded ceiling", maxResourceGrantTTL)
	}
	if resourceGrantTTL > maxResourceGrantTTL {
		t.Errorf("default TTL %v exceeds the ceiling %v", resourceGrantTTL, maxResourceGrantTTL)
	}
}
