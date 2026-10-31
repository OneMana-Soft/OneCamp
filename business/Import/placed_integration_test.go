//go:build integration

package business

// A person matched by name alone attributes their work but gets no place
// here (placed.go). Linear hands over the Launch team and its Launch project:
// Ana Lima, to whom Linear gives no address, is matched to the member of that
// name; Bo Ray by his address; and a placeholder stands in for Cy Wu, whose
// address no member has. All three are the team's and the project's members,
// Ana and Bo their admins. Ana made the three Zinnia tasks: "Zinnia launch
// plan" is assigned to her, "Zinnia brief" to Bo and "Zinnia venue" to Cy.
// So Bo and Cy's placeholder join the team and the project, Bo as an admin,
// and keep their tasks. Ana joins neither, and her task comes in unassigned,
// saying in its description that Linear assigned it to her; she still made
// all three. She can't open it, find it in search, or hear of a comment on
// it, as its assignee or a member could. The report names her for the team,
// the project and the task.
// Run: PKGS=./business/Import/ integ_rel.sh <wt> <log> '^TestAPersonMatchedByNameAloneJoinsNothingAndIsAssignedNothing$'

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	taskController "github.com/akashc777/OneCamp/controllers/Task"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// zinniaProvider is Linear as it hands over the Launch team and project.
type zinniaProvider struct{ importProvider.Provider }

var zinniaMembers, zinniaAdmins = []string{"u-ana", "u-bo", "u-cy"}, []string{"u-ana", "u-bo"}

func (zinniaProvider) DefaultStatusMap() map[string]string   { return map[string]string{} }
func (zinniaProvider) DefaultPriorityMap() map[string]string { return map[string]string{} }

func (zinniaProvider) IterUsers(ctx context.Context, _ *importModels.Job, _ importProvider.JobOptions) (<-chan importProvider.SourceUser, <-chan error) {
	return importProvider.Stream(ctx, "zinnia.IterUsers", func() ([]importProvider.SourceUser, error) {
		return []importProvider.SourceUser{
			{SourceID: "u-ada", Email: "ada@acme.test", DisplayName: "Ada"},
			{SourceID: "u-ana", DisplayName: "Ana Lima"},
			{SourceID: "u-bo", Email: "bo@acme.test", DisplayName: "Bo Ray"},
			{SourceID: "u-cy", Email: "cy@elsewhere.test", DisplayName: "Cy Wu"},
		}, nil
	})
}

func (zinniaProvider) IterTeams(ctx context.Context, _ *importModels.Job, _ importProvider.JobOptions) (<-chan importProvider.SourceTeam, <-chan error) {
	return importProvider.Stream(ctx, "zinnia.IterTeams", func() ([]importProvider.SourceTeam, error) {
		return []importProvider.SourceTeam{{SourceID: "t-launch", Name: "Launch", MemberIds: zinniaMembers, AdminIds: zinniaAdmins}}, nil
	})
}

func (zinniaProvider) IterProjects(ctx context.Context, _ *importModels.Job, _ importProvider.JobOptions) (<-chan importProvider.SourceProject, <-chan error) {
	return importProvider.Stream(ctx, "zinnia.IterProjects", func() ([]importProvider.SourceProject, error) {
		return []importProvider.SourceProject{{SourceID: "p-launch", Name: "Launch", TeamSourceID: "t-launch", MemberIds: zinniaMembers, AdminIds: zinniaAdmins}}, nil
	})
}

func (zinniaProvider) IterTasksOfProject(ctx context.Context, _ *importModels.Job, _ importProvider.JobOptions, _ string) (<-chan importProvider.SourceTask, <-chan error) {
	return importProvider.Stream(ctx, "zinnia.IterTasksOfProject", func() ([]importProvider.SourceTask, error) {
		task := func(id, name, assignee string) importProvider.SourceTask {
			return importProvider.SourceTask{SourceID: id, Name: name, Status: "Todo", CreatedBy: "u-ana", AssigneeIds: []string{assignee}}
		}
		return []importProvider.SourceTask{task("L-1", "Zinnia launch plan", "u-ana"), task("L-2", "Zinnia brief", "u-bo"), task("L-3", "Zinnia venue", "u-cy")}, nil
	})
}

