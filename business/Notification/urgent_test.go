package notification

import (
	"encoding/json"
	"testing"
	"time"

	weekHours "github.com/akashc777/OneCamp/business/WeekHours"
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

// Focus time holds pushes (the push gate) and email (the dispatcher), so a
// teammate is told so: during Maya's focus block Jonas saw no bar, and Notify
// anyway answered "not paused" (TN-BUG-8). Named for whichever lasts longer.
func TestHeldForFocusTime(t *testing.T) {
	now := time.Date(2026, 10, 13, 14, 30, 0, 0, time.UTC)
	at := func(m int) *time.Time { v := now.Add(time.Duration(m) * time.Minute); return &v }
	s, e, tz := "22:00", "07:00", "UTC"
	cases := []struct {
		name         string
		pause, focus *time.Time
		quiet        bool
		reason       string
		until        *time.Time
	}{
		{"in focus time", nil, at(90), false, "focus", at(90)},
		{"focus outlasts a pause", at(30), at(90), false, "focus", at(90)},
		{"a pause outlasts focus", at(120), at(90), false, "paused", at(120)},
		{"focus and a pause end together", at(90), at(90), false, "paused", at(90)},
		{"focus over, an expired pause", at(-10), at(-5), false, "", nil},
		{"focus over, no quiet hours now", nil, at(-5), true, "", nil},
	}
	for _, c := range cases {
		p := &prefModels.UserNotificationPreference{NotificationsPausedUntil: c.pause, FocusUntil: c.focus}
		if c.quiet {
			p.QuietHoursEnabled, p.QuietHoursStart, p.QuietHoursEnd, p.QuietHoursTZ = true, &s, &e, &tz
		}
		got := HeldFor(p, now)
		if got.Reason != c.reason || (c.until == nil) != (got.Until == nil) || (c.until != nil && !got.Until.Equal(*c.until)) {
			t.Errorf("%s: got %+v, want %s until %v", c.name, got, c.reason, c.until)
		}
	}
}

// A schedule (notify.schedule) holds as quiet hours do: outside its ranges a
// teammate is told "quiet_hours" and until when the next range starts, and
// nothing of the hours themselves; inside them, nothing is held.
func TestHeldForASchedule(t *testing.T) {
	mondays := &prefModels.Schedule{TZ: "UTC", Week: map[string][]weekHours.Span{"1": {{Start: "09:00", End: "17:00"}}}}
	p := &prefModels.UserNotificationPreference{Schedule: mondays}
	saturday := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	got := HeldFor(p, saturday)
	if want := time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC); got.Reason != "quiet_hours" || got.Until == nil || !got.Until.Equal(want) {
		t.Fatalf("Saturday: %+v, want quiet_hours until %v", got, want)
	}
	if b, _ := json.Marshal(got); string(b) != `{"until":"2026-11-09T09:00:00Z","reason":"quiet_hours"}` {
		t.Errorf("a teammate is told %s", b)
	}
	if got := HeldFor(p, time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)); got.Reason != "" {
		t.Errorf("Monday at ten: %+v", got)
	}
}
