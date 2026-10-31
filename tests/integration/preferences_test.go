//go:build integration
// +build integration

package integration_test

// Person Preferences (design #37, calendar A3a) against real Postgres 12 over
// HTTP: a person reads every key with its default, sets their own (refused in
// words when a key's check fails, cleared when set back to its default), and
// nobody else's; the migration's backfill carries the quiet hours' zone over;
// and the ranked suggestions read each person's kept hours and zone.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestPersonPreferencesOverHTTP|TestSuggestionsReadKeptHoursOverHTTP' -v

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	availabilityBusiness "github.com/akashc777/OneCamp/business/Availability"
	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	eventController "github.com/akashc777/OneCamp/controllers/Calendar"
	preferencesController "github.com/akashc777/OneCamp/controllers/Preferences"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/timing"
)

func prefs(t *testing.T, who *userModels.UserInfo) map[string]json.RawMessage {
	t.Helper()
	rec := call(t, preferencesController.GetMine, http.MethodGet, "/user/preferences", who, "", nil)
	var out struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		t.Fatalf("read: %d %s", rec.Code, rec.Body.String())
	}
	return out.Data
}

func setPref(t *testing.T, who *userModels.UserInfo, key, value string) (int, string) {
	t.Helper()
	rec := call(t, preferencesController.SetMine, http.MethodPut, "/user/preferences/"+key, who, `{"value":`+value+`}`, map[string]string{"key": key})
	return rec.Code, rec.Body.String()
}

func TestPersonPreferencesOverHTTP(t *testing.T) {
	env, dg := calendarStores(t)
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	maya := seedCalendarPerson(t, env.PG, dg, "Maya")

	got := prefs(t, sam)
	for key, def := range map[string]string{"zone": "null", "week_start": "1", "clock": `"auto"`, "calendar.working_hours": "null", "calendar.default_length": "60"} {
		if string(got[key]) != def {
			t.Errorf("%s reads %s, want its default %s", key, got[key], def)
		}
	}

	if code, body := setPref(t, sam, "zone", `"Europe/London"`); code != http.StatusOK {
		t.Fatalf("zone: %d %s", code, body)
	}
	hours := `{"1":[{"start":"09:00","end":"12:30"},{"start":"13:30","end":"17:00"}],"2":[{"start":"09:00","end":"17:00"}]}`
	if code, body := setPref(t, sam, "calendar.working_hours", hours); code != http.StatusOK {
		t.Fatalf("hours: %d %s", code, body)
	}
	if code, body := setPref(t, sam, "calendar.working_hours", `{"1":[{"start":"09:00","end":"12:00"},{"start":"11:00","end":"13:00"}]}`); code != http.StatusBadRequest || !strings.Contains(body, "A day's ranges can't overlap.") {
		t.Errorf("overlapping ranges: %d %s", code, body)
	}
	if code, body := setPref(t, sam, "zone", `"Mars/Olympus"`); code != http.StatusBadRequest || !strings.Contains(body, "That isn't a time zone this server knows.") {
		t.Errorf("a zone nobody knows: %d %s", code, body)
	}
	if code, _ := setPref(t, sam, "nobody.registered", `1`); code != http.StatusNotFound {
		t.Errorf("an unknown key: %d", code)
	}
	got = prefs(t, sam)
	if string(got["zone"]) != `"Europe/London"` {
		t.Errorf("zone: %s", got["zone"])
	}
	var week map[string][]map[string]string
	if json.Unmarshal(got["calendar.working_hours"], &week) != nil || len(week["1"]) != 2 {
		t.Errorf("hours: %s", got["calendar.working_hours"])
	}
	// Maya's are her own: Sam's set none of hers.
	if string(prefs(t, maya)["zone"]) != "null" {
		t.Error("Sam's zone reached Maya")
	}
	// Set back to its default, a key's row goes.
	if code, _ := setPref(t, sam, "week_start", `1`); code != http.StatusOK {
		t.Fatal("week_start")
	}
	var rows int
	if err := env.PG.QueryRow(`SELECT count(*) FROM person_preferences WHERE user_id = $1 AND key = 'week_start'`, sam.UserPostgresInfo.Id).Scan(&rows); err != nil || rows != 0 {
		t.Errorf("a default kept a row: %d %v", rows, err)
	}

	// The migration's backfill: a quiet hours' zone becomes the person's zone.
	if _, err := env.PG.Exec(`INSERT INTO users_notification_preferences (user_id, quiet_hours_tz, unsubscribe_token) VALUES ($1, 'Asia/Kolkata', 'tok-maya')
		ON CONFLICT (user_id) DO UPDATE SET quiet_hours_tz = 'Asia/Kolkata'`, maya.UserPostgresInfo.Id); err != nil {
		t.Fatal(err)
	}
	_, here, _, _ := runtime.Caller(0)
	up, err := os.ReadFile(filepath.Join(filepath.Dir(here), "..", "..", "migrations", migrationNamed(t, "person_preferences", "up")))
	if err != nil {
		t.Fatal(err)
	}
	backfill := string(up)[strings.Index(string(up), "INSERT INTO person_preferences"):]
	if _, err := env.PG.Exec(backfill); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if z := string(prefs(t, maya)["zone"]); z != `"Asia/Kolkata"` {
		t.Errorf("Maya's zone after the backfill: %s", z)
	}
}