func TestAPersonMatchedByNameAloneJoinsNothingAndIsAssignedNothing(t *testing.T) {
	ctx := context.Background()
	world := seenWorld(t)

	people := map[string]uuid.UUID{"ada": uuid.New(), "ana": uuid.New(), "bo": uuid.New()}
	names := map[string]string{"ada": "Ada", "ana": "Ana Lima", "bo": "Bo Ray"}
	var nodes []map[string]any
	for who, id := range people {
		if _, err := world.env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@acme.test', $2, $3, NOW(), NOW())`,
			id, who, names[who]); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + who, "dgraph.type": "User", "user_uuid": id.String(), "user_name": names[who], "user_deleted_at": "0001-01-01T00:00:00Z"})
	}
	if _, err := world.env.PG.Exec(`INSERT INTO admin_users (email_id) VALUES ('ada@acme.test')`); err != nil {
		t.Fatal(err)
	}
	uids := world.dg.Mutate(t, nodes)
	ada := people["ada"]
	job := &importModels.Job{Id: uuid.New(), Provider: importModels.ProviderLinear, SourceWorkspaceName: "acme", Source: importModels.SourceAPI,
		Status: importModels.StatusRunning, TriggeredBy: &ada}
	if err := importModels.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	importer := &userModels.UserInfo{UserPostgresInfo: userModels.User{Id: ada}}
	importer.UserDgraphInfo.Uid, importer.UserDgraphInfo.Uuid = uids["ada"], ada.String()

	// The import's stages, as RunImport runs them.
	work, prov, opts := importContext(ctx), zinniaProvider{}, importProvider.JobOptions{}
	if err := runUsersStage(work, prov, job, opts); err != nil {
		t.Fatalf("people: %v", err)
	}
	if err := runTeamsStage(work, prov, job, opts, importer); err != nil {
		t.Fatalf("teams: %v", err)
	}
	if err := runProjectsStage(work, prov, job, opts, importer); err != nil {
		t.Fatalf("projects: %v", err)
	}
	chunk, err := importModels.ClaimNextChunk(ctx, job.Id, []string{importModels.ChunkProjectTasks}, "zinnia")
	if err != nil || chunk == nil {
		t.Fatalf("the project's tasks chunk: %+v (%v)", chunk, err)
	}
	if err := processTaskChunk(work, prov, job, opts, chunk, importer); err != nil {
		t.Fatalf("tasks: %v", err)
	}

	made := func(entity, src string) uuid.UUID {
		t.Helper()
		m, err := importModels.IdMapOf(ctx, job.Id, entity)
		if err != nil {
			t.Fatal(err)
		}
		for k, id := range m {
			if k[strings.LastIndex(k, ":")+1:] == src {
				return id
			}
		}
		t.Fatalf("%s %s didn't come across: %v", entity, src, m)
		return uuid.Nil
	}
	cy := made(importModels.EntityUser, "u-cy")
	if cy == people["ana"] || cy == people["bo"] {
		t.Fatalf("Cy came across as a member, %s, not a placeholder", cy)
	}

	// Bo and Cy's placeholder join the team and the project, Bo as an admin; Ana joins neither.
	for _, place := range []struct{ what, query string }{
		{"the team", fmt.Sprintf(`{ q(func: eq(team_uuid, %q)) { members: team_members { user_uuid } admins: team_admins { user_uuid } } }`, made(importModels.EntityTeam, "t-launch"))},
		{"the project", fmt.Sprintf(`{ q(func: eq(project_uuid, %q)) { members: project_members { user_uuid } admins: project_admins { user_uuid } } }`, made(importModels.EntityProject, "p-launch"))},
	} {
		type person struct {
			Uuid string `json:"user_uuid"`
		}
		var out struct {
			Q []map[string][]person `json:"q"`
		}
		if err := json.Unmarshal(world.dg.Query(t, place.query), &out); err != nil || len(out.Q) != 1 {
			t.Fatalf("%s in the graph: %+v (%v)", place.what, out, err)
		}
		in := func(list string, id uuid.UUID) bool {
			return slices.ContainsFunc(out.Q[0][list], func(u person) bool { return u.Uuid == id.String() })
		}
		if !in("members", people["bo"]) || !in("admins", people["bo"]) || !in("members", cy) {
			t.Errorf("%s left out Bo or Cy's placeholder: %+v", place.what, out.Q[0])
		}
		if in("members", people["ana"]) || in("admins", people["ana"]) {
			t.Errorf("Ana, matched by her name alone, joined %s: %+v", place.what, out.Q[0])
		}
	}

	// Her task comes in unassigned, saying whom Linear assigned it to; she made all three.
	type task struct {
		Description string `json:"task_description"`
		Assignee    *struct {
			Uuid string `json:"user_uuid"`
		} `json:"task_assignee"`
	}
	read := func(src string) (task, uuid.UUID) {
		t.Helper()
		id := made(importModels.EntityTask, src)
		var out struct {
			Q []task `json:"q"`
		}
		if err := json.Unmarshal(world.dg.Query(t, fmt.Sprintf(`{ q(func: eq(task_uuid, %q)) { task_description task_assignee { user_uuid } } }`, id)), &out); err != nil || len(out.Q) != 1 {
			t.Fatalf("%s in the graph: %+v (%v)", src, out, err)
		}
		var creator uuid.UUID
		if err := world.env.PG.QueryRow(`SELECT created_by FROM tasks WHERE id = $1`, id).Scan(&creator); err != nil || creator != people["ana"] {
			t.Errorf("%s was made by %v here (%v); want Ana", src, creator, err)
		}
		return out.Q[0], id
	}
	plan, planID := read("L-1")
	if plan.Assignee != nil {
		t.Errorf("Zinnia launch plan is assigned to %s, whom only her name matched", plan.Assignee.Uuid)
	}
	if !strings.HasPrefix(plan.Description, "<p><em>Imported from Linear: assigned to Ana Lima, not matched to a member by email.</em></p>") {
		t.Errorf("Zinnia launch plan doesn't say whom Linear assigned it to: %q", plan.Description)
	}
	for src, want := range map[string]uuid.UUID{"L-2": people["bo"], "L-3": cy} {
		if tk, _ := read(src); tk.Assignee == nil || tk.Assignee.Uuid != want.String() || strings.Contains(tk.Description, "not matched") {
			t.Errorf("%s: %+v, want it assigned to %s as before", src, tk, want)
		}
	}

	// The report names her for the team, the project and the task, and no one else.
	rows, err := world.env.PG.Query(`SELECT code, COALESCE(source_id, ''), message FROM import_errors WHERE import_id = $1`, job.Id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	lines := map[string]string{}
	for rows.Next() {
		var code, src, msg string
		if err := rows.Scan(&code, &src, &msg); err != nil {
			t.Fatal(err)
		}
		lines[code+" "+src] = msg
	}
	for key, want := range map[string]string{
		"MEMBERS_NOT_ADDED t-launch": "not added to this team: Ana Lima.",
		"MEMBERS_NOT_ADDED p-launch": "not added to this project: Ana Lima.",
		"ASSIGNEE_NOT_MATCHED L-1":   "assigned to Ana Lima in Linear",
	} {
		if !strings.Contains(lines[key], want) {
			t.Errorf("the report's %s line: %q, want it to say %q", key, lines[key], want)
		}
		delete(lines, key)
	}
	if len(lines) > 0 {
		t.Errorf("the report says more: %v", lines)
	}

	// Ana can't open her task, find any of them in search, or hear of a
	// comment on one; Bo, a member, can.
	planUUID := planID.String()
	if _, out := callAs(t, ctx, people["ana"], taskController.GetTaskInfo, map[string]string{"task_uuid": planUUID}, "", nil); strings.Contains(out, "Zinnia") {
		t.Errorf("Ana opens Zinnia launch plan:\n%s", out)
	}
	if _, out := callAs(t, ctx, people["bo"], taskController.GetTaskInfo, map[string]string{"task_uuid": planUUID}, "", nil); !strings.Contains(out, "Zinnia launch plan") {
		t.Errorf("Bo can't open Zinnia launch plan:\n%s", out)
	}
	if readers, err := taskDomain.Readers(ctx, planUUID, []string{people["ana"].String(), people["bo"].String()}); err != nil ||
		readers[people["ana"].String()] || !readers[people["bo"].String()] {
		t.Errorf("who may read Zinnia launch plan, and hear of its comments: %v (%v); want Bo, not Ana", readers, err)
	}
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(250 * time.Millisecond) {
		if _, n := searchAs(t, ctx, people["bo"], "zinnia"); n == 3 {
			break
		}
		if time.Now().After(deadline) {
			out, n := searchAs(t, ctx, people["bo"], "zinnia")
			t.Fatalf("Bo's search finds %d of the 3 Zinnia tasks: %s", n, out)
		}
	}
	if out, n := searchAs(t, ctx, people["ana"], "zinnia"); n != 0 {
		t.Errorf("Ana's search finds %d Zinnia tasks: %s", n, out)
	}
	if code, out := callAs(t, ctx, people["bo"], taskController.CreateTaskComment, nil, "", map[string]string{"task_uuid": planUUID, "task_comment_body": "Zinnia budget approved"}); code >= 300 {
		t.Fatalf("Bo's comment on Zinnia launch plan: %d %s", code, out)
	}
	brief := made(importModels.EntityTask, "L-2").String()
	if code, out := callAs(t, ctx, ada, taskController.CreateTaskComment, nil, "", map[string]string{"task_uuid": brief, "task_comment_body": "Zinnia brief looks good"}); code >= 300 {
		t.Fatalf("Ada's comment on Zinnia brief: %d %s", code, out)
	}
	// Bo hears of Ada's comment, made after his; by then any notice of his had gone too.
	heard := func(who uuid.UUID, text string) bool {
		topic := helpers.GetMqttTopicForUserActivity(who.String()) + " "
		return slices.ContainsFunc(world.live(), func(m string) bool { return strings.HasPrefix(m, topic) && strings.Contains(m, text) })
	}
	for deadline := time.Now().Add(30 * time.Second); !heard(people["bo"], "Zinnia brief looks good"); time.Sleep(250 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("Bo never heard of Ada's comment on his task: %v", world.live())
		}
	}
	if heard(people["ana"], "Zinnia") {
		t.Errorf("Ana heard of a comment on her task: %v", world.live())
	}
}
