//go:build integration

package business

// Head 3 (linear-15, linear-53): a project from its source keeps its plan,
// and its tasks the time they entered their status, which the forecast's
// pace and the reports read (task_status_since).
// Run: go test -tags=integration ./business/Import/ -run TestAnImportedProjectKeepsItsPlanAndItsTasksTheirTimes -v

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	planBusiness "github.com/akashc777/OneCamp/business/ProjectPlan"
	projectPlanController "github.com/akashc777/OneCamp/controllers/ProjectPlan"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

func TestAnImportedProjectKeepsItsPlanAndItsTasksTheirTimes(t *testing.T) {
	ctx := context.Background()
	env, dg := importEnv(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := env.PG.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	by, launch := uuid.New(), uuid.New()
	exec(`INSERT INTO users (id, email_id, created_at, updated_at) VALUES ($1, 'maya@example.com', NOW(), NOW())`, by)
	exec(`INSERT INTO projects (id, project_name, created_by) VALUES ($1, 'Launch', $2)`, launch, by)
	live := "0001-01-01T00:00:00Z"
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:maya", "dgraph.type": "User", "user_uuid": by.String(), "user_name": "maya", "user_deleted_at": live},
		{"uid": "_:team", "dgraph.type": "Team", "team_uuid": uuid.NewString(), "team_name": "Eng", "team_deleted_at": live},
		{"uid": "_:p", "dgraph.type": "Project", "project_uuid": launch.String(), "project_name": "Launch", "project_deleted_at": live, "project_team": map[string]any{"uid": "_:team"}},
	})
	job := &importModels.Job{Id: uuid.New(), Provider: "linear", SourceWorkspaceName: "w", Source: "api", Status: importModels.StatusRunning}
	if err := importModels.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	maya := &userModels.UserInfo{UserPostgresInfo: userModels.User{Id: by}, UserDgraphInfo: dgraphStruct.DgraphUser{Uid: uids["maya"], Uuid: by.String()}}

	// The plan comes whole but for its lead, who never came across: the
	// report says so, and the rest is the project's.
	sp := &importProvider.SourceProject{SourceID: "launch", Plan: &importProvider.SourcePlan{
		LeadID:   "src-sam",
		Start:    &importProvider.SourceWhen{Day: mustDay(t, "2026-10-01"), Resolution: "quarter"},
		Target:   &importProvider.SourceWhen{Day: mustDay(t, "2027-06-30"), Resolution: "half"},
		Priority: "high",
		Status:   importProvider.SourcePlanStatus{Category: "paused", Name: "On hold"},
		Labels:   []importProvider.SourceLabel{{Name: "Mobile", Color: "#4ea7fc"}, {Name: "Q3 bet"}},
	}}
	keepPlan(ctx, job, sp, launch, maya)
	views, err := planBusiness.Views(ctx, []uuid.UUID{launch})
	if err != nil {
		t.Fatal(err)
	}
	v := views[launch]
	if v.Lead != nil || v.Start == nil || *v.Start != (planBusiness.When{Date: "2026-10-01", Resolution: "quarter"}) ||
		v.Target == nil || *v.Target != (planBusiness.When{Date: "2027-06-30", Resolution: "half"}) {
		t.Fatalf("Launch's lead, start and target: %+v %+v %+v", v.Lead, v.Start, v.Target)
	}
	if v.Priority != "high" || v.Status.Category != "paused" || len(v.Labels) != 2 {
		t.Fatalf("Launch's priority, status and labels: %q %+v %+v", v.Priority, v.Status, v.Labels)
	}
	if names := []string{v.Labels[0].Name, v.Labels[1].Name}; strings.Join(names, ",") != "Mobile,Q3 bet" && strings.Join(names, ",") != "Q3 bet,Mobile" {
		t.Fatalf("Launch's labels: %v", names)
	}
	rows, err := importModels.ListErrors(ctx, job.Id, importModels.SeverityWarning, 10, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("one warning, for the lead: %v %v", rows, err)
	}
	if code, _ := rows[0]["code"].(*string); code == nil || *code != "PLAN_PARTLY_KEPT" {
		t.Fatalf("the warning: %v", rows[0])
	}

	// A task done in its source entered done when it was done there; one with
	// no time of its own, when it was made.
	made := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	done := time.Date(2026, 10, 28, 16, 0, 0, 0, time.UTC)
	write := func(name string, since time.Time) uuid.UUID {
		t.Helper()
		id, _, err := writeTask(ctx, job, nil, importTaskInput{
			ProjectUUID: launch, Project: &dgraphStruct.DgraphProject{Uid: uids["p"], Uuid: launch.String(), Team: &dgraphStruct.DgraphTeam{Uid: uids["team"]}},
			Creator: maya, Importer: maya, Name: name, Status: "done", Priority: "medium", CreatedAt: made, StatusSince: since,
		})
		if err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return id
	}
	sinceOf := func(task uuid.UUID) time.Time {
		t.Helper()
		var out struct {
			T []struct {
				Since time.Time `json:"task_status_since"`
			} `json:"t"`
		}
		if err := json.Unmarshal(dg.Query(t, fmt.Sprintf(`{ t(func: eq(task_uuid, %q)) { task_status_since } }`, task)), &out); err != nil || len(out.T) != 1 {
			t.Fatalf("read %s: %v %+v", task, err, out)
		}
		return out.T[0].Since
	}
	if got := sinceOf(write("Ship", done)); !got.Equal(done) {
		t.Fatalf("Ship entered done at %v, want %v", got, done)
	}
	if got := sinceOf(write("Plan", time.Time{})); !got.Equal(made) {
		t.Fatalf("Plan entered its status at %v, want when it was made (%v)", got, made)
	}
}

