//go:build integration
// +build integration

package integration_test

// The task comments an import brought before imports wrote a person's
// comment's search entry have entries without their task's project, which
// nobody finds. The startup repair (business/Import commentSearchRepair.go)
// rewrites them as a person's comment's entry is written: the project's
// people find them, a restricted task's only those who may read the task,
// and a deleted one, one of a task in the trash, or one of an archived
// project, nobody. An entry edited since keeps the edit's text. Restoring the
// archived project brings its comment back to its people, as it brings back
// the project's other comments. The comments of imports that ran before their
// provider's fix (a Jira one, a Todoist one), of one that can't be dated, and
// those no import recorded, stay out of search: nobody finds them, the
// task's readers included. An Asana import's, whose comments no fix holds
// back, are found whenever it ran.
//
// seedWorkWorld's Website: ana runs it with fay, ben is a member, cy is in
// the team but not the project. "Send the newsletter" is open to the
// project; "Write the launch post" is kept to its admins and its maker;
// "Old idea" is in the trash; "Old pier" is an archived project of ana's
// and ben's.
//
// Run: go test -tags=integration ./tests/integration/ -run TestImportedTaskCommentsAreFoundAfterTheRepair -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	importBusiness "github.com/akashc777/OneCamp/business/Import"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	globalSearchController "github.com/akashc777/OneCamp/controllers/GlobalSearch"
	commentDomain "github.com/akashc777/OneCamp/domain/Comment"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	openSearchGlobalSearchModels "github.com/akashc777/OneCamp/models/openSearch/GlobalSearch"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

