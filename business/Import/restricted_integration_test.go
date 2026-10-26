//go:build integration

package business

// A task its source keeps to some people comes in restricted, and is never
// readable more widely here than it was there (restricted.go). Of the HR
// project's Jira issues:
//   - HR-4 "Zephyr pay bands" has a security level: made by Cy, assigned to
//     Al, reported by Rae, with a comment;
//   - HR-6 "Zephyr letters", its sub-task, has the level too, reported and
//     made by Sol;
//   - HR-8 "Zephyr seating plan", another sub-task, made by Ben, carries no
//     level of its own;
//   - HR-7 "Zephyr audit findings" has the level, made and reported by Nia
//     and assigned to Ned;
//   - HR-5 "Zephyr desk move" is open; of its comments, Jira keeps one to a
//     role and another is a service desk's internal note.
// Ada runs the workspace and the project and ran the import. Everyone was
// matched by email, but Nia, matched by her name, and Al and Ned, whom the
// workspace's earlier import of the same site matched, Al by email and Ned by
// his name. So each restricted issue is seen by Ada and the people Jira lets
// see it who were matched by email, each on its own: HR-4 by Rae and Al, not
// by Cy for having made it; HR-6 by Sol, not by HR-4's people; HR-8 and HR-7
// by Ada alone. Nobody else sees them, or HR-4's comment, through the task,
// the board, search and its hit count, the task list, or a notice. The
// open one shows everywhere, with the comment everyone may read; the two Jira
// keeps from some stay there, for everyone. The report says each task came in
// restricted, why, and who wasn't let in, and how many of HR-5's comments
// stayed in Jira, without a name or a comment's text.
// Run: PKGS=./business/Import/ integ_rel.sh <wt> <log> '^TestATaskItsSourceRestrictsComesInRestricted$'

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	MQTT "github.com/eclipse/paho.mqtt.golang"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	globalSearchController "github.com/akashc777/OneCamp/controllers/GlobalSearch"
	projectController "github.com/akashc777/OneCamp/controllers/Project"
	taskController "github.com/akashc777/OneCamp/controllers/Task"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/akashc777/OneCamp/tests/integration"
)

// keptProvider streams the project's issues, HR-4's sub-tasks and HR-4's
// comment, as Jira hands them over.
type keptProvider struct{ importProvider.Provider }

func (keptProvider) DefaultStatusMap() map[string]string   { return map[string]string{} }
func (keptProvider) DefaultPriorityMap() map[string]string { return map[string]string{} }

const keptLevel = "an issue security level"

func (keptProvider) IterTasksOfProject(context.Context, *importModels.Job, importProvider.JobOptions, string) (<-chan importProvider.SourceTask, <-chan error) {
	return keptStream(
		importProvider.SourceTask{SourceID: "HR-4", ProjectSourceID: "HR", Name: "Zephyr pay bands", Description: "<p>Zephyr raises for the leads</p>",
			Status: "To Do", CreatedBy: "u-cy", AssigneeIds: []string{"u-al"}, Restricted: keptLevel, Audience: []string{"u-rae"}, CommentCount: 1},
		importProvider.SourceTask{SourceID: "HR-5", ProjectSourceID: "HR", Name: "Zephyr desk move", Status: "To Do", CreatedBy: "u-ben"},
		importProvider.SourceTask{SourceID: "HR-7", ProjectSourceID: "HR", Name: "Zephyr audit findings", Status: "To Do",
			CreatedBy: "u-nia", AssigneeIds: []string{"u-ned"}, Restricted: keptLevel, Audience: []string{"u-nia"}},
		importProvider.SourceTask{SourceID: "__sub__:HR-4", ParentTaskID: "HR-4"},
	)
}

// HR-6 has its own level and its own reporter; HR-8's own search result
// carries no level, and it is kept by sitting under HR-4.
func (keptProvider) IterSubtasksOfTask(context.Context, *importModels.Job, importProvider.JobOptions, string) (<-chan importProvider.SourceTask, <-chan error) {
	return keptStream(
		importProvider.SourceTask{SourceID: "HR-6", ParentTaskID: "HR-4", ProjectSourceID: "HR", Name: "Zephyr letters", Status: "To Do",
			CreatedBy: "u-sol", Restricted: keptLevel, Audience: []string{"u-sol"}},
		importProvider.SourceTask{SourceID: "HR-8", ParentTaskID: "HR-4", ProjectSourceID: "HR", Name: "Zephyr seating plan", Status: "To Do", CreatedBy: "u-ben"},
	)
}

