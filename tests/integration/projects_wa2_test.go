//go:build integration
// +build integration

package integration_test

// Restricted tasks (WA2, design/projects-vs-asana-jira/WA2_PLAN.md) against
// real Postgres 12, Dgraph, OpenSearch and Redis, through the handlers people,
// their tools and their integrations reach.
//
// "Zephyr pay review" is in the Website project. Gia made it, Hal is its
// assignee, and Gia keeps it to Ivy as well. Ana and Fay run the project and
// see it. Ben is a member, who saw it until it was restricted; Cy is in the
// team but not the project; Dee runs the team. Below it are "Draft the Zephyr
// letters" and "Book the room"; "Zephyr memo for the board" is below a task
// everyone sees, and is restricted on its own. Ben had commented on it, was
// mentioned on it, had saved it for later, and has a webhook.
//
// Each surface is read by Ben before the restriction, so a surface that would
// show the task to him is known to; after it, every surface is walked as Ben,
// Cy and Dee, and none shows the task's words or names it except as "a task
// you can't see"; Ana, Fay, Gia, Hal and Ivy go on seeing it.
//
// Run: go test -tags=integration ./tests/integration/ -run TestWA2 -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	aiBusiness "github.com/akashc777/OneCamp/business/AI"
	activityBusiness "github.com/akashc777/OneCamp/business/Activity"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	activityController "github.com/akashc777/OneCamp/controllers/Activity"
	cycleController "github.com/akashc777/OneCamp/controllers/Cycle"
	globalSearchController "github.com/akashc777/OneCamp/controllers/GlobalSearch"
	projectController "github.com/akashc777/OneCamp/controllers/Project"
	savedItemController "github.com/akashc777/OneCamp/controllers/SavedItem"
	queryController "github.com/akashc777/OneCamp/controllers/SearchQuery"
	taskController "github.com/akashc777/OneCamp/controllers/Task"
	taskRestrictionController "github.com/akashc777/OneCamp/controllers/TaskRestriction"
	timeEntryController "github.com/akashc777/OneCamp/controllers/TimeEntry"
	userController "github.com/akashc777/OneCamp/controllers/User"
	v1Controller "github.com/akashc777/OneCamp/controllers/V1"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/taskread"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestWA2ARestrictedTaskIsSeenOnlyByItsPeople(t *testing.T) {
	t.Setenv("RESEND_API_KEY", "re_integration_not_a_key") // notification email on; nothing is sent from a test
	t.Setenv("EMAIL_ESSENTIAL_ONLY", "")
	w := seedWorkWorld(t)
	ctx := context.Background()
	integration.SetupRedis(t)
	search := integration.SetupOpenSearch(t)
	if err := opensearchInit.ConnectOpenSearch(&opensearchInit.OpenSearchConfig{Host: search.URL}); err != nil {
		t.Fatalf("create the indices: %v", err)
	}
	broker := &noticeBroker{}
	integration.UseMqttClient(t, broker)
	t.Cleanup(firebaseInit.ObservePushesForTest(func(map[string]string, []string) {}))
	type delivery struct {
		webhook uuid.UUID
		kind    string
		payload string
	}
	var hooksMu sync.Mutex
	var delivered []delivery
	t.Cleanup(webhookBusiness.ObserveDeliveriesForTest(func(id uuid.UUID, kind string, payload []byte) {
		hooksMu.Lock()
		delivered = append(delivered, delivery{id, kind, string(payload)})
		hooksMu.Unlock()
	}))

	// The graph's uid of a node, by its uuid predicate.
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
	live := "0001-01-01T00:00:00Z"
	website, webTeam, launch := uidOf("project_uuid", w.project), uidOf("team_uuid", w.team), uidOf("task_uuid", w.task)
	uids := map[string]string{}
	for _, name := range []string{"ana", "ben", "cy", "dee", "eve", "fay", "wes"} {
		uids[name] = uidOf("user_uuid", w.people[name].String())
	}
	var nodes []map[string]any
	for _, name := range []string{"gia", "hal", "ivy"} {
		id := uuid.New()
		w.people[name] = id
		w.exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@acme.test', $2, $2, NOW(), NOW())`, id, name)
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name, "user_deleted_at": live})
		uids[name] = "_:" + name
	}
	for name, id := range w.people {
		w.exec(`INSERT INTO users_notification_preferences (user_id, unsubscribe_token) VALUES ($1, 'unsub-' || $2) ON CONFLICT DO NOTHING`, id, name)
	}
	ref := func(name string) map[string]any { return map[string]any{"uid": uids[name]} }
	refs := func(names ...string) []map[string]any {
		out := make([]map[string]any, len(names))
		for i, n := range names {
			out[i] = ref(n)
		}
		return out
	}
	// Who is in the project and the team, as signing in reads it.
	for _, name := range []string{"ana", "ben", "fay", "gia", "hal", "ivy"} {
		nodes = append(nodes, map[string]any{"uid": uids[name], "user_projects": []map[string]any{{"uid": website}}, "user_teams": []map[string]any{{"uid": webTeam}}})
	}
	for _, name := range []string{"cy", "dee"} {
		nodes = append(nodes, map[string]any{"uid": uids[name], "user_teams": []map[string]any{{"uid": webTeam}}})
	}
	nodes = append(nodes,
		map[string]any{"uid": website, "project_members": refs("gia", "hal", "ivy")},
		map[string]any{"uid": webTeam, "team_members": refs("gia", "hal", "ivy")})

	// The restricted work: the task, the tasks below it, a task restricted on
	// its own below one everyone sees, which waits on it too.
	zephyr, letters, room, memo := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	for task, by := range map[string]string{zephyr: "gia", letters: "gia", room: "gia", memo: "ana"} {
		w.exec(`INSERT INTO tasks (id, project_id, created_by) VALUES ($1, $2, $3)`, task, w.project, w.people[by])
	}
	now := time.Now().UTC()
	at := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }
	task := func(key, id, name, by string, extra map[string]any) map[string]any {
		n := map[string]any{"uid": "_:" + key, "dgraph.type": "Task", "task_uuid": id, "task_name": name, "task_status": "todo",
			"task_priority": "medium", "task_deleted_at": live, "task_created_at": at(-48 * time.Hour),
			"task_project": map[string]any{"uid": website}, "task_created_by": ref(by)}
		for k, v := range extra {
			n[k] = v
		}
		return n
	}
	nodes = append(nodes,
		task("zephyr", zephyr, "Zephyr pay review", "gia", map[string]any{
			"task_description": "Zephyr raises for the team leads", "task_assignee": ref("hal"),
			"task_start_date": at(-24 * time.Hour), "task_due_date": at(72 * time.Hour),
			"task_sub_tasks": []map[string]any{{"uid": "_:letters"}, {"uid": "_:room"}}}),
		task("letters", letters, "Draft the Zephyr letters", "gia", map[string]any{"task_parent_task": map[string]any{"uid": "_:zephyr"}}),
		task("room", room, "Book the room", "gia", map[string]any{"task_parent_task": map[string]any{"uid": "_:zephyr"}}),
		task("memo", memo, "Zephyr memo for the board", "ana", map[string]any{"task_parent_task": map[string]any{"uid": launch}}),
		map[string]any{"uid": launch, "task_sub_tasks": []map[string]any{{"uid": "_:memo"}}, "task_blocked_by": []map[string]any{{"uid": "_:zephyr"}}},
		map[string]any{"uid": website, "project_tasks": []map[string]any{{"uid": "_:zephyr"}, {"uid": "_:letters"}, {"uid": "_:room"}, {"uid": "_:memo"}}},
		map[string]any{"uid": uids["hal"], "user_tasks": []map[string]any{{"uid": "_:zephyr"}}},
	)
	created := w.dg.Mutate(t, nodes)
	for _, name := range []string{"gia", "hal", "ivy"} {
		uids[name] = created[name]
	}

	// Indexed as making them indexes them.
	desc := "Zephyr raises for the team leads"
	taskDomain.CreateTaskWithAttachmentsInOpensearch(&openSearchStruct.OpenSearchTask{Uuid: zephyr, TaskName: "Zephyr pay review",
		TaskDescription: &desc, TaskProjectUuid: w.project, TaskProjectName: "Website", TaskStatus: "todo",
		TaskCreatedAt: now.Unix(), TaskCreatedByUserUuid: w.people["gia"].String()},
		[]*dgraphStruct.DgraphAttachment{{Uuid: uuid.NewString(), FileName: "zephyr-raises.xlsx", ObjectKey: "files/zephyr-raises.xlsx"}},
		&dgraphStruct.DgraphUser{Uuid: w.people["gia"].String()})
	for id, name := range map[string]string{letters: "Draft the Zephyr letters", memo: "Zephyr memo for the board", room: "Book the room"} {
		taskDomain.CreateTaskWithAttachmentsInOpensearch(&openSearchStruct.OpenSearchTask{Uuid: id, TaskName: name,
			TaskProjectUuid: w.project, TaskProjectName: "Website", TaskStatus: "todo", TaskCreatedAt: now.Unix(),
			TaskCreatedByUserUuid: w.people["gia"].String()}, nil, &dgraphStruct.DgraphUser{Uuid: w.people["gia"].String()})
	}

	// Rows beside the tasks: Hal's time on it, with a note; the cycle it is
	// in; Ben's "for later".
	w.exec(`INSERT INTO task_time_entries (task_uuid, project_uuid, user_id, started_at, ended_at, note) VALUES ($1, $2, $3, NOW() - interval '2 hours', NOW() - interval '1 hour', 'Zephyr spreadsheet checks')`,
		zephyr, w.project, w.people["hal"])
	cycle := uuid.New()
	w.exec(`INSERT INTO project_cycles (id, project_uuid, number, name, starts_at, ends_at) VALUES ($1, $2, 1, 'Week one', NOW() - interval '1 day', NOW() + interval '6 days')`, cycle, w.project)
	w.exec(`INSERT INTO task_cycles (task_uuid, cycle_id) VALUES ($1, $3), ($2, $3)`, zephyr, w.task, cycle)
	w.exec(`INSERT INTO saved_items (id, user_id, item_type, item_id, link, title, context) VALUES ($1, $2, 'task', $3, $4, 'Zephyr pay review', 'Website')`,
		uuid.New(), w.people["ben"], zephyr, "/app/task/"+zephyr)

	// Ana's and Ben's webhooks, for every task change and comment.
	hooks := map[string]uuid.UUID{}
	for _, name := range []string{"ana", "ben"} {
		hooks[name] = uuid.New()
		w.exec(`INSERT INTO webhooks (id, name, type, token_hash, target_url, events, created_by, scope_type) VALUES ($1, $2, 'outgoing', $3, $4, '["task.status_changed","task.comment.created"]', $5, 'org')`,
			hooks[name], name+"-hook", helpers.LinkSecretHash("tok-"+name), "https://hooks.example.com/"+name, w.people[name])
	}
	// A rule that posts (it is never told of a restricted task) and a doc's
	// to-dos (told, as they act inside it).
	var heardMu sync.Mutex
	heard := map[string][]string{}
	listen := func(who string) webhookBusiness.EventListener {
		return func(_ context.Context, kind string, data map[string]interface{}) {
			if data["task_id"] == zephyr {
				heardMu.Lock()
				heard[who] = append(heard[who], kind)
				heardMu.Unlock()
			}
		}
	}
	webhookBusiness.RegisterEventListener(listen("rule"))
	webhookBusiness.RegisterEventListenerSeeingRestricted(listen("todos"))

	// as is who, signed in, as the first middleware leaves a request.
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
	// call runs a handler as who (through a token when the name ends in
	// "+token"), with the route's parameters, a query and a JSON body.
	call := func(who string, h http.HandlerFunc, params map[string]string, query string, body any) (int, string) {
		t.Helper()
		person, viaToken := strings.CutSuffix(who, "+token")
		var rd io.Reader = http.NoBody
		if body != nil {
			raw, _ := json.Marshal(body)
			rd = bytes.NewReader(raw)
		}
		rc := chi.NewRouteContext()
		for k, v := range params {
			rc.URLParams.Add(k, v)
		}
		c := context.WithValue(as(person), chi.RouteCtxKey, rc)
		if viaToken {
			c = helpers.WithActsThroughToken(c)
		}
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPost, "/"+query, rd).WithContext(c))
		return rec.Code, compact(rec.Body.Bytes())
	}
	body := func(who string, h http.HandlerFunc, params map[string]string, query string, in any) string {
		t.Helper()
		_, out := call(who, h, params, query, in)
		return out
	}
	// eventually waits for cond, failing t (the test or subtest asking).
	eventually := func(t *testing.T, what string, cond func() bool) {
		t.Helper()
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
			if cond() {
				return
			}
		}
		t.Fatalf("timed out waiting until %s", what)
	}
	mention := func(name string) string {
		return fmt.Sprintf(`<span data-type="mention" data-id="%s@%s">@%s</span>`, w.people[name], uids[name], name)
	}
	comment := func(who, task, html string) (int, string) {
		t.Helper()
		code, out := call(who, taskController.CreateTaskComment, nil, "", map[string]any{"task_uuid": task, "task_comment_body": html})
		var got struct {
			Data struct {
				ID string `json:"comment_id"`
			} `json:"data"`
		}
		_ = json.Unmarshal([]byte(out), &got)
		if code == http.StatusOK {
			return code, got.Data.ID
		}
		return code, out
	}
	project := map[string]string{"project_uuid": w.project}
	from, to := at(-48*time.Hour), at(time.Hour)

	// What the assistant, and the MCP server's tools, answer as someone.
	ollama := httptest.NewServer(fakeOllama{})
	t.Cleanup(ollama.Close)
	t.Setenv("OLLAMA_HOST", ollama.URL)
	if _, err := w.env.PG.Exec(`UPDATE ai_settings SET embedding_dimension = $1, rate_limit_per_min = 1000`, fakeEmbeddingDim); err != nil {
		t.Fatal(err)
	}
	if err := ai.InitAIService(); err != nil || !ai.GetService().IsEnabled() {
		t.Fatalf("AI did not come up enabled: %v", err)
	}
	t.Cleanup(func() {
		_, _ = w.env.PG.Exec(`UPDATE ai_settings SET enabled = false`)
		_ = ai.ReloadAIService(context.Background())
	})
	if err := opensearchInit.RecreateAIEmbeddingsIndex(ctx, fakeEmbeddingDim); err != nil {
		t.Fatalf("create the embeddings index: %v", err)
	}
	aiBusiness.RegisterToolExecutors()
	indexEmbedding(t, map[string]any{"content_type": "task", "content_uuid": zephyr, "task_uuid": zephyr, "project_uuid": w.project,
		"content_text": "Zephyr pay review: Zephyr raises for the team leads", "author_name": "gia", "created_date": now.Unix()})
	tool := func(who, name string, params map[string]string) string {
		t.Helper()
		person, viaToken := strings.CutSuffix(who, "+token")
		exec, ok := ai.GetExecutor(name)
		if !ok {
			t.Fatalf("%s isn't registered", name)
		}
		c := ctx
		if viaToken {
			c = helpers.WithActsThroughToken(c)
		}
		out, _, err := exec(c, ai.ProposedAction{ToolName: name, Params: params}, w.people[person].String())
		if err != nil {
			return "error: " + err.Error()
		}
		return out
	}

	// Every surface, as someone. stubs: the restricted tasks may be named
	// here as "a task you can't see" (a dependency, a parent).
	type surface struct {
		name  string
		read  func(who string) string
		stubs bool
	}
	surfaces := []surface{
		{"the project's list", func(who string) string {
			return body(who, projectController.GetProjectTaskList, project, "?getAll=1", nil)
		}, true},
		{"the project's board", func(who string) string {
			return body(who, projectController.GetProjectTaskListForKanban, project, "?getAll=1", nil)
		}, true},
		{"the timeline", func(who string) string {
			return body(who, projectController.ProjectTimeline, project, "", nil)
		}, true},
		{"the project's page", func(who string) string {
			return body(who, projectController.ProjectInfo, project, "", nil)
		}, false},
		{"the task, opened", func(who string) string {
			return body(who, taskController.GetTaskInfo, map[string]string{"task_uuid": zephyr}, "", nil)
		}, false},
		{"a task below it, opened", func(who string) string {
			return body(who, taskController.GetTaskInfo, map[string]string{"task_uuid": letters}, "", nil)
		}, false},
		{"the task it holds up, opened", func(who string) string {
			return body(who, taskController.GetTaskInfo, map[string]string{"task_uuid": w.task}, "", nil)
		}, true},
		{"the task's history", func(who string) string {
			return body(who, taskController.GetTaskActivityList, map[string]string{"task_uuid": zephyr}, "", nil)
		}, false},
		{"the task's time", func(who string) string {
			return body(who, timeEntryController.GetTaskTime, map[string]string{"task_uuid": zephyr}, "", nil)
		}, false},
		{"My Tasks", func(who string) string {
			return body(who, userController.GetDgraphUserTaskList, nil, "?getAll=1", nil)
		}, true},
		{"My Tasks' board", func(who string) string {
			return body(who, userController.GetDgraphUserTaskListForKanban, nil, "?getAll=1", nil)
		}, true},
		{"the sidebar", func(who string) string {
			return body(who, userController.GetDgraphUserInfoByUUIDForSidebarNav, nil, "", nil)
		}, false},
		// The profile page reads Hal as this (its call status needs LiveKit,
		// which the harness hasn't).
		{"Hal's profile", func(who string) string {
			person, viaToken := strings.CutSuffix(who, "+token")
			c := as(person)
			if viaToken {
				c = helpers.WithActsThroughToken(c)
			}
			hal, err := userBusiness.GetDgraphUserInfoByUUID(c, w.people["hal"].String())
			if err != nil {
				return "error: " + err.Error()
			}
			raw, _ := json.Marshal(hal)
			return string(raw)
		}, false},
		{"the search", func(who string) string {
			return body(who, globalSearchController.GetUnifiedGlobalSearch, nil, "", map[string]string{"global_search_text": "zephyr"})
		}, false},
		{"the grammar's search", func(who string) string {
			return body(who, queryController.SearchQuery, nil, "", map[string]string{"q": "zephyr"})
		}, false},
		{"the API's search", func(who string) string {
			return body(who, v1Controller.SearchV1, nil, "?q=zephyr", nil)
		}, false},
		{"the API's tasks", func(who string) string {
			return body(who, v1Controller.ListTasksV1, nil, "", nil)
		}, false},
		{"the API's unread", func(who string) string {
			return body(who, v1Controller.Unread, nil, "", nil)
		}, false},
		{"the API's attention", func(who string) string {
			return body(who, v1Controller.Attention, nil, "", nil)
		}, false},
		{"the inbox", func(who string) string {
			return body(who, activityController.GetUnifiedActivity, nil, "?notices=1", nil)
		}, false},
		{"the comments feed", func(who string) string {
			return body(who, activityController.GetLatestComments, nil, "", nil)
		}, false},
		{"the mentions feed", func(who string) string {
			return body(who, activityController.GetLatestMentions, nil, "", nil)
		}, false},
		{"for later", func(who string) string {
			return body(who, savedItemController.List, nil, "", nil)
		}, false},
		{"the report", func(who string) string {
			return body(who, projectController.Report, nil, "?weeks=4&tz=UTC", nil)
		}, false},
		{"the workload", func(who string) string {
			return body(who, projectController.Workload, nil, "?weeks=2&tz=UTC", nil)
		}, false},
		{"the cycles", func(who string) string {
			return body(who, cycleController.ListCycles, project, "", nil)
		}, false},
		{"the burndown", func(who string) string {
			return body(who, cycleController.CycleBurndown, map[string]string{"project_uuid": w.project, "cycle_id": cycle.String()}, "?tz=UTC", nil)
		}, false},
		{"the time report", func(who string) string {
			return body(who, timeEntryController.ProjectTime, project, "?from="+from+"&to="+to, nil)
		}, true},
		{"the time export", func(who string) string {
			return body(who, timeEntryController.ProjectTime, project, "?format=csv&tz=UTC&from="+from+"&to="+to, nil)
		}, true},
		{"the assistant's search", func(who string) string { return tool(who, "search_workspace", map[string]string{"query": "zephyr"}) }, false},
		{"the assistant's project tasks", func(who string) string {
			return tool(who, "list_project_tasks", map[string]string{"project_uuid": w.project})
		}, false},
		{"the assistant's own tasks", func(who string) string { return tool(who, "list_tasks", map[string]string{}) }, false},
		{"the assistant's project overview", func(who string) string {
			return tool(who, "read_project", map[string]string{"project_uuid": w.project})
		}, false},
	}

	// Its words: every name, description, comment, note and file of it has
	// "Zephyr" (or "zephyr-raises"); a search's own "zephyr" echoed back
	// isn't the task.
	words := func(s string) bool { return wa2Words(s) }
	surfaceNamed := func(name string) surface {
		for _, s := range surfaces {
			if s.name == name {
				return s
			}
		}
		t.Fatalf("no surface %q", name)
		return surface{}
	}

	// Before: Ben sees it, and is told of it.
	if code, id := comment("ben", zephyr, "<p>Zephyr numbers look fine to me</p>"); code != http.StatusOK || id == "" {
		t.Fatalf("Ben's comment: %d %s", code, id)
	}
	if code, id := comment("ana", zephyr, "<p>Zephyr draft is up for "+mention("ben")+"</p>"); code != http.StatusOK || id == "" {
		t.Fatalf("Ana's mention of Ben: %d %s", code, id)
	}
	if code, out := call("gia", taskController.UpdateTaskStatus, nil, "", map[string]string{"task_uuid": zephyr, "task_status": "inProgress", "task_project_uuid": w.project}); code != http.StatusOK {
		t.Fatalf("Gia starts it: %d %s", code, out)
	}
	eventually(t, "Ben finds it in the search", func() bool { return words(surfaceNamed("the search").read("ben")) })
	eventually(t, "the webhooks are sent its change", func() bool {
		hooksMu.Lock()
		defer hooksMu.Unlock()
		sent := map[uuid.UUID]bool{}
		for _, d := range delivered {
			if d.kind == "task.status_changed" && strings.Contains(d.payload, zephyr) {
				sent[d.webhook] = true
			}
		}
		return sent[hooks["ana"]] && sent[hooks["ben"]]
	})
	eventually(t, "both listeners hear of it", func() bool {
		heardMu.Lock()
		defer heardMu.Unlock()
		return len(heard["rule"]) > 0 && len(heard["todos"]) > 0
	})
	eventually(t, "Ben is told he was mentioned on it", func() bool { return words(surfaceNamed("the mentions feed").read("ben")) })
	shownBefore := map[string]bool{}
	for _, s := range surfaces {
		shownBefore[s.name] = words(s.read("ben"))
	}
	for _, name := range []string{"the project's list", "the project's board", "the timeline", "the task, opened", "a task below it, opened",
		"the task's time", "the search", "the API's search", "the mentions feed", "for later", "the workload", "the time report",
		"the time export", "the assistant's search", "the assistant's project tasks"} {
		// Search and the assistant's search are written off the request, and
		// the assistant's once lagged the read under load (v2.75.0, v2.75.2):
		// what doesn't show it yet is waited for.
		if !shownBefore[name] {
			s := surfaceNamed(name)
			eventually(t, "before it is restricted, "+name+" shows Ben the task (or the walk below proves nothing there)", func() bool { return words(s.read("ben")) })
		}
	}
	badgeBefore, err := activityBusiness.UnreadActivityCount(as("ben"), w.people["ben"], uids["ben"])
	if err != nil {
		t.Fatal(err)
	}
	type counts struct{ list, burndown, cycle, report int }
	countsOf := func(who string) counts {
		t.Helper()
		var list struct {
			Data struct {
				Count int `json:"project_task_count"`
			} `json:"data"`
		}
		_ = json.Unmarshal([]byte(surfaceNamed("the project's list").read(who)), &list)
		var burn struct {
			Data struct {
				Burndown struct {
					Tasks int `json:"tasks"`
				} `json:"burndown"`
			} `json:"data"`
		}
		_ = json.Unmarshal([]byte(surfaceNamed("the burndown").read(who)), &burn)
		var cycles struct {
			Data struct {
				Cycles []struct {
					Progress struct {
						Total int `json:"total"`
					} `json:"progress"`
				} `json:"cycles"`
			} `json:"data"`
		}
		_ = json.Unmarshal([]byte(surfaceNamed("the cycles").read(who)), &cycles)
		var report struct {
			Data struct {
				Open int `json:"open"`
			} `json:"data"`
		}
		_ = json.Unmarshal([]byte(surfaceNamed("the report").read(who)), &report)
		c := counts{list: list.Data.Count, burndown: burn.Data.Burndown.Tasks, report: report.Data.Open}
		if len(cycles.Data.Cycles) > 0 {
			c.cycle = cycles.Data.Cycles[0].Progress.Total
		}
		return c
	}
	benBefore, anaBefore := countsOf("ben"), countsOf("ana")
	if benBefore != anaBefore || benBefore.list == 0 || benBefore.burndown != 2 {
		t.Fatalf("before it is restricted, Ben counts %+v and Ana %+v: they should agree, with both cycle tasks", benBefore, anaBefore)
	}
	// From here on, what is sent and heard is after the restriction.
	broker.mu.Lock()
	mark := len(broker.got)
	broker.mu.Unlock()
	hooksMu.Lock()
	hooksMark := len(delivered)
	hooksMu.Unlock()
	heardMu.Lock()
	ruleMark, todosMark := len(heard["rule"]), len(heard["todos"])
	heardMu.Unlock()

	t.Run("who may restrict it, and to whom", func(t *testing.T) {
		restriction := map[string]string{"task_ref": zephyr}
		expect := func(what string, code int, out string, want int, says string) {
			t.Helper()
			if code != want || !strings.Contains(out, says) {
				t.Errorf("%s: %d %s, want %d saying %q", what, code, out, want, says)
			}
		}
		code, out := call("ben", taskRestrictionController.SetRestriction, restriction, "", map[string]any{"restricted": true})
		expect("a member who didn't make it", code, out, http.StatusForbidden, "Only this project's admins, or whoever made the task")
		code, out = call("ben+token", taskRestrictionController.SetRestriction, restriction, "", map[string]any{"restricted": true})
		expect("a member through a token", code, out, http.StatusForbidden, "Only this project's admins")
		code, out = call("gia", taskRestrictionController.SetRestriction, restriction, "", map[string]any{"restricted": true, "people": []string{w.people["cy"].String()}})
		expect("letting in someone outside the project", code, out, http.StatusBadRequest, "Only people in the task's project")
		code, out = call("gia", taskRestrictionController.SetRestriction, restriction, "", map[string]any{"restricted": true, "people": []string{"Ivy"}})
		expect("someone who isn't a person", code, out, http.StatusBadRequest, "isn't a person")
		code, out = call("gia", taskRestrictionController.SetRestriction, restriction, "", map[string]any{"restricted": false, "people": []string{w.people["ivy"].String()}})
		expect("opening it to some by name", code, out, http.StatusBadRequest, "lets no one in by name")

		code, out = call("gia", taskRestrictionController.SetRestriction, restriction, "", map[string]any{"restricted": true, "people": []string{w.people["ivy"].String()}})
		expect("its creator keeps it to Ivy", code, out, http.StatusOK, `"restricted":true`)
		code, out = call("ana", taskRestrictionController.SetRestriction, map[string]string{"task_ref": memo}, "", map[string]any{"restricted": true})
		expect("an admin keeps a memo to the people the rule lets in", code, out, http.StatusOK, `"restricted":true`)
		// The tasks below it were restricted with it; Ana opens one again.
		code, out = call("hal", taskRestrictionController.Restriction, map[string]string{"task_ref": letters}, "", nil)
		expect("a task below it, read by the assignee of the task above", code, out, http.StatusOK, `"restricted_with":"`+zephyr+`"`)
		if !strings.Contains(out, w.people["ivy"].String()) {
			t.Errorf("a task below it has its people: %s", out)
		}
		code, out = call("ana", taskRestrictionController.SetRestriction, map[string]string{"task_ref": room}, "", map[string]any{"restricted": false})
		expect("an admin opens a task below it again", code, out, http.StatusOK, `"restricted":false`)
		code, out = call("ben", taskRestrictionController.Restriction, restriction, "", nil)
		expect("Ben asks who sees it", code, out, http.StatusNotFound, "")
	})

	t.Run("counts leave out what the reader can't see", func(t *testing.T) {
		ben, ana := countsOf("ben"), countsOf("ana")
		if ana != anaBefore {
			t.Errorf("Ana's counts changed: %+v, were %+v", ana, anaBefore)
		}
		var list struct {
			Data struct {
				Tasks []struct {
					UUID string `json:"task_uuid"`
				} `json:"project_tasks"`
			} `json:"data"`
		}
		_ = json.Unmarshal([]byte(surfaceNamed("the project's list").read("ana")), &list)
		hidden := 0
		for _, row := range list.Data.Tasks {
			if row.UUID == zephyr || row.UUID == letters || row.UUID == memo {
				hidden++
			}
		}
		if hidden == 0 || ana.list-ben.list != hidden {
			t.Errorf("Ben's list counts %d, Ana's %d, with %d of hers kept from him", ben.list, ana.list, hidden)
		}
		if ben.burndown != 1 || ben.cycle != 1 || ana.cycle != 2 {
			t.Errorf("the cycle: Ben's burndown counts %d tasks and its progress %d, Ana's %d; want 1, 1 and 2", ben.burndown, ben.cycle, ana.cycle)
		}
		if ben.report >= ana.report {
			t.Errorf("the report: Ben counts %d open, Ana %d", ben.report, ana.report)
		}
		if badge, err := activityBusiness.UnreadActivityCount(as("ben"), w.people["ben"], uids["ben"]); err != nil || badge >= badgeBefore {
			t.Errorf("Ben's inbox badge: %d (%v), was %d", badge, err, badgeBefore)
		}
	})

	var later struct{ comment, subTask string }
	t.Run("what happens on it after reaches only its people", func(t *testing.T) {
		code, id := comment("gia", zephyr, "<p>Zephyr letters go out Friday</p>")
		if code != http.StatusOK || id == "" {
			t.Fatalf("Gia's comment: %d %s", code, id)
		}
		later.comment = id
		if got := emailedOf(t, w, "task.comment", id, 2); !slices.Equal(got, []string{"ana", "hal"}) {
			t.Errorf("emailed %v, want [ana hal]: Ana commented before and runs the project, Hal is its assignee; never Ben, who commented before it was restricted", got)
		}
		code, out := comment("ana", zephyr, "<p>Zephyr and "+mention("ben")+"</p>")
		if code != http.StatusBadRequest || !strings.Contains(out, "ben isn't") {
			t.Errorf("mentioning Ben on it: %d %s, want a refusal that names him", code, out)
		}
		if code, out := call("gia", taskController.UpdateTaskStatus, nil, "", map[string]string{"task_uuid": zephyr, "task_status": "inReview", "task_project_uuid": w.project}); code != http.StatusOK {
			t.Fatalf("Gia moves it on: %d %s", code, out)
		}
		code, out = call("gia", taskController.CreateSubTask, nil, "", map[string]string{"task_uuid": zephyr, "task_project_uuid": w.project, "task_name": "Zephyr bonus pool"})
		var sub struct {
			Data struct {
				UUID string `json:"task_uuid"`
			} `json:"data"`
		}
		_ = json.Unmarshal([]byte(out), &sub)
		if code != http.StatusOK || sub.Data.UUID == "" {
			t.Fatalf("Gia adds a task below it: %d %s", code, out)
		}
		later.subTask = sub.Data.UUID
		code, out = call("ben", taskController.CreateSubTask, nil, "", map[string]string{"task_uuid": zephyr, "task_project_uuid": w.project, "task_name": "A task below what I can't see"})
		if code == http.StatusOK {
			t.Errorf("Ben adds a task below it: %d %s", code, out)
		}
		eventually(t, "Ana's webhook is sent the change and the comment", func() bool {
			hooksMu.Lock()
			defer hooksMu.Unlock()
			sent := map[string]bool{}
			for _, d := range delivered[hooksMark:] {
				if d.webhook == hooks["ana"] && strings.Contains(d.payload, zephyr) {
					sent[d.kind] = true
				}
			}
			return sent["task.status_changed"] && sent["task.comment.created"]
		})
		eventually(t, "the to-dos hear of the comment and the change", func() bool {
			heardMu.Lock()
			defer heardMu.Unlock()
			return len(heard["todos"]) >= todosMark+2
		})
		time.Sleep(time.Second) // anything else would be sent by now
		hooksMu.Lock()
		for _, d := range delivered[hooksMark:] {
			if d.webhook == hooks["ben"] && (strings.Contains(d.payload, zephyr) || words(d.payload)) {
				t.Errorf("Ben's webhook was sent %s: %s", d.kind, d.payload)
			}
		}
		hooksMu.Unlock()
		heardMu.Lock()
		if n := len(heard["rule"]); n != ruleMark {
			t.Errorf("the rule that posts heard %d more of its events after it was restricted: %v", n-ruleMark, heard["rule"][ruleMark:])
		}
		heardMu.Unlock()
	})

	readers := []string{"ana", "fay", "gia", "hal", "ivy"}
	kept := []string{"ben", "cy", "dee"}
	secretTasks := []string{zephyr, letters, memo, later.subTask}
	t.Run("no surface shows it to anyone it is kept from", func(t *testing.T) {
		for _, s := range surfaces {
			for _, who := range kept {
				out := s.read(who)
				if leaks := wa2Leaks(out, s.stubs, secretTasks, []string{later.comment}); len(leaks) > 0 {
					t.Errorf("%s shows %s %v:\n%s", s.name, who, leaks, out)
				}
			}
		}
		// Through a token, an admin is a member: shown no more than Ben is
		// (the memo is Ana's own, which she made).
		for _, s := range surfaces {
			out := s.read("ana+token")
			for _, title := range []string{"Zephyr pay review", "Draft the Zephyr letters", "Zephyr bonus pool", "go out Friday"} {
				if strings.Contains(out, title) {
					t.Errorf("%s shows Ana's token %q:\n%s", s.name, title, out)
				}
			}
		}
	})

	t.Run("its people go on seeing it", func(t *testing.T) {
		for _, who := range readers {
			for _, name := range []string{"the project's list", "the project's board", "the task, opened", "a task below it, opened", "the search", "the API's search", "the workload"} {
				if out := surfaceNamed(name).read(who); !words(out) {
					t.Errorf("%s doesn't show %s the task:\n%s", name, who, out)
				}
			}
		}
		for name, who := range map[string]string{"My Tasks": "hal", "the API's tasks": "hal", "the assistant's own tasks": "hal", "the assistant's search": "hal",
			"the time export": "ana", "the assistant's project tasks": "ana"} {
			if out := surfaceNamed(name).read(who); !words(out) {
				t.Errorf("%s doesn't show %s the task:\n%s", name, who, out)
			}
		}
		if out := surfaceNamed("the time export").read("ana"); !strings.Contains(out, "Zephyr spreadsheet checks") {
			t.Errorf("the time export leaves Ana's own project's notes out:\n%s", out)
		}
		if out := body("hal", taskController.GetTaskInfo, map[string]string{"task_uuid": later.subTask}, "", nil); !strings.Contains(out, "Zephyr bonus pool") {
			t.Errorf("Hal, its assignee, can't open a task added below it later:\n%s", out)
		}
	})

	// WA2b: what its people read of it carries the lock (task_restricted), on
	// a row, a card and the panel; nobody it is kept from reads the flag of
	// anything, and an open task never carries it.
	t.Run("its people see the lock on it", func(t *testing.T) {
		for _, who := range readers {
			for _, name := range []string{"the project's list", "the project's board", "the task, opened"} {
				if out := surfaceNamed(name).read(who); !strings.Contains(out, `"task_restricted":true`) {
					t.Errorf("%s shows %s the task without its lock:\n%s", name, who, out)
				}
			}
		}
		if out := surfaceNamed("My Tasks").read("hal"); !strings.Contains(out, `"task_restricted":true`) {
			t.Errorf("My Tasks shows Hal, its assignee, the task without its lock:\n%s", out)
		}
		for _, who := range kept {
			if out := surfaceNamed("the project's list").read(who); strings.Contains(out, "task_restricted") {
				t.Errorf("the list marks a task restricted for %s, who sees none:\n%s", who, out)
			}
		}
		out := body("ana", taskController.GetTaskInfo, map[string]string{"task_uuid": zephyr}, "", nil)
		if !strings.Contains(out, `"restrict":true`) {
			t.Errorf("the panel doesn't offer Ana, its project's admin, who sees it:\n%s", out)
		}
	})

	t.Run("a link to it says only that there is a task you can't see", func(t *testing.T) {
		out := body("ben", taskController.GetTaskInfo, map[string]string{"task_uuid": w.task}, "", nil)
		var got struct {
			Data struct {
				BlockedBy []map[string]any `json:"task_blocked_by"`
				SubTasks  []map[string]any `json:"task_sub_tasks"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Data.BlockedBy) != 1 || got.Data.BlockedBy[0]["task_uuid"] != zephyr || got.Data.BlockedBy[0]["task_hidden"] != true || got.Data.BlockedBy[0]["task_name"] != nil {
			t.Errorf("what it waits on, as Ben sees it: %v", got.Data.BlockedBy)
		}
		for _, s := range got.Data.SubTasks {
			if s["task_uuid"] == memo {
				t.Errorf("the memo restricted below it is listed for Ben: %v", s)
			}
		}
		out = body("ben", taskController.GetTaskInfo, map[string]string{"task_uuid": room}, "", nil)
		var opened struct {
			Data struct {
				Name   string         `json:"task_name"`
				Parent map[string]any `json:"task_parent_task"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &opened); err != nil {
			t.Fatal(err)
		}
		if opened.Data.Name != "Book the room" || opened.Data.Parent["task_hidden"] != true || opened.Data.Parent["task_name"] != nil {
			t.Errorf("the task below it opened again, as Ben sees it: %s", out)
		}
	})

	t.Run("live updates name it only to its people", func(t *testing.T) {
		mine := map[string]string{}
		for _, who := range readers {
			mine[w.people[who].String()] = who
		}
		broker.mu.Lock()
		defer broker.mu.Unlock()
		told := 0
		for _, m := range broker.got[mark:] {
			if !words(string(m.payload)) {
				continue
			}
			// Only a person's own topic, and only one of its people's.
			kind, id, ok := helpers.ParseMqttTopic(m.topic)
			if _, theirs := mine[strings.ToLower(id)]; ok && theirs && m.topic == helpers.GetMqttTopicForUserActivity(id) {
				told++
				continue
			}
			t.Errorf("a %s topic (%s) was told of it: %s", kind, id, m.payload)
		}
		if told == 0 {
			t.Error("none of its people was told live of what happened on it")
		}
	})

	t.Run("its people can open it to the project again", func(t *testing.T) {
		if code, out := call("ana", taskRestrictionController.SetRestriction, map[string]string{"task_ref": zephyr}, "", map[string]any{"restricted": false}); code != http.StatusOK {
			t.Fatalf("Ana opens it: %d %s", code, out)
		}
		for _, name := range []string{"the project's list", "the task, opened", "a task below it, opened"} {
			if out := surfaceNamed(name).read("ben"); !words(out) {
				t.Errorf("opened again, %s still keeps it from Ben:\n%s", name, out)
			}
		}
		if out := body("ben", taskController.GetTaskInfo, map[string]string{"task_uuid": memo}, "", nil); words(out) {
			t.Errorf("the memo, restricted on its own, opened with it:\n%s", out)
		}
	})
}

