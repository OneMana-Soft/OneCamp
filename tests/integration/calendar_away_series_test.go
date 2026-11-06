//go:build integration

package integration_test

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	eventController "github.com/akashc777/OneCamp/controllers/Calendar"
	"github.com/akashc777/OneCamp/tests/integration"
)

// Calendar head C, "Away, finished" (gcal-9b), over HTTP on Postgres: a
// daily series Sam makes while Maya is away is declined for her occurrence by
// occurrence inside the away, with her message, recorded as her away's
// (event_instance_answers.by_away, migration 572) and read as "Out of office"
// by Sam; the series itself and the days after stay hers to answer. Lee
// declines a day by hand, which is never marked.
func TestAnAwayDeclinesASeriesDayByDayOverHTTP(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	env, dg := calendarStores(t)
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	maya := seedCalendarPerson(t, env.PG, dg, "Maya")
	lee := seedCalendarPerson(t, env.PG, dg, "Lee")
	from := time.Now().UTC().Add(48 * time.Hour).Truncate(24 * time.Hour)
	makeEvent(t, maya, `{"kind":"out_of_office","title":"Out of office","start":{"at":"`+rfc(from)+`","zone":"UTC"},"end":{"at":"`+rfc(from.Add(48*time.Hour))+`"},`+
		`"out_of_office":{"decline":"new","message":"Back Thursday."}}`)

	st := from.Add(-24*time.Hour + 9*time.Hour) // the day before the away, 09:00
	series := makeEvent(t, sam, `{"title":"Standup","start":{"at":"`+rfc(st)+`","zone":"UTC"},"end":{"at":"`+rfc(st.Add(15*time.Minute))+`"},`+
		`"recurrence":{"rule":"FREQ=DAILY;COUNT=4"},`+
		`"attendees":[{"user_uuid":"`+maya.UserDgraphInfo.Uuid+`"},{"user_uuid":"`+lee.UserDgraphInfo.Uuid+`"}]}`).ID

	var whole string
	if err := env.PG.QueryRow(`SELECT response FROM event_attendees WHERE event_id = $1 AND user_id = $2`, series, maya.UserPostgresInfo.Id).Scan(&whole); err != nil {
		t.Fatal(err)
	}
	if whole == "declined" {
		t.Fatal("the series was declined whole for Maya")
	}
	var declined, recorded int
	if err := env.PG.QueryRow(`SELECT count(*), count(*) FILTER (WHERE by_away AND note = 'Back Thursday.') FROM event_instance_answers
		WHERE event_id = $1 AND user_id = $2 AND response = 'declined'`, series, maya.UserPostgresInfo.Id).Scan(&declined, &recorded); err != nil {
		t.Fatal(err)
	}
	if declined != 2 || recorded != 2 {
		t.Fatalf("Maya's declined days: %d, recorded as her away's: %d; want the 2 inside it, both recorded", declined, recorded)
	}

	// Lee declines the first day inside the away by hand.
	days := apiList(t, sam, rfc(st.Add(-time.Hour)), rfc(st.Add(4*24*time.Hour)))
	var inside string
	for _, e := range days {
		if e.Title == "Standup" && e.Start.At != nil && e.Start.At.Equal(st.Add(24*time.Hour)) {
			inside = e.ID
		}
	}
	if inside == "" {
		t.Fatalf("no Standup on the away's first day among %d events", len(days))
	}
	if code, out := v2(t, eventController.AnswerCalendarEvent, http.MethodPost, "/calendar/events/"+inside+"/answer", lee,
		`{"answer":"declined","note":"Clash","scope":"this"}`, inside); code != http.StatusOK {
		t.Fatalf("Lee's answer: %d %s", code, out["msg"])
	}

	type mark struct {
		response string
		away     bool
	}
	got := map[string]map[string]mark{}
	for _, e := range apiList(t, sam, rfc(st.Add(-time.Hour)), rfc(st.Add(4*24*time.Hour))) {
		if e.Title != "Standup" || e.Start.At == nil {
			continue
		}
		day := e.Start.At.UTC().Format("01-02")
		got[day] = map[string]mark{}
		for _, g := range e.Attendees {
			got[day][g.UUID] = mark{g.Response, g.OutOfOffice}
		}
	}
	for i := 0; i < 4; i++ {
		day := st.Add(time.Duration(i) * 24 * time.Hour).Format("01-02")
		m := got[day][maya.UserDgraphInfo.Uuid]
		inAway := i == 1 || i == 2
		if (m.response == "declined") != inAway || m.away != inAway {
			t.Fatalf("Sam reads Maya on %s: %+v, want declined and marked only inside her away", day, m)
		}
	}
	if l := got[st.Add(24*time.Hour).Format("01-02")][lee.UserDgraphInfo.Uuid]; l.response != "declined" || l.away {
		t.Fatalf("Sam reads Lee's decline by hand: %+v, want declined and not marked", l)
	}

	// Maya answers that day again by hand (CR-B1 M3b): the record is hers now,
	// so by_away is false in Postgres and Sam sees no mark.
	if code, out := v2(t, eventController.AnswerCalendarEvent, http.MethodPost, "/calendar/events/"+inside+"/answer", maya,
		`{"answer":"declined","note":"Still away, sorry.","scope":"this"}`, inside); code != http.StatusOK {
		t.Fatalf("Maya's answer by hand: %d %s", code, out["msg"])
	}
	var byAway bool
	if err := env.PG.QueryRow(`SELECT by_away FROM event_instance_answers WHERE event_id = $1 AND user_id = $2 AND recurrence_id = $3`,
		series, maya.UserPostgresInfo.Id, st.Add(24*time.Hour)).Scan(&byAway); err != nil || byAway {
		t.Fatalf("by_away after Maya's answer by hand: %v %v, want false", byAway, err)
	}
	for _, e := range apiList(t, sam, rfc(st.Add(23*time.Hour)), rfc(st.Add(25*time.Hour))) {
		for _, g := range e.Attendees {
			if e.Title == "Standup" && g.UUID == maya.UserDgraphInfo.Uuid && g.OutOfOffice {
				t.Fatal("Sam still sees Maya's answer by hand as her away's")
			}
		}
	}
}

// Migration 572 (CR-B1 M3c): by_away comes and goes with up and down on both
// answer tables, and up again keeps every row it had.
func TestCalendarAnswerSourceMigrationUpDownUp(t *testing.T) {
	read := func(dir string) string {
		t.Helper()
		_, here, _, _ := runtime.Caller(0)
		matches, _ := filepath.Glob(filepath.Join(filepath.Dir(here), "..", "..", "migrations", "572_calendar_answer_source."+dir+".sql"))
		if len(matches) != 1 {
			t.Fatalf("want one 572 %s migration, found %v", dir, matches)
		}
		b, err := os.ReadFile(matches[0])
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	up, down := read("up"), read("down")
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
		for _, table := range []string{"event_attendees", "event_instance_answers"} {
			if got := columnExists(t, env, table, "by_away"); got != step.want {
				t.Fatalf("step %d: %s.by_away exists = %v", i, table, got)
			}
		}
	}
}