func (keptProvider) IterCommentsOfTask(_ context.Context, _ *importModels.Job, _ importProvider.JobOptions, task string) (<-chan importProvider.SourceComment, <-chan error) {
	out, errs := make(chan importProvider.SourceComment, 3), make(chan error)
	switch task {
	case "HR-4":
		out <- importProvider.SourceComment{SourceID: "c-41", TaskSourceID: "HR-4", Body: "<p>Quokka bonus pool for the leads</p>", AuthorSourceID: "u-rae"}
	case "HR-5":
		out <- importProvider.SourceComment{SourceID: "c-51", TaskSourceID: "HR-5", Body: "<p>Quokka desks arrive Monday</p>", AuthorSourceID: "u-ben"}
		out <- importProvider.SourceComment{SourceID: "c-52", TaskSourceID: "HR-5", Body: "<p>Quokka salaries of the movers</p>", AuthorSourceID: "u-rae",
			Restricted: "a project role or group"}
		out <- importProvider.SourceComment{SourceID: "c-53", TaskSourceID: "HR-5", Body: "<p>Quokka vendor quote is 40k</p>", AuthorSourceID: "u-rae",
			Restricted: "an internal note"}
	}
	close(out)
	close(errs)
	return out, errs
}

func keptStream(tasks ...importProvider.SourceTask) (<-chan importProvider.SourceTask, <-chan error) {
	out, errs := make(chan importProvider.SourceTask, len(tasks)), make(chan error)
	for _, st := range tasks {
		out <- st
	}
	close(out)
	close(errs)
	return out, errs
}

// keptBroker keeps every realtime message the server publishes.
type keptBroker struct {
	MQTT.Client
	mu  sync.Mutex
	got []string
}

func (b *keptBroker) Publish(topic string, _ byte, _ bool, payload interface{}) MQTT.Token {
	p, _ := payload.([]byte)
	b.mu.Lock()
	b.got = append(b.got, topic+" "+string(p))
	b.mu.Unlock()
	return keptToken{}
}

type keptToken struct{}

func (keptToken) Wait() bool                     { return true }
func (keptToken) WaitTimeout(time.Duration) bool { return true }
func (keptToken) Done() <-chan struct{}          { c := make(chan struct{}); close(c); return c }
func (keptToken) Error() error                   { return nil }

