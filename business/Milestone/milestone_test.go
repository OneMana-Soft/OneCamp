package business

import (
	"strings"
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	milestoneModel "github.com/akashc777/OneCamp/models/postgres/Milestone"
	"github.com/google/uuid"
)

func TestAMilestoneKeepsItsWordsInBoundsAndADayItCanRead(t *testing.T) {
	day := func(s string) *string { return &s }
	got, err := Input{Name: "  Beta \n launch ", Description: " the first users ", TargetDate: day(" 2026-11-30 ")}.Clean()
	if err != nil || got.Name != "Beta launch" || got.Description != "the first users" || got.TargetDate == nil || *got.TargetDate != "2026-11-30" {
		t.Fatalf("a milestone as it keeps it: %+v %v", got, err)
	}
	if got, err := (Input{Name: "Beta", TargetDate: day("")}).Clean(); err != nil || got.TargetDate != nil {
		t.Fatalf("an empty day is none: %+v %v", got, err)
	}
	for name, in := range map[string]Input{
		"no name":                 {Name: " \t"},
		"a name of 81":            {Name: strings.Repeat("é", 81)},
		"a description of 2,001":  {Name: "Beta", Description: strings.Repeat("a", 2001)},
		"a NUL":                   {Name: "Be\x00ta"},
		"a day it can't read":     {Name: "Beta", TargetDate: day("30/11/2026")},
		"a day that isn't one":    {Name: "Beta", TargetDate: day("2026-02-30")},
		"a day before the epochs": {Name: "Beta", TargetDate: day("1969-12-31")},
	} {
		if _, err := in.Clean(); err == nil {
			t.Errorf("%s: kept", name)
		} else if _, ok := err.(*MilestoneError); !ok {
			t.Errorf("%s: %v isn't in words for the person", name, err)
		}
	}
	if _, err := (Input{Name: strings.Repeat("é", 80), Description: strings.Repeat("a", 2000)}).Clean(); err != nil {
		t.Fatalf("80 and 2,000 characters are kept: %v", err)
	}
}

func TestAMilestoneCountsAndNamesOnlyWhatItsReaderMayOpen(t *testing.T) {
	p, q, r := uuid.New(), uuid.New(), uuid.New()
	m := milestoneModel.Milestone{Id: uuid.New(), ProjectUUID: p, Name: "Launch", Projects: []uuid.UUID{p, q}}
	day := func(d int) *time.Time { v := time.Date(2026, 11, d, 0, 0, 0, 0, time.UTC); return &v }
	facts := map[string]fact{
		"t1": {status: dgraphStruct.TASK_STATUS_DONE, project: key(p), start: day(2), due: day(5)},
		"t2": {status: dgraphStruct.TASK_STATUS_TODO, project: key(q), due: day(20)},
		"t3": {status: dgraphStruct.TASK_STATUS_INPROGRESS, project: key(p), due: day(9)},
		// Its project doesn't show the milestone any more.
		"t5": {status: dgraphStruct.TASK_STATUS_TODO, project: key(r), due: day(1)},
	}
	members := map[uuid.UUID][]string{m.Id: {"t1", "T2", "t3", "t4", "t5"}} // t4: a task the reader can't open
	stands := map[string]stand{key(p): {name: "Site", open: true}, key(q): {name: "Shop"}}

	v := views(p, []milestoneModel.Milestone{m}, members, facts, stands)[0]
	if v.Progress.Total != 2 || v.Progress.Done != 1 || v.Progress.Started != 1 {
		t.Errorf("counted: %+v, want the two tasks of the project they may open", v.Progress)
	}
	if len(v.Projects) != 1 || v.Projects[0].Name != "Site" || v.HiddenProjects != 1 || !v.Home || v.CanEdit {
		t.Errorf("named: %+v hidden %d home %v edit %v", v.Projects, v.HiddenProjects, v.Home, v.CanEdit)
	}
	if v.Starts == nil || !v.Starts.Equal(*day(2)) || v.Ends == nil || !v.Ends.Equal(*day(9)) {
		t.Errorf("its span runs from its counted tasks' first day to their last: %v to %v", v.Starts, v.Ends)
	}

	// Who may open Shop counts and names it too, and who runs Site edits it.
	stands[key(q)] = stand{name: "Shop", open: true}
	stands[key(p)] = stand{name: "Site", open: true, manage: true}
	v = views(q, []milestoneModel.Milestone{m}, members, facts, stands)[0]
	if v.Progress.Total != 3 || len(v.Projects) != 2 || v.HiddenProjects != 0 || v.Home || !v.CanEdit || !v.Ends.Equal(*day(20)) {
		t.Errorf("as someone in both, from Shop: %+v", v)
	}
}

func TestTheFirstUnfinishedMilestoneIsNext(t *testing.T) {
	p := uuid.New()
	ms := []milestoneModel.Milestone{{Id: uuid.New(), Projects: []uuid.UUID{p}}, {Id: uuid.New(), Projects: []uuid.UUID{p}}, {Id: uuid.New(), Projects: []uuid.UUID{p}}}
	facts := map[string]fact{
		"a": {status: dgraphStruct.TASK_STATUS_DONE, project: key(p)},
		"b": {status: dgraphStruct.TASK_STATUS_CANCELED, project: key(p)},
		"c": {status: dgraphStruct.TASK_STATUS_TODO, project: key(p)},
	}
	// The first is closed; the second has no tasks yet, so it isn't finished.
	members := map[uuid.UUID][]string{ms[0].Id: {"a", "b"}, ms[2].Id: {"c"}}
	vs := views(p, ms, members, facts, map[string]stand{key(p): {open: true}})
	if !Finished(vs[0].Progress) || vs[0].Next || !vs[1].Next || vs[2].Next {
		t.Fatalf("next: %v %v %v", vs[0].Next, vs[1].Next, vs[2].Next)
	}
}

func TestATaskWithOneDayStillSpans(t *testing.T) {
	d := func(n int) *time.Time { v := time.Date(2026, 11, n, 0, 0, 0, 0, time.UTC); return &v }
	starts, ends := widen(nil, nil, fact{due: d(10)})
	if !starts.Equal(*d(10)) || !ends.Equal(*d(10)) {
		t.Fatalf("a due day alone: %v to %v", starts, ends)
	}
	starts, ends = widen(starts, ends, fact{start: d(3)})
	if !starts.Equal(*d(3)) || !ends.Equal(*d(10)) {
		t.Fatalf("a start alone widens the start: %v to %v", starts, ends)
	}
	if s, e := widen(starts, ends, fact{}); s != starts || e != ends {
		t.Fatal("an undated task leaves the span")
	}
}

func TestAMilestoneShowsInItsProjectsWhateverTheCase(t *testing.T) {
	p := uuid.New()
	m := &milestoneModel.Milestone{Projects: []uuid.UUID{p}}
	if !ShowsIn(m, strings.ToUpper(p.String())) || ShowsIn(m, uuid.NewString()) || ShowsIn(m, "") {
		t.Fatal("shows in")
	}
}
