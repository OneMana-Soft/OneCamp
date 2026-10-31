//go:build integration

package integration_test

// linear-16: milestones. A project's admins make them (a name, a few words, a
// target day) and order them; whoever may edit a task puts it in one, one by
// one or many at once; a milestone's progress and span count the tasks its
// reader may open, and the first unfinished one is marked next. Shared with
// another project (by the admins of both), it counts that project's tasks
// too, for those who may open them; its home's admins keep changing it.
// Run: integ_rel.sh <wt> <log> 'TestLinear16'

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	milestoneController "github.com/akashc777/OneCamp/controllers/Milestone"
	taskBulkController "github.com/akashc777/OneCamp/controllers/TaskBulk"
	"github.com/akashc777/OneCamp/tests/integration"
)

type l16View struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	TargetDate *string `json:"target_date"`
	Progress   struct {
		Total, Started, Done int
	} `json:"progress"`
	Starts   *time.Time `json:"starts"`
	Ends     *time.Time `json:"ends"`
	Home     bool       `json:"home"`
	Projects []struct {
		UUID string `json:"project_uuid"`
		Name string `json:"project_name"`
	} `json:"projects"`
	HiddenProjects int  `json:"hidden_projects"`
	Next           bool `json:"next"`
	CanEdit        bool `json:"can_edit"`
}

type l16List struct {
	Milestones []l16View `json:"milestones"`
	CanManage  bool      `json:"can_manage"`
}

// l16Shop adds a second project, Shop, that Ana and Cal run (Ben and Cal
// aren't in each other's), with a done task and an open one, and answers its
// id and its tasks.
func l16Shop(s *wa2Scale) (string, []string) {
	s.t.Helper()
	cal := s.person("cal", false)
	shop := uuid.NewString()
	s.exec(`INSERT INTO projects (id, project_name, team_id, created_by) SELECT $1, 'Shop', team_id, $3 FROM projects WHERE id = $2`, shop, s.project, s.people["ana"])
	live, now := "0001-01-01T00:00:00Z", time.Now().UTC()
	tasks := []string{uuid.NewString(), uuid.NewString()}
	nodes := []map[string]any{
		{"uid": "_:shop", "dgraph.type": "Project", "project_uuid": shop, "project_name": "Shop", "project_deleted_at": live,
			"project_members": []map[string]any{{"uid": s.uids["ana"]}, {"uid": cal}}, "project_admins": []map[string]any{{"uid": s.uids["ana"]}, {"uid": cal}},
			"project_created_by": map[string]any{"uid": s.uids["ana"]}, "project_team": map[string]any{"uid": s.uids["team"]},
			"project_tasks": []map[string]any{{"uid": "_:s0"}, {"uid": "_:s1"}}},
		{"uid": s.uids["ana"], "user_projects": []map[string]any{{"uid": "_:shop"}}},
		{"uid": cal, "user_projects": []map[string]any{{"uid": "_:shop"}}},
	}
	for i, status := range []string{"done", "todo"} {
		nodes = append(nodes, map[string]any{"uid": fmt.Sprintf("_:s%d", i), "dgraph.type": "Task", "task_uuid": tasks[i], "task_name": fmt.Sprintf("Shop task %d", i),
			"task_status": status, "task_priority": "medium", "task_deleted_at": live, "task_created_at": now.Format(time.RFC3339),
			"task_due_date": now.AddDate(0, 0, 30+i).Format(time.RFC3339),
			"task_project":  map[string]any{"uid": "_:shop"}, "task_created_by": map[string]any{"uid": s.uids["ana"]}})
	}
	s.dg.Mutate(s.t, nodes)
	s.exec(`INSERT INTO tasks (id, project_id, created_by) SELECT unnest($1::uuid[]), $2, $3`, "{"+tasks[0]+","+tasks[1]+"}", shop, s.people["ana"])
	return shop, tasks
}

// l16Do runs a milestone handler as who on project's milestone (either may
// be ""), with a JSON body (nil for none).
func l16Do(s *wa2Scale, who, method string, h http.HandlerFunc, project, milestone string, body any) (int, string) {
	s.t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	return s.call(who, method, "/", h, map[string]string{"project_uuid": project, "milestone_id": milestone}, raw)
}

func l16Read(s *wa2Scale, who, project string) (int, l16List) {
	s.t.Helper()
	code, out := l16Do(s, who, http.MethodGet, milestoneController.ListMilestones, project, "", nil)
	var got struct{ Data l16List }
	if code == http.StatusOK {
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			s.t.Fatalf("milestones as %s: %v in %s", who, err, out)
		}
	}
	return code, got.Data
}

