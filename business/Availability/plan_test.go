package business

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// Monday 9 November 2026, in UTC, at hour:min.
func mon(day, hour, min int) time.Time { return time.Date(2026, 11, 9+day, hour, min, 0, 0, time.UTC) }

func planAsk(from, to time.Time) Ask {
	return Ask{Step: 15 * time.Minute, From: from, To: to, Earliest: from, Zone: time.UTC}
}

func inOrder(tasks ...PlanTask) []Ordered {
	out := make([]Ordered, len(tasks))
	for i, t := range tasks {
		out[i] = Ordered{UUID: t.UUID, Reason: "because " + t.Name}
	}
	return out
}

// Blocks go only into free time inside the hours, one after another, each
// as long as its task's estimate: the morning's meeting is skipped.
func TestPlanPlacesBlocksInFreeTimeWithinHours(t *testing.T) {
	me := Person{ID: "me", Hours: week("UTC", calendarBusiness.Span{Start: "09:00", End: "17:00"}),
		Busy: []calendarBusiness.Interval{{Start: mon(0, 9, 0), End: mon(0, 12, 0)}}}
	a := PlanTask{UUID: "a", Name: "Write the brief", Minutes: 60}
	b := PlanTask{UUID: "b", Name: "Reply to legal", Minutes: 30}
	c := PlanTask{UUID: "c", Name: "Review the deck", Minutes: 300}
	got, left := place([]PlanTask{a, b, c}, inOrder(a, b, c), me, planAsk(mon(0, 8, 0), mon(6, 23, 59)))
	if len(left) != 0 || len(got) != 3 {
		t.Fatalf("placed %+v, left %+v", got, left)
	}
	want := [][2]time.Time{{mon(0, 12, 0), mon(0, 13, 0)}, {mon(0, 13, 0), mon(0, 13, 30)}, {mon(1, 9, 0), mon(1, 14, 0)}}
	for i, p := range got {
		if !p.Start.Equal(want[i][0]) || !p.End.Equal(want[i][1]) {
			t.Errorf("%s: %v-%v, want %v-%v", p.TaskUUID, p.Start, p.End, want[i][0], want[i][1])
		}
		if p.Reason != "because "+p.TaskName {
			t.Errorf("%s keeps its reason: %q", p.TaskUUID, p.Reason)
		}
	}
}

// A task waits for its blocker's block when the blocker is in the plan, even
// when the order puts it first; one waiting on anything else is left out,
// its blocker named only when the reader may read it.
func TestPlanPlacesBlockedTasksAfterTheirBlockers(t *testing.T) {
	me := Person{ID: "me", Hours: week("UTC", calendarBusiness.Span{Start: "09:00", End: "17:00"})}
	a := PlanTask{UUID: "a", Name: "Draft", Minutes: 120}
	c := PlanTask{UUID: "c", Name: "Send", Minutes: 30, BlockedOpen: 1, Blocker: &taskBusiness.ReadableRef{UUID: "a", Name: "Draft"}}
	d := PlanTask{UUID: "d", Name: "Ship", Minutes: 30, BlockedOpen: 1, Blocker: &taskBusiness.ReadableRef{UUID: "x", Name: "Legal sign-off"}}
	e := PlanTask{UUID: "e", Name: "Launch", Minutes: 30, BlockedOpen: 2, Blocker: &taskBusiness.ReadableRef{UUID: "a", Name: "Draft"}}
	f := PlanTask{UUID: "f", Name: "Hidden wait", Minutes: 30, BlockedOpen: 1}
	got, left := place([]PlanTask{a, c, d, e, f}, inOrder(c, d, e, f, a), me, planAsk(mon(0, 9, 0), mon(6, 23, 59)))
	if len(got) != 2 || got[0].TaskUUID != "a" || got[1].TaskUUID != "c" {
		t.Fatalf("placed %+v", got)
	}
	if got[1].Start.Before(got[0].End) || got[1].After != "a" {
		t.Errorf("Send starts %v, before Draft ends %v (after %q)", got[1].Start, got[0].End, got[1].After)
	}
	reasons := map[string]string{}
	for _, l := range left {
		reasons[l.TaskUUID] = l.Reason
	}
	for id, want := range map[string]string{"d": "Waiting on Legal sign-off", "e": "Waiting on 2 tasks", "f": "Waiting on 1 task"} {
		if reasons[id] != want {
			t.Errorf("%s: %q, want %q", id, reasons[id], want)
		}
	}
}

