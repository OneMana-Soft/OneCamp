//go:build integration
// +build integration

package integration_test

// Ending focus time on the server, on each calendar store (REGRESSION-AUDIT,
// 23 Oct 2026, the owner's "End focus button is not working"). The web's End
// could only end the focus event its own browser had started, from a record in
// that browser's storage; focus time started on the phone, in the desktop
// app, on the calendar or in another browser opened the calendar instead and
// ended nothing. POST /event/endFocus ends whatever focus time the caller is
// in, by the rule that holds their notifications, and nothing else.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	calendarAdapter "github.com/akashc777/OneCamp/adapter/Calendar"
	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	eventController "github.com/akashc777/OneCamp/controllers/Calendar"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	prefModels "github.com/akashc777/OneCamp/models/postgres/UserNotificationPreference"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestEndFocusEndsFocusTimeStartedAnywhere(t *testing.T) {
	integration.ForEachCalendarStore(t, endFocusEndsFocusTimeStartedAnywhere)
}

func endFocusEndsFocusTimeStartedAnywhere(t *testing.T) {
	env, dg := calendarStores(t)
	ctx := context.Background()
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	riley := seedCalendarPerson(t, env.PG, dg, "Riley")
	now := time.Now()
	// The web's ISO times carry milliseconds.
	iso := func(at time.Time) string { return at.UTC().Format("2006-01-02T15:04:05.000Z07:00") }
	add := func(who *userModels.UserInfo, title, notes string, start, end time.Time, focus bool) string {
		t.Helper()
		no := false
		created, err := calendarBusiness.CreateEvent(ctx, who, calendarAdapter.CreateOrUpdateEventInput{
			Title: title, Description: notes, StartTime: iso(start), EndTime: iso(end),
			Participants: []string{}, IsFocus: &focus, IsAway: &no,
		})
		if err != nil || created == nil || created.EventUuid == "" {
			t.Fatalf("make %q: %+v %v", title, created, err)
		}
		return created.EventUuid
	}
	// Sam's focus time, started elsewhere and renamed there, with notes; a
	// second block overlapping it; and what End must leave alone: a meeting
	// running now, focus time later today, and Riley's focus time.
	deep := add(sam, "Deep work: launch plan", "Heads down on the plan", now.Add(-10*time.Minute), now.Add(50*time.Minute), true)
	short := add(sam, "Focus time", "", now.Add(-5*time.Minute), now.Add(25*time.Minute), true)
	meeting := add(sam, "Launch sync", "", now.Add(-15*time.Minute), now.Add(45*time.Minute), false)
	later := add(sam, "Focus time", "", now.Add(3*time.Hour), now.Add(4*time.Hour), true)
	rileys := add(riley, "Focus time", "", now.Add(-10*time.Minute), now.Add(50*time.Minute), true)
	if until, err := prefModels.FocusUntilOf(sam.UserPostgresInfo.Id); err != nil || until == nil {
		t.Fatalf("in focus time, Sam's focus_until is %v (%v); want its end", until, err)
	}

	before := time.Now()
	rec := call(t, eventController.EndFocusController, http.MethodPost, "/event/endFocus", sam, "", nil)
	after := time.Now()
	if got, want := endedBy(t, rec), slices.Sorted(slices.Values([]string{deep, short})); !slices.Equal(got, want) {
		t.Errorf("End ended %v; want Sam's two running focus blocks %v", got, want)
	}

	if until, err := prefModels.FocusUntilOf(sam.UserPostgresInfo.Id); err != nil || until != nil {
		t.Errorf("after End, Sam's focus_until is %v (%v); want none", until, err)
	}
	if s, err := calendarBusiness.Statuses.StatusFor(ctx, sam.UserDgraphInfo.Uuid, time.Now()); err != nil || s.Kind == calendarBusiness.StatusFocus {
		t.Errorf("after End, Sam's status is %+v (%v); want not in focus", s, err)
	}
	if until, err := prefModels.FocusUntilOf(riley.UserPostgresInfo.Id); err != nil || until == nil {
		t.Errorf("Sam's End ended Riley's focus time: focus_until %v (%v)", until, err)
	}

	event := func(who *userModels.UserInfo, id string) (title, notes string, start, end time.Time) {
		t.Helper()
		ev, err := calendarBusiness.GetEvent(ctx, who, id)
		if err != nil || ev.StartTime == nil || ev.EndTime == nil {
			t.Fatalf("read %s: %+v %v", id, ev, err)
		}
		return ev.Title, ev.Description, *ev.StartTime, *ev.EndTime
	}
	near := func(a, b time.Time) bool { return a.Sub(b).Abs() < time.Second }
	title, notes, start, end := event(sam, deep)
	if title != "Deep work: launch plan" || notes != "Heads down on the plan" || !near(start, now.Add(-10*time.Minute)) {
		t.Errorf("End changed more than the end: %q, %q, from %v", title, notes, start)
	}
	if end.Before(before.Add(-time.Second)) || end.After(after.Add(time.Second)) {
		t.Errorf("the block ends at %v; want the moment of End, %v to %v", end, before, after)
	}
	for id, want := range map[string]time.Time{meeting: now.Add(45 * time.Minute), later: now.Add(4 * time.Hour)} {
		if _, _, _, end := event(sam, id); !near(end, want) {
			t.Errorf("End moved the end of %s to %v; want it left at %v", id, end, want)
		}
	}
	if _, _, _, end := event(riley, rileys); !near(end, now.Add(50*time.Minute)) {
		t.Errorf("End moved Riley's focus time to end at %v", end)
	}

	// Pressed again, or on a second device, with no focus time left: nothing
	// to end, and no error.
	again := call(t, eventController.EndFocusController, http.MethodPost, "/event/endFocus", sam, "", nil)
	if got := endedBy(t, again); len(got) != 0 {
		t.Errorf("a second End ended %v; want none", got)
	}
}

// endedBy is the ids an End answer says it ended, sorted; it fails the
// test on any answer but a 200.
func endedBy(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("End answered %d %s", rec.Code, rec.Body.String())
	}
	var answer struct {
		Data struct {
			Ended []string `json:"ended"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
		t.Fatalf("End's answer %s: %v", rec.Body.String(), err)
	}
	return slices.Sorted(slices.Values(answer.Data.Ended))
}
