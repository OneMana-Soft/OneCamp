package todoist

import (
	"encoding/json"
	"testing"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

// Todoist's due has three shapes. An all-day "2026-10-22" is a calendar
// day (written where the team is). A floating "2026-10-22T17:00:00" is that
// wall-clock time in the Todoist user's own zone; it failed both parses and
// was dropped. "…Z" is an instant.
func TestATodoistDueArrivesInEachOfItsShapes(t *testing.T) {
	var snap todoistSnapshot
	if err := json.Unmarshal([]byte(`{"user":{"id":"u1","full_name":"Ada","tz_info":{"timezone":"America/Los_Angeles"}}}`), &snap); err != nil {
		t.Fatal(err)
	}
	item := func(due string) *todoistItem {
		var it todoistItem
		if err := json.Unmarshal([]byte(`{"id":"1","project_id":"p","content":"Pay rent","priority":1,"due":{"date":"`+due+`"}}`), &it); err != nil {
			t.Fatal(err)
		}
		return &it
	}
	show := func(v *time.Time) string {
		if v == nil {
			return ""
		}
		return v.UTC().Format(time.RFC3339)
	}

	st := buildSourceTask(&snap, item("2026-10-22"), nil)
	if st.DueDay != (importProvider.Day{Year: 2026, Month: time.October, Day: 22}) || st.DueDate != nil {
		t.Errorf("all-day: day %v, instant %v; want the day 2026-10-22 and no instant", st.DueDay, show(st.DueDate))
	}
	st = buildSourceTask(&snap, item("2026-10-22T17:00:00"), nil)
	if show(st.DueDate) != "2026-10-23T00:00:00Z" || !st.DueDay.IsZero() {
		t.Errorf("floating 17:00 in Los Angeles: instant %q, day %v; want 2026-10-23T00:00:00Z", show(st.DueDate), st.DueDay)
	}
	st = buildSourceTask(&snap, item("2026-10-22T17:00:00Z"), nil)
	if show(st.DueDate) != "2026-10-22T17:00:00Z" || !st.DueDay.IsZero() {
		t.Errorf("fixed 17:00Z: instant %q, day %v; want 2026-10-22T17:00:00Z", show(st.DueDate), st.DueDay)
	}
	// "Local" is the server's own zone, nobody's: the time is read in UTC.
	snap.User.TZInfo.Timezone = "Local"
	st = buildSourceTask(&snap, item("2026-10-22T17:00:00"), nil)
	if show(st.DueDate) != "2026-10-22T17:00:00Z" {
		t.Errorf(`floating 17:00 for a user in "Local": instant %q; want 2026-10-22T17:00:00Z`, show(st.DueDate))
	}
}

// Todoist's lowest priority (API 1, shown as P4) is "no priority", the
// default every task has. It was mapped to low, so every imported task came
// in as low.
func TestTodoistsDefaultPriorityIsNone(t *testing.T) {
	m := New().DefaultPriorityMap()
	if got, ok := m["1"]; !ok || got != "" {
		t.Errorf(`priority "1" (P4, no priority) maps to %q (present %v), want ""`, got, ok)
	}
	for k, want := range map[string]string{"4": "high", "3": "medium", "2": "low"} {
		if m[k] != want {
			t.Errorf("priority %q maps to %q, want %q", k, m[k], want)
		}
	}
}
