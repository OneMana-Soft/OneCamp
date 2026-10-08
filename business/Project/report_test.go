package business

import (
	"reflect"
	"testing"
	"time"

	domain "github.com/akashc777/OneCamp/domain/Project"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	timeModels "github.com/akashc777/OneCamp/models/postgres/TimeEntry"
	"github.com/google/uuid"
)

func TestBuildReport(t *testing.T) {
	ist, _ := time.LoadLocation("Asia/Kolkata")
	at := func(s string) *time.Time { v, _ := time.ParseInLocation("2006-01-02 15:04", s, ist); return &v }
	// A due date is the start of its day in the picker's zone.
	due := func(day string) *time.Time { return at(day + " 00:00") }
	maya := &dgraphStruct.DgraphUser{Uuid: "maya", UserName: "Maya"}
	jonas := &dgraphStruct.DgraphUser{Uuid: "jonas", UserName: "Jonas"}
	gone := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	left := &dgraphStruct.DgraphUser{Uuid: "left", UserName: "Left", DeletedAt: &gone}
	agent := &dgraphStruct.DgraphUser{Uuid: "agent", UserName: "Release Captain", IsBot: true}
	a, b := uuid.New(), uuid.New()

	projects := []domain.ReportProject{
		{UUID: a.String(), Name: "Q4 launch",
			Open: []*dgraphStruct.DgraphTask{
				{Status: dgraphStruct.TASK_STATUS_TODO, Assignee: maya, DueDate: due("2026-10-01"), Priority: "high", CreatedAt: at("2026-09-29 10:00")},
				{Status: dgraphStruct.TASK_STATUS_INPROGRESS, Assignee: maya, DueDate: due("2026-10-10"), Priority: "medium", CreatedAt: at("2026-08-01 10:00")},
				{Status: dgraphStruct.TASK_STATUS_INREVIEW, Assignee: jonas, Priority: "LOW"},
				{Status: dgraphStruct.TASK_STATUS_BACKLOG, Assignee: left, Priority: "someday"},
				{Status: dgraphStruct.TASK_STATUS_TODO, Assignee: agent, DueDate: due("2026-10-02")},
			},
			Closed: []*dgraphStruct.DgraphTask{
				// Done on its due day, in this week: on time.
				{Status: dgraphStruct.TASK_STATUS_DONE, Assignee: jonas, DueDate: due("2026-10-07"), StatusSince: at("2026-10-07 22:00"), CreatedAt: at("2026-09-15 09:00")},
				// Done the day after it was due, in the first week: late.
				{Status: dgraphStruct.TASK_STATUS_DONE, Assignee: maya, DueDate: due("2026-09-15"), StatusSince: at("2026-09-16 10:00")},
				// Cancelled: added, never done.
				{Status: dgraphStruct.TASK_STATUS_CANCELED, CreatedAt: at("2026-09-30 10:00"), StatusSince: at("2026-10-01 10:00")},
				// Done before the first week: not counted.
				{Status: dgraphStruct.TASK_STATUS_DONE, Assignee: maya, StatusSince: at("2026-09-13 23:00")},
			}},
		{UUID: b.String(), Name: "another project",
			Open: []*dgraphStruct.DgraphTask{{Status: dgraphStruct.TASK_STATUS_TODO, Assignee: jonas}}},
	}
	hours := []timeModels.WeekSeconds{
		{ProjectUUID: a, Week: time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC), Seconds: 5400},
		{ProjectUUID: b, Week: time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC), Seconds: 3600},
		{ProjectUUID: a, Week: time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC), Seconds: 3600},
	}
	now := *at("2026-10-08 12:00") // a Thursday
	r := BuildReport(projects, hours, now, ist, 4, map[string]bool{a.String(): true})

	if want := []string{"2026-09-14", "2026-09-21", "2026-09-28", "2026-10-05"}; !reflect.DeepEqual(r.Weeks, want) {
		t.Fatalf("weeks %v, want %v", r.Weeks, want)
	}
	if !reflect.DeepEqual(r.Done, []int{1, 0, 0, 1}) || r.DoneTotal != 2 {
		t.Errorf("done each week %v (%d), want [1 0 0 1] (2)", r.Done, r.DoneTotal)
	}
	if !reflect.DeepEqual(r.Added, []int{1, 0, 2, 0}) {
		t.Errorf("added each week %v, want [1 0 2 0]", r.Added)
	}
	// The agent's task counts in the totals, not as anyone's load.
	if r.Open != 5 || r.Overdue != 2 || r.DueThisWeek != 1 {
		t.Errorf("open %d overdue %d due this week %d, want 5 2 1", r.Open, r.Overdue, r.DueThisWeek)
	}
	if r.OnTime == nil || *r.OnTime != 50 {
		t.Errorf("on time %v, want 50%%", r.OnTime)
	}
	if len(r.Projects) != 1 || r.Projects[0].Name != "Q4 launch" || r.Projects[0].ToDo != 3 || r.Projects[0].InProgress != 1 ||
		r.Projects[0].InReview != 1 || r.Projects[0].Overdue != 2 || r.Projects[0].Done != 2 {
		t.Errorf("the project's row: %+v", r.Projects)
	}
	if len(r.All) != 2 || r.All[0].Name != "another project" {
		t.Errorf("every project to choose from, by name: %+v", r.All)
	}
	// Maya has the most open, Jonas next, the deleted account's is nobody's, last.
	var order []string
	for _, p := range r.People {
		order = append(order, p.UUID)
	}
	if !reflect.DeepEqual(order, []string{"maya", "jonas", ""}) {
		t.Fatalf("people %v, want [maya jonas \"\"]", order)
	}
	if r.People[0].Open() != 2 || r.People[0].Overdue != 1 || r.People[0].Done != 1 || r.People[1].Done != 1 {
		t.Errorf("Maya %+v, Jonas %+v", r.People[0], r.People[1])
	}
	var prio []string
	for _, p := range r.Priorities {
		prio = append(prio, p.Priority)
	}
	if !reflect.DeepEqual(prio, []string{"high", "medium", "low", ""}) || r.Priorities[3].Open != 2 || r.Priorities[3].Overdue != 1 {
		t.Errorf("priorities %+v", r.Priorities)
	}
	// Only the chosen project's hours, in the weeks shown.
	if !reflect.DeepEqual(r.Hours, []float64{0, 0, 1.5, 0}) {
		t.Errorf("hours %v, want [0 0 1.5 0]", r.Hours)
	}
}