// A blocker that found no time leaves what waits on it out too.
func TestPlanLeavesOutWhatWaitsOnAnUnplacedBlocker(t *testing.T) {
	me := Person{ID: "me", Hours: week("UTC", calendarBusiness.Span{Start: "09:00", End: "10:00"})}
	a := PlanTask{UUID: "a", Name: "Long job", Minutes: 120}
	c := PlanTask{UUID: "c", Name: "After it", Minutes: 30, BlockedOpen: 1, Blocker: &taskBusiness.ReadableRef{UUID: "a", Name: "Long job"}}
	got, left := place([]PlanTask{a, c}, inOrder(a, c), me, planAsk(mon(0, 9, 0), mon(6, 23, 59)))
	if len(got) != 0 || len(left) != 2 || left[0].Reason != noTimeLeft || left[1].Reason != "Waiting on Long job" {
		t.Errorf("placed %+v, left %+v", got, left)
	}
}

// A task's block is its estimate, or 30 minutes without one (4 hours at
// most); done-for-this-week tasks (skip) aren't planned again.
func TestPlanTasksAreTheEstimateOrHalfAnHour(t *testing.T) {
	due := mon(2, 17, 0)
	est := func(n int) *int { return &n }
	list := []*dgraphStruct.DgraphTask{
		{Uuid: "A", Name: "With estimate", EstimateMinutes: est(45), Project: &dgraphStruct.DgraphProject{Name: "Launch"}, DueDate: &due},
		{Uuid: "b", Name: "Without"},
		{Uuid: "c", Name: "Huge", EstimateMinutes: est(2400)},
		{Uuid: "d", Name: "Already blocked"},
		nil,
	}
	got := planTasksFrom(list, map[string]bool{"d": true})
	if len(got) != 3 {
		t.Fatalf("%+v", got)
	}
	if got[0].UUID != "a" || got[0].Minutes != 45 || got[0].ProjectName != "Launch" || got[0].Due == nil || !got[0].Due.Equal(due) {
		t.Errorf("with an estimate: %+v", got[0])
	}
	if got[1].Minutes != 30 || got[2].Minutes != 240 {
		t.Errorf("without: %d, huge: %d", got[1].Minutes, got[2].Minutes)
	}
}

// The week is the rest of the work week in the viewer's zone; on a weekend,
// or with under an hour of it left, next week.
func TestPlanCoversTheRestOfTheWorkWeek(t *testing.T) {
	kolkata, _ := time.LoadLocation("Asia/Kolkata")
	h := *week("Asia/Kolkata", calendarBusiness.Span{Start: "09:00", End: "17:00"})
	cases := []struct {
		now, from, to time.Time
	}{
		{time.Date(2026, 11, 11, 10, 0, 0, 0, kolkata), time.Date(2026, 11, 11, 10, 0, 0, 0, kolkata), time.Date(2026, 11, 16, 0, 0, 0, 0, kolkata)},
		{time.Date(2026, 11, 7, 10, 0, 0, 0, kolkata), time.Date(2026, 11, 9, 0, 0, 0, 0, kolkata), time.Date(2026, 11, 16, 0, 0, 0, 0, kolkata)},
		{time.Date(2026, 11, 13, 16, 30, 0, 0, kolkata), time.Date(2026, 11, 16, 0, 0, 0, 0, kolkata), time.Date(2026, 11, 23, 0, 0, 0, 0, kolkata)},
	}
	for _, c := range cases {
		from, to := weekWindow(h, c.now)
		if !from.Equal(c.from) || !to.Equal(c.to) {
			t.Errorf("at %v: %v to %v, want %v to %v", c.now, from.In(kolkata), to.In(kolkata), c.from, c.to)
		}
	}
}