// doneProvider streams P1's one task, Ship, done in its source at done, and
// Ship's one sub-task, Check, done at checked. Each was last changed later
// than it was done, so a time read from anywhere else shows.
type doneProvider struct {
	tasksProvider
	made, done, checked time.Time
}

func (p doneProvider) IterTasksOfProject(context.Context, *importModels.Job, importProvider.JobOptions, string) (<-chan importProvider.SourceTask, <-chan error) {
	return oneTask(importProvider.SourceTask{SourceID: "ship", ProjectSourceID: "P1", Name: "Ship", Status: "done", Completed: true,
		CreatedBy: "u-maya", Created: p.made, Updated: p.done.Add(48 * time.Hour), StatusSince: &p.done})
}

func (p doneProvider) IterSubtasksOfTask(_ context.Context, _ *importModels.Job, _ importProvider.JobOptions, parent string) (<-chan importProvider.SourceTask, <-chan error) {
	return oneTask(importProvider.SourceTask{SourceID: parent + "-check", ParentTaskID: parent, ProjectSourceID: "P1", Name: "Check", Status: "done", Completed: true,
		CreatedBy: "u-maya", Created: p.made, Updated: p.checked.Add(48 * time.Hour), StatusSince: &p.checked})
}

func oneTask(st importProvider.SourceTask) (<-chan importProvider.SourceTask, <-chan error) {
	out, errs := make(chan importProvider.SourceTask, 1), make(chan error)
	out <- st
	close(out)
	close(errs)
	return out, errs
}