func TestATaskItsSourceRestrictsComesInRestricted(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	broker := &keptBroker{}
	integration.UseMqttClient(t, broker)
	var pushMu sync.Mutex
	var pushes []string
	t.Cleanup(firebaseInit.ObservePushesForTest(func(data map[string]string, _ []string) {
		raw, _ := json.Marshal(data)
		pushMu.Lock()
		pushes = append(pushes, string(raw))
		pushMu.Unlock()
	}))
	dg := integration.SetupDgraph(t)
	search := integration.SetupOpenSearch(t)
	if err := opensearchInit.ConnectOpenSearch(&opensearchInit.OpenSearchConfig{Host: search.URL}); err != nil {
		t.Fatalf("create the indices: %v", err)
	}

	// Ada runs the workspace and the project; the rest are its members.
	names := []string{"ada", "ben", "rae", "al", "cy", "sol", "nia", "ned"}
	people := map[string]uuid.UUID{}
	for _, n := range names {
		people[n] = uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@acme.test', $2, $2, NOW(), NOW())`, people[n], n); err != nil {
			t.Fatal(err)
		}
	}
	project, team := uuid.New(), uuid.New()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO admin_users (email_id) VALUES ('ada@acme.test')`, nil},
		{`INSERT INTO teams (id, team_name, created_by) VALUES ($1, 'People', $2)`, []any{team, people["ada"]}},
		{`INSERT INTO projects (id, project_name, team_id, created_by) VALUES ($1, 'HR', $2, $3)`, []any{project, team, people["ada"]}},
	} {
		if _, err := env.PG.Exec(q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	live := "0001-01-01T00:00:00Z"
	in := func(names ...string) []map[string]any {
		var out []map[string]any
		for _, n := range names {
			out = append(out, map[string]any{"uid": "_:" + n})
		}
		return out
	}
	nodes := []map[string]any{{"uid": "_:hr", "dgraph.type": "Project", "project_uuid": project.String(), "project_name": "HR",
		"project_deleted_at": live, "project_members": in(names...), "project_admins": in("ada"), "project_created_by": map[string]any{"uid": "_:ada"},
		"project_team": map[string]any{"uid": "_:people", "dgraph.type": "Team", "team_uuid": team.String(), "team_name": "People",
			"team_members": in(names...), "team_admins": in("ada"), "team_projects": in("hr")}}}
	for _, n := range names {
		nodes = append(nodes, map[string]any{"uid": "_:" + n, "dgraph.type": "User", "user_uuid": people[n].String(), "user_name": n,
			"user_deleted_at": live, "user_projects": in("hr"), "user_teams": in("people")})
	}
	uids := dg.Mutate(t, nodes)

	// How each came across: the workspace's earlier import of the site first
	// matched Al (by email) and Ned (by his name).
	ada := people["ada"]
	newJob := func(status string) *importModels.Job {
		t.Helper()
		j := &importModels.Job{Id: uuid.New(), Provider: importModels.ProviderJira, SourceWorkspaceName: "acme", Source: "api",
			Status: status, TriggeredBy: &ada}
		if err := importModels.CreateJob(ctx, j); err != nil {
			t.Fatal(err)
		}
		return j
	}
	// One import of a workspace runs at a time: the earlier one has finished.
	earlier, job := newJob(importModels.StatusCompleted), newJob(importModels.StatusRunning)
	for n, by := range map[string]string{"al": "email", "ned": "name"} {
		if err := importModels.RecordMapping(ctx, earlier.Id, earlier.Provider, earlier.SourceWorkspaceName, importModels.EntityUser,
			"u-"+n, people[n], nil, mustMarshal(map[string]any{"matched_by": by}), false); err != nil {
			t.Fatal(err)
		}
	}
	matchedBy := map[string]string{"ada": "email", "ben": "email", "rae": "email", "cy": "email", "sol": "email",
		"nia": "name", "al": "workspace_map", "ned": "workspace_map"}
	for _, n := range names {
		if err := importModels.UpsertIdMappingWithOwnership(ctx, job.Id, importModels.EntityUser, "u-"+n, people[n], nil,
			mustMarshal(map[string]any{"matched_by": matchedBy[n]}), false); err != nil {
			t.Fatal(err)
		}
	}
	if err := importModels.UpsertIdMappingWithOwnership(ctx, job.Id, importModels.EntityProject, "HR", project, nil, mustMarshal(map[string]any{}), true); err != nil {
		t.Fatal(err)
	}
	importer := &userModels.UserInfo{UserPostgresInfo: userModels.User{Id: ada}}
	importer.UserDgraphInfo.Uid, importer.UserDgraphInfo.Uuid = uids["ada"], ada.String()

	// The import, as RunImport runs it: the project's issues, the sub-tasks'
	// chunk they queued, and HR-4's comments. CreateChunks keeps a chunk the
	// project's issues already queued and skips this one, so each stage works
	// the stored chunk: the report lines it writes cite it.
	run := func(kind, parent string, stage func(*importModels.Chunk) error) {
		t.Helper()
		c := &importModels.Chunk{Id: uuid.New(), ImportId: job.Id, ChunkType: kind, ParentSourceId: &parent, Status: importModels.ChunkStatusInProgress, MaxAttempts: 3}
		if err := importModels.CreateChunks(ctx, []*importModels.Chunk{c}); err != nil {
			t.Fatal(err)
		}
		if err := env.PG.QueryRow(`SELECT id FROM import_chunks WHERE import_id = $1 AND chunk_type = $2 AND channel_slack_id = $3`, job.Id, kind, parent).Scan(&c.Id); err != nil {
			t.Fatal(err)
		}
		if err := stage(c); err != nil {
			t.Fatalf("the %s chunk: %v", kind, err)
		}
	}
	work := importContext(ctx)
	run(importModels.ChunkProjectTasks, "HR", func(c *importModels.Chunk) error {
		return processTaskChunk(work, keptProvider{}, job, importProvider.JobOptions{}, c, importer)
	})
	run(importModels.ChunkTaskSubtasks, "HR-4", func(c *importModels.Chunk) error {
		return processSubtaskChunk(work, keptProvider{}, job, importProvider.JobOptions{}, c, importer)
	})
	for _, task := range []string{"HR-4", "HR-5"} {
		run(importModels.ChunkTaskComments, task, func(c *importModels.Chunk) error {
			return processTaskCommentChunk(work, keptProvider{}, job, importProvider.JobOptions{}, c, importer)
		})
	}
	made := map[string]string{}
	for _, entity := range []string{importModels.EntityTask, importModels.EntitySubtask} {
		m, err := importModels.IdMapOf(ctx, job.Id, entity)
		if err != nil {
			t.Fatal(err)
		}
		for src, id := range m {
			made[src[strings.LastIndex(src, ":")+1:]] = id.String()
		}
	}
	for _, src := range []string{"HR-4", "HR-5", "HR-6", "HR-7", "HR-8"} {
		if made[src] == "" {
			t.Fatalf("%s wasn't imported: %v", src, made)
		}
	}

	// as is who, signed in, as the first middleware leaves a request.
	as := func(who string) context.Context {
		t.Helper()
		pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, people[who])
		if err != nil || pg == nil {
			t.Fatalf("load %s: %v", who, err)
		}
		gr, err := userDomain.GetActiveDgraphUserInfoByUUID(ctx, people[who].String())
		if err != nil || gr == nil {
			t.Fatalf("load %s's profile: %v", who, err)
		}
		return taskread.Install(context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr}))
	}
	call := func(who string, h http.HandlerFunc, params map[string]string, query string, body any) (int, string) {
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
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPost, "/"+query, rd).WithContext(context.WithValue(as(who), chi.RouteCtxKey, rc)))
		return rec.Code, rec.Body.String()
	}
	// Who may see each restricted one, and what tells of it.
	may := map[string][]string{"HR-4": {"ada", "rae", "al"}, "HR-6": {"ada", "sol"}, "HR-7": {"ada"}, "HR-8": {"ada"}}
	marks := map[string][]string{"HR-4": {"pay bands", "raises for the leads", "bonus pool"}, "HR-6": {"letters"},
		"HR-7": {"audit findings"}, "HR-8": {"seating plan"}}
	tells := func(s, task string) bool {
		s = strings.ToLower(s)
		for _, m := range append(marks[task], made[task]) {
			if strings.Contains(s, m) {
				return true
			}
		}
		return false
	}
	searchHits := func(who, text string) (string, int) {
		t.Helper()
		_, out := call(who, globalSearchController.GetUnifiedGlobalSearch, nil, "", map[string]string{"global_search_text": text})
		var page struct {
			Data struct {
				Page []json.RawMessage `json:"page"`
			} `json:"data"`
		}
		_ = json.Unmarshal([]byte(out), &page)
		return out, len(page.Data.Page)
	}
	// commentsHeld is how many comment entries search holds for the tasks.
	commentsHeld := func(tasks ...string) int {
		t.Helper()
		q, _ := json.Marshal(map[string]any{"query": map[string]any{"terms": map[string]any{"comment_task_id": tasks}}})
		resp, err := opensearchInit.OpenSearchClient.Indices.Count(ctx, &opensearchapi.IndicesCountReq{Indices: []string{"comments"}, Body: bytes.NewReader(q)})
		if err != nil {
			t.Fatalf("count the comments search holds: %v", err)
		}
		return resp.Count
	}
	// Search is written off the import; it has the five issues once Ada
	// finds them, and holds the two comments that came across.
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(250 * time.Millisecond) {
		_, n := searchHits("ada", "zephyr")
		held := commentsHeld(made["HR-4"], made["HR-5"])
		if n == 5 && held == 2 {
			break
		}
		if time.Now().After(deadline) {
			out, _ := searchHits("ada", "zephyr")
			t.Fatalf("Ada's search for zephyr found %d of 5, and search holds %d of the 2 comments: %s", n, held, out)
		}
	}

	projectParams := map[string]string{"project_uuid": project.String()}
	// listOf is who's task list (this line has no CSV of it). Anything
	// else, a refusal among them, fails here, so it can't pass a leg that a
	// task stays out of it.
	listOf := func(who string) string {
		t.Helper()
		code, out := call(who, projectController.GetProjectTaskList, projectParams, "?getAll=1", nil)
		var answer struct {
			Data json.RawMessage `json:"data"`
		}
		if code != http.StatusOK || json.Unmarshal([]byte(out), &answer) != nil || len(answer.Data) == 0 {
			t.Errorf("%s's task list isn't one (%d):\n%s", who, code, out)
		}
		return out
	}
	for _, who := range names {
		// Each restricted one as its own task.
		for task, seers := range may {
			code, out := call(who, taskController.GetTaskInfo, map[string]string{"task_uuid": made[task]}, "", nil)
			if sees := slices.Contains(seers, who); sees != (code == http.StatusOK && tells(out, task)) {
				t.Errorf("%s as %s (%d), want seen %v:\n%s", task, who, code, sees, out)
			}
		}
		// The board, the task list and search tell nobody else of any (this
		// line has no CSV of the list).
		_, board := call(who, projectController.GetProjectTaskListForKanban, projectParams, "?getAll=1", nil)
		list := listOf(who)
		zephyr, hits := searchHits(who, "zephyr")
		quokka, comments := searchHits(who, "quokka")
		want := 1 // the open one
		for task, seers := range may {
			if slices.Contains(seers, who) {
				want++
				continue
			}
			for surface, out := range map[string]string{"the board": board, "the task list": list, "search": zephyr + quokka} {
				if tells(out, task) {
					t.Errorf("%s tells %s of %s:\n%s", surface, who, task, out)
				}
			}
		}
		if hits != want {
			t.Errorf("%s's search counts %d hits; want %d", who, hits, want)
		}
		// Search finds no imported comment on this line: their entries name no project (v2.76.0 adds it, under the hidden-task filter).
		if comments != 0 {
			t.Errorf("%s's search counts %d comments; want none", who, comments)
		}
		// The comments Jira keeps from some came across for nobody.
		_, hr5 := call(who, taskController.GetTaskInfo, map[string]string{"task_uuid": made["HR-5"]}, "", nil)
		for surface, out := range map[string]string{"HR-5": hr5, "search": quokka, "the board": board} {
			if strings.Contains(out, "salaries of the movers") || strings.Contains(out, "vendor quote") {
				t.Errorf("%s shows %s a comment Jira keeps from some:\n%s", surface, who, out)
			}
		}
		if !strings.Contains(hr5, "desks arrive") {
			t.Errorf("HR-5's open comment didn't come across for %s:\n%s", who, hr5)
		}
		for surface, out := range map[string]string{"the board": board, "the task list": list} {
			if !strings.Contains(strings.ToLower(out), "desk move") {
				t.Errorf("%s leaves the open task out for %s:\n%s", surface, who, out)
			}
		}
	}
	for _, who := range []string{"rae", "al"} {
		if out := listOf(who); !strings.Contains(out, "Zephyr pay bands") {
			t.Errorf("the task list leaves HR-4 out for %s:\n%s", who, out)
		}
	}

	// No notice of them went anywhere: live, pushed or emailed.
	restricted := func(s string) bool {
		if strings.Contains(s, "salaries of the movers") || strings.Contains(s, "vendor quote") {
			return true
		}
		for task := range may {
			if tells(s, task) {
				return true
			}
		}
		return false
	}
	broker.mu.Lock()
	for _, m := range broker.got {
		if restricted(m) {
			t.Errorf("a live message tells of restricted work: %s", m)
		}
	}
	broker.mu.Unlock()
	pushMu.Lock()
	for _, p := range pushes {
		if restricted(p) {
			t.Errorf("a push tells of restricted work: %s", p)
		}
	}
	pushMu.Unlock()
	var emailed int
	if err := env.PG.QueryRow(`SELECT count(*) FROM notification_email_queue`).Scan(&emailed); err != nil || emailed != 0 {
		t.Errorf("%d emails were queued (%v)", emailed, err)
	}

	// Who made each here: one of the people Jira lets see it, or Ada.
	for task, by := range map[string]string{"HR-4": "ada", "HR-6": "sol", "HR-7": "ada", "HR-8": "ada"} {
		var creator uuid.UUID
		if err := env.PG.QueryRow(`SELECT created_by FROM tasks WHERE id = $1`, made[task]).Scan(&creator); err != nil || creator != people[by] {
			t.Errorf("%s was made by %v here (%v); want %s", task, creator, err, by)
		}
	}
	// Al, matched by email, is HR-4's assignee; Ned, matched by his name,
	// isn't HR-7's, and it has none. Read from the field: both are project
	// members, so their ids are elsewhere in the answer too.
	for task, want := range map[string]string{"HR-4": "al", "HR-7": ""} {
		_, out := call("ada", taskController.GetTaskInfo, map[string]string{"task_uuid": made[task]}, "", nil)
		var info struct {
			Data struct {
				Assignee struct {
					UUID string `json:"user_uuid"`
					Name string `json:"user_name"`
				} `json:"task_assignee"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &info); err != nil {
			t.Fatalf("%s's info: %v\n%s", task, err, out)
		}
		wantUUID := ""
		if want != "" {
			wantUUID = people[want].String()
		}
		if a := info.Data.Assignee; a.UUID != wantUUID {
			t.Errorf("%s is assigned to %q (%s); want %q", task, a.Name, a.UUID, want)
		}
	}

	// The report says each came in restricted, why, and who wasn't let in,
	// without its name; its import record keeps them.
	for task, want := range map[string][]string{
		"HR-4": {keptLevel, "cy (who made it)"},
		"HR-6": {keptLevel},
		"HR-7": {keptLevel, "ned (its assignee)", "nia (one Jira lets see it)", "nia (who made it)"},
		"HR-8": {keptLevel, "ben (who made it)"},
	} {
		var msg string
		if err := env.PG.QueryRow(`SELECT message FROM import_errors WHERE import_id = $1 AND source_id = $2 AND code = 'TASK_RESTRICTED'`, job.Id, task).Scan(&msg); err != nil {
			t.Errorf("the report has no line for %s: %v", task, err)
			continue
		}
		for _, w := range want {
			if !strings.Contains(msg, w) {
				t.Errorf("%s's line doesn't say %q: %s", task, w, msg)
			}
		}
		if tells(msg, task) {
			t.Errorf("%s's line names it: %s", task, msg)
		}
	}
	var kept string
	if err := env.PG.QueryRow(`SELECT message FROM import_errors WHERE import_id = $1 AND source_id = 'HR-5' AND code = 'COMMENTS_RESTRICTED'`, job.Id).Scan(&kept); err != nil {
		t.Errorf("the report doesn't count HR-5's comments that stayed in Jira: %v", err)
	}
	for _, w := range []string{"a project role or group", "an internal note", "remain in Jira", "Left there: 2."} {
		if !strings.Contains(kept, w) {
			t.Errorf("HR-5's comments line doesn't say %q: %s", w, kept)
		}
	}
	if restricted(kept) {
		t.Errorf("HR-5's comments line quotes them: %s", kept)
	}
	md, err := importModels.GetIdMapMetadata(ctx, job.Id, importModels.EntityTask, "HR-7")
	if err != nil || !strings.Contains(string(md), `"not_let_in"`) {
		t.Fatalf("HR-7's record: %s (%v)", md, err)
	}
	for _, w := range []string{`{"as":"assignee","source_id":"u-ned"}`, `{"as":"audience","source_id":"u-nia"}`, `{"as":"creator","source_id":"u-nia"}`} {
		if !strings.Contains(strings.ReplaceAll(string(md), " ", ""), w) {
			t.Errorf("HR-7's record doesn't keep %s: %s", w, md)
		}
	}
}
