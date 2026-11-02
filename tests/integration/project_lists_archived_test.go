//go:build integration
// +build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	projectController "github.com/akashc777/OneCamp/controllers/Project"
	teamController "github.com/akashc777/OneCamp/controllers/Team"
	userController "github.com/akashc777/OneCamp/controllers/User"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// V-161 (31 Oct): a list someone picks a project from never offers an archived
// one. The demo's nightly refresh archives yesterday's "Q4 launch" as a
// "-retired-" copy; /user/userProjectList listed it first, as the graph keeps a
// person's projects oldest first, and the board's "Add from OneCamp" opened it
// and waited for ever. Each such list leaves archived projects out through the
// shared zero-epoch filter (helpers/dgraphquery.Live). The team's own list
// keeps them, marked, because that is where its admins restore one.
func TestProjectListsLeaveArchivedProjectsOut(t *testing.T) {
	w := seedWorkWorld(t)
	ctx := context.Background()
	integration.SetupRedis(t)
	search := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(rw, `{"took":1,"errors":false,"items":[],"updated":0,"failures":[]}`)
	}))
	t.Cleanup(search.Close)
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client
	t.Cleanup(firebaseInit.ObservePushesForTest(func(map[string]string, []string) {}))

	// Website, the world's project, is the older of ana's and ben's two, so
	// the graph lists it first; it is the one archived below. Launch is made
	// after it, in the same team.
	ana, ben := w.account("ana"), w.account("ben")
	site, err := projectBusiness.GetBasicDgraphProjectInfo(ctx, w.project, ana.UserDgraphInfo.Uid)
	if err != nil || site == nil || site.Team == nil || site.Team.Uid == "" {
		t.Fatalf("the world's project and its team: %+v, %v", site, err)
	}
	launch := uuid.NewString()
	w.exec(`INSERT INTO projects (id, project_name, team_id, created_by) VALUES ($1, 'Launch', $2, $3)`, launch, w.team, w.people["ana"])
	anaRef, benRef := map[string]any{"uid": ana.UserDgraphInfo.Uid}, map[string]any{"uid": ben.UserDgraphInfo.Uid}
	theirs := []map[string]any{{"uid": site.Uid}, {"uid": "_:launch"}}
	w.dg.Mutate(t, []map[string]any{
		{"uid": "_:launch", "dgraph.type": "Project", "project_uuid": launch, "project_name": "Launch",
			"project_deleted_at": "0001-01-01T00:00:00Z", "project_members": []map[string]any{anaRef, benRef},
			"project_admins": []map[string]any{anaRef}, "project_created_by": anaRef,
			"project_team": map[string]any{"uid": site.Team.Uid}},
		{"uid": site.Team.Uid, "team_projects": []map[string]any{{"uid": "_:launch"}}},
		// A person's own edges to their projects, as joining one writes them.
		{"uid": ana.UserDgraphInfo.Uid, "user_projects": theirs},
		{"uid": ben.UserDgraphInfo.Uid, "user_projects": theirs},
	})

	// do runs a handler as the person, with the route's parameters and a
	// JSON body, and fails the test on anything but 200.
	do := func(as userModels.UserInfo, h http.HandlerFunc, params map[string]string, body any) []byte {
		t.Helper()
		var rd io.Reader = http.NoBody
		if body != nil {
			raw, _ := json.Marshal(body)
			rd = bytes.NewReader(raw)
		}
		rc := chi.NewRouteContext()
		for k, v := range params {
			rc.URLParams.Add(k, v)
		}
		c := context.WithValue(context.WithValue(ctx, helpers.UserInfoContextKey, as), chi.RouteCtxKey, rc)
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPost, "/", rd).WithContext(c))
		if rec.Code != http.StatusOK {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		return rec.Body.Bytes()
	}
	type row struct {
		Name      string     `json:"project_name"`
		DeletedAt *time.Time `json:"project_deleted_at"`
	}
	// names is the list as its reader sees it, in its order; a project the
	// list marks as archived reads "(archived)".
	names := func(raw []byte, rows func(any) []row, into any) string {
		t.Helper()
		if err := json.Unmarshal(raw, into); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		var out []string
		for _, r := range rows(into) {
			if helpers.IsSoftDeleted(r.DeletedAt) {
				r.Name += " (archived)"
			}
			out = append(out, r.Name)
		}
		return strings.Join(out, ", ")
	}
	type userList struct {
		Data struct {
			Projects []row `json:"user_projects"`
		} `json:"data"`
	}
	ofUser := func(v any) []row { return v.(*userList).Data.Projects }
	// The board's "Add from OneCamp", search's filters and the editor's
	// project picker read /user/userProjectList.
	picker := func(who string) string {
		return names(do(w.account(who), userController.GetDgraphUserProjectList, nil, nil), ofUser, &userList{})
	}
	// My Tasks' project filters read the projects on /user/profile, the
	// person as the auth middleware reads them.
	profile := func(who string) string {
		t.Helper()
		u := w.account(who)
		gr, err := userDomain.GetActiveDgraphUserInfoByUUID(ctx, w.people[who].String())
		if err != nil || gr == nil {
			t.Fatalf("%s as the middleware reads them: %v", who, err)
		}
		u.UserDgraphInfo = *gr
		return names(do(u, userController.GetLoggedInUserProfile, nil, nil), ofUser, &userList{})
	}
	// The New task form and the board's tools read the projects they run.
	runs := func(who string) string {
		var got struct {
			Data []row `json:"data"`
		}
		return names(do(w.account(who), projectController.GetDgraphProjectListByAdminDgraphUID, nil, nil),
			func(any) []row { return got.Data }, &got)
	}
	team := func(who string) string {
		var got struct {
			Data struct {
				Projects []row `json:"team_projects"`
			} `json:"data"`
		}
		return names(do(w.account(who), teamController.GetDgraphTeamProjectListByTeamUUID, map[string]string{"team_uuid": w.team}, nil),
			func(any) []row { return got.Data.Projects }, &got)
	}
	expect := func(what, got, want string) {
		t.Helper()
		if got != want {
			t.Errorf("%s: %q, want %q", what, got, want)
		}
	}

	expect("ben's projects, both live", picker("ben"), "Website, Launch")

	do(ana, projectController.ArchiveProject, nil, map[string]string{"project_uuid": w.project})
	expect("ben's projects to pick from", picker("ben"), "Launch")
	expect("ana's projects to pick from", picker("ana"), "Launch")
	expect("ben's projects on his profile", profile("ben"), "Launch")
	expect("the projects ana runs", runs("ana"), "Launch")
	expect("the team's projects, to its admin", team("dee"), "Website (archived), Launch")

	do(ana, projectController.UnArchiveProject, nil, map[string]string{"project_uuid": w.project})
	expect("ben's projects once it's restored", picker("ben"), "Website, Launch")
	expect("ben's profile once it's restored", profile("ben"), "Website, Launch")
	expect("the projects ana runs once it's restored", runs("ana"), "Website, Launch")
}