// emailedOf is who was emailed of the event whose dedup key holds key, of type
// kind, by name: at least want of them, and anyone else in the same batch.
func emailedOf(t *testing.T, w *workWorld, kind, key string, want int) []string {
	t.Helper()
	byID := map[string]string{}
	for name, id := range w.people {
		byID[id.String()] = name
	}
	var got []string
	read := func() bool {
		rows, err := w.env.PG.Query(`SELECT user_id::text FROM notification_email_queue WHERE event_type = $1 AND strpos(dedup_key, $2) > 0`, kind, key)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		got = nil
		for rows.Next() {
			var id string
			_ = rows.Scan(&id)
			got = append(got, byID[id])
		}
		return len(got) >= want
	}
	waitFor(t, kind+" emails", read)
	time.Sleep(500 * time.Millisecond) // anyone else would be in the same batch
	read()
	slices.Sort(got)
	return got
}

// wa2Leaks is what body shows of the restricted work: its words, a comment on
// it, or one of its tasks by id other than as a stub that says only that it is
// there (where stubs are allowed, as a dependency or a parent is).
func wa2Leaks(body string, stubs bool, tasks, comments []string) []string {
	var leaks []string
	if wa2Words(body) {
		leaks = append(leaks, "its words")
	}
	for _, id := range comments {
		if id != "" && strings.Contains(body, id) {
			leaks = append(leaks, "the comment "+id)
		}
	}
	stubbed := map[string]int{}
	if stubs {
		var v any
		if json.Unmarshal([]byte(body), &v) == nil {
			wa2Stubs(v, stubbed)
		}
	}
	for _, id := range tasks {
		if id == "" {
			continue
		}
		if n := strings.Count(body, id); n > stubbed[id] {
			leaks = append(leaks, fmt.Sprintf("the task %s (%d times, %d as a stub)", id, n, stubbed[id]))
		}
	}
	return leaks
}

