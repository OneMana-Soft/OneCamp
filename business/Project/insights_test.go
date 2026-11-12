package business

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	domain "github.com/akashc777/OneCamp/domain/Project"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	taskFieldModel "github.com/akashc777/OneCamp/models/postgres/TaskField"
	"github.com/google/uuid"
)

func TestInsights(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2026, 9, d, 12, 0, 0, 0, time.UTC) }
	at := func(d int) *time.Time { x := day(d); return &x }
	change := func(d int, from, to string) *dgraphStruct.DgraphTaskActivity {
		return &dgraphStruct.DgraphTaskActivity{Type: dgraphStruct.ACTIVITY_TYPE_STATUS, LogTime: at(d), PrevState: from, NextState: to}
	}
	const (
		backlog, todo, doing, review = dgraphStruct.TASK_STATUS_BACKLOG, dgraphStruct.TASK_STATUS_TODO, dgraphStruct.TASK_STATUS_INPROGRESS, dgraphStruct.TASK_STATUS_INREVIEW
		done, dropped                = dgraphStruct.TASK_STATUS_DONE, dgraphStruct.TASK_STATUS_CANCELED
	)
	maya := &dgraphStruct.DgraphUser{Uuid: "maya", UserName: "Maya"}
	raj := &dgraphStruct.DgraphUser{Uuid: "raj", UserName: "Raj"}
	agent := &dgraphStruct.DgraphUser{Uuid: "agent", UserName: "Agent", IsBot: true}
	bug := &dgraphStruct.TaskWorkType{ID: "t-bug", Builtin: "bug", Name: "Bug"}
	task := func(name string, made int, status string, who *dgraphStruct.DgraphUser, prio string, kind *dgraphStruct.TaskWorkType, changes ...*dgraphStruct.DgraphTaskActivity) *dgraphStruct.DgraphTask {
		return &dgraphStruct.DgraphTask{Uuid: name, Name: name, CreatedAt: at(made), Status: status, Assignee: who, Priority: prio, WorkType: kind, Activity: changes}
	}
	first := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC) // a Monday
	now := day(25)
	projects := []domain.ReportProject{
		{UUID: "p1", Name: "Launch",
			Open: []*dgraphStruct.DgraphTask{
				task("F", 15, doing, raj, "low", nil, change(16, todo, doing)),
				task("G", 24, backlog, nil, "", nil),
			},
			Closed: []*dgraphStruct.DgraphTask{
				// Triaged before the weeks shown, then started, reviewed and done in them.
				task("A", 1, done, maya, "high", bug, change(3, backlog, todo), change(8, todo, doing), change(10, doing, review), change(11, review, done)),
				// Done without being started: a lead time, no cycle time.
				task("B", 9, done, agent, "", nil, change(12, todo, done)),
				// Triaged in the weeks shown; done, reopened and done again.
				task("C", 8, done, maya, "high", bug, change(14, backlog, doing), change(20, doing, done), change(21, done, doing), change(23, doing, done)),
				// Done before the weeks shown, and cancelled: neither counts.
				task("D", 1, done, maya, "high", nil, change(2, todo, done)),
				task("E", 8, dropped, maya, "high", nil, change(10, todo, dropped)),
			}},
		{UUID: "p2", Name: "Other",
			Closed: []*dgraphStruct.DgraphTask{task("H", 12, done, nil, "", nil, change(12, todo, doing), change(13, doing, done))}},
	}
	spread := func(v ...float64) helpers.Spread { return helpers.SpreadOf(v) }

	in := buildInsights(projects, insightIn{first: first, now: now})
	if !reflect.DeepEqual(in.Cycle, spread(72, 216, 24)) || !reflect.DeepEqual(in.Lead, spread(240, 72, 360, 24)) {
		t.Errorf("cycle %+v lead %+v", in.Cycle, in.Lead)
	}
	if !reflect.DeepEqual(in.Triage, spread(144)) || !reflect.DeepEqual(in.Age, spread(240, 24)) {
		t.Errorf("triage %+v age %+v", in.Triage, in.Age)
	}
	steps := map[string]helpers.Spread{}
	for _, s := range in.InStatus {
		steps[s.Status] = s.Spread
	}
	want := map[string]helpers.Spread{backlog: spread(48, 144), todo: spread(120, 72), doing: spread(48, 192, 24), review: spread(24)}
	if !reflect.DeepEqual(steps, want) {
		t.Errorf("time in status %+v", steps)
	}
	var order []string
	for _, p := range in.Points {
		order = append(order, p.Name)
	}
	if !reflect.DeepEqual(order, []string{"C", "H", "B", "A"}) {
		t.Errorf("each task done in the weeks shown, the latest first: %v", order)
	}
	a := in.Points[3]
	if a.Task != "A" || a.Project != "p1" || a.Person != "maya" || a.Priority != "high" || a.Type != "t-bug" || a.Cycle == nil || *a.Cycle != 72 || a.Lead != 240 || !a.DoneAt.Equal(day(11)) {
		t.Errorf("A's point %+v", a)
	}
	if b := in.Points[2]; b.Cycle != nil || b.Person != "" {
		t.Errorf("B was never started, and an agent is nobody's load: %+v", b)
	}

	slices := map[string]InsightSlice{}
	for _, s := range in.Slices {
		slices[s.By+":"+s.Key] = s
	}
	for key, w := range map[string]InsightSlice{
		"project:p1":    {By: "project", Key: "p1", Name: "Launch", Cycle: spread(72, 216), Lead: spread(240, 72, 360), Age: spread(240, 24)},
		"project:p2":    {By: "project", Key: "p2", Name: "Other", Cycle: spread(24), Lead: spread(24), Age: spread()},
		"person:maya":   {By: "person", Key: "maya", Name: "Maya", Cycle: spread(72, 216), Lead: spread(240, 360), Age: spread()},
		"person:raj":    {By: "person", Key: "raj", Name: "Raj", Cycle: spread(), Lead: spread(), Age: spread(240)},
		"person:":       {By: "person", Cycle: spread(24), Lead: spread(24), Age: spread(24)},
		"priority:high": {By: "priority", Key: "high", Name: "high", Cycle: spread(72, 216), Lead: spread(240, 360), Age: spread()},
		"priority:":     {By: "priority", Cycle: spread(24), Lead: spread(72, 24), Age: spread(24)},
		"type:t-bug":    {By: "type", Key: "t-bug", Name: "Bug", Cycle: spread(72, 216), Lead: spread(240, 360), Age: spread()},
		"type:":         {By: "type", Cycle: spread(24), Lead: spread(72, 24), Age: spread(240, 24)},
	} {
		// The spreads as before; the counts and effort beside them are
		// TestInsightsByEveryDim's.
		got := slices[key]
		got.Open, got.Overdue, got.Done, got.Effort = 0, 0, 0, InsightEffort{}
		if !reflect.DeepEqual(got, w) {
			t.Errorf("slice %s = %+v, want %+v", key, slices[key], w)
		}
	}
	if _, ok := slices["person:agent"]; ok {
		t.Error("an agent's work is no one's: no slice for it")
	}

	// Only the projects kept.
	if only := buildInsights(projects, insightIn{first: first, now: now, only: map[string]bool{"p1": true}}); only.Lead.N != 3 || len(only.Points) != 3 {
		t.Errorf("only p1: lead %+v, %d points", only.Lead, len(only.Points))
	}
}

