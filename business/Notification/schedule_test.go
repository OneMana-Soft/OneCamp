package notification

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	preferencesBusiness "github.com/akashc777/OneCamp/business/Preferences"
	prefModels "github.com/akashc777/OneCamp/models/postgres/UserNotificationPreference"
	"github.com/google/uuid"
)

// withKept stands the person's other preferences (by key) and Postgres's zone
// list in for the store, for one test.
func withKept(t *testing.T, kept map[string]string, pgKnows func(string) bool) {
	t.Helper()
	wasValue, wasZone := keptValue, knownZone
	t.Cleanup(func() { keptValue, knownZone = wasValue, wasZone })
	keptValue = func(_ context.Context, _ uuid.UUID, key string) (json.RawMessage, error) {
		if v, ok := kept[key]; ok {
			return json.RawMessage(v), nil
		}
		return nil, nil
	}
	knownZone = func(name string) (bool, error) { return pgKnows(name), nil }
}

// keep is a value through the key's whole check, as Set runs it: the pure
// check, then what is kept for the person.
func keep(v string) (json.RawMessage, error) {
	if err := scheduleProblem(json.RawMessage(v)); err != nil {
		return nil, err
	}
	return keptSchedule(context.Background(), uuid.New(), json.RawMessage(v))
}

const mondays = `"1":[{"start":"09:00","end":"17:00"}]`

// sevenRanges is seven ranges of a day, none overlapping: one too many.
var sevenRanges = func() string {
	var out []string
	for h := 9; h < 16; h++ {
		out = append(out, fmt.Sprintf(`{"start":"%02d:00","end":"%02d:30"}`, h, h))
	}
	return strings.Join(out, ",")
}()

// What a schedule is refused for, in words for the person: a value that
// isn't a week of hours, a week with no hours in it (that is a pause), hours
// that don't read, a zone Go or Postgres can't read (one Postgres can't
// read would fail every push lookup that touches the person), and following
// working hours the person hasn't set.
func TestAScheduleIsRefusedInWords(t *testing.T) {
	withKept(t, map[string]string{}, func(z string) bool { return z != "Europe/Kyiv" })
	for _, c := range []struct{ value, says string }{
		{`"weekdays"`, "A schedule is the hours notifications may reach you, each day's in your time zone."},
		{`{"tz":"Asia/Kolkata","week":{}}`, "Use Pause for a whole stretch off."},
		{`{"tz":"Asia/Kolkata","week":{"1":[],"2":[]}}`, "Use Pause for a whole stretch off."},
		{`{"tz":"Asia/Kolkata"}`, "Use Pause for a whole stretch off."},
		{`{"tz":"","week":{` + mondays + `}}`, "Pick a time zone."},
		{`{"tz":"Mars/Olympus_Mons","week":{` + mondays + `}}`, "That isn't a time zone this server knows."},
		{`{"tz":"Local","week":{` + mondays + `}}`, "That isn't a time zone this server knows."},
		{`{"tz":"Europe/Kyiv","week":{` + mondays + `}}`, "That isn't a time zone this server knows."},
		{`{"tz":"UTC","week":{"1":[{"start":"17:00","end":"09:00"}]}}`, "Each range has to end after it starts."},
		{`{"tz":"UTC","week":{"1":[{"start":"09:00","end":"13:00"},{"start":"12:00","end":"17:00"}]}}`, "A day's ranges can't overlap."},
		{`{"tz":"UTC","week":{"1":[{"start":"9am","end":"17:00"}]}}`, "Hours look like 09:00."},
		{`{"tz":"UTC","week":{"7":[{"start":"09:00","end":"17:00"}]}}`, "Days run from Sunday (0) to Saturday (6)."},
		{`{"tz":"UTC","week":{"1":[` + sevenRanges + `]}}`, "Up to six ranges a day."},
		{`{"working_hours":true}`, "Set your working hours first."},
	} {
		_, err := keep(c.value)
		var refused *preferencesBusiness.Refused
		if !errors.As(err, &refused) || refused.Msg != c.says {
			t.Errorf("%s: %v, want %q", c.value, err, c.says)
		}
	}
}

// Following working hours with hours but no zone asks for the zone; a zone
// Postgres can't read is refused there too.
func TestFollowingWorkingHoursNeedsHoursAndAZone(t *testing.T) {
	withKept(t, map[string]string{prefWorkingHours: `{` + mondays + `}`}, func(z string) bool { return z != "Europe/Kyiv" })
	var refused *preferencesBusiness.Refused
	if _, err := keep(`{"working_hours":true}`); !errors.As(err, &refused) || refused.Msg != "Set your time zone first." {
		t.Errorf("hours, no zone: %v", err)
	}
	withKept(t, map[string]string{prefWorkingHours: `{` + mondays + `}`, "zone": `"Europe/Kyiv"`}, func(z string) bool { return z != "Europe/Kyiv" })
	if _, err := keep(`{"working_hours":true}`); !errors.As(err, &refused) || refused.Msg != "That isn't a time zone this server knows." {
		t.Errorf("a zone Postgres can't read: %v", err)
	}
}

// What is kept: a week as given, in one shape (anything else in the value
// left out, and days with no ranges, which the push gate would otherwise
// read "3": null of); and, following working hours, the person's hours and zone
// copied in, whatever hours the value carried.
func TestWhatAScheduleKeeps(t *testing.T) {
	withKept(t, map[string]string{prefWorkingHours: `{"2":[{"start":"10:00","end":"18:00"}]}`, "zone": `"America/New_York"`}, func(string) bool { return true })
	got, err := keep(`{"tz":"Asia/Kolkata","week":{` + mondays + `,"2":[],"3":null},"days":[1],"note":"x"}`)
	if err != nil || string(got) != `{"tz":"Asia/Kolkata","week":{`+mondays+`}}` {
		t.Errorf("a week: %s %v", got, err)
	}
	got, err = keep(`{"working_hours":true,"tz":"UTC","week":{` + mondays + `}}`)
	if err != nil || string(got) != `{"tz":"America/New_York","week":{"2":[{"start":"10:00","end":"18:00"}]},"working_hours":true}` {
		t.Errorf("following working hours: %s %v", got, err)
	}
	var s prefModels.Schedule
	if json.Unmarshal(got, &s) != nil || !s.WorkingHours || s.Hours().TZ != "America/New_York" {
		t.Errorf("the kept value doesn't read as a schedule: %s", got)
	}
}

// The key is a person preference: null clears it, so null is never checked.
func TestTheScheduleIsAPreference(t *testing.T) {
	if !slices.Contains(preferencesBusiness.Keys(), prefModels.ScheduleKey) {
		t.Fatalf("%s isn't registered", prefModels.ScheduleKey)
	}
}