// The week turns over when under planMinLeft of it is left, in the member's
// own zone. A member keeping every hour in UTC has the rest of this week at
// 61 and at 60 minutes before Monday 00:00 UTC, and next week's whole at 59.
// At Monday 04:21 IST (22:51 UTC on Sunday, when the 9 Nov gate's plan came
// up short) that member has the last 69 minutes of their week, while a member
// in Asia/Kolkata has the whole of their own week ahead (V-827).
func TestPlanWeekTurnsOverWithUnderAnHourLeft(t *testing.T) {
	kolkata, _ := time.LoadLocation("Asia/Kolkata")
	allDay := func(tz string) calendarBusiness.WorkingHours {
		return calendarBusiness.WorkingHours{Days: []int{0, 1, 2, 3, 4, 5, 6}, Start: "00:00", End: "24:00", TZ: tz}
	}
	at := func(day, hour, min int, loc *time.Location) time.Time {
		return time.Date(2026, 11, day, hour, min, 0, 0, loc)
	}
	cases := []struct {
		name          string
		tz            string
		now, from, to time.Time
	}{
		{"mid-week", "UTC", at(11, 10, 0, time.UTC), at(11, 10, 0, time.UTC), at(16, 0, 0, time.UTC)},
		{"61 minutes left", "UTC", at(15, 22, 59, time.UTC), at(15, 22, 59, time.UTC), at(16, 0, 0, time.UTC)},
		{"60 minutes left", "UTC", at(15, 23, 0, time.UTC), at(15, 23, 0, time.UTC), at(16, 0, 0, time.UTC)},
		{"59 minutes left", "UTC", at(15, 23, 1, time.UTC), at(16, 0, 0, time.UTC), at(23, 0, 0, time.UTC)},
		{"Mon 04:21 IST in UTC", "UTC", at(9, 4, 21, kolkata), at(9, 4, 21, kolkata), at(9, 0, 0, time.UTC)},
		{"Mon 04:21 IST in Kolkata", "Asia/Kolkata", at(9, 4, 21, kolkata), at(9, 4, 21, kolkata), at(16, 0, 0, kolkata)},
		{"59 minutes left in Kolkata", "Asia/Kolkata", at(15, 23, 1, kolkata), at(16, 0, 0, kolkata), at(23, 0, 0, kolkata)},
	}
	for _, c := range cases {
		from, to := weekWindow(allDay(c.tz), c.now)
		if !from.Equal(c.from) || !to.Equal(c.to) {
			t.Errorf("%s: %v to %v, want %v to %v", c.name, from, to, c.from, c.to)
		}
	}
}

// Without a model the plan is by due date, then the shorter task first, and
// says why each comes where it does.
func TestPlanByDueDateThenLength(t *testing.T) {
	d1, d2 := mon(1, 17, 0), mon(3, 17, 0)
	tasks := []PlanTask{
		{UUID: "late", Minutes: 30, Due: &d2},
		{UUID: "none", Minutes: 15},
		{UUID: "soonLong", Minutes: 90, Due: &d1},
		{UUID: "soonShort", Minutes: 30, Due: &d1},
	}
	got := byDue(tasks, time.UTC)
	var ids []string
	for _, o := range got {
		ids = append(ids, o.UUID)
	}
	if strings.Join(ids, " ") != "soonShort soonLong late none" {
		t.Errorf("order: %v", ids)
	}
	if got[0].Reason != "Due Tue 10 Nov" || got[3].Reason != "No due date" {
		t.Errorf("reasons: %q %q", got[0].Reason, got[3].Reason)
	}
}

// fakePlan stands in for the reads: the viewer's open tasks, a week of
// 09:00-17:00 in UTC, and busy times.
func fakePlan(t *testing.T, tasks []PlanTask, busy []calendarBusiness.Interval) {
	t.Helper()
	oldT, oldB, oldH, oldS, oldO := openTasksOf, busyFor, hoursOf, peopleSeenBy, planOrderer
	t.Cleanup(func() { openTasksOf, busyFor, hoursOf, peopleSeenBy, planOrderer = oldT, oldB, oldH, oldS, oldO })
	openTasksOf = func(context.Context, *userModels.UserInfo, map[string]bool) ([]PlanTask, error) { return tasks, nil }
	busyFor = func(context.Context, string, time.Time, time.Time) (calendarBusiness.Busy, error) {
		return calendarBusiness.Busy{Intervals: busy}, nil
	}
	hoursOf = func(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]calendarBusiness.WorkingHours, error) {
		return map[uuid.UUID]calendarBusiness.WorkingHours{ids[0]: *week("UTC", calendarBusiness.Span{Start: "09:00", End: "17:00"})}, nil
	}
	peopleSeenBy = func(_ context.Context, _ string, people []string) ([]string, []string, error) {
		return people, nil, nil
	}
}