func TestImportedTaskCommentsAreFoundAfterTheRepair(t *testing.T) {
	w := seedWorkWorld(t)
	ctx := context.Background()
	search := integration.SetupOpenSearch(t)
	if err := opensearchInit.ConnectOpenSearch(&opensearchInit.OpenSearchConfig{Host: search.URL}); err != nil {
		t.Fatalf("create the indices: %v", err)
	}

	uidOf := func(pred, id string) string {
		t.Helper()
		var out struct {
			Q []struct {
				UID string `json:"uid"`
			} `json:"q"`
		}
		if err := json.Unmarshal(w.dg.Query(t, fmt.Sprintf(`{ q(func: eq(%s, %q)) { uid } }`, pred, id)), &out); err != nil || len(out.Q) != 1 {
			t.Fatalf("the node with %s %s: %+v %v", pred, id, out, err)
		}
		return out.Q[0].UID
	}
	website, team := uidOf("project_uuid", w.project), uidOf("team_uuid", w.team)
	person := func(name string) string { return uidOf("user_uuid", w.people[name].String()) }
	live := "0001-01-01T00:00:00Z"
	archivedAt := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	written := time.Date(2026, 3, 2, 9, 30, 0, 0, time.UTC)
	pier, pierTask := uuid.NewString(), uuid.NewString()
	w.exec(`INSERT INTO projects (id, project_name, team_id, created_by) VALUES ($1, 'Old pier', $2, $3)`, pier, w.team, w.people["ana"])
	w.exec(`INSERT INTO tasks (id, project_id, created_by) VALUES ($1, $2, $3)`, pierTask, pier, w.people["ana"])

	// Who is in which project, as signing in reads it; the archived project.
	nodes := []map[string]any{{"uid": "_:pier", "dgraph.type": "Project", "project_uuid": pier, "project_name": "Old pier",
		"project_deleted_at": archivedAt.Format(time.RFC3339), "project_members": []map[string]any{{"uid": person("ana")}, {"uid": person("ben")}},
		"project_admins": []map[string]any{{"uid": person("ana")}}, "project_team": map[string]any{"uid": team},
		"project_tasks": []map[string]any{{"uid": "_:pierTask", "dgraph.type": "Task", "task_uuid": pierTask, "task_name": "Paint the rails",
			"task_status": "todo", "task_deleted_at": live, "task_project": map[string]any{"uid": "_:pier"}, "task_created_by": map[string]any{"uid": person("ana")}}}}}
	for _, name := range []string{"ana", "ben"} {
		nodes = append(nodes, map[string]any{"uid": person(name), "user_projects": []map[string]any{{"uid": website}, {"uid": "_:pier"}}, "user_teams": []map[string]any{{"uid": team}}})
	}
	nodes = append(nodes, map[string]any{"uid": person("fay"), "user_projects": []map[string]any{{"uid": website}}, "user_teams": []map[string]any{{"uid": team}}},
		map[string]any{"uid": person("cy"), "user_teams": []map[string]any{{"uid": team}}})
	w.dg.Mutate(t, nodes)

	// The comments, each with a word of its own, as an import wrote them: in
	// the graph on their task, and in search with their HTML and no project.
	type comment struct{ id, task, word, html string }
	open := comment{uuid.NewString(), w.mine, "quokka", "<p>The <strong>quokka</strong> plan is ready</p>"}
	kept := comment{uuid.NewString(), w.task, "zephyr", "<p>Zephyr pay bands attached</p>"}
	withdrawn := comment{uuid.NewString(), w.mine, "walrus", "<p>Walrus draft, withdrawn</p>"}
	trashed := comment{uuid.NewString(), w.deleted, "kiwi", "<p>Kiwi idea notes</p>"}
	archived := comment{uuid.NewString(), pierTask, "ibex", "<p>Ibex rail colours</p>"}
	edited := comment{uuid.NewString(), w.mine, "narwhal", "<p>Narwhal draft words</p>"}
	all := []comment{open, kept, withdrawn, trashed, archived, edited}
	jiraBefore := comment{uuid.NewString(), w.mine, "heron", "<p>Heron budget lines</p>"}
	todoistBefore := comment{uuid.NewString(), w.mine, "lynx", "<p>Lynx guest list</p>"}
	undated := comment{uuid.NewString(), w.mine, "puffin", "<p>Puffin route notes</p>"}
	unrecorded := comment{uuid.NewString(), w.mine, "otter", "<p>Otter supplier terms</p>"}
	asana := comment{uuid.NewString(), w.mine, "gecko", "<p>Gecko print run</p>"}
	held := []comment{jiraBefore, todoistBefore, undated, unrecorded}
	every := append(append(append([]comment{}, all...), held...), asana)
	for _, c := range every {
		deleted := live
		if c == withdrawn {
			deleted = archivedAt.Format(time.RFC3339)
		}
		w.dg.Mutate(t, []map[string]any{{"uid": uidOf("task_uuid", c.task), "task_comments": []map[string]any{{"dgraph.type": "Comment",
			"comment_uuid": c.id, "comment_text": c.html, "comment_task": map[string]any{"uid": uidOf("task_uuid", c.task)},
			"comment_by": map[string]any{"uid": person("ana")}, "comment_created_at": written.Format(time.RFC3339), "comment_deleted_at": deleted}}}})
		w.exec(`INSERT INTO comments (id, created_by) VALUES ($1, $2)`, c.id, w.people["ana"])
		commentDomain.CreateTaskCommentWithAttachmentsInOpenSearch(&openSearchStruct.OpenSearchComment{Uuid: c.id, CommentBody: c.html,
			CommentTaskUuid: c.task, CommentByUserUuid: w.people["ana"].String(), CommentByUserFullName: "ana", CommentCreatedAt: written.Unix()}, nil)
	}
	// The imports that brought them in, against the Jira and the membership
	// fixes, which this install first ran at fixedAt: every comment in all
	// came with a Jira import after its fix.
	fixedAt := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	before, after := fixedAt.Add(-24*time.Hour), fixedAt.Add(24*time.Hour)
	for _, key := range []string{importBusiness.JiraRestrictionsFixed, importBusiness.MembershipFixed} {
		w.exec(`INSERT INTO system_configs (key, value, updated_at) VALUES ($1, 'claimed', $2)
			ON CONFLICT (key) DO UPDATE SET updated_at = EXCLUDED.updated_at`, key, fixedAt)
	}
	importOf := func(provider string, at *time.Time, cs ...comment) {
		job := uuid.NewString()
		w.exec(`INSERT INTO import_jobs (id, provider, source_workspace_name, source, status, started_at, created_at)
			VALUES ($1, $2, 'Harbor', 'api', 'completed', $3, $3)`, job, provider, at)
		for _, c := range cs {
			w.exec(`INSERT INTO import_id_map (import_id, entity_type, source_id, onecamp_uuid) VALUES ($1, 'comment', $2, $3)`, job, "c-"+c.word, c.id)
		}
	}
	importOf("jira", &after, all...)
	importOf("jira", &before, jiraBefore)
	importOf("todoist", &before, todoistBefore)
	importOf("jira", nil, undated)
	importOf("asana", &before, asana)

	// Since then: the walrus comment was deleted, the kiwi comment's task went
	// to the trash, and the narwhal comment was edited.
	gone := archivedAt.Unix()
	commentDomain.DeleteCommentWithAttachmentsInOpenSearch(&openSearchStruct.OpenSearchComment{Uuid: withdrawn.id, CommentDeletedAt: &gone}, nil)
	if err := openSearchGlobalSearchModels.SyncCascadingDeletionInOpenSearch(ctx, []string{"comment_task_id"}, w.deleted, gone, []string{openSearchStruct.COMMENT_INDEX}, "cascade"); err != nil {
		t.Fatal(err)
	}
	commentDomain.UpdateCommentInOpenSearch(openSearchStruct.OpenSearchComment{Uuid: edited.id, CommentBody: "Narwhal final words", CommentUpdatedAt: time.Now().Unix()})

	// Ana keeps "Write the launch post" to its admins and its maker.
	task, err := taskBusiness.GetDgraphBasicTaskInfo(ctx, w.task, person("ana"))
	if err != nil || task == nil {
		t.Fatalf("read the task: %v", err)
	}
	if err := workAccess.Restrict(ctx, task, w.people["ana"], nil, time.Now()); err != nil {
		t.Fatalf("restrict: %v", err)
	}

	as := func(who string) context.Context {
		t.Helper()
		pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, w.people[who])
		if err != nil || pg == nil {
			t.Fatalf("load %s: %v", who, err)
		}
		gr, err := userDomain.GetActiveDgraphUserInfoByUUID(ctx, w.people[who].String())
		if err != nil || gr == nil {
			t.Fatalf("load %s's profile: %v", who, err)
		}
		return taskread.Install(context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr}))
	}
	searchFor := func(who, text string) string {
		t.Helper()
		raw, _ := json.Marshal(map[string]string{"global_search_text": text})
		rec := httptest.NewRecorder()
		globalSearchController.GetUnifiedGlobalSearch(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(as(who)))
		return rec.Body.String()
	}
	finds := func(who string, c comment) bool { return strings.Contains(searchFor(who, c.word), c.id) }
	// Everything written above is searchable from here on.
	if _, err := opensearchInit.OpenSearchClient.Indices.Refresh(ctx, &opensearchapi.IndicesRefreshReq{Indices: []string{openSearchStruct.COMMENT_INDEX}}); err != nil {
		t.Fatal(err)
	}

	// Before: nobody finds any of them, the people of their projects included.
	for _, c := range every {
		for _, who := range []string{"ana", "ben", "fay"} {
			if finds(who, c) {
				t.Fatalf("before the repair, %s finds the %s comment: the old entries are already found, so this test proves nothing", who, c.word)
			}
		}
	}

	rep, ran, err := importBusiness.CommentSearchRepair.Run(ctx)
	if err != nil || !ran {
		t.Fatalf("the repair: ran %v, %v (%s)", ran, err, rep)
	}
	want := importBusiness.TaskCommentSearchRepair{Entries: len(every), Rewritten: 6, Archived: 1, Deleted: 1, HeldBack: len(held)}
	if rep != want {
		t.Errorf("the repair did %s; want 11 entries, 6 rewritten (1 archived), 1 deleted, 4 held back, none orphaned", rep)
	}

	t.Run("the project's people find an open task's comment, in plain text", func(t *testing.T) {
		for _, who := range []string{"ana", "ben", "fay"} {
			if !finds(who, open) {
				t.Errorf("%s doesn't find the quokka comment", who)
			}
		}
		if out := searchFor("ana", open.word); strings.Contains(out, "<strong>") {
			t.Errorf("the quokka comment's hit still holds its HTML: %s", out)
		}
		if finds("cy", open) {
			t.Error("cy, outside the project, finds the quokka comment")
		}
	})
	t.Run("comments of imports before their fix, undated or unrecorded, are found by nobody", func(t *testing.T) {
		for _, c := range held {
			for _, who := range []string{"ana", "ben", "fay"} {
				if finds(who, c) {
					t.Errorf("%s, who may read its task, finds the %s comment", who, c.word)
				}
			}
		}
	})
	t.Run("an import's comment of a provider no fix holds back is found by the project's people", func(t *testing.T) {
		for _, who := range []string{"ana", "ben", "fay"} {
			if !finds(who, asana) {
				t.Errorf("%s doesn't find the gecko comment", who)
			}
		}
	})
	t.Run("a restricted task's comment is found by its readers only", func(t *testing.T) {
		for _, who := range []string{"ana", "fay"} {
			if !finds(who, kept) {
				t.Errorf("%s, who may read the task, doesn't find the zephyr comment", who)
			}
		}
		for _, who := range []string{"ben", "cy"} {
			if finds(who, kept) {
				t.Errorf("%s, who may not read the task, finds the zephyr comment", who)
			}
		}
	})
	t.Run("a deleted comment, one of a task in the trash and one of an archived project are found by nobody", func(t *testing.T) {
		for _, c := range []comment{withdrawn, trashed, archived} {
			for _, who := range []string{"ana", "ben", "fay"} {
				if finds(who, c) {
					t.Errorf("%s finds the %s comment", who, c.word)
				}
			}
		}
	})
	t.Run("an edited comment keeps the edit's text", func(t *testing.T) {
		if !strings.Contains(searchFor("ana", "final"), edited.id) {
			t.Error("ana doesn't find the narwhal comment by its edited text")
		}
		if strings.Contains(searchFor("ana", "draft"), edited.id) {
			t.Error("ana finds the narwhal comment by the text it had before the edit")
		}
	})
	t.Run("it runs once, and again changes nothing", func(t *testing.T) {
		if _, ran, err := importBusiness.CommentSearchRepair.Run(ctx); err != nil || ran {
			t.Errorf("the second start's repair: ran %v, %v; want it recorded done", ran, err)
		}
		again, err := importBusiness.RepairTaskCommentSearch(ctx)
		if err != nil || again != (importBusiness.TaskCommentSearchRepair{Entries: 1 + len(held), Deleted: 1, HeldBack: len(held)}) {
			t.Errorf("repairing again: %s, %v; want only the deleted comment's and the held-back ones' entries left, untouched", again, err)
		}
	})
	t.Run("restoring the archived project brings its comment back to its people", func(t *testing.T) {
		// As restoring a project does: live in the graph, then what its
		// archive's cascade marked in search cleared.
		w.dg.Mutate(t, []map[string]any{{"uid": uidOf("project_uuid", pier), "project_deleted_at": live}})
		if err := openSearchGlobalSearchModels.SyncCascadingUnarchiveInOpenSearch(ctx, []string{"comment_project_id"}, pier, []string{openSearchStruct.COMMENT_INDEX}, "cascade"); err != nil {
			t.Fatal(err)
		}
		if _, err := opensearchInit.OpenSearchClient.Indices.Refresh(ctx, &opensearchapi.IndicesRefreshReq{Indices: []string{openSearchStruct.COMMENT_INDEX}}); err != nil {
			t.Fatal(err)
		}
		for _, who := range []string{"ana", "ben"} {
			if !finds(who, archived) {
				t.Errorf("after the old pier project is restored, %s doesn't find the ibex comment", who)
			}
		}
		if finds("fay", archived) {
			t.Error("fay, outside the old pier project, finds the ibex comment")
		}
	})
}