// l16Make makes a milestone in the project as who, and answers its id.
func l16Make(s *wa2Scale, who, project, name string, day *string) (int, string) {
	s.t.Helper()
	code, out := l16Do(s, who, http.MethodPost, milestoneController.CreateMilestone, project, "", map[string]any{"name": name, "target_date": day})
	var got struct{ Data struct{ ID string } }
	_ = json.Unmarshal([]byte(out), &got)
	return code, got.Data.ID
}

// l16Put puts a task in a milestone ("" to take it out) as who.
func l16Put(s *wa2Scale, who, task, milestone string) (int, string) {
	s.t.Helper()
	return l16Do(s, who, http.MethodPost, milestoneController.SetTaskMilestone, "", "", map[string]string{"task_uuid": task, "milestone_id": milestone})
}

func l16Find(t *testing.T, l l16List, name string) l16View {
	t.Helper()
	for _, m := range l.Milestones {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("no milestone %q in %+v", name, l.Milestones)
	return l16View{}
}

// V-51: a project with none answers an empty list, never null. The web read
// null as "not read yet", so the section drew its loading rows for ever and
// its Add stayed off: nobody could make a project's first milestone.
func TestLinear16AProjectWithNoMilestonesAnswersAnEmptyList(t *testing.T) {
	env := integration.ConnectServerPostgres(t)
	integration.StubMqttClient(t)
	s := newWA2Scale(t, env, integration.SetupDgraph(t))
	code, out := l16Do(s, "ana", http.MethodGet, milestoneController.ListMilestones, s.project, "", nil)
	if code != http.StatusOK {
		t.Fatalf("Ana reads the milestones of a project with none: %d %s", code, out)
	}
	var got struct {
		Data struct {
			Milestones json.RawMessage `json:"milestones"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("the answer isn't JSON: %v: %s", err, out)
	}
	if string(got.Data.Milestones) != "[]" {
		t.Fatalf("a project with no milestones answers milestones %s, want []", got.Data.Milestones)
	}
}

func TestLinear16MilestonesAreTheProjectsAdminsToMake(t *testing.T) {
	env := integration.ConnectServerPostgres(t)
	integration.StubMqttClient(t)
	s := newWA2Scale(t, env, integration.SetupDgraph(t))
	s.person("vic", true)
	s.person("oz", false)
	day := "2026-11-30"

	// Only the project's admins make one; someone outside it doesn't see it.
	for who, want := range map[string]int{"ben": http.StatusForbidden, "vic": http.StatusForbidden, "oz": http.StatusNotFound} {
		if code, _ := l16Make(s, who, s.project, "Beta", &day); code != want {
			t.Fatalf("%s made a milestone: %d, want %d", who, code, want)
		}
	}
	bad := "30/11/2026"
	for name, in := range map[string]map[string]any{"no name": {"name": "  "}, "a day it can't read": {"name": "Beta", "target_date": bad}} {
		if code, out := l16Do(s, "ana", http.MethodPost, milestoneController.CreateMilestone, s.project, "", in); code != http.StatusBadRequest {
			t.Fatalf("a milestone with %s: %d %s", name, code, out)
		}
	}
	code, beta := l16Make(s, "ana", s.project, "Beta", &day)
	if code != http.StatusOK || beta == "" {
		t.Fatalf("Ana makes Beta: %d", code)
	}
	if code, _ := l16Make(s, "ana", s.project, "Launch", nil); code != http.StatusOK {
		t.Fatalf("Ana makes Launch: %d", code)
	}

	// Whoever may see the project sees them, in its order; only its admins
	// may change them.
	if code, _ := l16Read(s, "oz", s.project); code != http.StatusNotFound {
		t.Fatalf("someone outside the project read its milestones: %d", code)
	}
	code, list := l16Read(s, "vic", s.project)
	if code != http.StatusOK || len(list.Milestones) != 2 || list.Milestones[0].Name != "Beta" || list.CanManage {
		t.Fatalf("the viewer's list: %d %+v", code, list)
	}
	if b := list.Milestones[0]; b.TargetDate == nil || *b.TargetDate != day || !b.Home || b.CanEdit || !b.Next || list.Milestones[1].Next {
		t.Fatalf("Beta as the viewer sees it: %+v", b)
	}
	if _, list = l16Read(s, "ana", s.project); !list.CanManage || !list.Milestones[0].CanEdit {
		t.Fatalf("Ana may change them: %+v", list)
	}
	launch := list.Milestones[1].ID

	if code, _ := l16Do(s, "ben", http.MethodPost, milestoneController.ReorderMilestones, s.project, "", map[string]any{"ids": []string{launch, beta}}); code != http.StatusForbidden {
		t.Fatalf("a member reordered the milestones: %d", code)
	}
	if code, out := l16Do(s, "ana", http.MethodPost, milestoneController.ReorderMilestones, s.project, "", map[string]any{"ids": []string{launch, beta}}); code != http.StatusOK {
		t.Fatalf("Ana reorders them: %d %s", code, out)
	}
	if _, list = l16Read(s, "ana", s.project); list.Milestones[0].ID != launch {
		t.Fatalf("Launch comes first now: %+v", list.Milestones)
	}

	if code, _ := l16Do(s, "ben", http.MethodPost, milestoneController.EditMilestone, s.project, beta, map[string]any{"name": "Beta 1"}); code != http.StatusForbidden {
		t.Fatalf("a member renamed Beta: %d", code)
	}
	if code, out := l16Do(s, "ana", http.MethodPost, milestoneController.EditMilestone, s.project, beta, map[string]any{"name": "  Beta   one ", "target_date": nil}); code != http.StatusOK {
		t.Fatalf("Ana renames Beta and takes its day off: %d %s", code, out)
	}
	if _, list = l16Read(s, "ana", s.project); l16Find(t, list, "Beta one").TargetDate != nil {
		t.Fatalf("Beta one has no day now: %+v", list.Milestones)
	}

	if code, _ := l16Do(s, "vic", http.MethodPost, milestoneController.DeleteMilestone, s.project, launch, nil); code != http.StatusForbidden {
		t.Fatalf("the viewer deleted Launch: %d", code)
	}
	if code, _ := l16Do(s, "ana", http.MethodPost, milestoneController.DeleteMilestone, s.project, uuid.NewString(), nil); code != http.StatusNotFound {
		t.Fatalf("a milestone the project doesn't show: %d", code)
	}
	if code, _ := l16Do(s, "ana", http.MethodPost, milestoneController.DeleteMilestone, s.project, launch, nil); code != http.StatusOK {
		t.Fatalf("Ana deletes Launch: %d", code)
	}
	if _, list = l16Read(s, "ana", s.project); len(list.Milestones) != 1 {
		t.Fatalf("Launch is gone: %+v", list.Milestones)
	}
}

func TestLinear16ATaskJoinsAMilestoneItsProjectShows(t *testing.T) {
	env := integration.ConnectServerPostgres(t)
	integration.StubMqttClient(t)
	s := newWA2Scale(t, env, integration.SetupDgraph(t))
	s.addTasks(12) // statuses go todo, inProgress, inReview, backlog, done, canceled; even ones are dated
	s.person("vic", true)
	s.person("oz", false)
	_, alpha := l16Make(s, "ana", s.project, "Alpha", nil)
	_, beta := l16Make(s, "ana", s.project, "Beta", nil)

	// Whoever may edit a task puts it in one; a viewer may not, and someone
	// outside the project doesn't find the task.
	if code, out := l16Put(s, "ben", s.tasks[0], beta); code != http.StatusOK || !json.Valid([]byte(out)) {
		t.Fatalf("Ben puts a task in Beta: %d %s", code, out)
	}
	for who, want := range map[string]int{"vic": http.StatusForbidden, "oz": http.StatusNotFound} {
		if code, _ := l16Put(s, who, s.tasks[1], beta); code != want {
			t.Fatalf("%s put a task in Beta: %d, want %d", who, code, want)
		}
	}
	read := func(who, task string) (int, string) {
		code, out := s.readWith(who, milestoneController.GetTaskMilestone, map[string]string{"task_uuid": task}, "")
		var got struct{ Data *struct{ ID string } }
		_ = json.Unmarshal([]byte(out), &got)
		if got.Data == nil {
			return code, ""
		}
		return code, got.Data.ID
	}
	if code, id := read("vic", s.tasks[0]); code != http.StatusOK || id != beta {
		t.Fatalf("the viewer reads the task's milestone: %d %q", code, id)
	}
	if code, _ := read("oz", s.tasks[0]); code != http.StatusNotFound {
		t.Fatalf("someone outside the project read the task's milestone: %d", code)
	}

	// A milestone its project doesn't show is refused.
	other := uuid.New()
	s.exec(`INSERT INTO milestones (id, project_uuid, name) VALUES ($1, $2, 'Elsewhere')`, other, uuid.New())
	s.exec(`INSERT INTO milestone_projects (project_uuid, milestone_id) SELECT project_uuid, id FROM milestones WHERE id = $1`, other)
	if code, out := l16Put(s, "ana", s.tasks[1], other.String()); code != http.StatusBadRequest {
		t.Fatalf("a milestone of another project: %d %s", code, out)
	}

	// Many at once: the first six into Beta, which holds the first already.
	bulk := func(ids []string, to string) (int, string) {
		raw, _ := json.Marshal(map[string]any{"task_uuids": ids, "op": "milestone", "value": to})
		return s.call("ana", http.MethodPost, "/", taskBulkController.Bulk, nil, raw)
	}
	code, out := bulk(s.tasks[:6], beta)
	var res struct {
		Data struct{ Changed, Unchanged []string }
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || code != http.StatusOK || len(res.Data.Changed) != 5 || len(res.Data.Unchanged) != 1 {
		t.Fatalf("six tasks into Beta at once: %d %s", code, out)
	}

	// Its progress counts where its six stand: done and canceled close a
	// task, in progress and in review have started; its span runs over the
	// dated ones (0, 2 and 4).
	_, list := l16Read(s, "ben", s.project)
	b := l16Find(t, list, "Beta")
	if b.Progress.Total != 6 || b.Progress.Done != 2 || b.Progress.Started != 2 || b.Starts == nil || b.Ends == nil || !b.Ends.After(*b.Starts) {
		t.Fatalf("Beta's progress and span: %+v", b)
	}

	// Alpha holds only closed tasks: finished, so Beta is next.
	if code, out := bulk([]string{s.tasks[10], s.tasks[11]}, alpha); code != http.StatusOK {
		t.Fatalf("two closed tasks into Alpha: %d %s", code, out)
	}
	_, list = l16Read(s, "ben", s.project)
	if a := l16Find(t, list, "Alpha"); a.Progress.Total != 2 || a.Progress.Done != 2 || a.Next || !l16Find(t, list, "Beta").Next {
		t.Fatalf("Alpha is finished and Beta next: %+v", list.Milestones)
	}

	// A task kept from Ben counts for nothing in his list, and its milestone
	// is his to know only when he may open it.
	if !s.restrict(1) {
		t.Fatal("restricted tasks are part of the server")
	}
	if _, list = l16Read(s, "ben", s.project); l16Find(t, list, "Beta").Progress.Total != 5 {
		t.Fatalf("Ben counts the task kept from him: %+v", l16Find(t, list, "Beta"))
	}
	if _, list = l16Read(s, "ana", s.project); l16Find(t, list, "Beta").Progress.Total != 6 {
		t.Fatalf("Ana counts every task: %+v", l16Find(t, list, "Beta"))
	}
	if code, _ := read("ben", s.tasks[2]); code != http.StatusNotFound {
		t.Fatalf("Ben read the milestone of a task kept from him: %d", code)
	}

	// Out again, one by one.
	if code, out := l16Put(s, "ana", s.tasks[0], ""); code != http.StatusOK || !json.Valid([]byte(out)) {
		t.Fatalf("Ana takes the first task out: %d %s", code, out)
	}
	if _, list = l16Read(s, "ana", s.project); l16Find(t, list, "Beta").Progress.Total != 5 {
		t.Fatalf("Beta holds five now: %+v", l16Find(t, list, "Beta"))
	}
}

func TestLinear16ASharedMilestoneCountsWhatEachReaderMayOpen(t *testing.T) {
	env := integration.ConnectServerPostgres(t)
	integration.StubMqttClient(t)
	s := newWA2Scale(t, env, integration.SetupDgraph(t))
	s.addTasks(2)
	shop, shopTasks := l16Shop(s)
	_, launch := l16Make(s, "ana", s.project, "Launch", nil)
	share := func(who, project string, to string) int {
		code, _ := l16Do(s, who, http.MethodPost, milestoneController.ShareMilestone, project, launch, map[string]string{"project_uuid": to})
		return code
	}

	// Only those who run both projects share it: Ben runs neither, and Cal
	// can't see Ana's project.
	if code := share("ben", s.project, shop); code != http.StatusForbidden {
		t.Fatalf("a member shared the milestone: %d", code)
	}
	if code := share("cal", s.project, shop); code != http.StatusNotFound {
		t.Fatalf("Cal shared a milestone of a project he can't see: %d", code)
	}
	if code := share("ana", s.project, shop); code != http.StatusOK {
		t.Fatalf("Ana shares Launch with Shop: %d", code)
	}
	if code := share("ana", s.project, shop); code != http.StatusOK {
		t.Fatalf("sharing it again: %d", code)
	}

	// Both projects' tasks join it.
	if code, out := l16Put(s, "cal", shopTasks[0], launch); code != http.StatusOK {
		t.Fatalf("Cal puts a Shop task in Launch: %d %s", code, out)
	}
	if code, out := l16Put(s, "ana", s.tasks[0], launch); code != http.StatusOK {
		t.Fatalf("Ana puts one of hers in: %d %s", code, out)
	}

	// Each reader counts and names what they may open.
	_, list := l16Read(s, "ana", s.project)
	if l := l16Find(t, list, "Launch"); l.Progress.Total != 2 || l.Progress.Done != 1 || len(l.Projects) != 2 || l.HiddenProjects != 0 || !l.Home || !l.CanEdit {
		t.Fatalf("Launch as Ana sees it: %+v", l)
	}
	_, list = l16Read(s, "ben", s.project)
	if l := l16Find(t, list, "Launch"); l.Progress.Total != 1 || len(l.Projects) != 1 || l.HiddenProjects != 1 || l.CanEdit {
		t.Fatalf("Launch as Ben sees it (Shop's task isn't his to count): %+v", l)
	}
	code, list := l16Read(s, "cal", shop)
	if l := l16Find(t, list, "Launch"); code != http.StatusOK || l.Home || l.CanEdit || l.Progress.Total != 1 || l.HiddenProjects != 1 || l.Projects[0].Name != "Shop" {
		t.Fatalf("Launch as Cal sees it in Shop: %d %+v", code, l)
	}

	// Its home's admins change it: Cal can't, from Shop.
	if code, _ := l16Do(s, "cal", http.MethodPost, milestoneController.EditMilestone, shop, launch, map[string]any{"name": "Cal's launch"}); code != http.StatusForbidden {
		t.Fatalf("Cal renamed a milestone shared with his project: %d", code)
	}

	// Every project a person is in, with its milestones: Ben's is Ana's
	// project alone.
	mine := func(who string) map[string][]l16View {
		code, out := s.call(who, http.MethodGet, "/", milestoneController.MyMilestones, nil, nil)
		var got struct {
			Data struct{ Projects map[string][]l16View }
		}
		if err := json.Unmarshal([]byte(out), &got); err != nil || code != http.StatusOK {
			t.Fatalf("%s's milestones: %d %s", who, code, out)
		}
		return got.Data.Projects
	}
	if m := mine("ben"); len(m) != 1 || len(m[s.project]) != 1 || m[s.project][0].Progress.Total != 1 {
		t.Fatalf("Ben's projects' milestones: %+v", m)
	}
	if m := mine("cal"); len(m) != 1 || len(m[shop]) != 1 {
		t.Fatalf("Cal's projects' milestones: %+v", m)
	}

	// Taken off Shop (by Cal, from his own list), it leaves with Shop's tasks.
	if code, out := l16Do(s, "cal", http.MethodPost, milestoneController.UnshareMilestone, shop, launch, map[string]string{}); code != http.StatusOK {
		t.Fatalf("Cal takes Launch off Shop: %d %s", code, out)
	}
	if _, list = l16Read(s, "ana", s.project); l16Find(t, list, "Launch").Progress.Total != 1 || len(l16Find(t, list, "Launch").Projects) != 1 {
		t.Fatalf("Launch without Shop: %+v", l16Find(t, list, "Launch"))
	}

	// Its home's admins take it back from a project they shared it with, but
	// never off its home.
	if code := share("ana", s.project, shop); code != http.StatusOK {
		t.Fatalf("Ana shares it again: %d", code)
	}
	if code, out := l16Do(s, "ana", http.MethodPost, milestoneController.UnshareMilestone, s.project, launch, map[string]string{"project_uuid": shop}); code != http.StatusOK {
		t.Fatalf("Ana takes Launch off Shop: %d %s", code, out)
	}
	if code, _ := l16Do(s, "ana", http.MethodPost, milestoneController.UnshareMilestone, s.project, launch, map[string]string{}); code != http.StatusBadRequest {
		t.Fatalf("a milestone taken off its home: %d", code)
	}
}
