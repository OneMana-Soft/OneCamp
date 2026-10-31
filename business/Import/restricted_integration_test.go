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
// the board, search and its hit count, the task list's CSV, or a notice. The
// open one shows everywhere, with the comment everyone may read; the two Jira
// keeps from some stay there, for everyone. The report says each task came in
// restricted, why, and who wasn't let in, and how many of HR-5's comments
// stayed in Jira, without a name or a comment's text. HR-4's file and its
// comment's open, and are found, only for HR-4's people; HR-5's for everyone;
// the file of a comment Jira keeps stays in Jira with it. HR-4 shows its file,
// and a file an older import left unlinked from its task in the graph still
// opens only for the task's readers.
// Run: PKGS=./business/Import/ integ_rel.sh <wt> <log> '^TestATaskItsSourceRestrictsComesInRestricted$'

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	projectController "github.com/akashc777/OneCamp/controllers/Project"
	taskController "github.com/akashc777/OneCamp/controllers/Task"
	userController "github.com/akashc777/OneCamp/controllers/User"
	"github.com/akashc777/OneCamp/models/dgexec"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
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
			Status: "To Do", CreatedBy: "u-cy", AssigneeIds: []string{"u-al"}, Restricted: keptLevel, Audience: []string{"u-rae"}, CommentCount: 1,
			AttachmentRefs: keptFile("f-41", "ibis-offer-letters.pdf", "task", "HR-4")},
		importProvider.SourceTask{SourceID: "HR-5", ProjectSourceID: "HR", Name: "Zephyr desk move", Status: "To Do", CreatedBy: "u-ben",
			AttachmentRefs: keptFile("f-51", "ibis-desk-map.txt", "task", "HR-5")},
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
		out <- importProvider.SourceComment{SourceID: "c-41", TaskSourceID: "HR-4", Body: "<p>Quokka bonus pool for the leads</p>", AuthorSourceID: "u-rae",
			AttachmentRefs: keptFile("f-42", "ibis-raise-table.csv", "comment", "c-41")}
	case "HR-5":
		out <- importProvider.SourceComment{SourceID: "c-51", TaskSourceID: "HR-5", Body: "<p>Quokka desks arrive Monday</p>", AuthorSourceID: "u-ben"}
		out <- importProvider.SourceComment{SourceID: "c-52", TaskSourceID: "HR-5", Body: "<p>Quokka salaries of the movers</p>", AuthorSourceID: "u-rae",
			Restricted: "a project role or group", AttachmentRefs: keptFile("f-52", "ibis-mover-salaries.csv", "comment", "c-52")}
		out <- importProvider.SourceComment{SourceID: "c-53", TaskSourceID: "HR-5", Body: "<p>Quokka vendor quote is 40k</p>", AuthorSourceID: "u-rae",
			Restricted: "an internal note"}
	}
	close(out)
	close(errs)
	return out, errs
}

// keptFile is one file Jira holds on an issue or a comment, with the content
// address Jira gives every attachment (FetchAttachment here doesn't read it,
// but a file chunk without one is refused).
func keptFile(id, name, kind, parent string) []importProvider.SourceAttachment {
	return []importProvider.SourceAttachment{{SourceID: id, Name: name, Mime: "text/plain", Size: int64(len(name)),
		URL:    "https://acme.atlassian.net/rest/api/3/attachment/content/" + id,
		Parent: importProvider.SourceRef{Kind: kind, SourceID: parent}}}
}