// migrationNamed is the one migration file ending in _name.dir.sql.
func migrationNamed(t *testing.T, name, dir string) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(here), "..", "..", "migrations", "*_"+name+"."+dir+".sql"))
	if len(matches) != 1 {
		t.Fatalf("want one %s %s migration, found %v", name, dir, matches)
	}
	return filepath.Base(matches[0])
}

func TestSuggestionsReadKeptHoursOverHTTP(t *testing.T) {
	env, dg := calendarStores(t)
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	jonas := seedCalendarPerson(t, env.PG, dg, "Jonas")
	for who, zone := range map[*userModels.UserInfo]string{sam: "Asia/Kolkata", jonas: "Europe/London"} {
		if code, body := setPref(t, who, "zone", `"`+zone+`"`); code != http.StatusOK {
			t.Fatalf("zone: %d %s", code, body)
		}
	}
	fill := func(start, end string) string {
		week := map[string][]map[string]string{}
		for d := 1; d <= 5; d++ {
			week[strconv.Itoa(d)] = []map[string]string{{"start": start, "end": end}}
		}
		raw, _ := json.Marshal(week)
		return string(raw)
	}
	if code, body := setPref(t, sam, "calendar.working_hours", fill("10:00", "18:30")); code != http.StatusOK {
		t.Fatalf("Sam's hours: %d %s", code, body)
	}
	if code, body := setPref(t, jonas, "calendar.working_hours", fill("09:00", "17:00")); code != http.StatusOK {
		t.Fatalf("Jonas's hours: %d %s", code, body)
	}
	// Each kept hours read back in their zone, for the week's drawn hours and the form's warning.
	rec := call(t, eventController.PeopleHours, http.MethodPost, "/event/hours", sam, `{"people":["`+jonas.UserDgraphInfo.Uuid+`","`+sam.UserDgraphInfo.Uuid+`"]}`, nil)
	var hours struct {
		Data map[string]struct {
			TZ   string                         `json:"tz"`
			Week map[string][]map[string]string `json:"week"`
		} `json:"data"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &hours) != nil || hours.Data[jonas.UserDgraphInfo.Uuid].TZ != "Europe/London" || hours.Data[sam.UserDgraphInfo.Uuid].Week["1"][0]["start"] != "10:00" {
		t.Fatalf("hours: %d %s", rec.Code, rec.Body.String())
	}
	// Next week's Monday to Tuesday.
	london, _ := time.LoadLocation("Europe/London")
	d := time.Now().In(london)
	for d.Weekday() != time.Monday {
		d = d.AddDate(0, 0, 1)
	}
	from := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, london).AddDate(0, 0, 7)
	body := `{"participants":["` + jonas.UserDgraphInfo.Uuid + `"],"duration_minutes":30,"from":"` + rfc(from) + `","to":"` + rfc(from.Add(24*time.Hour)) + `"}`
	rec = call(t, eventController.SuggestTimes, http.MethodPost, "/event/suggestTimes", sam, body, nil)
	var out struct {
		Data availabilityBusiness.Ranked `json:"data"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil || len(out.Data.Suggestions) == 0 {
		t.Fatalf("suggest: %d %s", rec.Code, rec.Body.String())
	}
	kolkata, _ := time.LoadLocation("Asia/Kolkata")
	for _, s := range out.Data.Suggestions {
		if s.Reason != "Everyone's free" {
			continue
		}
		l, k := s.Start.In(london), s.Start.In(kolkata)
		if l.Hour() < 9 || s.End.In(london).After(time.Date(l.Year(), l.Month(), l.Day(), 17, 0, 0, 0, london)) || k.Hour() < 10 || s.End.In(kolkata).After(time.Date(k.Year(), k.Month(), k.Day(), 18, 30, 0, 0, kolkata)) {
			t.Errorf("outside someone's hours: London %v, India %v", l, k)
		}
	}
}

// The engine's speed with real reads (calendar A3b): twenty people, each with
// their own hours and meetings in the one store, over fourteen days, ranked
// with their busy times read side by side, in 150 ms or less (the fastest of
// three, so the machine's other work isn't counted against it).
func TestTwentyPeopleRankInBudgetWithRealReads(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	env, dg := calendarStores(t)
	people := make([]*userModels.UserInfo, 20)
	start := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Hour)
	for i := range people {
		people[i] = seedCalendarPerson(t, env.PG, dg, "Person"+strconv.Itoa(i))
		if code, body := setPref(t, people[i], "zone", `"Europe/London"`); code != http.StatusOK {
			t.Fatalf("zone: %d %s", code, body)
		}
		if code, body := setPref(t, people[i], "calendar.working_hours", `{"1":[{"start":"08:00","end":"18:00"}],"2":[{"start":"08:00","end":"18:00"}],"3":[{"start":"08:00","end":"18:00"}],"4":[{"start":"08:00","end":"18:00"}],"5":[{"start":"08:00","end":"18:00"}]}`); code != http.StatusOK {
			t.Fatalf("hours: %d %s", code, body)
		}
		for m := 0; m < 2; m++ {
			at := start.Add(time.Duration(24*m+i%5) * time.Hour)
			when := `"start":{"at":"` + rfc(at) + `","zone":"Europe/London"},"end":{"at":"` + rfc(at.Add(45*time.Minute)) + `"}`
			if code, body := v2(t, eventController.CreateCalendarEvent, http.MethodPost, "/calendar/events", people[i], `{"title":"Planning",`+when+`}`, ""); code != http.StatusCreated {
				t.Fatalf("event: %d %v", code, body)
			}
		}
	}
	ids := make([]string, 0, 19)
	for _, p := range people[1:] {
		ids = append(ids, p.UserDgraphInfo.Uuid)
	}
	in := calendarBusiness.FindTimeInput{Participants: ids, DurationMinutes: 30, From: start, To: start.Add(14 * 24 * time.Hour)}
	var res *availabilityBusiness.Ranked
	took := timing.Fastest(3, func() {
		var err error
		if res, err = availabilityBusiness.Suggestions(context.Background(), people[0].UserDgraphInfo.Uuid, in, time.Now()); err != nil {
			t.Fatal(err)
		}
	})
	if len(res.Suggestions) == 0 {
		t.Fatal("nothing offered")
	}
	if took > 150*time.Millisecond {
		t.Errorf("twenty people over fourteen days took %v, over 150 ms", took)
	}
	t.Logf("twenty people over fourteen days: %v", took)
}