func planner() *userModels.UserInfo {
	u := &userModels.UserInfo{}
	u.UserDgraphInfo.Uuid = uuid.NewString()
	u.UserDgraphInfo.Uid = "0x1"
	return u
}

// The order comes from the workspace's model when there is one: its order
// and reasons; a task it left out comes after, by due date. Without the AI
// edition, with AI off or when the model fails, the plan is by due date and
// says so once.
func TestPlanWeekOrdersByTheModelOrByDueDate(t *testing.T) {
	d1 := mon(1, 17, 0)
	tasks := []PlanTask{{UUID: "a", Name: "A", Minutes: 30, Due: &d1}, {UUID: "b", Name: "B", Minutes: 30}, {UUID: "c", Name: "C", Minutes: 30}}
	fakePlan(t, tasks, nil)
	now := mon(0, 8, 0)

	planOrderer = func(context.Context, *userModels.UserInfo, []PlanTask, time.Time, time.Time) (*PlanOrder, error) {
		return &PlanOrder{Order: []Ordered{{UUID: "c", Reason: "Unblocks the launch"}, {UUID: "b", Reason: "Quick win"}}, Model: "qwen3 4B", Local: true}, nil
	}
	p, err := PlanWeek(context.Background(), planner(), PlanInput{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if p.By != "model" || p.Model != "qwen3 4B" || !p.Local || p.Note != "" || len(p.Proposals) != 3 {
		t.Fatalf("%+v", p)
	}
	if p.Proposals[0].TaskUUID != "c" || p.Proposals[0].Reason != "Unblocks the launch" || p.Proposals[2].TaskUUID != "a" || p.Proposals[2].Reason != "Due Tue 10 Nov" {
		t.Errorf("order: %+v", p.Proposals)
	}
	if !p.Proposals[0].Start.Equal(mon(0, 9, 0)) {
		t.Errorf("first block at %v", p.Proposals[0].Start)
	}

	for _, c := range []struct {
		orderer PlanOrderer
		note    string
	}{
		{nil, "Planned by due date"},
		{func(context.Context, *userModels.UserInfo, []PlanTask, time.Time, time.Time) (*PlanOrder, error) {
			return nil, nil
		}, "Planned by due date: AI is off"},
		{func(context.Context, *userModels.UserInfo, []PlanTask, time.Time, time.Time) (*PlanOrder, error) {
			return nil, errors.New("model down")
		}, "Planned by due date: AI didn't answer"},
	} {
		planOrderer = c.orderer
		p, err := PlanWeek(context.Background(), planner(), PlanInput{}, now)
		if err != nil {
			t.Fatal(err)
		}
		if p.By != "due" || p.Note != c.note || len(p.Proposals) != 3 || p.Proposals[0].TaskUUID != "a" {
			t.Errorf("%q: %+v", c.note, p)
		}
	}
}

// Empty weeks say why, in words: nothing open, or no free time.
func TestPlanWeekSaysWhyItIsEmpty(t *testing.T) {
	now := mon(0, 8, 0)
	fakePlan(t, nil, nil)
	planOrderer = nil
	p, err := PlanWeek(context.Background(), planner(), PlanInput{}, now)
	if err != nil || p.Note != "Nothing to plan: no open tasks" || len(p.Proposals) != 0 {
		t.Errorf("%+v %v", p, err)
	}
	fakePlan(t, []PlanTask{{UUID: "a", Name: "A", Minutes: 30}}, []calendarBusiness.Interval{{Start: mon(0, 0, 0), End: mon(7, 0, 0)}})
	planOrderer = nil
	p, err = PlanWeek(context.Background(), planner(), PlanInput{}, now)
	if err != nil || p.Note != "No free time left this week" || len(p.Proposals) != 0 || len(p.Left) != 1 {
		t.Errorf("%+v %v", p, err)
	}
}
