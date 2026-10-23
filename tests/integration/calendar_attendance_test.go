//go:build integration
// +build integration

package integration_test

// Answers and invitations (calendar A2) in the one store, against real
// Postgres 12: the migration up, down and up; a member's answer with a note
// and one instance's own; guests by email, each with a link that answers for
// them alone from a public page and stops when they are taken off; the
// organiser's copy of a link; a nudge; and a declined meeting that makes
// nobody busy. The service's own rules are pinned in
// business/Calendar/attendance_test.go; these hold the SQL to them.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestCalendarAttendance' -v

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	eventController "github.com/akashc777/OneCamp/controllers/Calendar"
	"github.com/akashc777/OneCamp/tests/integration"
)

// attendanceMigration is calendar A2's migration in one direction, found by
// name: the integrator numbers it at merge.
func attendanceMigration(t *testing.T, dir string) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(here), "..", "..", "migrations", "*_calendar_attendance."+dir+".sql"))
	if len(matches) != 1 {
		t.Fatalf("want one calendar_attendance %s migration, found %v", dir, matches)
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func columnExists(t *testing.T, env *integration.Env, table, column string) bool {
	t.Helper()
	var n int
	if err := env.PG.QueryRow(`SELECT count(*) FROM information_schema.columns WHERE table_name = $1 AND column_name = $2`, table, column).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

// The migration goes up, down and up again on Postgres 12; its answers for
// one instance are one per guest and instance, by id or by address in any
// case; a guest's invite state is one it knows.
func TestCalendarAttendanceMigrationUpDownUp(t *testing.T) {
	up, down := attendanceMigration(t, "up"), attendanceMigration(t, "down")
	if strings.Contains(up, "gen_random_uuid") {
		t.Fatal("ids are made in Go")
	}
	env := integration.SetupEnv(t)
	for i, step := range []struct {
		sql  string
		want bool
	}{{"", true}, {down, false}, {up, true}, {down, false}, {up, true}} {
		if step.sql != "" {
			if _, err := env.PG.Exec(step.sql); err != nil {
				t.Fatalf("step %d: %v", i, err)
			}
		}
		if got := tableExists(t, env.PG, "event_instance_answers"); got != step.want {
			t.Fatalf("step %d: event_instance_answers exists = %v", i, got)
		}
		for _, c := range [][2]string{{"event_attendees", "rsvp_token_hash"}, {"event_attendees", "invite_state"}, {"events", "email_on_answers"}} {
			if got := columnExists(t, env, c[0], c[1]); got != step.want {
				t.Fatalf("step %d: %s.%s exists = %v", i, c[0], c[1], got)
			}
		}
	}
	user, calendar, event := uuid.New(), uuid.New(), uuid.New()
	exec := func(q string, args ...any) error { _, err := env.PG.Exec(q, args...); return err }
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, 'u', 'u', NOW(), NOW())`, user, user.String()+"@example.test"))
	must(exec(`INSERT INTO calendars (id, owner_id) VALUES ($1, $2)`, calendar, user))
	must(exec(`INSERT INTO events (id, calendar_id, created_by, ical_uid, start_at, end_at, rrule) VALUES ($1, $2, $3, $4, NOW(), NOW() + interval '1 hour', 'FREQ=WEEKLY')`,
		event, calendar, user, event.String()+"@onecamp"))
	rid := time.Date(2026, 10, 27, 9, 15, 0, 0, time.UTC)
	must(exec(`INSERT INTO event_instance_answers (event_id, recurrence_id, user_id, response, responded_at) VALUES ($1, $2, $3, 'declined', NOW())`, event, rid, user))
	if err := exec(`INSERT INTO event_instance_answers (event_id, recurrence_id, user_id, response, responded_at) VALUES ($1, $2, $3, 'accepted', NOW())`, event, rid, user); err == nil {
		t.Error("two answers for one instance by one guest")
	}
	must(exec(`INSERT INTO event_instance_answers (event_id, recurrence_id, email, response, responded_at) VALUES ($1, $2, 'Alex@Example.org', 'tentative', NOW())`, event, rid))
	if err := exec(`INSERT INTO event_instance_answers (event_id, recurrence_id, email, response, responded_at) VALUES ($1, $2, 'alex@example.org', 'accepted', NOW())`, event, rid); err == nil {
		t.Error("one address answered twice in two cases")
	}
	if err := exec(`INSERT INTO event_instance_answers (event_id, recurrence_id, response, responded_at) VALUES ($1, $2, 'accepted', NOW())`, event, rid); err == nil {
		t.Error("an answer by nobody")
	}
	if err := exec(`INSERT INTO event_attendees (event_id, email, invite_state) VALUES ($1, 'jo@example.org', 'maybe')`, event); err == nil {
		t.Error("an invite state nobody knows")
	}
	// Deleting the event takes its answers with it.
	must(exec(`DELETE FROM events WHERE id = $1`, event))
	var left int
	must(env.PG.QueryRow(`SELECT count(*) FROM event_instance_answers WHERE event_id = $1`, event).Scan(&left))
	if left != 0 {
		t.Errorf("%d answers outlived their event", left)
	}
}

// public calls a public handler: nobody signed in.
func public(t *testing.T, h http.HandlerFunc, method, path, body string, params map[string]string) (int, map[string]json.RawMessage) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rctx := chi.NewRouteContext()
	for k, v := range params {
		rctx.URLParams.Add(k, v)
	}
	rec := httptest.NewRecorder()
	h(rec, req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx)))
	var out map[string]json.RawMessage
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// guestOf is someone's entry on an event, by name or address.
func guestOf(e calendarBusiness.Event, who string) *calendarBusiness.Attendee {
	for i := range e.Attendees {
		if e.Attendees[i].Name == who || strings.EqualFold(e.Attendees[i].Email, who) {
			return &e.Attendees[i]
		}
	}
	return nil
}

func TestCalendarAttendanceOverHTTP(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	t.Setenv("APP_SECRET_KEK", "calendar-integration-tests")
	env, dg := calendarStores(t)
	ctx := context.Background()
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	maya := seedCalendarPerson(t, env.PG, dg, "Maya")
	jonas := seedCalendarPerson(t, env.PG, dg, "Jonas")

	// Tomorrow, from the clock: a nudge asks only about a meeting still to
	// come, and a fixed day failed this test once that day's hour had passed.
	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 1)
	at := func(hour int) string { return day.Add(time.Duration(hour) * time.Hour).Format(time.RFC3339) }

	// Sam invites Maya, Jonas (by his address) and Alex (by email).
	code, body := v2(t, eventController.CreateCalendarEvent, http.MethodPost, "/calendar/events", sam,
		`{"title":"Design review","start":{"at":"`+at(9)+`","zone":"Europe/London"},"end":{"at":"`+at(10)+`"},`+
			`"attendees":[{"user_uuid":"`+maya.UserDgraphInfo.Uuid+`"},{"email":"`+strings.ToUpper(jonas.UserPostgresInfo.EmailID)+`"},`+
			`{"email":"Alex Kim <alex@example.org>"},{"email":"`+sam.UserPostgresInfo.EmailID+`"}]}`, "")
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, body)
	}
	e := dataEvent(t, body)
	if len(e.Attendees) != 3 || guestOf(e, "Jonas") == nil || guestOf(e, "Jonas").UUID != jonas.UserDgraphInfo.Uuid || guestOf(e, "alex@example.org").Name != "Alex Kim" {
		t.Fatalf("guests: %+v", e.Attendees)
	}

	// Maya answers maybe with a note; the event's sequence stays.
	code, body = v2(t, eventController.AnswerCalendarEvent, http.MethodPost, "/calendar/events/"+e.ID+"/answer", maya, `{"answer":"tentative","note":"Ten minutes late"}`, e.ID)
	if code != http.StatusOK {
		t.Fatalf("answer: %d %v", code, body)
	}
	code, body = v2(t, eventController.GetCalendarEvent, http.MethodGet, "/calendar/events/"+e.ID, sam, "", e.ID)
	seen := dataEvent(t, body)
	if m := guestOf(seen, "Maya"); code != 200 || m == nil || m.Response != "tentative" || m.Note != "Ten minutes late" || seen.Sequence != e.Sequence {
		t.Fatalf("Sam sees Maya %+v (sequence %d, was %d)", m, seen.Sequence, e.Sequence)
	}
	if code, _ := v2(t, eventController.AnswerCalendarEvent, http.MethodPost, "/calendar/events/"+e.ID+"/answer", sam, `{"answer":"accepted"}`, e.ID); code != http.StatusBadRequest {
		t.Errorf("the organiser answered: %d", code)
	}

	// The legacy list carries everyone's answers too.
	from, to := day, day.AddDate(0, 0, 1)
	listed, err := calendarBusiness.GetEventsList(ctx, sam, &from, &to)
	if err != nil || len(listed) != 1 || len(listed[0].Guests) != 3 {
		t.Fatalf("the legacy list: %+v %v", listed, err)
	}

	// Alex's link, copied by Sam, opens their page and answers for them.
	code, body = v2(t, eventController.CalendarGuestLink, http.MethodPost, "/calendar/events/"+e.ID+"/guest-link", sam, `{"email":"alex@example.org"}`, e.ID)
	var link struct {
		URL string `json:"url"`
	}
	if code != 200 || json.Unmarshal(body["data"], &link) != nil || !strings.Contains(link.URL, "/rsvp/") {
		t.Fatalf("the link: %d %v", code, body)
	}
	if code, _ := v2(t, eventController.CalendarGuestLink, http.MethodPost, "/calendar/events/"+e.ID+"/guest-link", maya, `{"email":"alex@example.org"}`, e.ID); code != http.StatusForbidden {
		t.Errorf("a guest copied a link: %d", code)
	}
	secret := link.URL[strings.LastIndex(link.URL, "/")+1:]
	code, body = public(t, eventController.GetPublicInvitation, http.MethodGet, "/public/rsvp/"+secret, "", map[string]string{"token": secret})
	var page calendarBusiness.InvitationView
	if code != 200 || json.Unmarshal(body["data"], &page) != nil || page.Title != "Design review" || page.Organiser != "Sam" || page.You.Email != "alex@example.org" {
		t.Fatalf("the page: %d %v", code, body)
	}
	for _, g := range page.Guests {
		if strings.Contains(g.Name, "@") {
			t.Errorf("an address on Alex's page: %q", g.Name)
		}
	}
	code, body = public(t, eventController.AnswerPublicInvitation, http.MethodPost, "/public/rsvp/"+secret, `{"answer":"accepted","note":"See you"}`, map[string]string{"token": secret})
	if code != 200 {
		t.Fatalf("Alex's answer: %d %v", code, body)
	}
	_, body = v2(t, eventController.GetCalendarEvent, http.MethodGet, "/calendar/events/"+e.ID, sam, "", e.ID)
	if a := guestOf(dataEvent(t, body), "alex@example.org"); a == nil || a.Response != "accepted" || a.Note != "See you" {
		t.Errorf("Sam sees Alex %+v", a)
	}

	// A nudge asks Jonas alone; again within the day, nobody.
	code, body = v2(t, eventController.NudgeCalendarGuests, http.MethodPost, "/calendar/events/"+e.ID+"/nudge", sam, "", e.ID)
	var asked struct {
		Asked int `json:"asked"`
	}
	if code != 200 || json.Unmarshal(body["data"], &asked) != nil || asked.Asked != 1 {
		t.Errorf("nudge: %d %v", code, body)
	}
	_, body = v2(t, eventController.NudgeCalendarGuests, http.MethodPost, "/calendar/events/"+e.ID+"/nudge", sam, "", e.ID)
	if json.Unmarshal(body["data"], &asked) != nil || asked.Asked != 0 {
		t.Errorf("a second nudge asked %d", asked.Asked)
	}

	// Maya says no: she stays on it, and isn't busy then.
	if code, _ := v2(t, eventController.AnswerCalendarEvent, http.MethodPost, "/calendar/events/"+e.ID+"/answer", maya, `{"answer":"declined"}`, e.ID); code != 200 {
		t.Fatalf("Maya's no: %d", code)
	}
	busy, err := calendarBusiness.ReadBusy(ctx, maya.UserDgraphInfo.Uuid, from, to, calendarBusiness.BusyOptions{})
	if err != nil || len(busy.Intervals) != 0 {
		t.Errorf("Maya said no and is busy: %+v %v", busy, err)
	}
	if busy, _ := calendarBusiness.ReadBusy(ctx, jonas.UserDgraphInfo.Uuid, from, to, calendarBusiness.BusyOptions{}); len(busy.Intervals) != 1 {
		t.Errorf("Jonas hasn't answered and isn't busy: %+v", busy)
	}

	// Alex taken off: their link stops at once, said like any other.
	keep := `{"sequence":` + itoa(seen.Sequence) + `,"notify":"none","attendees":[{"user_uuid":"` + maya.UserDgraphInfo.Uuid + `"},{"user_uuid":"` + jonas.UserDgraphInfo.Uuid + `"}]}`
	if code, body := v2(t, eventController.UpdateCalendarEvent, http.MethodPatch, "/calendar/events/"+e.ID, sam, keep, e.ID); code != 200 {
		t.Fatalf("taking Alex off: %d %v", code, body)
	}
	code, body = public(t, eventController.GetPublicInvitation, http.MethodGet, "/public/rsvp/"+secret, "", map[string]string{"token": secret})
	unknown, _ := public(t, eventController.GetPublicInvitation, http.MethodGet, "/public/rsvp/nope", "", map[string]string{"token": "nope"})
	if code != http.StatusNotFound || unknown != http.StatusNotFound {
		t.Errorf("Alex's old link %d, an unknown one %d", code, unknown)
	}
}

// One instance answered on its own, in the SQL: it stands for that instance,
// and a yes to all of it clears it.
func TestCalendarAttendanceForOneInstance(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	env, dg := calendarStores(t)
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	maya := seedCalendarPerson(t, env.PG, dg, "Maya")
	code, body := v2(t, eventController.CreateCalendarEvent, http.MethodPost, "/calendar/events", sam,
		`{"title":"Stand-up","start":{"at":"2026-10-20T08:15:00Z","zone":"Europe/London"},"end":{"at":"2026-10-20T08:30:00Z"},`+
			`"recurrence":{"rule":"FREQ=WEEKLY;BYDAY=TU"},"attendees":[{"user_uuid":"`+maya.UserDgraphInfo.Uuid+`"}]}`, "")
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, body)
	}
	const from, to = "2026-10-19T00:00:00Z", "2026-11-04T00:00:00Z"
	in := apiList(t, maya, from, to)
	if code, body := v2(t, eventController.AnswerCalendarEvent, http.MethodPost, "/calendar/events/"+in[1].ID+"/answer", maya,
		`{"answer":"declined","note":"Dentist","scope":"this"}`, in[1].ID); code != 200 {
		t.Fatalf("one instance: %d %v", code, body)
	}
	want := []string{"needs_action", "declined", "needs_action"}
	for i, e := range apiList(t, sam, from, to) {
		if g := guestOf(e, "Maya"); g == nil || g.Response != want[i] {
			t.Errorf("instance %d: %+v, want %s", i, g, want[i])
		}
	}
	if code, _ := v2(t, eventController.AnswerCalendarEvent, http.MethodPost, "/calendar/events/"+in[0].ID+"/answer", maya, `{"answer":"accepted"}`, in[0].ID); code != 200 {
		t.Fatal("yes to all")
	}
	for i, e := range apiList(t, sam, from, to) {
		if g := guestOf(e, "Maya"); g == nil || g.Response != "accepted" {
			t.Errorf("after all, instance %d: %+v", i, g)
		}
	}
}
