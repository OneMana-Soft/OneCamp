package models

import (
	"testing"
	"time"
)

// One predicate for whether an invitation is live. An invitation made before
// links had an expiry lasts a week from when it was made: the list called it
// live forever, while Google and GitHub still let its address in.
func TestAnInvitationIsLiveForAWeek(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	ahead, behind := now.Add(time.Hour), now.Add(-time.Hour)
	for name, c := range map[string]struct {
		inv  Invitation
		live bool
	}{
		"a link with a week left":            {Invitation{Status: InvitationStatusSent, TokenExpiresAt: &ahead}, true},
		"a link past its expiry":             {Invitation{Status: InvitationStatusSent, TokenExpiresAt: &behind}, false},
		"used":                               {Invitation{Status: InvitationStatusJoined, TokenExpiresAt: &ahead}, false},
		"marked expired":                     {Invitation{Status: InvitationStatusExpired, TokenExpiresAt: &ahead}, false},
		"no expiry, made two days ago":       {Invitation{Status: InvitationStatusPending, CreatedAt: now.Add(-48 * time.Hour)}, true},
		"no expiry, made eight days ago":     {Invitation{Status: InvitationStatusPending, CreatedAt: now.Add(-8 * 24 * time.Hour)}, false},
		"no expiry, a week to the very hour": {Invitation{Status: InvitationStatusSent, CreatedAt: now.Add(-InvitationLinkLifetime)}, false},
	} {
		inv := c.inv
		if got := inv.LiveAt(now); got != c.live {
			t.Errorf("%s: live %v, want %v", name, got, c.live)
		}
	}
	var none *Invitation
	if none.LiveAt(now) {
		t.Error("no invitation is live")
	}
}
