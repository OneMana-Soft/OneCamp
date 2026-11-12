//go:build integration

package integration_test

// CAL52-T3: "Plan my week". The plan proposes blocks for the viewer's own
// open tasks in their free time, in their hours, and writes nothing: a
// proposal becomes a block only through the calendar's create, on confirm.
// A task waiting on a blocker in the plan comes after it; one waiting on
// anything else is left out, its blocker named only to a reader who may read
// it (a restricted task they can't see is counted, never named).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	availabilityBusiness "github.com/akashc777/OneCamp/business/Availability"
	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	eventController "github.com/akashc777/OneCamp/controllers/Calendar"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/tests/integration"
)

// cal52MidWeek is when a plan is asked for unless a case says otherwise: a
// Wednesday morning, days from the week's end in any zone. On the real clock
// the plan came up short on Monday mornings IST, while a UTC week had under
// two hours left (V-827).
var cal52MidWeek = time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)

// planWeek is who's plan with their hours every hour of every day in UTC, so
// the plan's room is the clock's alone.
func (w *dg10World) planWeek(who string) (availabilityBusiness.WeekPlan, string) {
	w.t.Helper()
	return w.planWeekIn(who, "UTC")
}

// planWeekAt is who's plan asked for at now, their hours every hour of every
// day in tz.
func (w *dg10World) planWeekAt(who, tz string, now time.Time) availabilityBusiness.WeekPlan {
	w.t.Helper()
	restore := helpers.SetClockForTest(func() time.Time { return now })
	defer restore()
	p, _ := w.planWeekIn(who, tz)
	return p
}