// A report keeps the latest done tasks' points, up to its cap; the spreads
// still count them all.
func TestInsightPointsAreCapped(t *testing.T) {
	defer func(n int) { insightPointsCap = n }(insightPointsCap)
	insightPointsCap = 2
	made := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	var closed []*dgraphStruct.DgraphTask
	for d := 10; d <= 13; d++ {
		end := time.Date(2026, 9, d, 12, 0, 0, 0, time.UTC)
		closed = append(closed, &dgraphStruct.DgraphTask{Uuid: end.Format("02"), Name: end.Format("02"), CreatedAt: &made, Status: dgraphStruct.TASK_STATUS_DONE,
			Activity: []*dgraphStruct.DgraphTaskActivity{{Type: dgraphStruct.ACTIVITY_TYPE_STATUS, LogTime: &end, PrevState: dgraphStruct.TASK_STATUS_TODO, NextState: dgraphStruct.TASK_STATUS_DONE}}})
	}
	in := buildInsights([]domain.ReportProject{{UUID: "p", Closed: closed}}, insightIn{first: time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC), now: time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)})
	if in.Lead.N != 4 || len(in.Points) != 2 || in.Points[0].Name != "13" || in.Points[1].Name != "12" || !in.PointsCapped {
		t.Errorf("lead %+v points %+v capped %v", in.Lead, in.Points, in.PointsCapped)
	}
}