// A status and a reason over HTTP (calendar A3b): a meeting under way says
// "In a meeting until …" on the reader's clock, and why its time isn't free;
// never its title.
func TestStatusAndExplainOverHTTP(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	env, dg := calendarStores(t)
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	maya := seedCalendarPerson(t, env.PG, dg, "Maya")
	if code, body := setPref(t, maya, "clock", `"12"`); code != http.StatusOK {
		t.Fatalf("clock: %d %s", code, body)
	}
	start := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Minute)
	when := `"start":{"at":"` + rfc(start) + `","zone":"UTC"},"end":{"at":"` + rfc(start.Add(time.Hour)) + `"}`
	if code, body := v2(t, eventController.CreateCalendarEvent, http.MethodPost, "/calendar/events", sam, `{"title":"Interview with Priya",`+when+`}`, ""); code != http.StatusCreated {
		t.Fatalf("event: %d %v", code, body)
	}
	rec := call(t, eventController.PersonStatus, http.MethodGet, "/event/status/"+sam.UserDgraphInfo.Uuid, maya, "", map[string]string{"user_uuid": sam.UserDgraphInfo.Uuid})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"busy"`) || !strings.Contains(rec.Body.String(), "In a meeting until ") || !strings.Contains(rec.Body.String(), "M\"") || strings.Contains(rec.Body.String(), "Priya") {
		t.Errorf("status: %d %s", rec.Code, rec.Body.String())
	}
	rec = call(t, eventController.ExplainTime, http.MethodPost, "/event/explain", maya, `{"person":"`+sam.UserDgraphInfo.Uuid+`","start":"`+rfc(start.Add(5*time.Minute))+`","end":"`+rfc(start.Add(35*time.Minute))+`"}`, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "In a meeting then") || strings.Contains(rec.Body.String(), "Priya") {
		t.Errorf("explain: %d %s", rec.Code, rec.Body.String())
	}
}

// busyKeptPrivate is a workspace that keeps when members are busy to
// themselves (calendar.freebusy.members off).
type busyKeptPrivate struct{ policyBusiness.Store }

func (s busyKeptPrivate) Bool(ctx context.Context, p policyBusiness.BoolPolicy) bool {
	if p.Key == policyBusiness.CalendarFreeBusy.Key {
		return false
	}
	return s.Store.Bool(ctx, p)
}

// A workspace that keeps when members are busy to themselves keeps A3's
// answers too, by the calendar's one rule: Maya asking about Sam, who keeps
// hours and is in a meeting now, gets no status, no reason, no hours and no
// ranked time that counts him, and each answer names him private; Sam still
// reads his own.
func TestA3KeepsBusyTimesPrivateWhenTheWorkspaceDoes(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	env, dg := calendarStores(t)
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	maya := seedCalendarPerson(t, env.PG, dg, "Maya")
	if code, body := setPref(t, sam, "zone", `"UTC"`); code != http.StatusOK {
		t.Fatalf("zone: %d %s", code, body)
	}
	if code, body := setPref(t, sam, "calendar.working_hours", `{"1":[{"start":"00:00","end":"23:45"}],"2":[{"start":"00:00","end":"23:45"}],"3":[{"start":"00:00","end":"23:45"}],"4":[{"start":"00:00","end":"23:45"}],"5":[{"start":"00:00","end":"23:45"}]}`); code != http.StatusOK {
		t.Fatalf("hours: %d %s", code, body)
	}
	start := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Minute)
	when := `"start":{"at":"` + rfc(start) + `","zone":"UTC"},"end":{"at":"` + rfc(start.Add(time.Hour)) + `"}`
	if code, body := v2(t, eventController.CreateCalendarEvent, http.MethodPost, "/calendar/events", sam, `{"title":"Interview with Priya",`+when+`}`, ""); code != http.StatusCreated {
		t.Fatalf("event: %d %v", code, body)
	}
	prev := policyBusiness.Policies
	policyBusiness.Policies = busyKeptPrivate{prev}
	t.Cleanup(func() { policyBusiness.Policies = prev })
	samID := sam.UserDgraphInfo.Uuid

	// The answers are read as JSON: the API indents what it writes.
	statusTo := func(asker *userModels.UserInfo) (code int, kind, words string, private bool, body string) {
		rec := call(t, eventController.PersonStatus, http.MethodGet, "/event/status/"+samID, asker, "", map[string]string{"user_uuid": samID})
		var s struct {
			Data struct {
				Kind    string `json:"kind"`
				Words   string `json:"words"`
				Private bool   `json:"private"`
			} `json:"data"`
		}
		if json.Unmarshal(rec.Body.Bytes(), &s) != nil {
			t.Errorf("a status that isn't JSON: %s", rec.Body.String())
		}
		return rec.Code, s.Data.Kind, s.Data.Words, s.Data.Private, rec.Body.String()
	}
	if code, kind, words, private, body := statusTo(maya); code != http.StatusOK || !private || kind != "" || words != "" {
		t.Errorf("Sam's status, to Maya: %d %s", code, body)
	}
	rec := call(t, eventController.ExplainTime, http.MethodPost, "/event/explain", maya, `{"person":"`+samID+`","start":"`+rfc(start.Add(5*time.Minute))+`","end":"`+rfc(start.Add(35*time.Minute))+`"}`, nil)
	if body := rec.Body.String(); rec.Code != http.StatusForbidden || strings.Contains(body, "Interview") || strings.Contains(body, "meeting") {
		t.Errorf("why Sam isn't free, to Maya: %d %s", rec.Code, body)
	}
	rec = call(t, eventController.PeopleHours, http.MethodPost, "/event/hours", maya, `{"people":["`+samID+`"]}`, nil)
	var hours struct {
		Data    map[string]json.RawMessage `json:"data"`
		Private []string                   `json:"private"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &hours) != nil || len(hours.Data) != 0 || len(hours.Private) != 1 || hours.Private[0] != samID {
		t.Errorf("Sam's hours, to Maya: %d %s", rec.Code, rec.Body.String())
	}
	from := time.Now().UTC()
	rec = call(t, eventController.SuggestTimes, http.MethodPost, "/event/suggestTimes", maya, `{"participants":["`+samID+`"],"duration_minutes":30,"from":"`+rfc(from)+`","to":"`+rfc(from.Add(48*time.Hour))+`"}`, nil)
	var times struct {
		Data struct {
			Private []string `json:"private"`
		} `json:"data"`
	}
	// Sam is named once, as private: no time counts him free or busy, and no reason names him.
	if body := rec.Body.String(); rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &times) != nil || len(times.Data.Private) != 1 || times.Data.Private[0] != samID ||
		strings.Count(body, samID) != 1 || strings.Contains(body, "Sam") {
		t.Errorf("times with Sam, to Maya: %d %s", rec.Code, body)
	}

	if code, kind, _, private, body := statusTo(sam); code != http.StatusOK || private || kind != "busy" {
		t.Errorf("Sam's own status: %d %s", code, body)
	}
}
