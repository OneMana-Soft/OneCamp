package controllers

import (
	"testing"

	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
)

// meet-02 (1), CR-95's M1: a guest let in by an event invitation is audited
// with whose invitation it was; anyone else's join says only that they weren't.
func TestAGuestsJoinIsAuditedWithWhoseInvitationLetThemIn(t *testing.T) {
	plain := joined("meet-1", "g-1", nil)
	if plain["invited"] != false || plain["invitation_email"] != nil || plain["invitation_event"] != nil || plain["room"] != "meet-1" || plain["grant_id"] != "g-1" {
		t.Errorf("a guest with no invitation: %v", plain)
	}
	alex := joined("meet-1", "g-1", &calendarBusiness.Invitee{EventID: "ev-1", Email: "alex@example.org", Name: "Alex Kim"})
	if alex["invited"] != true || alex["invitation_email"] != "alex@example.org" || alex["invitation_event"] != "ev-1" {
		t.Errorf("Alex by his invitation: %v", alex)
	}
	if _, ok := alex["invitation_name"]; ok {
		t.Errorf("the audit row repeats the name they may have typed over: %v", alex)
	}
}