// FetchAttachment hands over a file's bytes: its name.
func (keptProvider) FetchAttachment(_ context.Context, _ *importModels.Job, _ importProvider.JobOptions, att importProvider.SourceAttachment, dest io.Writer) (string, int64, error) {
	n, err := io.WriteString(dest, att.Name)
	return att.Mime, int64(n), err
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

func TestATaskItsSourceRestrictsComesInRestricted(t *testing.T) {
	ctx := context.Background()
	world := seenWorld(t)
	env, dg := world.env, world.dg

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

	call := func(who string, h http.HandlerFunc, params map[string]string, query string, body any) (int, string) {
		t.Helper()
		return callAs(t, ctx, people[who], h, params, query, body)
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
		return searchAs(t, ctx, people[who], text)
	}
	// Search is written off the import: what came across is in it once Ada,
	// who may see all of it, finds it.
	adaFinds := func(text string, want int) {
		t.Helper()
		for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(250 * time.Millisecond) {
			if _, n := searchHits("ada", text); n == want {
				return
			}
			if time.Now().After(deadline) {
				out, n := searchHits("ada", text)
				t.Fatalf("Ada's search for %s found %d of %d: %s", text, n, want, out)
			}
		}
	}
	adaFinds("zephyr", 5) // the five issues
	adaFinds("quokka", 2) // the two comments that came across

	projectParams := map[string]string{"project_uuid": project.String()}
	// csvOf is who's task-list CSV. Anything else, a refusal among them,
	// fails here, so it can't pass a leg that a task stays out of it.
	csvOf := func(who string) string {
		t.Helper()
		code, out := call(who, projectController.GetProjectTaskList, projectParams, "?format=csv", nil)
		if head, _, _ := strings.Cut(out, "\n"); code != http.StatusOK || !strings.Contains(head, "Key,Title,") {
			t.Errorf("%s's task list CSV isn't one (%d):\n%s", who, code, out)
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
		// The board, the CSV and search tell nobody else of any.
		_, board := call(who, projectController.GetProjectTaskListForKanban, projectParams, "?getAll=1", nil)
		csv := csvOf(who)
		zephyr, hits := searchHits(who, "zephyr")
		quokka, comments := searchHits(who, "quokka")
		want := 1 // the open one
		for task, seers := range may {
			if slices.Contains(seers, who) {
				want++
				continue
			}
			for surface, out := range map[string]string{"the board": board, "the task list's CSV": csv, "search": zephyr + quokka} {
				if tells(out, task) {
					t.Errorf("%s tells %s of %s:\n%s", surface, who, task, out)
				}
			}
		}
		if hits != want {
			t.Errorf("%s's search counts %d hits; want %d", who, hits, want)
		}
		wantComments := 1 // HR-5's open one
		if slices.Contains(may["HR-4"], who) {
			wantComments++ // HR-4's
		}
		if comments != wantComments {
			t.Errorf("%s's search counts %d comments; want %d", who, comments, wantComments)
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
		for surface, out := range map[string]string{"the board": board, "the task list's CSV": csv} {
			if !strings.Contains(strings.ToLower(out), "desk move") {
				t.Errorf("%s leaves the open task out for %s:\n%s", surface, who, out)
			}
		}
	}
	for _, who := range []string{"rae", "al"} {
		if out := csvOf(who); !strings.Contains(out, "Zephyr pay bands") {
			t.Errorf("the CSV leaves HR-4 out for %s:\n%s", who, out)
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
	for _, m := range world.live() {
		if restricted(m) {
			t.Errorf("a live message tells of restricted work: %s", m)
		}
	}
	for _, p := range world.pushed() {
		if restricted(p) {
			t.Errorf("a push tells of restricted work: %s", p)
		}
	}
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

	// The files come across last, as the attachment stage takes them after
	// the issues and their comments: HR-4's and its comment's, the open
	// HR-5's, and not the one on the comment Jira keeps to a role, which
	// stays in Jira with it. HR-4's reach only HR-4's readers, through search
	// and through the file route, which answers anyone else as it answers a
	// file that isn't there; HR-5's reaches everyone. Search finds an imported
	// comment's file for nobody yet, its task's readers included: its entry
	// names no project (PROJ-LEAD-33 took it to main, 25 Oct).
	integration.SetupMinio(t, "import-kept-files")
	rows, err := env.PG.Query(`SELECT id, object_key, channel_slack_id FROM import_chunks WHERE import_id = $1 AND chunk_type = $2`, job.Id, importModels.ChunkAttachment)
	if err != nil {
		t.Fatal(err)
	}
	var stored []*importModels.Chunk
	for rows.Next() {
		c := &importModels.Chunk{ImportId: job.Id, ChunkType: importModels.ChunkAttachment, Status: importModels.ChunkStatusInProgress, MaxAttempts: 3}
		if err := rows.Scan(&c.Id, &c.ObjectKey, &c.ParentSourceId); err != nil {
			t.Fatal(err)
		}
		stored = append(stored, c)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	// The graph refuses HR-4's file's first link: its chunk fails, and runs
	// again.
	linkNow, refused := linkFile, 0
	t.Cleanup(func() { linkFile = linkNow })
	linkFile = func(ctx context.Context, kind, holder string, file *dgraphStruct.DgraphAttachment) error {
		if holder == made["HR-4"] && refused == 0 {
			refused++
			return errors.New("the graph is away")
		}
		return linkNow(ctx, kind, holder, file)
	}
	for _, c := range stored {
		err := processAttachmentChunk(work, keptProvider{}, job, importProvider.JobOptions{}, c, importer)
		if err != nil && strings.Contains(err.Error(), "the graph is away") {
			err = processAttachmentChunk(work, keptProvider{}, job, importProvider.JobOptions{}, c, importer)
		}
		if err != nil {
			t.Fatalf("the file chunk %s: %v", *c.ObjectKey, err)
		}
	}
	if refused != 1 {
		t.Fatalf("HR-4's file's link was refused %d times; want once", refused)
	}
	fileIds, err := importModels.IdMapOf(ctx, job.Id, importModels.EntityFile)
	if err != nil {
		t.Fatal(err)
	}
	file := map[string]string{}
	for src, id := range fileIds {
		file[src[strings.LastIndex(src, ":")+1:]] = id.String()
	}
	if len(file) != 3 || file["f-41"] == "" || file["f-42"] == "" || file["f-51"] == "" {
		t.Fatalf("the files that came across: %v; want HR-4's, its comment's and HR-5's, and not the kept comment's", file)
	}
	// HR-4's file, though its chunk ran twice, was kept once, is one node in
	// the graph, made by Ada's own node, and HR-4 lists it once.
	var hr4Files int
	if err := env.PG.QueryRow(`SELECT COUNT(*) FROM attachments WHERE src_value = $1`, made["HR-4"]).Scan(&hr4Files); err != nil || hr4Files != 1 {
		t.Errorf("HR-4's files kept: %d (%v); want its one file kept once", hr4Files, err)
	}
	linked, err := dgexec.Read[struct {
		Nodes []struct {
			By struct {
				Uid string `json:"uid"`
			} `json:"attachment_created_by"`
		} `json:"nodes"`
		Task []struct {
			Files int `json:"files"`
		} `json:"task"`
	}](ctx, `query Kept($file: string, $task: string) {
		nodes(func: eq(attachment_uuid, $file)) { attachment_created_by { uid } }
		task(func: eq(task_uuid, $task)) { files: count(task_attachments @filter(eq(attachment_uuid, $file))) }
	}`, map[string]string{"$file": file["f-41"], "$task": made["HR-4"]})
	if err != nil || len(linked.Nodes) != 1 || linked.Nodes[0].By.Uid != importer.UserDgraphInfo.Uid || len(linked.Task) != 1 || linked.Task[0].Files != 1 {
		t.Errorf("HR-4's file in the graph: %+v (%v); want one node, made by Ada's (%s), listed once by HR-4", linked, err, importer.UserDgraphInfo.Uid)
	}
	adaFinds("ibis", 2) // HR-4's file and HR-5's
	fileNames := map[string]string{"f-41": "ibis-offer-letters", "f-42": "ibis-raise-table", "f-51": "ibis-desk-map"}
	for _, who := range names {
		reads := slices.Contains(may["HR-4"], who)
		for f, name := range fileNames {
			want := reads || f == "f-51"
			code, out := call(who, userController.GetProjectFile, map[string]string{"project_uuid": project.String(), "obj_uuid": file[f]}, "", nil)
			if want != (code == http.StatusOK) || (!want && code != http.StatusNotFound) {
				t.Errorf("%s asks for %s (%d), want it opened %v:\n%s", who, name, code, want, out)
			}
		}
		out, hits := searchHits(who, "ibis")
		want := 1 // HR-5's
		if reads {
			want = 2
		}
		if hits != want {
			t.Errorf("%s's search for the files counts %d hits; want %d:\n%s", who, hits, want, out)
		}
		for _, kept := range []string{"offer-letters", "raise-table", "mover-salaries"} {
			if (!reads || kept == "mover-salaries") && strings.Contains(out, kept) {
				t.Errorf("search tells %s of %s:\n%s", who, kept, out)
			}
		}
	}
	// The import links a task's file to the task in the graph, as an upload
	// does, so the task shows it to its readers.
	if _, out := call("ada", taskController.GetTaskInfo, map[string]string{"task_uuid": made["HR-4"]}, "", nil); !strings.Contains(out, "ibis-offer-letters") {
		t.Errorf("HR-4 doesn't show Ada its file:\n%s", out)
	}
	// An older import's task file has no such link: the file route goes by
	// the import's row, so the members HR-4 is kept from still get what a
	// file that isn't there gets, and its readers still open it.
	if _, err := dgexec.Mutate(ctx, `query Unlink($task: string, $file: string) {
		task as var(func: eq(task_uuid, $task))
		file as var(func: eq(attachment_uuid, $file))
	}`, map[string]string{"$task": made["HR-4"], "$file": file["f-41"]}, dgexec.Mutation{DelNQuads: "uid(task) <task_attachments> uid(file) ."}); err != nil {
		t.Fatal(err)
	}
	for _, who := range names {
		want := slices.Contains(may["HR-4"], who)
		code, out := call(who, userController.GetProjectFile, map[string]string{"project_uuid": project.String(), "obj_uuid": file["f-41"]}, "", nil)
		if want != (code == http.StatusOK) || (!want && code != http.StatusNotFound) {
			t.Errorf("%s asks for ibis-offer-letters, unlinked from HR-4 (%d), want it opened %v:\n%s", who, code, want, out)
		}
	}
}
