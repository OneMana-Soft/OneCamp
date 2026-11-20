package notification

import (
	"testing"

	prefModels "github.com/akashc777/OneCamp/models/postgres/UserNotificationPreference"
)

// An SLA's alert is news of the person's own work: it rides the "tasks
// assigned to you" switch (business/SLA alerts.go), as an Urgent task's does.
func TestAnSLAAlertsEmailRidesTheAssignedSwitch(t *testing.T) {
	if eventEnabledForPref(EventTaskSLA, &prefModels.UserNotificationPreference{EmailTaskAssigned: false, EmailTaskStatus: true, EmailMentions: true}) {
		t.Error("an SLA alert is emailed with assigned-to-you emails off")
	}
	if !eventEnabledForPref(EventTaskSLA, &prefModels.UserNotificationPreference{EmailTaskAssigned: true}) {
		t.Error("an SLA alert isn't emailed with assigned-to-you emails on")
	}
}
