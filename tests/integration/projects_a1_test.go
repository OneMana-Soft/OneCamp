//go:build integration
// +build integration

package integration_test

// Projects A1 (Work Access, #86) against real Postgres 12 and Dgraph, through
// the handlers people reach: a person's role in a project and the project's
// "Who can change tasks" decide what they do there; an admin outside the
// project's member list is in the project (bug a); a project's own admins
// archive and restore it (bug b); two teams may give a project the same name
// (bug c, migration 320); a workspace admin reaches a team's stewardship in
// one recorded step (D5); and the admins' one-time notice (D1).
//
// Run: go test -tags=integration ./tests/integration/ -run TestA1 -v

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	taskWriter "github.com/akashc777/OneCamp/business/TaskWriter"
	teamBusiness "github.com/akashc777/OneCamp/business/Team"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	entityLinkController "github.com/akashc777/OneCamp/controllers/EntityLink"
	noticeController "github.com/akashc777/OneCamp/controllers/Notice"
	projectController "github.com/akashc777/OneCamp/controllers/Project"
	taskController "github.com/akashc777/OneCamp/controllers/Task"
	teamController "github.com/akashc777/OneCamp/controllers/Team"
	userController "github.com/akashc777/OneCamp/controllers/User"
	v1Controller "github.com/akashc777/OneCamp/controllers/V1"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	roleModel "github.com/akashc777/OneCamp/models/postgres/ProjectRole"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestA1WorkAccessThroughTheHandlers(t *testing.T) {
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

	// call runs a handler as who, with the route's parameters and a JSON body.
	call := func(who string, h http.HandlerFunc, params map[string]string, body any) (int, string) {
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
		c := context.WithValue(context.WithValue(ctx, helpers.UserInfoContextKey, w.account(who)), chi.RouteCtxKey, rc)
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPost, "/", rd).WithContext(c))
		return rec.Code, compact(rec.Body.Bytes())
	}
	expect := func(what string, code int, body string, want int, words string) {
		t.Helper()
		if code != want || (words != "" && !strings.Contains(body, words)) {
			t.Errorf("%s: %d %s, want %d %q", what, code, body, want, words)
		}
	}
	level := func(who string, kind, id string) accessBusiness.Level {
		t.Helper()
		d, err := accessBusiness.Rules.Resolve(ctx, w.subject(who), accessBusiness.Resource{Kind: kind, UUID: id})
		if err != nil {
			t.Fatalf("%s on %s %s: %v", who, kind, id, err)
		}
		return d.Level
	}
	minutes := 10
	change := func(who, task string) error {
		t.Helper()
		minutes++
		u := w.account(who)
		_, _, err := taskWriter.Tasks.Apply(ctx, &u, uuid.MustParse(task), []taskWriter.Op{{Kind: taskWriter.OpEstimate, Value: json.RawMessage(fmt.Sprint(minutes))}}, 0)
		return err
	}
	role := func(by, who, to string) (int, string) {
		t.Helper()
		return call(by, projectController.SetMemberRole, map[string]string{"project_uuid": w.project, "user_uuid": w.people[who].String()}, map[string]string{"role": to})
	}
	// edges is who's place in the project as the graph has it.
	edges := func(who string) (member, admin bool) {
		t.Helper()
		p, err := projectBusiness.GetBasicDgraphProjectInfo(ctx, w.project, w.account(who).UserDgraphInfo.Uid)
		if err != nil {
			t.Fatal(err)
		}
		return p.IsProjectMember > 0, p.IsProjectAdmin > 0
	}
	row := func(who string) string {
		t.Helper()
		rows, err := roleModel.Roles(ctx, uuid.MustParse(w.project))
		if err != nil {
			t.Fatal(err)
		}
		return rows[w.people[who]]
	}
	comment := func(who, task string) (int, string) {
		t.Helper()
		return call(who, taskController.CreateTaskComment, nil, map[string]string{"task_uuid": task, "task_comment_body": "<p>Looks good</p>"})
	}
	policy := func(by string, value any) (int, string) {
		t.Helper()
		return call(by, projectController.SetProjectPolicy, map[string]string{"project_uuid": w.project, "key": policyBusiness.ProjectsEditWho.Key}, map[string]any{"value": value})
	}
	// opsSite is the Ops team's project named as the Web team's (bug c).
	var opsSite string
	archive := func(who string, h http.HandlerFunc) (int, string) {
		t.Helper()
		return call(who, h, nil, map[string]string{"project_uuid": w.project})
	}

	t.Run("roles decide what someone does in the project", func(t *testing.T) {
		// Members change tasks by default.
		if got := level("ben", workAccess.KindProject, w.project); got != accessBusiness.LevelEdit {
			t.Errorf("a member: %s, want edit", got)
		}
		if err := change("ben", w.task); err != nil {
			t.Errorf("a member's change: %v", err)
		}

		// A viewer reads, and neither changes nor comments; the graph keeps them in.
		code, body := role("ana", "ben", "viewer")
		expect("making a member a viewer", code, body, http.StatusOK, "")
		if got := row("ben"); got != roleModel.Viewer {
			t.Errorf("the viewer's row: %q", got)
		}
		if member, admin := edges("ben"); !member || admin {
			t.Errorf("a viewer's edges: member %v, admin %v", member, admin)
		}
		if got := level("ben", workAccess.KindProject, w.project); got != accessBusiness.LevelView {
			t.Errorf("a viewer: %s, want view", got)
		}
		if err := change("ben", w.task); !errors.Is(err, workAccess.ErrNotAllowed) {
			t.Errorf("a viewer's change: %v", err)
		}
		code, body = comment("ben", w.task)
		expect("a viewer commenting", code, body, http.StatusForbidden, "You can see this project but not comment in it.")
		code, body = call("ben", taskController.CreateTask, nil, map[string]any{"task_project_uuid": w.project, "task_name": "A viewer's task"})
		expect("a viewer making a task", code, body, http.StatusForbidden, "You can see this project but not add tasks to it.")
		code, body = call("ben", taskController.CreateSubTask, nil, map[string]any{"task_uuid": w.task, "task_project_uuid": w.project, "task_name": "A viewer's sub-task"})
		expect("a viewer making a sub-task", code, body, http.StatusForbidden, "You can see this project but not add tasks to it.")
		code, body = call("ben", projectController.ProjectInfo, map[string]string{"project_uuid": w.project}, nil)
		expect("a viewer opening the project", code, body, http.StatusOK, "")
		code, body = call("ana", projectController.GetDgraphProjectCombinedMemberInfo, map[string]string{"project_uuid": w.project}, nil)
		expect("the members, with their roles", code, body, http.StatusOK, `"user_project_role":"viewer"`)

		// A commenter comments, and still changes nothing.
		code, body = role("ana", "ben", "commenter")
		expect("making a viewer a commenter", code, body, http.StatusOK, "")
		if got := level("ben", workAccess.KindProject, w.project); got != accessBusiness.LevelComment {
			t.Errorf("a commenter: %s, want comment", got)
		}
		code, body = comment("ben", w.task)
		expect("a commenter commenting", code, body, http.StatusOK, "")
		if err := change("ben", w.task); !errors.Is(err, workAccess.ErrNotAllowed) {
			t.Errorf("a commenter's change: %v", err)
		}

		// An admin is one in both stores, and stops being one in both.
		code, body = role("ana", "ben", "admin")
		expect("making a commenter an admin", code, body, http.StatusOK, "")
		if member, admin := edges("ben"); !member || !admin || row("ben") != roleModel.Admin {
			t.Errorf("an admin: member %v, admin %v, row %q", member, admin, row("ben"))
		}
		if got := level("ben", workAccess.KindProject, w.project); got != accessBusiness.LevelFull {
			t.Errorf("an admin: %s, want full", got)
		}
		code, body = role("ana", "ben", "member")
		expect("making an admin a member", code, body, http.StatusOK, "")
		if member, admin := edges("ben"); !member || admin || row("ben") != roleModel.Member {
			t.Errorf("a member again: member %v, admin %v, row %q", member, admin, row("ben"))
		}

		// Only the project's admins and its team's admins change roles.
		code, body = role("ben", "ana", "viewer")
		expect("a member changing an admin's role", code, body, http.StatusForbidden, "")
		code, body = role("ana", "ana", "viewer")
		expect("the creator stepping down", code, body, http.StatusForbidden, "")
		code, body = role("dee", "ben", "commenter")
		expect("the team's admin changing a role", code, body, http.StatusOK, "")
		code, body = role("dee", "ben", "member")
		expect("the team's admin changing it back", code, body, http.StatusOK, "")
		code, body = role("cy", "ben", "viewer")
		expect("someone outside the project", code, body, http.StatusNotFound, "")
	})

	t.Run("who can change tasks is the project's to choose", func(t *testing.T) {
		code, body := policy("ben", "admins")
		expect("a member choosing", code, body, http.StatusForbidden, "Only the project's admins choose who can change its tasks.")

		code, body = policy("ana", "admins")
		expect("admins only", code, body, http.StatusOK, "")
		if err := change("ben", w.task); !errors.Is(err, workAccess.ErrNotAllowed) || !strings.Contains(err.Error(), "Only this project's admins change its tasks.") {
			t.Errorf("a member's change, admins only: %v", err)
		}
		if err := change("ana", w.task); err != nil {
			t.Errorf("an admin's change, admins only: %v", err)
		}
		code, body = comment("ben", w.task)
		expect("a member commenting, admins only", code, body, http.StatusOK, "")
		code, body = call("ben", projectController.ProjectPolicies, map[string]string{"project_uuid": w.project}, nil)
		expect("the project's policies, read by a member", code, body, http.StatusOK, `"value":"admins"`)

		code, body = policy("ana", "assigned")
		expect("members on the tasks assigned to them", code, body, http.StatusOK, "")
		if err := change("ben", w.mine); err != nil {
			t.Errorf("a member changing a task assigned to them: %v", err)
		}
		if err := change("ben", w.task); err == nil || !strings.Contains(err.Error(), "In this project you can change only the tasks assigned to you.") {
			t.Errorf("a member changing a task assigned to someone else: %v", err)
		}

		code, body = policy("ana", "nobody")
		expect("a choice that isn't one", code, body, http.StatusBadRequest, "")
		code, body = policy("ana", nil)
		expect("back to the workspace's", code, body, http.StatusOK, "")
		if err := change("ben", w.task); err != nil {
			t.Errorf("a member's change, the workspace's default again: %v", err)
		}
	})

	t.Run("bug a: an admin outside the member list is in the project", func(t *testing.T) {
		code, body := call("fay", projectController.ProjectInfo, map[string]string{"project_uuid": w.project}, nil)
		expect("opening the project", code, body, http.StatusOK, "")
		code, body = call("fay", taskController.GetTaskActivityList, map[string]string{"task_uuid": w.task}, nil)
		expect("reading a task's activity", code, body, http.StatusOK, "")
		code, body = comment("fay", w.task)
		expect("commenting", code, body, http.StatusOK, "")
		var made struct {
			Data struct {
				Uuid string `json:"comment_id"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(body), &made); err != nil || made.Data.Uuid == "" {
			t.Fatalf("the comment made: %v %s", err, body)
		}
		code, body = call("fay", taskController.CreateOrUpdateReactionOnCommentTask, nil, map[string]string{"comment_id": made.Data.Uuid, "reaction_emoji_id": "tada"})
		expect("reacting", code, body, http.StatusOK, "")
	})

	// The task's history pages back from its oldest line shown: the older
	// lines are asked for by that line, and the lookup counted only the
	// project's members, so an admin outside its member list got the first
	// page and then nothing.
	t.Run("bug a: an admin outside the member list pages back through a task's history", func(t *testing.T) {
		task, err := taskBusiness.GetDgraphBasicTaskInfo(ctx, w.task, w.account("ana").UserDgraphInfo.Uid)
		if err != nil || task == nil || task.Uid == "" {
			t.Fatalf("the task: %+v %v", task, err)
		}
		older, newer := uuid.NewString(), uuid.NewString()
		w.dg.Mutate(t, map[string]any{"uid": task.Uid, "task_activities": []map[string]any{
			{"dgraph.type": "Activity", "activity_uuid": older, "activity_type": dgraphStruct.ACTIVITY_TYPE_STATUS, "activity_time": "2026-10-01T09:00:00Z", "activity_next_state": "todo"},
			{"dgraph.type": "Activity", "activity_uuid": newer, "activity_type": dgraphStruct.ACTIVITY_TYPE_STATUS, "activity_time": "2026-10-02T09:00:00Z", "activity_next_state": "inProgress"},
		}})
		history := func(who string) (int, string) {
			t.Helper()
			rc := chi.NewRouteContext()
			rc.URLParams.Add("task_uuid", w.task)
			c := context.WithValue(context.WithValue(ctx, helpers.UserInfoContextKey, w.account(who)), chi.RouteCtxKey, rc)
			rec := httptest.NewRecorder()
			taskController.GetTaskActivityList(rec, httptest.NewRequest(http.MethodGet, "/?first=1&before="+newer, nil).WithContext(c))
			return rec.Code, compact(rec.Body.Bytes())
		}
		code, body := history("fay")
		expect("the older history, for an admin outside the member list", code, body, http.StatusOK, `"activity_uuid":"`+older+`"`)
		code, body = history("cy")
		expect("the older history, for someone outside the project", code, body, http.StatusNotFound, "")
	})

	t.Run("a task's links are the task's: changed by whoever may change it", func(t *testing.T) {
		board := uuid.NewString()
		w.dg.Mutate(t, []map[string]any{{"uid": "_:board", "dgraph.type": "Board", "board_uuid": board, "board_title": "Sketches",
			"board_private": false, "board_deleted_at": "0001-01-01T00:00:00Z"}})
		link := map[string]string{"source_type": "task", "source_uuid": w.task, "ref_type": "board", "ref_uuid": board}
		code, body := role("ana", "ben", "viewer")
		expect("making a member a viewer", code, body, http.StatusOK, "")
		code, body = call("ben", entityLinkController.AddLink, nil, link)
		expect("a viewer linking a board to a task", code, body, http.StatusForbidden, "")
		code, body = role("ana", "ben", "member")
		expect("and a member again", code, body, http.StatusOK, "")
		code, body = call("fay", entityLinkController.AddLink, nil, link)
		expect("an admin outside the member list linking it", code, body, http.StatusOK, "")
		code, body = call("fay", entityLinkController.GetSourceLinks, map[string]string{"source_type": "task", "source_uuid": w.task}, nil)
		expect("and reading the task's links", code, body, http.StatusOK, board)
	})

	t.Run("bug b: the project's admins archive and restore it", func(t *testing.T) {
		code, body := archive("cy", projectController.ArchiveProject)
		expect("someone in the team but not the project", code, body, http.StatusNotFound, "")
		code, body = archive("ben", projectController.ArchiveProject)
		expect("a member", code, body, http.StatusForbidden, "Only the project's admins and its team's admins can archive it.")
		for _, who := range []string{"ana", "dee"} {
			code, body = archive(who, projectController.ArchiveProject)
			expect(who+" archiving", code, body, http.StatusOK, "")
			code, body = archive(who, projectController.UnArchiveProject)
			expect(who+" restoring", code, body, http.StatusOK, "")
		}
	})

	t.Run("bug c: two teams may give a project the same name", func(t *testing.T) {
		dee := w.account("dee")
		ops := uuid.New()
		w.exec(`INSERT INTO teams (id, team_name, created_by) VALUES ($1, 'Ops', $2)`, ops, w.people["dee"])
		uids := w.dg.Mutate(t, []map[string]any{{"uid": "_:ops", "dgraph.type": "Team", "team_uuid": ops.String(), "team_name": "Ops",
			"team_members": []map[string]any{{"uid": dee.UserDgraphInfo.Uid}}, "team_admins": []map[string]any{{"uid": dee.UserDgraphInfo.Uid}}}})
		team, err := teamBusiness.GetBasicDgraphTeamInfoByUUID(ctx, ops.String(), dee.UserDgraphInfo.Uid)
		if err != nil || team == nil || team.Uid != uids["ops"] {
			t.Fatalf("the second team: %+v %v", team, err)
		}
		made, err := projectBusiness.CreateProject(ctx, "Website", dee.UserDgraphInfo.Uid, dee.UserPostgresInfo.Id, team, ops, &dee.UserDgraphInfo)
		if err != nil || made == nil {
			t.Fatalf("a project named as another team's: %v", err)
		}
		opsSite = made.Uuid
		code, body := call("dee", projectController.CreateProject, nil, map[string]string{"project_team_uuid": w.team, "project_name": "Website"})
		expect("a project named as one in the same team", code, body, http.StatusConflict, `This team already has a project named \"Website\".`)
	})

	t.Run("D5: a workspace admin takes a team's stewardship in one recorded step", func(t *testing.T) {
		code, body := role("wes", "ben", "viewer")
		expect("a workspace admin outside the team", code, body, http.StatusNotFound, "")
		code, body = call("ben", teamController.MakeMeTeamAdmin, map[string]string{"team_uuid": w.team}, nil)
		expect("someone who isn't a workspace admin", code, body, http.StatusForbidden, "")

		code, body = call("wes", teamController.MakeMeTeamAdmin, map[string]string{"team_uuid": w.team}, nil)
		expect("a workspace admin", code, body, http.StatusOK, `"changed":true`)
		recorded := func() int {
			t.Helper()
			var n int
			if err := w.env.PG.QueryRow(`SELECT COUNT(*) FROM admin_audit_log WHERE action = 'team.admin.self' AND metadata->>'team_uuid' = $1 AND actor_id = $2`,
				w.team, w.people["wes"]).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}
		if n := recorded(); n != 1 {
			t.Errorf("the step is recorded %d times, want once", n)
		}
		code, body = role("wes", "ben", "viewer")
		expect("the new steward changing a role", code, body, http.StatusOK, "")
		code, body = role("wes", "ben", "member")
		expect("and changing it back", code, body, http.StatusOK, "")

		code, body = call("wes", teamController.MakeMeTeamAdmin, map[string]string{"team_uuid": w.team}, nil)
		expect("again", code, body, http.StatusOK, `"changed":false`)
		if n := recorded(); n != 1 {
			t.Errorf("again, recorded %d times, want once", n)
		}
	})

	t.Run("D1: the admins are told once that members change tasks", func(t *testing.T) {
		notices := func(who string) string {
			t.Helper()
			code, body := call(who, noticeController.Notices, nil, nil)
			if code != http.StatusOK {
				t.Fatalf("%s's notices: %d %s", who, code, body)
			}
			return body
		}
		const key = `"key":"projects.members_edit"`
		// The project was made after members began to change tasks here.
		if got := notices("ana"); strings.Contains(got, key) {
			t.Errorf("a project made since: %s", got)
		}
		w.exec(`UPDATE system_configs SET value = to_char((NOW() + interval '1 hour') AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"') WHERE key = 'projects.members_edit.since'`)

		if got := notices("ana"); !strings.Contains(got, key) || !strings.Contains(got, `"project_uuid":"`+w.project+`"`) {
			t.Errorf("the project's admin: %s", got)
		}
		if got := notices("wes"); !strings.Contains(got, key) || strings.Contains(got, "project_uuid") {
			t.Errorf("a workspace admin who runs no project: %s", got)
		}
		if got := notices("dee"); !strings.Contains(got, `"project_uuid":"`+opsSite+`"`) {
			t.Errorf("the admin of the Ops team's project: %s", got)
		}
		for _, who := range []string{"ben", "cy"} {
			if got := notices(who); strings.Contains(got, key) {
				t.Errorf("%s isn't an admin of an older project or the workspace: %s", who, got)
			}
		}

		code, body := call("ana", noticeController.DismissNotice, map[string]string{"key": "projects.members_edit"}, nil)
		expect("dismissing it", code, body, http.StatusOK, "")
		if got := notices("ana"); strings.Contains(got, key) {
			t.Errorf("dismissed, still: %s", got)
		}
		code, body = call("ana", noticeController.DismissNotice, map[string]string{"key": "projects.members_edit"}, nil)
		expect("dismissing it again", code, body, http.StatusOK, "")
		code, body = call("ana", noticeController.DismissNotice, map[string]string{"key": "no.such.notice"}, nil)
		expect("dismissing a notice that isn't one", code, body, http.StatusNotFound, "")
		if got := notices("fay"); !strings.Contains(got, key) {
			t.Errorf("another admin, not dismissed: %s", got)
		}

		// A workspace that keeps tasks with its admins has nothing to tell.
		wes := w.account("wes")
		by := policyBusiness.Changer{UserID: wes.UserPostgresInfo.Id, Email: wes.UserPostgresInfo.EmailID}
		if _, err := policyBusiness.Admin.Set(ctx, policyBusiness.ProjectsEditWho.Key, json.RawMessage(`"admins"`), by); err != nil {
			t.Fatal(err)
		}
		if got := notices("fay"); strings.Contains(got, key) {
			t.Errorf("admins only across the workspace, still: %s", got)
		}
		if _, err := policyBusiness.Admin.Set(ctx, policyBusiness.ProjectsEditWho.Key, json.RawMessage(`"members"`), by); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("My Tasks says what they may do with each of their tasks", func(t *testing.T) {
		mine := func() string {
			t.Helper()
			rec := httptest.NewRecorder()
			userController.GetDgraphUserTaskList(rec, httptest.NewRequest(http.MethodGet, "/?pageSize=50&pageIndex=0", nil).
				WithContext(context.WithValue(ctx, helpers.UserInfoContextKey, w.account("ben"))))
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), w.mine) {
				t.Fatalf("ben's tasks: %d %s", rec.Code, rec.Body.String())
			}
			return compact(rec.Body.Bytes())
		}
		if got := mine(); !strings.Contains(got, `"task_access":{"edit":true,"delete":false`) {
			t.Errorf("a member's task, members changing tasks: %s", got)
		}
		code, body := policy("ana", "admins")
		expect("admins only", code, body, http.StatusOK, "")
		if got := mine(); !strings.Contains(got, `"task_access":{"edit":false,"delete":false`) {
			t.Errorf("a member's task, admins only: %s", got)
		}
		code, body = policy("ana", nil)
		expect("back to the workspace's", code, body, http.StatusOK, "")
	})

	// A token carries none of its owner's admin powers (e4e1684e): not the
	// workspace's, and not a project's either. In a project that keeps its
	// tasks with its admins, the admin's token is refused what only admins
	// may do; in the app, as themselves, they still may.
	t.Run("a project admin's API token acts as a member, never as the project's admin", func(t *testing.T) {
		byToken := func(who string) context.Context {
			return context.WithValue(context.WithValue(ctx, helpers.UserInfoContextKey, w.account(who)), helpers.ApiTokenIDContextKey, uuid.NewString())
		}
		status := func(c context.Context, to string) (int, string) {
			t.Helper()
			rc := chi.NewRouteContext()
			rc.URLParams.Add("id", w.task)
			rec := httptest.NewRecorder()
			v1Controller.UpdateTaskStatusV1(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"status":"`+to+`"}`)).
				WithContext(context.WithValue(c, chi.RouteCtxKey, rc)))
			return rec.Code, compact(rec.Body.Bytes())
		}
		code, body := policy("ana", "admins")
		expect("admins only", code, body, http.StatusOK, "")
		code, body = status(byToken("ana"), "inProgress")
		expect("the project admin's token changing a task, admins only", code, body, http.StatusForbidden, "Only this project's admins change its tasks.")
		if err := change("ana", w.task); err != nil {
			t.Errorf("the project admin in the app, admins only: %v", err)
		}
		code, body = policy("ana", nil)
		expect("back to the workspace's", code, body, http.StatusOK, "")
		code, body = status(byToken("ana"), "inProgress")
		expect("the project admin's token, members changing tasks", code, body, http.StatusOK, "")
	})

	t.Run("leaving the team takes their roles in its projects", func(t *testing.T) {
		if got := row("ben"); got != roleModel.Member {
			t.Fatalf("ben's row before: %q", got)
		}
		code, body := call("dee", teamController.RemoveMemberFromTeam, nil, map[string]string{"team_uuid": w.team, "member_uuid": w.people["ben"].String()})
		expect("the team's admin taking ben out of the team", code, body, http.StatusOK, "")
		if member, admin := edges("ben"); member || admin {
			t.Errorf("ben's edges after leaving the team: member %v, admin %v", member, admin)
		}
		if got := row("ben"); got != "" {
			t.Errorf("ben's row after leaving the team: %q", got)
		}
	})
}

// compact is a JSON answer without the spaces the server writes it with, so a
// test can look for a field and its value as one string.
func compact(body []byte) string {
	var b bytes.Buffer
	if err := json.Compact(&b, body); err != nil {
		return string(body)
	}
	return b.String()
}
