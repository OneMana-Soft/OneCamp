package notification

// Calendar head E (gcal-5): each person chooses calendar emails by kind, as
// Google lets them: invitations (a booking of their time is one), changes
// (a cancellation is one), and replies to their events. A reminder by email
// is its own switch: the person asked for it on the event or as a default.

import (
	"testing"

	prefModels "github.com/akashc777/OneCamp/models/postgres/UserNotificationPreference"
)

// everyEmail is a person with every kind of email on, as everyone starts.
func everyEmail() *prefModels.UserNotificationPreference {
	return &prefModels.UserNotificationPreference{
		EmailEnabled: true, EmailMentions: true, EmailDMs: true, EmailTaskAssigned: true,
		EmailTaskStatus: true, EmailComments: true, EmailCalls: true, EmailChannelInvites: true,
		EmailCalendarInvites: true, EmailCalendarChanges: true, EmailCalendarAnswers: true,
	}
}

func TestEachCalendarEmailHasItsOwnSwitch(t *testing.T) {
	cases := []struct {
		kind   EventType
		toggle func(p *prefModels.UserNotificationPreference) *bool
	}{
		{EventCalendarInvited, func(p *prefModels.UserNotificationPreference) *bool { return &p.EmailCalendarInvites }},
		{EventCalendarBooking, func(p *prefModels.UserNotificationPreference) *bool { return &p.EmailCalendarInvites }},
		{EventCalendarUpdated, func(p *prefModels.UserNotificationPreference) *bool { return &p.EmailCalendarChanges }},
		{EventCalendarCancelled, func(p *prefModels.UserNotificationPreference) *bool { return &p.EmailCalendarChanges }},
		{EventCalendarAnswered, func(p *prefModels.UserNotificationPreference) *bool { return &p.EmailCalendarAnswers }},
	}
	for _, c := range cases {
		// Off, it isn't sent though every other email is on.
		off := everyEmail()
		*c.toggle(off) = false
		if eventEnabledForPref(c.kind, off) {
			t.Errorf("%s was sent to a person who turned its kind off", c.kind)
		}
		// On, it is sent though every other kind is off.
		on := &prefModels.UserNotificationPreference{EmailEnabled: true}
		*c.toggle(on) = true
		if !eventEnabledForPref(c.kind, on) {
			t.Errorf("%s wasn't sent to a person who has its kind on and the rest off", c.kind)
		}
	}
	// Each kind is its own: turning one off leaves the other two.
	changesOff := everyEmail()
	changesOff.EmailCalendarChanges = false
	for _, kind := range []EventType{EventCalendarInvited, EventCalendarBooking, EventCalendarAnswered} {
		if !eventEnabledForPref(kind, changesOff) {
			t.Errorf("turning changes off stopped %s", kind)
		}
	}
	// A reminder by email is the person's own ask: no kind stops it.
	none := &prefModels.UserNotificationPreference{EmailEnabled: true}
	if !eventEnabledForPref(EventCalendarReminder, none) {
		t.Error("a reminder the person asked for by email was stopped by a calendar email switch")
	}
}
