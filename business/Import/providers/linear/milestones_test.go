package linear

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/google/uuid"
)

func day(s string) *string { return &s }

// L-2: a project's milestones come after its issues, each a Milestone task
// due on its target date and done when Linear says so. The project gets a
// Milestone select field, its options the milestones in Linear's order, and
// each issue in a milestone (a sub-issue too) says which. A project with no
// milestones gets no field. Before, milestones were dropped.
func TestAProjectsMilestonesComeAsMilestoneTasks(t *testing.T) {
	created := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	alpha := linearMilestone{ID: "ms-a", Name: "Alpha", Description: "First **cut**", TargetDate: day("2026-10-30"), SortOrder: 1, Status: "overdue", CreatedAt: created}
	beta := linearMilestone{ID: "ms-b", Name: "Beta", TargetDate: day("2026-11-20"), SortOrder: 2, Status: "done", CreatedAt: created}
	alpha.Project.ID, beta.Project.ID = "proj", "proj"
	p := New()
	job := &importModels.Job{Id: uuid.New()}
	p.snapshotCache[job.Id] = &workspaceSnapshot{
		Projects: []linearProject{{ID: "proj", Name: "Launch", TeamID: "eng"}, {ID: "bare", Name: "Bare", TeamID: "eng"}},
		Issues: []linearIssue{
			{ID: "a", Identifier: "ENG-3", TeamID: "eng", ProjectID: "proj", MilestoneID: "ms-a", Milestone: &alpha},
			{ID: "b", Identifier: "ENG-4", TeamID: "eng", ProjectID: "proj"},
			{ID: "a1", Identifier: "ENG-12", TeamID: "eng", ProjectID: "proj", ParentID: "a", MilestoneID: "ms-b", Milestone: &beta},
			{ID: "c", Identifier: "ENG-9", TeamID: "eng", ProjectID: "bare"},
		},
		milestones: milestonesByProject([]linearMilestone{beta, alpha}),
	}

	projects, errs := p.IterProjects(context.Background(), job, nil)
	fields := map[string][]importProvider.SourceField{}
	for sp := range projects {
		fields[sp.SourceID] = sp.Fields
	}
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if len(fields["bare"]) != 0 {
		t.Errorf("a project with no milestones gets no field: %+v", fields["bare"])
	}
	if f := fields["proj"]; len(f) != 1 || f[0].SourceID != milestoneFieldID || f[0].Name != "Milestone" || f[0].Type != importProvider.FieldSelect ||
		len(f[0].Options) != 2 || f[0].Options[0].SourceID != "ms-a" || f[0].Options[0].Label != "Alpha" || f[0].Options[1].Label != "Beta" {
		t.Fatalf("the project's Milestone field, Alpha then Beta as Linear orders them: %+v", f)
	}

	tasks, errs := p.IterTasksOfProject(context.Background(), job, nil, "proj")
	var got []importProvider.SourceTask
	for st := range tasks {
		got = append(got, st)
	}
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("three issues then two milestones, got %d: %+v", len(got), got)
	}
	if got[0].Type != "" || got[1].Type != "" || got[2].Type != "" {
		t.Errorf("issues come as tasks: %q %q %q", got[0].Type, got[1].Type, got[2].Type)
	}
	if !hasMilestone(got[0], "ms-a", "Alpha") || !hasMilestone(got[1], "", "") || !hasMilestone(got[2], "ms-b", "Beta") {
		t.Errorf("each issue in a milestone says which, the sub-issue too, and the one in none says nothing: %+v / %+v / %+v", got[0].Fields, got[1].Fields, got[2].Fields)
	}
	a, b := got[3], got[4]
	if a.SourceID != "milestone:ms-a" || a.Type != importProvider.TaskMilestone || a.Name != "Alpha" || a.ProjectSourceID != "proj" ||
		a.DueDay != (importProvider.Day{Year: 2026, Month: time.October, Day: 30}) || a.DueDate != nil || a.Completed || !a.Created.Equal(created) ||
		!strings.Contains(a.Description, "First") || a.SourceKey != "" {
		t.Errorf("Alpha, a Milestone task due on its day, open while Linear says it's overdue: %+v", a)
	}
	if b.SourceID != "milestone:ms-b" || b.Type != importProvider.TaskMilestone || !b.Completed ||
		b.DueDay != (importProvider.Day{Year: 2026, Month: time.November, Day: 20}) {
		t.Errorf("Beta, done as Linear marks it: %+v", b)
	}

	subs, errs := p.IterSubtasksOfTask(context.Background(), job, nil, "a")
	var sub []importProvider.SourceTask
	for st := range subs {
		sub = append(sub, st)
	}
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if len(sub) != 1 || !hasMilestone(sub[0], "ms-b", "Beta") {
		t.Errorf("a sub-issue read with its parent says its milestone: %+v", sub)
	}

	bare, errs := p.IterTasksOfProject(context.Background(), job, nil, "bare")
	n := 0
	for st := range bare {
		n++
		if st.Type != "" {
			t.Errorf("a project with no milestones makes no Milestone task: %+v", st)
		}
	}
	if err := <-errs; err != nil || n != 1 {
		t.Fatalf("the bare project's one issue, got %d (%v)", n, err)
	}
}