// CR-163 row 2279 (F1, F2, S1): the import's own stages carry a project's
// plan and its tasks' times, on real stores. The project stage gives a new
// project its plan after its members join, so one of them leads it; a person
// who came across but isn't a member, or whose account is deactivated,
// doesn't, and the report says why while the rest of the plan comes. A
// workspace holding as many labels as it may takes the plan without its
// labels. The task stages keep when a done task and a done sub-task were
// done in their source. The plan is read back through its route.
func TestAnImportsStagesCarryThePlanAndTheTimes(t *testing.T) {
	ctx := context.Background()
	w := newTaskWorld(t, "linear")
	sam := uuid.New()
	if _, err := w.env.PG.Exec(`INSERT INTO users (id, email_id, created_at, updated_at) VALUES ($1, 'sam@example.com', NOW(), NOW())`, sam); err != nil {
		t.Fatal(err)
	}
	w.dg.Mutate(t, []map[string]any{{"uid": "_:sam", "dgraph.type": "User", "user_uuid": sam.String(), "user_name": "Sam", "user_deleted_at": "2026-10-01T09:00:00Z"}})
	if err := importModels.UpsertIdMappingWithOwnership(ctx, w.job.Id, importModels.EntityUser, "u-sam", sam, nil, mustMarshal(map[string]any{"matched_by": matchedByEmail}), false); err != nil {
		t.Fatal(err)
	}
	// Maya, Bo and Sam came across by their address, so the import places
	// them: a project's members join it (joining). The world maps Maya and Bo
	// by name alone.
	maya := w.placeByEmail(t, "u-maya")
	w.placeByEmail(t, "u-bo")
	if err := resolveOneTeam(ctx, w.job, &importProvider.SourceTeam{SourceID: "T2", Name: "Ops"}, w.importer); err != nil {
		t.Fatal(err)
	}

	start := planBusiness.When{Date: "2026-10-01", Resolution: "quarter"}
	target := planBusiness.When{Date: "2027-06-30", Resolution: "half"}
	project := func(src, lead string, members []string, labels ...string) uuid.UUID {
		t.Helper()
		plan := &importProvider.SourcePlan{
			LeadID:   lead,
			Start:    &importProvider.SourceWhen{Day: mustDay(t, start.Date), Resolution: start.Resolution},
			Target:   &importProvider.SourceWhen{Day: mustDay(t, target.Date), Resolution: target.Resolution},
			Priority: "high",
			Status:   importProvider.SourcePlanStatus{Category: "paused"},
		}
		for _, l := range labels {
			plan.Labels = append(plan.Labels, importProvider.SourceLabel{Name: l})
		}
		sp := &importProvider.SourceProject{SourceID: src, Name: "Roadmap " + src, TeamSourceID: "T2", MemberIds: members, Plan: plan}
		if err := resolveOneProject(ctx, w.job, sp, w.importer); err != nil {
			t.Fatalf("project %s: %v", src, err)
		}
		id, err := importModels.LookupIdMapping(ctx, w.job.Id, importModels.EntityProject, src)
		if err != nil || id == uuid.Nil {
			t.Fatalf("project %s wasn't made: %v", src, err)
		}
		return id
	}
	planOf := func(src string, p uuid.UUID) planBusiness.View {
		t.Helper()
		code, out := callAs(t, ctx, w.importer.UserPostgresInfo.Id, projectPlanController.GetPlan, map[string]string{"project_uuid": p.String()}, "", nil)
		var got struct {
			Data struct {
				Plan planBusiness.View `json:"plan"`
			} `json:"data"`
		}
		if code != http.StatusOK || json.Unmarshal([]byte(out), &got) != nil {
			t.Fatalf("%s's plan through its route: %d %s", src, code, out)
		}
		return got.Data.Plan
	}
	said := func(src string) []string {
		t.Helper()
		rows, err := importModels.ListErrors(ctx, w.job.Id, importModels.SeverityWarning, 100, 0)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range rows {
			if s, _ := r["source_id"].(*string); s != nil && *s == src {
				code, _ := r["code"].(*string)
				msg, _ := r["message"].(string)
				if code != nil {
					out = append(out, *code+": "+msg)
				}
			}
		}
		return out
	}
	labelNames := func(v planBusiness.View) []string {
		var names []string
		for _, l := range v.Labels {
			names = append(names, l.Name)
		}
		slices.Sort(names)
		return names
	}
	// rest is the plan but for its lead, as every project here asks it.
	rest := func(src string, v planBusiness.View, labels ...string) {
		t.Helper()
		if v.Start == nil || *v.Start != start || v.Target == nil || *v.Target != target || v.Priority != "high" || v.Status.Category != "paused" {
			t.Fatalf("%s's dates, priority and status: %+v %+v %q %+v", src, v.Start, v.Target, v.Priority, v.Status)
		}
		if got := labelNames(v); !slices.Equal(got, labels) {
			t.Fatalf("%s's labels %v, want %v", src, got, labels)
		}
	}

	// Maya is one of Roadmap P2's members, so she leads it.
	led := planOf("P2", project("P2", "u-maya", []string{"u-maya"}, "Mobile", "Q3 bet"))
	if led.Lead == nil || !strings.EqualFold(led.Lead.UUID, maya.String()) {
		t.Fatalf("P2's lead %+v, want Maya (%s); its report says %q", led.Lead, maya, said("P2"))
	}
	rest("P2", led, "Mobile", "Q3 bet")
	if got := said("P2"); len(got) != 0 {
		t.Fatalf("P2 came whole, and its report says %v", got)
	}

	// Bo came across, but isn't one of P3's members: it comes without a lead,
	// and the report says why.
	notMember := planOf("P3", project("P3", "u-bo", []string{"u-maya"}, "Mobile"))
	if notMember.Lead != nil {
		t.Fatalf("P3 is led by %+v, who isn't one of its members", notMember.Lead)
	}
	rest("P3", notMember, "Mobile")
	if got, want := said("P3"), []string{"PLAN_PARTLY_KEPT: Its plan came without its lead (who isn't one of its admins or members here)."}; !slices.Equal(got, want) {
		t.Fatalf("P3's report %q, want %q", got, want)
	}

	// Sam is one of P4's members, but his account is deactivated: he keeps
	// his place, takes no new work, and doesn't lead it.
	deactivated := planOf("P4", project("P4", "u-sam", []string{"u-maya", "u-sam"}, "Q3 bet"))
	if deactivated.Lead != nil {
		t.Fatalf("P4 is led by %+v, whose account is deactivated", deactivated.Lead)
	}
	rest("P4", deactivated, "Q3 bet")
	if got, want := said("P4"), []string{"PLAN_PARTLY_KEPT: Its plan came without its lead (Sam can't be made the project's lead: their account is deactivated)."}; !slices.Equal(got, want) {
		t.Fatalf("P4's report %q, want %q", got, want)
	}

	// S1: the workspace holds as many labels as it may, so the plan's save is
	// refused for its new label; it's made again without its labels, and the
	// report gives the refusal's words.
	var live int
	if err := w.env.PG.QueryRow(`SELECT COUNT(*) FROM project_labels WHERE deleted_at IS NULL`).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if _, err := w.env.PG.Exec(`INSERT INTO project_labels (name) SELECT 'Filler ' || g FROM generate_series(1, $1::int) g`, planBusiness.MostLabels-live); err != nil {
		t.Fatal(err)
	}
	full := planOf("P5", project("P5", "u-maya", []string{"u-maya"}, "Mobile", "Growth"))
	if full.Lead == nil || !strings.EqualFold(full.Lead.UUID, maya.String()) {
		t.Fatalf("P5's lead %+v, want Maya", full.Lead)
	}
	rest("P5", full)
	want := fmt.Sprintf("PLAN_PARTLY_KEPT: Its plan came without its labels: The workspace has %d labels, as many as it may. Delete one first.", planBusiness.MostLabels)
	if got := said("P5"); !slices.Equal(got, []string{want}) {
		t.Fatalf("P5's report %q, want %q", got, want)
	}

	// Lee came across by name alone, and a name doesn't show he's the same
	// person: he neither joins P6 nor leads it, and the report says both.
	lee := uuid.New()
	if _, err := w.env.PG.Exec(`INSERT INTO users (id, email_id, display_name, created_at, updated_at) VALUES ($1, 'lee@example.com', 'Lee', NOW(), NOW())`, lee); err != nil {
		t.Fatal(err)
	}
	w.dg.Mutate(t, []map[string]any{{"uid": "_:lee", "dgraph.type": "User", "user_uuid": lee.String(), "user_name": "Lee"}})
	if err := importModels.UpsertIdMappingWithOwnership(ctx, w.job.Id, importModels.EntityUser, "u-lee", lee, nil, mustMarshal(map[string]any{"matched_by": matchedByName}), false); err != nil {
		t.Fatal(err)
	}
	byName := planOf("P6", project("P6", "u-lee", []string{"u-maya", "u-lee"}))
	if byName.Lead != nil {
		t.Fatalf("P6 is led by %+v, who came across by name alone", byName.Lead)
	}
	rest("P6", byName)
	got := said("P6")
	slices.Sort(got)
	if want := []string{
		"MEMBERS_NOT_ADDED: Matched to members here by name alone, so not added to this project: Lee. A name doesn't show it's the same person: add them on purpose.",
		"PLAN_PARTLY_KEPT: Its plan came without its lead (who came across by name alone).",
	}; !slices.Equal(got, want) {
		t.Fatalf("P6's report %q, want %q", got, want)
	}

	// The task stages: Ship and its sub-task Check entered done when they were
	// done in their source.
	prov := doneProvider{
		made:    time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC),
		done:    time.Date(2026, 10, 28, 16, 0, 0, 0, time.UTC),
		checked: time.Date(2026, 10, 27, 11, 30, 0, 0, time.UTC),
	}
	w.importTasks(t, prov)
	w.importSteps(t, prov, "ship")
	for _, c := range []struct {
		entity, src string
		want        time.Time
	}{{importModels.EntityTask, "ship", prov.done}, {importModels.EntitySubtask, "ship-check", prov.checked}} {
		id, err := importModels.LookupIdMapping(ctx, w.job.Id, c.entity, c.src)
		if err != nil || id == uuid.Nil {
			t.Fatalf("%s wasn't made: %v", c.src, err)
		}
		var out struct {
			T []struct {
				Since time.Time `json:"task_status_since"`
			} `json:"t"`
		}
		if err := json.Unmarshal(w.dg.Query(t, fmt.Sprintf(`{ t(func: eq(task_uuid, %q)) { task_status_since } }`, id)), &out); err != nil || len(out.T) != 1 {
			t.Fatalf("read %s: %v %+v", c.src, err, out)
		}
		if !out.T[0].Since.Equal(c.want) {
			t.Fatalf("%s entered done at %v, want %v", c.src, out.T[0].Since, c.want)
		}
	}
}

func mustDay(t *testing.T, s string) importProvider.Day {
	t.Helper()
	d, ok := importProvider.ParseDay(s)
	if !ok {
		t.Fatalf("bad day %q", s)
	}
	return d
}
