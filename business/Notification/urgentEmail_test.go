package notification

import (
	"testing"

	prefModels "github.com/akashc777/OneCamp/models/postgres/UserNotificationPreference"
)

// An Urgent task's email is news of the person's own work: it rides the
// "tasks assigned to you" switch (business/Task urgent.go), so turning that
// off turns it off too.
func TestUrgentEmailRidesTheAssignedSwitch(t *testing.T) {
	if eventEnabledForPref(EventTaskUrgent, &prefModels.UserNotificationPreference{EmailTaskAssigned: false, EmailTaskStatus: true, EmailMentions: true}) {
		t.Error("sent with assigned-to-you off")
	}
	if !eventEnabledForPref(EventTaskUrgent, &prefModels.UserNotificationPreference{EmailTaskAssigned: true}) {
		t.Error("held with assigned-to-you on")
	}
}
