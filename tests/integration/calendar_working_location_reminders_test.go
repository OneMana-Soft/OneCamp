//go:build integration
// +build integration

package integration_test

// Calendar head 5d's reminder rule over HTTP (gcal-9b). A working location
// says where its owner works and, as in Google Calendar, never notifies. Made
// through the calendar's own API, for one day or every week, its owner's
// reminders read as the default with no minutes. Once every Resource Event has
// been through its sinks, no reminder is planned for it: a weekly one keeps
// only the planner's replan wake, which reminds nobody. An all-day event made
// the same way still takes the all-day default, so the planner ran. The pure
// rule is pinned in business/Calendar (TestAWorkingLocationRemindsNobodyByDefault).
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestAWorkingLocationRemindsNobodyOverHTTP' -v

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	eventController "github.com/akashc777/OneCamp/controllers/Calendar"
)

func TestAWorkingLocationRemindsNobodyOverHTTP(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	env, dg := calendarStores(t)
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	day := time.Now().UTC().AddDate(0, 0, 3).Truncate(24 * time.Hour)
	date := func(d time.Time) string { return d.Format("2006-01-02") }
	allDay := `"start":{"date":"` + date(day) + `"},"end":{"date":"` + date(day.AddDate(0, 0, 1)) + `"}`

	home := makeEvent(t, sam, `{"kind":"working_location","location":"Home",`+allDay+`}`)
	office := makeEvent(t, sam, `{"kind":"working_location","location":"Office","start":{"date":"`+date(day.AddDate(0, 0, 1))+`"},`+
		`"end":{"date":"`+date(day.AddDate(0, 0, 2))+`"},"recurrence":{"rule":"FREQ=WEEKLY"}}`)
	offsite := makeEvent(t, sam, `{"title":"Offsite",`+allDay+`}`)
	if !resourceEventBusiness.Sinks.Settle(30 * time.Second) {
		t.Fatal("the events' sinks didn't settle")
	}

	// Its owner's reminders read as the default, with no minutes.
	for _, c := range []struct {
		name, id string
		want     []int
	}{
		{"Home, for one day", home.ID, nil},
		{"Office, every week", office.ID, nil},
		{"the offsite, all day", offsite.ID, []int{15 * 60}},
	} {
		var rem struct {
			Minutes []int `json:"minutes"`
			Default bool  `json:"default"`
		}
		code, body := v2(t, eventController.GetCalendarReminders, http.MethodGet, "/calendar/events/"+c.id+"/reminders", sam, "", c.id)
		if code != http.StatusOK || json.Unmarshal(body["data"], &rem) != nil || !rem.Default || !slices.Equal(rem.Minutes, c.want) {
			t.Errorf("%s: Sam's reminders read %d %s, want the default %v", c.name, code, body["data"], c.want)
		}
	}

	// Nothing planned that reminds anyone: the weekly one keeps only its replan wake.
	planned := func(id string) (reminders, wakes int) {
		t.Helper()
		if err := env.PG.QueryRow(`SELECT count(*) FILTER (WHERE NOT COALESCE((payload->>'replan')::boolean, false)),
				count(*) FILTER (WHERE COALESCE((payload->>'replan')::boolean, false))
			FROM scheduled_jobs WHERE job_type = 'calendar_reminder' AND status = 'pending' AND payload->>'event_id' = $1`, id).Scan(&reminders, &wakes); err != nil {
			t.Fatal(err)
		}
		return reminders, wakes
	}
	if r, w := planned(home.ID); r != 0 || w != 0 {
		t.Errorf("Home, for one day: %d reminders and %d replan wakes planned, want none", r, w)
	}
	if r, w := planned(office.ID); r != 0 || w != 1 {
		t.Errorf("Office, every week: %d reminders and %d replan wakes planned, want its replan wake alone", r, w)
	}
	// The all-day event's default is planned, so the planner did run.
	if _, m, ok := waitingReminder(t, env, sam.UserPostgresInfo.Id, offsite.ID); !ok || m != 15*60 {
		t.Errorf("the offsite's reminder: planned %v, %d minutes before, want 900", ok, m)
	}
}