// Work slices by a project's select, multi-select and person fields (B9-b1):
// each task by its option or person, several for a multi-select, "" for none;
// another project's tasks are in none of its slices; an option taken away
// since makes no slice; a person not found is a former member; a done task's
// point carries its keys; a text field slices by its values (each kind is
// TestInsightsByEveryFieldKind's); and the fields stop at the cap.
func TestInsightsByCustomFields(t *testing.T) {
	day := func(d int) *time.Time { x := time.Date(2026, 9, d, 12, 0, 0, 0, time.UTC); return &x }
	p1, p2 := uuid.New(), uuid.New()
	team := &taskFieldModel.Field{ID: uuid.New(), ProjectID: p1, Name: "Team", Type: taskFieldModel.TypeSelect,
		Options: []taskFieldModel.Option{{ID: "web", Label: "Web"}, {ID: "ops", Label: "Ops"}}}
	areas := &taskFieldModel.Field{ID: uuid.New(), ProjectID: p1, Name: "Areas", Type: taskFieldModel.TypeMultiSelect,
		Options: []taskFieldModel.Option{{ID: "docs", Label: "Docs"}, {ID: "site", Label: "Site"}}}
	who := &taskFieldModel.Field{ID: uuid.New(), ProjectID: p1, Name: "Reviewer", Type: taskFieldModel.TypePerson}
	notes := &taskFieldModel.Field{ID: uuid.New(), ProjectID: p1, Name: "Notes", Type: taskFieldModel.TypeText}
	values := func(kv ...string) map[string]json.RawMessage {
		m := map[string]json.RawMessage{}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = json.RawMessage(kv[i+1])
		}
		return m
	}
	task := func(name string, fields map[string]json.RawMessage, done bool) *dgraphStruct.DgraphTask {
		t := &dgraphStruct.DgraphTask{Uuid: name, Name: name, CreatedAt: day(8), Status: dgraphStruct.TASK_STATUS_TODO, Fields: fields}
		if done {
			t.Status = dgraphStruct.TASK_STATUS_DONE
			t.Activity = []*dgraphStruct.DgraphTaskActivity{{Type: dgraphStruct.ACTIVITY_TYPE_STATUS, LogTime: day(10), PrevState: dgraphStruct.TASK_STATUS_TODO, NextState: dgraphStruct.TASK_STATUS_DONE}}
		}
		return t
	}
	tf, af, wf := team.ID.String(), areas.ID.String(), who.ID.String()
	projects := []domain.ReportProject{
		{UUID: p1.String(), Name: "Launch",
			Open: []*dgraphStruct.DgraphTask{
				task("A", values(tf, `"web"`, af, `["docs","site"]`, wf, `"maya"`, notes.ID.String(), `"hi"`), false),
				task("B", values(tf, `"gone"`, af, `["docs"]`, wf, `"left"`), false),
				task("C", nil, false),
			},
			Closed: []*dgraphStruct.DgraphTask{task("D", values(tf, `"ops"`, af, `["site","docs"]`), true)}},
		{UUID: p2.String(), Name: "Other", Open: []*dgraphStruct.DgraphTask{task("E", values(tf, `"web"`), false)}},
	}
	dims, capped := fieldDims(fieldAsk{fields: []*taskFieldModel.Field{team, notes, areas, who}, names: map[string]string{"maya": "Maya"}}, projects)
	if len(dims) != 4 || capped {
		t.Fatalf("%d dims (capped %v), want Team, Notes, Areas and Reviewer", len(dims), capped)
	}
	in := buildInsights(projects, insightIn{first: *day(7), now: *day(25), fields: dims})
	got := map[string]string{}
	for _, s := range in.Slices {
		if len(s.By) > 6 && s.By[:6] == "field:" {
			got[s.By[6:]+"/"+s.Key] = fmt.Sprintf("%s age %d done %d", s.Name, s.Age.N, s.Lead.N)
		}
	}
	want := map[string]string{
		tf + "/web": "Web age 1 done 0", tf + "/": " age 2 done 0", tf + "/ops": "Ops age 0 done 1",
		af + "/docs": "Docs age 2 done 1", af + "/site": "Site age 1 done 1", af + "/": " age 1 done 0",
		wf + "/maya": "Maya age 1 done 0", wf + "/left": "Former member age 1 done 0", wf + "/": " age 1 done 1",
		notes.ID.String() + "/hi": "hi age 1 done 0", notes.ID.String() + "/": " age 2 done 1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("field slices\n got %v\nwant %v", got, want)
	}
	wantFields := []InsightField{
		{By: "field:" + tf, Name: "Team", Type: "select", Project: p1.String()},
		{By: "field:" + notes.ID.String(), Name: "Notes", Type: "text", Project: p1.String()},
		{By: "field:" + af, Name: "Areas", Type: "multi_select", Project: p1.String()},
		{By: "field:" + wf, Name: "Reviewer", Type: "person", Project: p1.String()},
	}
	if !reflect.DeepEqual(in.Fields, wantFields) {
		t.Errorf("fields %+v", in.Fields)
	}
	onlyFields := func(in map[string][]string) map[string][]string {
		out := map[string][]string{}
		for by, keys := range in {
			if strings.HasPrefix(by, "field:") {
				out[by] = keys
			}
		}
		return out
	}
	if len(in.Points) != 1 || !reflect.DeepEqual(onlyFields(in.Points[0].In), map[string][]string{"field:" + tf: {"ops"}, "field:" + af: {"site", "docs"}, "field:" + wf: {""}, "field:" + notes.ID.String(): {""}}) {
		t.Errorf("D's point: %+v", in.Points)
	}
	// Without fields, none: the report as before.
	if plain := buildInsights(projects, insightIn{first: *day(7), now: *day(25)}); len(plain.Fields) != 0 || plain.Fields == nil || len(onlyFields(plain.Points[0].In)) != 0 {
		t.Errorf("no fields: %+v, point %+v", plain.Fields, plain.Points[0])
	}
	// The cap.
	many := make([]*taskFieldModel.Field, insightFieldsCap+1)
	for i := range many {
		many[i] = &taskFieldModel.Field{ID: uuid.New(), ProjectID: p1, Type: taskFieldModel.TypeSelect}
	}
	if dims, capped := fieldDims(fieldAsk{fields: many}, nil); len(dims) != insightFieldsCap || !capped {
		t.Errorf("%d fields: %d dims, capped %v", len(many), len(dims), capped)
	}
}