// hasMilestone says whether a task's fields hold the milestone id, shown as
// name, in the Milestone field; "" asks whether it holds none.
func hasMilestone(st importProvider.SourceTask, id, name string) bool {
	for _, v := range st.Fields {
		if v.Field.SourceID == milestoneFieldID {
			return id != "" && v.Value == id && v.Text == name && v.Field.Type == importProvider.FieldSelect
		}
	}
	return id == ""
}

// The plan the person confirms counts a project's milestones among the
// tasks it will make; a project the import leaves out (another team's,
// when one team is picked) doesn't add its milestones.
func TestThePlanCountsAProjectsMilestonesAsTasks(t *testing.T) {
	ms := []linearMilestone{{ID: "m1"}, {ID: "m2"}, {ID: "m3"}}
	ms[0].Project.ID, ms[1].Project.ID, ms[2].Project.ID = "proj", "proj", "left-out"
	snap := &workspaceSnapshot{
		Projects:   []linearProject{{ID: "proj"}},
		Issues:     []linearIssue{{ID: "a", ProjectID: "proj"}, {ID: "a1", ProjectID: "proj", ParentID: "a"}},
		milestones: milestonesByProject(ms),
	}
	if plan := snap.plan(); plan.TaskCount != 4 || plan.SubtaskCount != 1 {
		t.Fatalf("tasks %d (subtasks %d), want the two issues and the project's two milestones", plan.TaskCount, plan.SubtaskCount)
	}
}

// Every project's milestones are read in one list, page by page.
func TestEveryMilestoneIsReadEveryPage(t *testing.T) {
	var hits int32
	withFakeLinear(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body, _ := readBody(r)
		if !strings.Contains(body, "projectMilestones(") {
			t.Errorf("not the milestones list: %q", body)
		}
		if atomic.AddInt32(&hits, 1) == 1 {
			_, _ = w.Write([]byte(`{"data": {"projectMilestones": {
				"nodes": [{"id": "m1", "name": "Alpha", "description": null, "targetDate": "2026-10-30", "sortOrder": 1.5, "status": "next",
					"createdAt": "2026-09-01T09:00:00Z", "updatedAt": "2026-09-02T09:00:00Z", "project": {"id": "proj"}}],
				"pageInfo": {"hasNextPage": true, "endCursor": "p1"}}}}`))
			return
		}
		if !strings.Contains(body, `"cursor":"p1"`) {
			t.Errorf("page two asks after page one: %q", body)
		}
		_, _ = w.Write([]byte(`{"data": {"projectMilestones": {
			"nodes": [{"id": "m2", "name": "Beta", "description": "Ship it", "targetDate": null, "sortOrder": 0.5, "status": "unstarted",
				"createdAt": "2026-09-03T09:00:00Z", "updatedAt": "2026-09-03T09:00:00Z", "project": {"id": "other"}}],
			"pageInfo": {"hasNextPage": false, "endCursor": ""}}}}`))
	})
	got, err := New().fetchMilestones(context.Background(), "tok")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "m1" || got[0].Description != "" || got[0].TargetDate == nil || *got[0].TargetDate != "2026-10-30" ||
		got[0].SortOrder != 1.5 || got[0].Project.ID != "proj" || got[1].TargetDate != nil || got[1].Description != "Ship it" || got[1].Project.ID != "other" {
		t.Fatalf("both pages' milestones, each with its project: %+v", got)
	}
}