func TestBuildReportWithoutHours(t *testing.T) {
	r := BuildReport(nil, nil, time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC), time.UTC, 2, nil)
	if r.Hours != nil || r.OnTime != nil || len(r.Weeks) != 2 || r.Projects == nil || r.People == nil {
		t.Fatalf("an empty report with no hours: %+v", r)
	}
}

func TestWeekStartAcrossClockChanges(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	// Clocks go back on Sunday 1 November 2026; that Sunday is still in the
	// week of Monday 26 October, and the Tuesday after is in 2 November's.
	for at, want := range map[time.Time]string{
		time.Date(2026, 11, 1, 23, 30, 0, 0, ny): "2026-10-26",
		time.Date(2026, 11, 3, 0, 30, 0, 0, ny):  "2026-11-02",
		time.Date(2026, 3, 8, 3, 30, 0, 0, ny):   "2026-03-02",
	} {
		if got := weekStart(at, ny).Format(time.DateOnly); got != want {
			t.Errorf("weekStart(%v) = %s, want %s", at, got, want)
		}
	}
}

func TestNarrowToKeepsToTheReadersProjects(t *testing.T) {
	mine := []domain.ReportProject{{UUID: "a"}, {UUID: "b"}}
	if got := narrowTo(mine, []string{" a ", "gone"}); !reflect.DeepEqual(got, map[string]bool{"a": true}) {
		t.Errorf("one of mine and one gone: %v", got)
	}
	// Every choice gone (the demo's projects are new each night): all of them, not none.
	if got := narrowTo(mine, []string{"gone", "also-gone"}); len(got) != 0 {
		t.Errorf("only gone projects asked for: %v, want no narrowing", got)
	}
	if got := narrowTo(mine, nil); len(got) != 0 {
		t.Errorf("none asked for: %v", got)
	}
}

// Done by the end of its due day in the reader's zone, wherever the due date's
// instant falls: a seeded due date at 17:00 UTC, finished the next morning in
// India, is a day late, not on time.
func TestOnTimeIsByTheEndOfTheDueDay(t *testing.T) {
	ist, _ := time.LoadLocation("Asia/Kolkata")
	due := time.Date(2026, 9, 30, 17, 0, 0, 0, time.UTC) // 22:30 on 30 Sep in India
	late := time.Date(2026, 10, 1, 9, 0, 0, 0, ist)
	onTime := time.Date(2026, 9, 30, 23, 30, 0, 0, ist)
	projects := []domain.ReportProject{{UUID: uuid.NewString(), Name: "P", Closed: []*dgraphStruct.DgraphTask{
		{Status: dgraphStruct.TASK_STATUS_DONE, DueDate: &due, StatusSince: &late},
		{Status: dgraphStruct.TASK_STATUS_DONE, DueDate: &due, StatusSince: &onTime},
	}}}
	r := BuildReport(projects, nil, time.Date(2026, 10, 8, 12, 0, 0, 0, ist), ist, 4, nil)
	if r.OnTime == nil || *r.OnTime != 50 {
		t.Fatalf("on time %v, want 50%%", r.OnTime)
	}
}
