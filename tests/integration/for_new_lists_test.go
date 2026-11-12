//go:build integration

package integration_test

// UX-PROJECTS-H2-BE (PROJECTS-20, -19): the lists the app makes things from,
// through the handlers against real Postgres and Dgraph.
//
//   - New task's projects (GET /project/forNewTask) are every live project the
//     reader may add tasks to, by Work Access: a member's as well as an
//     admin's, by name. The composer read /project/projectListByAdminUID, the
//     projects the reader admins, so a member's New task from My tasks or Home
//     had none of their projects, and a member's project picked had no people
//     to assign it to.
//   - New project's teams (GET /team/forNewProject) are the teams the reader
//     may start projects in, by the workspace's "Who can make projects": a
//     team's admins, or everyone in it. The dialog read the teams the reader
//     admins, so with "Team members" chosen a member was told they had none.
//
// Run: integ_rel.sh <wt> <log> '^(TestNewTaskListsTheProjectsYouMayAddTasksTo|TestNewProjectListsTheTeamsYouMayStartProjectsIn)$'

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	taskKeys "github.com/akashc777/OneCamp/business/TaskKeys"
	teamBusiness "github.com/akashc777/OneCamp/business/Team"
	projectController "github.com/akashc777/OneCamp/controllers/Project"
	teamController "github.com/akashc777/OneCamp/controllers/Team"
	"github.com/akashc777/OneCamp/helpers"
)

type forNewProject struct {
	UUID string `json:"project_uuid"`
	Name string `json:"project_name"`
	Key  string `json:"project_key"`
	Team struct {
		Name string `json:"team_name"`
	} `json:"project_team"`
	Members []struct {
		UUID string `json:"user_uuid"`
	} `json:"project_members"`
}

func TestNewTaskListsTheProjectsYouMayAddTasksTo(t *testing.T) {
	w := seedWorkWorld(t)
	live := "0001-01-01T00:00:00Z"
	list := func(who string) []forNewProject {
		t.Helper()
		var got []forNewProject
		if code := pastCall(t, projectController.ProjectsForNewTask, http.MethodGet, "/project/forNewTask", nil, w.account(who), nil, &got); code != http.StatusOK {
			t.Fatalf("%s: %d", who, code)
		}
		if got == nil {
			t.Errorf("%s: none is an empty list, not null", who)
		}
		return got
	}
	names := func(ps []forNewProject) []string {
		out := []string{}
		for _, p := range ps {
			out = append(out, p.Name)
		}
		return out
	}
	same := func(what string, got, want []string) {
		t.Helper()
		if len(got) != len(want) {
			t.Errorf("%s: %v, want %v", what, got, want)
			return
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("%s: %v, want %v", what, got, want)
				return
			}
		}
	}

	// Website's key, as making it gives one.
	key, err := taskKeys.Ensure(context.Background(), uuid.MustParse(w.project), "Website")
	if err != nil || key == "" {
		t.Fatalf("Website's key: %q, %v", key, err)
	}

	// A member who isn't its admin: the project, with its team, its people (none outside) and its key.
	ben := list("ben")
	same("ben, a member", names(ben), []string{"Website"})
	if len(ben) == 1 {
		if ben[0].UUID != w.project || ben[0].Team.Name != "Web" || len(ben[0].Members) != 2 || ben[0].Key != key {
			t.Errorf("ben's Website: %+v", ben[0])
		}
	}
	// An admin who isn't one of its members; the team's members and its admin who aren't in it, nothing.
	same("fay, an admin", names(list("fay")), []string{"Website"})
	same("cy, in the team only", names(list("cy")), []string{})
	same("dee, the team's admin only", names(list("dee")), []string{})

	// Another two projects of ana's: one live (named in lower case), one deleted.
	billing, old := uuid.NewString(), uuid.NewString()
	for p, name := range map[string]string{billing: "billing", old: "Old site"} {
		w.exec(`INSERT INTO projects (id, project_name, team_id, created_by) VALUES ($1, $2, $3, $4)`, p, name, w.team, w.people["ana"])
	}
	gone := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	var ana struct {
		Q []struct {
			UID string `json:"uid"`
		} `json:"q"`
	}
	if err := json.Unmarshal(w.dg.Query(t, `{ q(func: eq(user_uuid, "`+w.people["ana"].String()+`")) { uid } }`), &ana); err != nil || len(ana.Q) != 1 {
		t.Fatalf("ana's node: %v", err)
	}
	me := []map[string]any{{"uid": ana.Q[0].UID}}
	w.dg.Mutate(t, []map[string]any{
		{"uid": "_:billing", "dgraph.type": "Project", "project_uuid": billing, "project_name": "billing", "project_deleted_at": live, "project_members": me},
		{"uid": "_:old", "dgraph.type": "Project", "project_uuid": old, "project_name": "Old site", "project_deleted_at": gone, "project_members": me, "project_admins": me},
	})
	same("ana: by name, whatever its case; a deleted one left out", names(list("ana")), []string{"billing", "Website"})

	// A viewer of it may not add tasks to it.
	w.exec(`INSERT INTO project_roles (project_id, user_id, role) VALUES ($1, $2, 'viewer')`, w.project, w.people["ben"])
	same("ben, a viewer", names(list("ben")), []string{})
}