// planWeekIn is who's plan with their hours every hour of every day in tz
// (the zone the web sends is the browser's).
func (w *dg10World) planWeekIn(who, tz string) (availabilityBusiness.WeekPlan, string) {
	w.t.Helper()
	body := map[string]any{"hours": map[string]any{"days": []int{0, 1, 2, 3, 4, 5, 6}, "start": "00:00", "end": "24:00", "tz": tz}}
	code, raw := w.call(who, false, eventController.PlanWeek, http.MethodPost, "/event/planWeek", nil, body)
	if code != http.StatusOK {
		w.t.Fatalf("%s's plan: %d %s", who, code, raw)
	}
	var res struct {
		Data availabilityBusiness.WeekPlan `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		w.t.Fatal(err)
	}
	return res.Data, raw
}

func (w *dg10World) busyOf(who string, from, to time.Time) []calendarBusiness.Interval {
	w.t.Helper()
	b, err := calendarBusiness.BusyFor(context.Background(), w.people[who].String(), from, to)
	if err != nil {
		w.t.Fatal(err)
	}
	return b.Intervals
}

func TestCAL52PlanMyWeek(t *testing.T) {
	w := newDG10World(t)
	integration.StubMqttClient(t)
	if helpers.CalendarInPostgres() {
		integration.MarkCalendarMoved(t, w.env.PG)
	}
	t.Cleanup(helpers.SetClockForTest(func() time.Time { return cal52MidWeek }))
	est := func(n int) map[string]any { return map[string]any{"task_estimate_minutes": n} }
	w.taskAt("c1", "p", time.Time{}, time.Time{}, est(60), "ana")
	w.taskAt("c2", "p", time.Time{}, time.Time{}, nil, "ana", "c1")
	w.taskAt("b2", "p", time.Time{}, time.Time{}, nil, "ben")
	w.taskAt("w1", "p", time.Time{}, time.Time{}, nil, "ana", "b2")
	w.taskAt("q1", "q", time.Time{}, time.Time{}, est(45), "ana")
	w.taskAt("finished", "p", time.Time{}, time.Time{}, map[string]any{"task_status": "done"}, "ana")

	first, _ := w.planWeek("ana")
	if first.By != "due" || first.Note == "" || len(first.Proposals) == 0 {
		t.Fatalf("Ana's plan without a model: %+v", first)
	}
	if sunday := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC); !first.From.Equal(cal52MidWeek) || !first.To.Equal(sunday) {
		t.Errorf("Ana's week asked for at %v: %v to %v, want to %v", cal52MidWeek, first.From, first.To, sunday)
	}
	byTask := map[string]availabilityBusiness.Proposal{}
	for _, p := range first.Proposals {
		byTask[p.TaskUUID] = p
	}
	for _, name := range []string{"c1", "c2", "q1"} {
		if _, ok := byTask[w.tasks[name]]; !ok {
			t.Errorf("%s isn't planned: %+v", name, first)
		}
	}
	if _, ok := byTask[w.tasks["finished"]]; ok {
		t.Error("a done task is planned")
	}
	if c1, c2 := byTask[w.tasks["c1"]], byTask[w.tasks["c2"]]; c2.Start.Before(c1.End) || c2.After != w.tasks["c1"] || c1.End.Sub(c1.Start) != time.Hour {
		t.Errorf("c2 after c1: %+v %+v", c1, c2)
	}
	if q1 := byTask[w.tasks["q1"]]; q1.End.Sub(q1.Start) != 45*time.Minute || q1.TaskName != "q1" || q1.ProjectName != "Marketing" {
		t.Errorf("q1 is its estimate long: %+v", q1)
	}
	waiting := ""
	for _, l := range first.Left {
		if l.TaskUUID == w.tasks["w1"] {
			waiting = l.Reason
		}
	}
	if waiting != "Waiting on b2" {
		t.Errorf("w1 waits on Ben's b2: %q (%+v)", waiting, first.Left)
	}

	// Ana's busy time is skipped, and planning writes nothing.
	at := first.Proposals[0].Start
	when := `"start":{"at":"` + rfc(at) + `","zone":"UTC"},"end":{"at":"` + rfc(at.Add(3*time.Hour)) + `"}`
	var meeting map[string]any
	_ = json.Unmarshal([]byte(`{"title":"Board meeting",`+when+`}`), &meeting)
	if code, body := w.call("ana", false, eventController.CreateCalendarEvent, http.MethodPost, "/calendar/events", nil, meeting); code != http.StatusCreated {
		t.Fatalf("Ana's meeting: %d %s", code, body)
	}
	before := w.busyOf("ana", first.From, first.To)
	second, _ := w.planWeek("ana")
	for _, p := range second.Proposals {
		if p.Start.Before(at.Add(3*time.Hour)) && at.Before(p.End) {
			t.Errorf("%s is planned over Ana's meeting: %v-%v", p.TaskName, p.Start, p.End)
		}
		if p.Start.Before(second.From) || p.End.After(second.To) {
			t.Errorf("%s is outside the week: %v-%v (%v-%v)", p.TaskName, p.Start, p.End, second.From, second.To)
		}
	}
	if after := w.busyOf("ana", first.From, first.To); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Errorf("planning wrote to Ana's calendar: %v then %v", before, after)
	}

	// A blocker Ben may not read (restricted in his own project) is counted,
	// never named.
	w.taskAt("vault", "p", time.Time{}, time.Time{}, nil, "cat")
	w.taskAt("rb", "p", time.Time{}, time.Time{}, nil, "ben", "vault")
	w.exec(`INSERT INTO task_restrictions (task_id, project_id, restricted_by) VALUES ($1, $2, $3)`, w.tasks["vault"], w.p, w.people["ana"])
	ben, raw := w.planWeek("ben")
	reason := ""
	for _, l := range ben.Left {
		if l.TaskUUID == w.tasks["rb"] {
			reason = l.Reason
		}
	}
	if reason != "Waiting on 1 task" || strings.Contains(raw, "vault") || strings.Contains(raw, w.tasks["vault"]) {
		t.Errorf("Ben's plan names what he can't read, or loses the count: %q %s", reason, raw)
	}
	for _, p := range ben.Proposals {
		if p.TaskUUID == w.tasks["c1"] || p.TaskUUID == w.tasks["q1"] {
			t.Errorf("Ben's plan holds Ana's task %s", p.TaskName)
		}
	}

	// Thirty open tasks: one plan, read and placed.
	for i := range 30 {
		w.taskAt(fmt.Sprintf("t%02d", i), "p", time.Time{}, time.Time{}, est(15+i%4*15), "ana")
	}
	var took time.Duration
	for range 3 {
		began := time.Now()
		p, _ := w.planWeek("ana")
		if d := time.Since(began); took == 0 || d < took {
			took = d
		}
		if len(p.Proposals)+len(p.Left) < 30 {
			t.Fatalf("thirty tasks: %d planned, %d left", len(p.Proposals), len(p.Left))
		}
	}
	t.Logf("CAL52-T3 TIMING: a week planned for 35 open tasks over HTTP: best of 3 %v", took)
	if took > 2*time.Second {
		t.Errorf("a week planned in %v", took)
	}
}

// The plan turns over to next week with under an hour of this one left
// (planMinLeft), in the member's own zone, over HTTP. 59 minutes before
// Monday 00:00 UTC a UTC member plans next week's whole; at 60 and at 61
// minutes, the rest of this week. At Monday 04:21 IST, when the 9 Nov gate's
// plan came up short, a UTC member has the last 69 minutes of their week and
// what doesn't fit is left for want of time, while a member in Asia/Kolkata
// plans the whole of their own week (V-827).
func TestCAL52PlanMyWeekAtTheWeeksEnd(t *testing.T) {
	w := newDG10World(t)
	integration.StubMqttClient(t)
	if helpers.CalendarInPostgres() {
		integration.MarkCalendarMoved(t, w.env.PG)
	}
	est := func(n int) map[string]any { return map[string]any{"task_estimate_minutes": n} }
	w.taskAt("c1", "p", time.Time{}, time.Time{}, est(60), "ana")
	w.taskAt("c2", "p", time.Time{}, time.Time{}, nil, "ana", "c1")
	w.taskAt("q1", "q", time.Time{}, time.Time{}, est(45), "ana")
	kolkata, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	at := func(day, hour, min int, loc *time.Location) time.Time {
		return time.Date(2026, 11, day, hour, min, 0, 0, loc)
	}
	cases := []struct {
		name, tz      string
		now, from, to time.Time
		whole         bool // c1, c2 and q1 all fit, c2 after c1
	}{
		{"59 minutes left", "UTC", at(15, 23, 1, time.UTC), at(16, 0, 0, time.UTC), at(23, 0, 0, time.UTC), true},
		{"60 minutes left", "UTC", at(15, 23, 0, time.UTC), at(15, 23, 0, time.UTC), at(16, 0, 0, time.UTC), false},
		{"61 minutes left", "UTC", at(15, 22, 59, time.UTC), at(15, 22, 59, time.UTC), at(16, 0, 0, time.UTC), false},
		{"Mon 04:21 IST in UTC", "UTC", at(9, 4, 21, kolkata), at(9, 4, 21, kolkata), at(9, 0, 0, time.UTC), false},
		{"Mon 04:21 IST in Kolkata", "Asia/Kolkata", at(9, 4, 21, kolkata), at(9, 4, 21, kolkata), at(16, 0, 0, kolkata), true},
	}
	for _, c := range cases {
		p := w.planWeekAt("ana", c.tz, c.now)
		if !p.From.Equal(c.from) || !p.To.Equal(c.to) {
			t.Errorf("%s: the plan covers %v to %v, want %v to %v", c.name, p.From, p.To, c.from, c.to)
		}
		planned := map[string]availabilityBusiness.Proposal{}
		for _, x := range p.Proposals {
			planned[x.TaskUUID] = x
			if x.Start.Before(p.From) || x.End.After(p.To) {
				t.Errorf("%s: %s is outside the week: %v-%v (%v-%v)", c.name, x.TaskName, x.Start, x.End, p.From, p.To)
			}
		}
		short := 0
		for _, l := range p.Left {
			if l.Reason == "No free time left this week" {
				short++
			}
		}
		if !c.whole {
			if short == 0 || len(p.Proposals)+len(p.Left) != 3 {
				t.Errorf("%s: the week's last hour holds part of the work, the rest left for want of time: %+v", c.name, p)
			}
			continue
		}
		for _, name := range []string{"c1", "c2", "q1"} {
			if _, ok := planned[w.tasks[name]]; !ok {
				t.Errorf("%s: %s isn't planned: %+v", c.name, name, p)
			}
		}
		if c1, c2 := planned[w.tasks["c1"]], planned[w.tasks["c2"]]; c2.Start.Before(c1.End) || c2.After != w.tasks["c1"] {
			t.Errorf("%s: c2 after c1: %+v %+v", c.name, c1, c2)
		}
	}
}
