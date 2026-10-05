package notification

import (
	"testing"
	"time"

	prefModels "github.com/akashc777/OneCamp/models/postgres/UserNotificationPreference"
)

func TestHeldFor(t *testing.T) {
	now := time.Date(2026, 10, 5, 23, 0, 0, 0, time.UTC)
	later := now.Add(time.Hour)
	earlier := now.Add(-time.Hour)
	s, e, tz := "22:00", "07:00", "UTC"
	if got := HeldFor(nil, now); got.Reason != "" {
		t.Fatalf("nil prefs: %+v", got)
	}
	if got := HeldFor(&prefModels.UserNotificationPreference{NotificationsPausedUntil: &later}, now); got.Reason != "paused" || !got.Until.Equal(later) {
		t.Fatalf("paused: %+v", got)
	}
	if got := HeldFor(&prefModels.UserNotificationPreference{NotificationsPausedUntil: &earlier}, now); got.Reason != "" {
		t.Fatalf("pause over: %+v", got)
	}
	q := &prefModels.UserNotificationPreference{QuietHoursEnabled: true, QuietHoursStart: &s, QuietHoursEnd: &e, QuietHoursTZ: &tz}
	got := HeldFor(q, now)
	if got.Reason != "quiet_hours" || !got.Until.Equal(time.Date(2026, 10, 6, 7, 0, 0, 0, time.UTC)) {
		t.Fatalf("quiet: %+v", got)
	}
}