func TestNewProjectListsTheTeamsYouMayStartProjectsIn(t *testing.T) {
	w := seedWorkWorld(t)
	ctx := context.Background()
	list := func(who string) []string {
		t.Helper()
		var got []struct {
			UUID string `json:"team_uuid"`
			Name string `json:"team_name"`
		}
		if code := pastCall(t, teamController.TeamsForNewProject, http.MethodGet, "/team/forNewProject", nil, w.account(who), nil, &got); code != http.StatusOK {
			t.Fatalf("%s: %d", who, code)
		}
		if got == nil {
			t.Errorf("%s: none is an empty list, not null", who)
		}
		out := []string{}
		for _, team := range got {
			out = append(out, team.Name)
		}
		return out
	}
	same := func(what string, got []string, want ...string) {
		t.Helper()
		if len(got) != len(want) {
			t.Errorf("%s: %v, want %v", what, got, want)
			return
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("%s: %v, want %v", what, got, want)
				return
			}
		}
	}

	// A team dee admins that was deleted.
	var dee struct {
		Q []struct {
			UID string `json:"uid"`
		} `json:"q"`
	}
	if err := json.Unmarshal(w.dg.Query(t, `{ q(func: eq(user_uuid, "`+w.people["dee"].String()+`")) { uid } }`), &dee); err != nil || len(dee.Q) != 1 {
		t.Fatalf("dee's node: %v", err)
	}
	w.dg.Mutate(t, []map[string]any{{"uid": "_:old", "dgraph.type": "Team", "team_uuid": uuid.NewString(), "team_name": "Old team",
		"team_deleted_at": time.Now().UTC().Add(-time.Hour).Format(time.RFC3339),
		"team_admins":     []map[string]any{{"uid": dee.Q[0].UID}}, "team_members": []map[string]any{{"uid": dee.Q[0].UID}}}})

	by := policyBusiness.Changer{UserID: w.people["wes"], Email: "wes@acme.test"}
	t.Cleanup(func() {
		_, _ = policyBusiness.Admin.Set(ctx, policyBusiness.ProjectsCreateWho.Key, json.RawMessage(`"team_admins"`), by)
	})

	// Through an API token someone acts as one of the team's members (Work
	// Access's CanMakeProjects): a team admin's power stays theirs in the app.
	viaToken := func(who string) []string {
		t.Helper()
		teams, err := teamBusiness.TeamsForNewProject(helpers.WithActsThroughToken(ctx), w.account(who).UserDgraphInfo.Uid)
		if err != nil {
			t.Fatalf("%s through a token: %v", who, err)
		}
		out := []string{}
		for _, team := range teams {
			out = append(out, team.Name)
		}
		return out
	}

	// The workspace's default: a team's admins start its projects.
	same("dee, the team's admin", list("dee"), "Web")
	same("dee through a token, as a member", viaToken("dee"))
	same("ben, one of its members", list("ben"))
	same("eve, in no team", list("eve"))

	// "Team members": everyone in a team starts projects in it.
	if _, err := policyBusiness.Admin.Set(ctx, policyBusiness.ProjectsCreateWho.Key, json.RawMessage(`"team_members"`), by); err != nil {
		t.Fatal(err)
	}
	same("ben, a member, when members may", list("ben"), "Web")
	same("cy, a member, when members may", list("cy"), "Web")
	same("dee, the team's admin, when members may", list("dee"), "Web")
	same("dee through a token, when members may", viaToken("dee"), "Web")
	same("eve, in no team, when members may", list("eve"))
}
