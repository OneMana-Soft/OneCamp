//go:build integration
// +build integration

package integration_test

// Search finds a project's work only for the people in the project, through
// every search there is: the app's and the public API's (PROJ-BUG-3).
//
// Search used to open every project of every team the person is in. Tom and
// Maya are in the Design team; "Layoffs plan" is Maya's and Priya's, not
// Tom's. Tom couldn't open the plan, but searching "severance" showed him its
// task, the task's comment and the name of the file on it, and the project
// itself by name. Priya, taken off the plan, kept finding it the same way,
// and kept finding the task she had made in it by her name on it (PROJ-BUG-7:
// leaving a project leaves the tasks you made there).
//
// Run: go test -tags=integration ./tests/integration/ -run TestSearchFindsAProjectsWorkOnlyForItsPeople -v

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	globalSearchController "github.com/akashc777/OneCamp/controllers/GlobalSearch"
	v1Controller "github.com/akashc777/OneCamp/controllers/V1"
	commentDomain "github.com/akashc777/OneCamp/domain/Comment"
	projectDomain "github.com/akashc777/OneCamp/domain/Project"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestSearchFindsAProjectsWorkOnlyForItsPeople(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	search := integration.SetupOpenSearch(t)
	if err := opensearchInit.ConnectOpenSearch(&opensearchInit.OpenSearchConfig{Host: search.URL}); err != nil {
		t.Fatalf("create the indices: %v", err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := env.PG.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	people := map[string]uuid.UUID{"maya": uuid.New(), "tom": uuid.New(), "priya": uuid.New()}
	for name, id := range people {
		exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@acme.test', $2, $2, NOW(), NOW())`, id, name)
	}
	team, website, layoffs := uuid.New(), uuid.New(), uuid.New()
	plan, faq, comment, file, payroll := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO teams (id, team_name, created_by) VALUES ($1, 'Design', $2)`, team, people["maya"])
	exec(`INSERT INTO projects (id, project_name, team_id, created_by) VALUES ($1, 'Website', $3, $4), ($2, 'Layoffs plan', $3, $4)`, website, layoffs, team, people["maya"])
	exec(`INSERT INTO tasks (id, project_id, created_by) VALUES ($1, $2, $4), ($3, $5, $4), ($6, $2, $7)`, plan, layoffs, faq, people["maya"], website, payroll, people["priya"])
	exec(`INSERT INTO comments (id, created_by) VALUES ($1, $2)`, comment, people["maya"])

	live := "0001-01-01T00:00:00Z"
	user := func(name string, projects ...string) map[string]any {
		var in []map[string]any
		for _, p := range projects {
			in = append(in, map[string]any{"uid": "_:" + p})
		}
		return map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": people[name].String(), "user_name": name,
			"user_deleted_at": live, "user_teams": []map[string]any{{"uid": "_:team"}}, "user_projects": in}
	}
	uids := dg.Mutate(t, []map[string]any{
		user("maya", "website", "layoffs"), user("tom", "website"), user("priya", "website", "layoffs"),
		{"uid": "_:team", "dgraph.type": "Team", "team_uuid": team.String(), "team_name": "Design", "team_deleted_at": live,
			"team_members": []map[string]any{{"uid": "_:maya"}, {"uid": "_:tom"}, {"uid": "_:priya"}}, "team_admins": []map[string]any{{"uid": "_:maya"}},
			"team_projects": []map[string]any{{"uid": "_:website"}, {"uid": "_:layoffs"}}},
		{"uid": "_:website", "dgraph.type": "Project", "project_uuid": website.String(), "project_name": "Website", "project_deleted_at": live,
			"project_members": []map[string]any{{"uid": "_:maya"}, {"uid": "_:tom"}, {"uid": "_:priya"}}, "project_admins": []map[string]any{{"uid": "_:maya"}},
			"project_team": map[string]any{"uid": "_:team"}},
		{"uid": "_:layoffs", "dgraph.type": "Project", "project_uuid": layoffs.String(), "project_name": "Layoffs plan", "project_deleted_at": live,
			"project_members": []map[string]any{{"uid": "_:maya"}, {"uid": "_:priya"}}, "project_admins": []map[string]any{{"uid": "_:maya"}},
			"project_team": map[string]any{"uid": "_:team"}},
	})

	// Indexed as creating them indexes them.
	now := time.Now().Unix()
	planDesc, faqDesc := "Draft the severance terms for the second round", "Answer the severance questions on the careers page"
	taskDomain.CreateTaskWithAttachmentsInOpensearch(&openSearchStruct.OpenSearchTask{Uuid: plan.String(), TaskName: "Severance letters",
		TaskDescription: &planDesc, TaskProjectUuid: layoffs.String(), TaskProjectName: "Layoffs plan", TaskStatus: "todo",
		TaskCreatedAt: now, TaskCreatedByUserUuid: people["maya"].String()},
		[]*dgraphStruct.DgraphAttachment{{Uuid: file.String(), FileName: "severance-round-two.xlsx", ObjectKey: "files/severance-round-two.xlsx"}},
		&dgraphStruct.DgraphUser{Uuid: people["maya"].String()})
	taskDomain.CreateTaskWithAttachmentsInOpensearch(&openSearchStruct.OpenSearchTask{Uuid: faq.String(), TaskName: "Severance FAQ page",
		TaskDescription: &faqDesc, TaskProjectUuid: website.String(), TaskProjectName: "Website", TaskStatus: "todo",
		TaskCreatedAt: now, TaskCreatedByUserUuid: people["maya"].String()}, nil, &dgraphStruct.DgraphUser{Uuid: people["maya"].String()})
	payrollDesc := "Send the severance amounts to payroll"
	taskDomain.CreateTaskWithAttachmentsInOpensearch(&openSearchStruct.OpenSearchTask{Uuid: payroll.String(), TaskName: "Severance payroll export",
		TaskDescription: &payrollDesc, TaskProjectUuid: layoffs.String(), TaskProjectName: "Layoffs plan", TaskStatus: "todo",
		TaskCreatedAt: now, TaskCreatedByUserUuid: people["priya"].String()}, nil, &dgraphStruct.DgraphUser{Uuid: people["priya"].String()})
	commentDomain.CreateTaskCommentWithAttachmentsInOpenSearch(&openSearchStruct.OpenSearchComment{Uuid: comment.String(),
		CommentBody: "Legal signed off the severance amounts", CommentByUserUuid: people["maya"].String(), CommentByUserFullName: "maya",
		CommentProjectUuid: layoffs.String(), CommentProjectName: "Layoffs plan", CommentTaskUuid: plan.String(), CommentCreatedAt: now}, nil)
	projectDomain.CreateProjectInOpenSearch(&openSearchStruct.OpenSearchProject{Uuid: layoffs.String(), ProjectName: "Layoffs plan",
		ProjectTeamUuid: team.String(), ProjectTeamName: "Design", ProjectCreatedAt: now})

	// The plan's work as each search answers it: the task, its comment, its
	// file, the project, and the task Priya made in it.
	secrets := []string{plan.String(), comment.String(), "severance-round-two.xlsx", layoffs.String(), payroll.String()}
	as := func(name string) context.Context {
		t.Helper()
		pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, people[name])
		if err != nil || pg == nil {
			t.Fatalf("load %s: %v", name, err)
		}
		gr, err := userDomain.GetActiveDgraphUserInfoByUUID(ctx, people[name].String())
		if err != nil || gr == nil {
			t.Fatalf("load %s's profile: %v", name, err)
		}
		return context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr})
	}
	searches := map[string]func(who, text string) string{
		"the app's search": func(who, text string) string {
			raw, _ := json.Marshal(map[string]string{"global_search_text": text})
			rec := httptest.NewRecorder()
			globalSearchController.GetUnifiedGlobalSearch(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(as(who)))
			return rec.Body.String()
		},
		"the public API's search": func(who, text string) string {
			rec := httptest.NewRecorder()
			v1Controller.SearchV1(rec, httptest.NewRequest(http.MethodGet, "/?q="+text, nil).WithContext(as(who)))
			return rec.Body.String()
		},
	}
	found := func(body string) []string {
		var got []string
		for _, s := range secrets {
			if strings.Contains(body, s) {
				got = append(got, s)
			}
		}
		return got
	}
	eventually := func(what string, cond func() bool) {
		t.Helper()
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
			if cond() {
				return
			}
		}
		t.Fatalf("timed out waiting until %s", what)
	}

	for name, run := range searches {
		eventually("Maya finds the plan's work through "+name, func() bool { return len(found(run("maya", "severance"))) == len(secrets) })
		if got := found(run("tom", "severance")); len(got) != 0 {
			t.Errorf("Tom, in the team but not the plan, finds %v through %s", got, name)
		}
		if body := run("tom", "severance"); !strings.Contains(body, faq.String()) {
			t.Errorf("Tom doesn't find his own project's task through %s: %s", name, body)
		}
		if got := found(run("tom", "layoffs")); len(got) != 0 {
			t.Errorf("Tom finds the plan by its name through %s: %v", name, got)
		}
	}

	t.Run("someone taken off the project stops finding it", func(t *testing.T) {
		if got := found(searches["the app's search"]("priya", "severance")); len(got) != len(secrets) {
			t.Fatalf("Priya, in the plan, finds only %v", got)
		}
		if err := projectBusiness.RemoveMemberFromProject(ctx, uids["priya"], people["priya"].String(), uids["layoffs"], layoffs.String()); err != nil {
			t.Fatal(err)
		}
		for name, run := range searches {
			if got := found(run("priya", "severance")); len(got) != 0 {
				t.Errorf("Priya, taken off the plan, still finds %v through %s", got, name)
			}
		}
	})
}
