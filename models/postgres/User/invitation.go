package models

import (
	"fmt"
	"time"
)

// What an invitation can be, as stored. "pending" is the column's default and
// what invitations made before tokens carry; "sent" is what one made now is.
// "expired" is only stored by the sign-up page once someone opens a link past
// its expiry: an invitation is expired as soon as LiveAt says so, whatever
// the row still says.
const (
	InvitationStatusPending = "pending"
	InvitationStatusSent    = "sent"
	InvitationStatusExpired = "expired"
	InvitationStatusJoined  = "joined"
)

// InvitationLinkLifetime is how long an invitation's link works.
const InvitationLinkLifetime = 7 * 24 * time.Hour

// ExpiresAt is when the invitation's link stops working: its token's expiry,
// or, for one made before links had an expiry, a week after it was made. Pure.
func (inv *Invitation) ExpiresAt() time.Time {
	if inv.TokenExpiresAt != nil {
		return *inv.TokenExpiresAt
	}
	return inv.CreatedAt.Add(InvitationLinkLifetime)
}

// InvitationLiveSQL is LiveAt's rule as an SQL condition on one invitations
// row, for a query that has to ask it of many rows at once. alias is the
// row's name in the query: "i" for "FROM invitations i". It is LiveAt,
// written once more in the one place that can't call it, beside it, and
// TestInvitationLiveSQLIsLiveAt (tests/integration) holds the two to the same
// answers. Pure.
func InvitationLiveSQL(alias string) string {
	return fmt.Sprintf("(%[1]s.status NOT IN ('%[2]s', '%[3]s') AND COALESCE(%[1]s.token_expires_at, %[1]s.created_at + interval '%[4]d seconds') > NOW())",
		alias, InvitationStatusJoined, InvitationStatusExpired, int(InvitationLinkLifetime.Seconds()))
}

// LiveAt reports whether the invitation can still let someone in at now: not
// used, not marked expired, and its link not run out (ExpiresAt).
//
// THE ONE ANSWER to "is this invitation live", asked wherever it matters: the
// admin's list, the sign-up link, Google and GitHub admitting someone,
// inviting an address again. It was written out four times, and the copies
// disagreed about an invitation with no expiry, which the list called live
// forever while the sign-up page took its link. (Migration 204 states the same
// rule in SQL, once, for the rows it marks.) Pure.
func (inv *Invitation) LiveAt(now time.Time) bool {
	if inv == nil {
		return false
	}
	switch inv.Status {
	case InvitationStatusJoined, InvitationStatusExpired:
		return false
	}
	return inv.ExpiresAt().After(now)
}