// wa2Words is whether s says any of the restricted work's words: each of its
// names, descriptions, comments, notes and files has "Zephyr", or is the file
// "zephyr-raises.xlsx". A search's query, "zephyr", echoed back says nothing.
func wa2Words(s string) bool {
	return strings.Contains(s, "Zephyr") || strings.Contains(s, "zephyr-raises")
}

// wa2Stubs counts, by task id, the stubs in v: objects that say only a task's
// id, that it is hidden, and the kind of link to it; and the ids named beside
// "A task you can't see" in a time report.
func wa2Stubs(v any, stubbed map[string]int) {
	switch x := v.(type) {
	case map[string]any:
		if id, ok := x["task_uuid"].(string); ok && x["task_hidden"] == true {
			stub := true
			for k := range x {
				if k != "task_uuid" && k != "task_hidden" && !strings.HasPrefix(k, "task_blocked_by|") && !strings.HasPrefix(k, "task_blocks|") {
					stub = false
				}
			}
			if stub {
				stubbed[id]++
			}
		}
		if id, ok := x["id"].(string); ok && x["name"] == "A task you can't see" {
			stubbed[id]++
		}
		for _, e := range x {
			wa2Stubs(e, stubbed)
		}
	case []any:
		for _, e := range x {
			wa2Stubs(e, stubbed)
		}
	}
}
