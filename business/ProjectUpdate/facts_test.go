package business

import (
	"strings"
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/ProjectUpdate"
)

var now = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

func ago(days int) *time.Time { t := now.AddDate(0, 0, -days); return &t }
func in(days int) *time.Time  { t := now.AddDate(0, 0, days); return &t }

func task(id, name, status string, statusSince, created, due *time.Time) *dgraphStruct.DgraphTask {
	return &dgraphStruct.DgraphTask{Uuid: id, Name: name, Status: status, StatusSince: statusSince, CreatedAt: created, DueDate: due,
		Assignee: &dgraphStruct.DgraphUser{UserName: "Maya"}}
}

func project() *dgraphStruct.DgraphProject {
	zero := time.Time{}
	qa := "QA"
	review := task("r", "SSO for the pilot", "inReview", ago(9), ago(20), in(1))
	review.CustomStatusName = &qa
	return &dgraphStruct.DgraphProject{
		Name:             "Q4 launch",
		TasksTodo:        []*dgraphStruct.DgraphTask{task("t", "Walkthrough", "todo", ago(2), ago(2), ago(3)), task("n", "No date", "todo", ago(1), ago(1), &zero)},
		TasksInProgresss: []*dgraphStruct.DgraphTask{task("p", "Announcement", "inProgress", ago(2), ago(10), in(3))},
		TasksInReview:    []*dgraphStruct.DgraphTask{review},
		TasksDone:        []*dgraphStruct.DgraphTask{task("d", "Import", "done", ago(3), ago(30), nil), task("old", "Old", "done", ago(40), ago(50), nil)},
		TasksDoneCount:   12,
	}
}

func TestGather(t *testing.T) {
	f := Gather(project(), now.AddDate(0, 0, -7), now, 3600+20*60)
	ids := func(ts []TaskFact) string {
		var s []string
		for _, x := range ts {
			s = append(s, x.ID)
		}
		return strings.Join(s, ",")
	}
	checks := map[string][2]string{
		"done": {ids(f.Done), "d"},
		// r is stalled: told once, as stuck, not also as in progress.
		"in progress": {ids(f.InProgress), "p"},
		"stuck":       {ids(f.Stuck), "r"},
		"overdue":     {ids(f.Overdue), "t"},
		"due soon":    {ids(f.DueSoon), ""},
	}
	for name, c := range checks {
		if c[0] != c[1] {
			t.Errorf("%s = %q, want %q", name, c[0], c[1])
		}
	}
	if f.Open != 4 || f.Created != 2 || f.AllDone != 12 || f.Started != 2 {
		t.Errorf("open %d created %d all done %d started %d", f.Open, f.Created, f.AllDone, f.Started)
	}
	if f.Stuck[0].Status != "QA" || f.Stuck[0].Days != 9 || f.Overdue[0].Days != 3 {
		t.Errorf("stuck %+v overdue %+v", f.Stuck[0], f.Overdue[0])
	}
}

func TestGatherCountsDaysWhereTheAuthorIs(t *testing.T) {
	// Due 8 Oct in India is stored as 7 Oct 18:30 UTC.
	ist := time.FixedZone("IST", 5*3600+1800)
	due := time.Date(2026, 10, 7, 18, 30, 0, 0, time.UTC)
	todo := task("x", "Ship", "todo", ago(1), ago(1), &due)
	p := &dgraphStruct.DgraphProject{Name: "P", TasksTodo: []*dgraphStruct.DgraphTask{todo}}
	nowIST := time.Date(2026, 10, 7, 21, 0, 0, 0, ist)
	f := Gather(p, nowIST.AddDate(0, 0, -7), nowIST, 0)
	if len(f.Overdue) != 0 || len(f.DueSoon) != 1 {
		t.Fatalf("overdue %v due soon %v", f.Overdue, f.DueSoon)
	}
	if got := DraftText(f); !strings.Contains(got, "Ship · due 8 Oct") {
		t.Errorf("the due date must read in the author's zone:\n%s", got)
	}
	// On 8 Oct in India it is due today, not late (in UTC it would read as
	// the 7th, a day overdue: the mistake this guards against).
	if f := Gather(p, now, time.Date(2026, 10, 8, 10, 0, 0, 0, ist), 0); len(f.Overdue) != 0 {
		t.Errorf("due today is not overdue: %v", f.Overdue)
	}
}

func TestSuggestHealth(t *testing.T) {
	cases := []struct {
		f    Facts
		want string
	}{
		{Facts{Open: 5}, model.OnTrack},
		{Facts{Open: 5, Stuck: make([]TaskFact, 1)}, model.AtRisk},
		{Facts{Open: 10, Overdue: make([]TaskFact, 1)}, model.AtRisk},
		{Facts{Open: 4, Overdue: make([]TaskFact, 1)}, model.OffTrack},
		{Facts{Open: 40, Overdue: make([]TaskFact, 3)}, model.OffTrack},
		{Facts{Open: 0, AllDone: 3}, model.Done},
	}
	for _, c := range cases {
		if got := SuggestHealth(c.f); got != c.want {
			t.Errorf("SuggestHealth(%+v) = %s, want %s", c.f, got, c.want)
		}
	}
}

func TestDraftText(t *testing.T) {
	got := DraftText(Gather(project(), now.AddDate(0, 0, -7), now, 3600+20*60))
	for _, want := range []string{
		"Since Wed 30 Sep: 1 task done, 2 in progress, 1 overdue.",
		"Done:\n- Import (Maya)",
		"In progress:\n- Announcement · In progress, due 10 Oct (Maya)\n\nStuck",
		"Stuck for 7 days or more:\n- SSO for the pilot · QA for 9 days, due 8 Oct (Maya)",
		"Overdue:\n- Walkthrough · was due 4 Oct (Maya)",
		"Time logged: 1h 20m.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("draft lacks %q:\n%s", want, got)
		}
	}
}

func TestWindowAndDuration(t *testing.T) {
	if w := Window(ago(3), now); !w.Equal(*ago(3)) {
		t.Errorf("recent last update: %v", w)
	}
	if w := Window(ago(30), now); !w.Equal(*ago(7)) {
		t.Errorf("old last update: %v", w)
	}
	if w := Window(nil, now); !w.Equal(*ago(7)) {
		t.Errorf("first update: %v", w)
	}
	if Duration(45*60) != "45m" || Duration(7200) != "2h" || Duration(3600+60) != "1h 1m" {
		t.Error("Duration")
	}
}

func TestEachTaskIsToldOnce(t *testing.T) {
	// Started, and late: it is listed under Overdue, and nowhere else.
	late := task("l", "Announcement", "inProgress", ago(1), ago(10), ago(1))
	p := &dgraphStruct.DgraphProject{Name: "P", TasksInProgresss: []*dgraphStruct.DgraphTask{late}}
	f := Gather(p, now.AddDate(0, 0, -7), now, 0)
	if len(f.Overdue) != 1 || len(f.InProgress) != 0 || len(f.Stuck) != 0 || f.Started != 1 {
		t.Fatalf("overdue %v in progress %v stuck %v started %d", f.Overdue, f.InProgress, f.Stuck, f.Started)
	}
	if text := DraftText(f); strings.Count(text, "Announcement") != 1 || !strings.Contains(text, "1 in progress") {
		t.Errorf("the task must appear once, and still count as under way:\n%s", text)
	}
}
