package models

import (
	"testing"

	"github.com/google/uuid"
)

// A resource's standing link is told apart from its other links: a room's
// address, never a doc's page, and "turn off the earlier links" keeps it.
func TestAStandingLinkIsToldApart(t *testing.T) {
	slug, page := "maya-chen", "launch-notes"
	room := &GuestGrant{Id: uuid.New(), ResourceType: ResourceMeeting, Slug: &slug}
	oneCall := &GuestGrant{Id: uuid.New(), ResourceType: ResourceMeeting}
	doc := &GuestGrant{Id: uuid.New(), ResourceType: ResourceDoc, Slug: &page}
	standing, others := StandingOf([]*GuestGrant{oneCall, room, doc})
	if standing != room || len(others) != 2 || others[0] != oneCall || others[1] != doc {
		t.Errorf("standing %v, others %v", standing, others)
	}
	if standing, others := StandingOf(nil); standing != nil || len(others) != 0 {
		t.Errorf("no links: %v, %v", standing, others)
	}
}
