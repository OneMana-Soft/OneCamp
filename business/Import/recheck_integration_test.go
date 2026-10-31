//go:build integration

package business

// An old Jira import, run before Jira's fix, re-checked against Jira
// (recheck.go). The import brought six HR issues in for everyone in the
// project. Jira has them now so:
//   - HR-1 "Zephyr pay bands" is under a security level, reported by Rae,
//     assigned to Pip (who came across by his name alone) and made by Cy;
//   - HR-2 "Zephyr desk move" is open; of its comments, Jira keeps one to a
//     role, and another is a service desk's internal note;
//   - HR-3 "Zephyr audit" is under a level, but Ada kept it here to Ben by
//     hand before the re-check;
//   - HR-4 "Zephyr old plan" is gone from Jira;
//   - HR-5 "Zephyr bonus pool" is under a level, reported by Rae;
//   - HR-6 "Zephyr appraisals" is under a level, assigned to Pip, with one
//     comment Jira keeps to a role and an open one, and was in OneCamp's
//     trash when the re-check ran.
// Another old import of Ada's brought FIN's issue, whose project is archived
// when the re-check runs: FIN-1 "Yarrow budget" is under a level, assigned to
// Pip, with a file, an internal note with a file of its own, and an open
// comment.
// Ada runs the workspace and ran the imports, with her own connection to
// Jira. A third old import of hers, of OPS, reaches another site now.
// The first pass fails to restrict HR-5 and to hide HR-2's internal note, so
// HR's import waits, naming both; the next pass finishes them, and counts
// each comment it hid once. The re-check keeps HR-1, HR-5, HR-6 and FIN-1 to Ada
// and Rae, and Cy, who made them and stays, counted; takes Pip off HR-1, HR-6
// and FIN-1; hides HR-2's two kept comments, HR-6's and FIN-1's from everyone,
// and leaves the open ones; leaves Ada's choice on HR-3
// and HR-4 as it was, listed; and hands OPS to the admins. HR-6 and FIN,
// restored, are as the re-check left them: FIN-1's file is found again, and
// its note's file, like the note, by no one. Ben, in the projects but not let
// in, can't read or find what is kept, nor hear of it. A second pass changes
// nothing. An import after the fix, and one rolled back, are never
// re-checked. Ada keeps OPS's tasks to its admins, then undoes that (her
// routes and their refusals: tests/integration
// import_access_recheck_test.go). She undoes FIN's re-check while FIN is
// archived again: its note stays out of search until FIN's
// restore, which brings FIN-1 back open, with Pip on it and its note found.
// Last, she keeps HR-1 and HR-5 to Rae and Ben by hand, Rae deletes the note
// the re-check hid, Ada puts HR-6 in the trash again and undoes the re-check:
// HR-1 and HR-5 keep her new choice, and Pip stays off HR-1, since its
// assignee would read it past her choice; HR-2's role comment comes back and
// the deleted note doesn't; and HR-6, restored, opens with Pip back and its
// comment found again.
// (This edition keeps no workspace memory, so the AI edition's memory legs are left out.)
// Run: PKGS=./business/Import/ integ_rel.sh <wt> <log> '^TestAnOldJiraImportIsRecheckedAgainstJira$'

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	taskController "github.com/akashc777/OneCamp/controllers/Task"
	restrictionController "github.com/akashc777/OneCamp/controllers/TaskRestriction"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	reviewModel "github.com/akashc777/OneCamp/models/postgres/ImportReview"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// oldJiraImport streams the issues and comments as the released importer
// brought them: nothing kept to anyone.
type oldJiraImport struct{ importProvider.Provider }

func (oldJiraImport) DefaultStatusMap() map[string]string   { return map[string]string{} }
func (oldJiraImport) DefaultPriorityMap() map[string]string { return map[string]string{} }

func (oldJiraImport) IterTasksOfProject(_ context.Context, _ *importModels.Job, _ importProvider.JobOptions, project string) (<-chan importProvider.SourceTask, <-chan error) {
	switch project {
	case "OPS":
		return keptStream(importProvider.SourceTask{SourceID: "OPS-1", ProjectSourceID: "OPS", Name: "Zephyr rota", Status: "To Do", CreatedBy: "u-cy"})
	case "FIN":
		return keptStream(importProvider.SourceTask{SourceID: "FIN-1", ProjectSourceID: "FIN", Name: "Yarrow budget", Status: "To Do",
			CreatedBy: "u-cy", AssigneeIds: []string{"u-pip"}, CommentCount: 2})
	}
	task := func(key, name string, assignees ...string) importProvider.SourceTask {
		return importProvider.SourceTask{SourceID: key, ProjectSourceID: "HR", Name: name, Status: "To Do", CreatedBy: "u-cy", AssigneeIds: assignees}
	}
	hr2, hr6 := task("HR-2", "Zephyr desk move"), task("HR-6", "Zephyr appraisals", "u-pip")
	hr2.CommentCount, hr6.CommentCount = 3, 2
	return keptStream(task("HR-1", "Zephyr pay bands", "u-pip"), hr2, task("HR-3", "Zephyr audit"),
		task("HR-4", "Zephyr old plan"), task("HR-5", "Zephyr bonus pool"), hr6)
}

func (oldJiraImport) IterSubtasksOfTask(context.Context, *importModels.Job, importProvider.JobOptions, string) (<-chan importProvider.SourceTask, <-chan error) {
	return keptStream()
}

func (oldJiraImport) IterCommentsOfTask(_ context.Context, _ *importModels.Job, _ importProvider.JobOptions, task string) (<-chan importProvider.SourceComment, <-chan error) {
	comment := func(id, body, author string) importProvider.SourceComment {
		return importProvider.SourceComment{SourceID: id, TaskSourceID: task, Body: "<p>" + body + "</p>", AuthorSourceID: author}
	}
	of := map[string][]importProvider.SourceComment{
		"HR-2": {comment("c-1", "Quokka desks arrive Monday", "u-ben"), comment("c-2", "Quokka salaries of the movers", "u-rae"),
			comment("c-3", "Quokka vendor quote is 40k", "u-rae")},
		"HR-6":  {comment("c-4", "Wombat appraisal scores", "u-rae"), comment("c-6", "Wombat lunch menu", "u-ben")},
		"FIN-1": {comment("c-5", "Yarrow supplier rates", "u-rae"), comment("c-7", "Yarrow kickoff notes", "u-ben")},
	}
	out, errs := make(chan importProvider.SourceComment, len(of[task])), make(chan error)
	for _, c := range of[task] {
		out <- c
	}
	close(out)
	close(errs)
	return out, errs
}

// jiraNow is Jira as it has the issues now, read with Ada's connection: OPS's
// issue answers as another site's.
type jiraNow struct{ importProvider.Provider }

func (jiraNow) Name() string { return importModels.ProviderJira }

func (jiraNow) Recheck(_ context.Context, _ uuid.UUID, tasks []string, _ map[string][]string) (map[string]importProvider.RecheckedTask, error) {
	level := "an issue security level"
	kept := func(project string, comments map[string]string, assignees ...string) importProvider.RecheckedTask {
		return importProvider.RecheckedTask{Task: importProvider.SourceTask{ProjectSourceID: project, CreatedBy: "u-cy", AssigneeIds: assignees,
			Restricted: level, Audience: []string{"u-rae"}}, KeptComments: comments}
	}
	now := map[string]importProvider.RecheckedTask{
		"HR-1": kept("HR", nil, "u-pip"),
		"HR-2": {Task: importProvider.SourceTask{ProjectSourceID: "HR", CreatedBy: "u-cy"},
			KeptComments: map[string]string{"c-2": "a project role or group", "c-3": "an internal note"}},
		"HR-3":  kept("HR", nil),
		"HR-4":  {Gone: true},
		"HR-5":  kept("HR", nil),
		"HR-6":  kept("HR", map[string]string{"c-4": "a project role or group"}, "u-pip"),
		"FIN-1": kept("FIN", map[string]string{"c-5": "an internal note"}, "u-pip"),
		"OPS-1": {Task: importProvider.SourceTask{ProjectSourceID: "OPS-ELSEWHERE", CreatedBy: "u-cy"}},
	}
	out := map[string]importProvider.RecheckedTask{}
	for _, k := range tasks {
		out[k] = now[k]
	}
	return out, nil
}

func TestAnOldJiraImportIsRecheckedAgainstJira(t *testing.T) {
	ctx := context.Background()
	world := seenWorld(t)
	env, dg := world.env, world.dg

	names := []string{"ada", "ben", "rae", "pip", "cy"}
	people := map[string]uuid.UUID{}
	for _, n := range names {
		people[n] = uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@acme.test', $2, $2, NOW(), NOW())`, people[n], n); err != nil {
			t.Fatal(err)
		}
	}
	ada := people["ada"]
	hr, ops, fin, team := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO admin_users (email_id) VALUES ('ada@acme.test')`, nil},
		{`INSERT INTO teams (id, team_name, created_by) VALUES ($1, 'People', $2)`, []any{team, ada}},
		{`INSERT INTO projects (id, project_name, team_id, created_by) VALUES ($1, 'HR', $2, $3), ($4, 'OPS', $2, $3), ($5, 'FIN', $2, $3)`, []any{hr, team, ada, ops, fin}},
		// Ada's own connection to Jira, which the re-check reads with.
		{`INSERT INTO import_oauth_tokens (provider, owner_user_id, access_token_enc) VALUES ('jira', $1, '\x00')`, []any{ada}},
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
	project := func(key string, id uuid.UUID) map[string]any {
		return map[string]any{"uid": "_:" + key, "dgraph.type": "Project", "project_uuid": id.String(), "project_name": strings.ToUpper(key),
			"project_deleted_at": live, "project_members": in(names...), "project_admins": in("ada"), "project_created_by": map[string]any{"uid": "_:ada"},
			"project_team": map[string]any{"uid": "_:people"}}
	}
	nodes := []map[string]any{project("hr", hr), project("ops", ops), project("fin", fin),
		{"uid": "_:people", "dgraph.type": "Team", "team_uuid": team.String(), "team_name": "People", "team_members": in(names...),
			"team_admins": in("ada"), "team_projects": in("hr", "ops", "fin")}}
	for _, n := range names {
		nodes = append(nodes, map[string]any{"uid": "_:" + n, "dgraph.type": "User", "user_uuid": people[n].String(), "user_name": n,
			"user_deleted_at": live, "user_projects": in("hr", "ops", "fin"), "user_teams": in("people")})
	}
	uids := dg.Mutate(t, nodes)
	importer := &userModels.UserInfo{UserPostgresInfo: userModels.User{Id: ada}}
	importer.UserDgraphInfo.Uid, importer.UserDgraphInfo.Uuid = uids["ada"], ada.String()

	// An import of a project, first run hoursAgo hours ago, ending as status;
	// with tasks, it brings the project's issues and their comments in.
	commented := map[string][]string{"HR": {"HR-2", "HR-6"}, "FIN": {"FIN-1"}}
	importOf := func(key string, id uuid.UUID, hoursAgo int, status string, tasks bool) *importModels.Job {
		t.Helper()
		j := &importModels.Job{Id: uuid.New(), Provider: importModels.ProviderJira, SourceWorkspaceName: "acme", Source: "api",
			Status: importModels.StatusRunning, TriggeredBy: &ada}
		if err := importModels.CreateJob(ctx, j); err != nil {
			t.Fatal(err)
		}
		if _, err := env.PG.Exec(`UPDATE import_jobs SET started_at = NOW() - make_interval(hours => $2), created_at = NOW() - make_interval(hours => $2) WHERE id = $1`, j.Id, hoursAgo); err != nil {
			t.Fatal(err)
		}
		for _, n := range names {
			by := "email"
			if n == "pip" {
				by = "name"
			}
			if err := importModels.UpsertIdMappingWithOwnership(ctx, j.Id, importModels.EntityUser, "u-"+n, people[n], nil,
				mustMarshal(map[string]any{"matched_by": by}), false); err != nil {
				t.Fatal(err)
			}
		}
		if err := importModels.UpsertIdMappingWithOwnership(ctx, j.Id, importModels.EntityProject, key, id, nil, mustMarshal(map[string]any{}), true); err != nil {
			t.Fatal(err)
		}
		work := importContext(ctx)
		stage := func(kind, parent string, run func(*importModels.Chunk) error) {
			t.Helper()
			c := &importModels.Chunk{Id: uuid.New(), ImportId: j.Id, ChunkType: kind, ParentSourceId: &parent, Status: importModels.ChunkStatusInProgress, MaxAttempts: 3}
			if err := importModels.CreateChunks(ctx, []*importModels.Chunk{c}); err != nil {
				t.Fatal(err)
			}
			if err := env.PG.QueryRow(`SELECT id FROM import_chunks WHERE import_id = $1 AND chunk_type = $2 AND channel_slack_id = $3`, j.Id, kind, parent).Scan(&c.Id); err != nil {
				t.Fatal(err)
			}
			if err := run(c); err != nil {
				t.Fatalf("the %s chunk: %v", kind, err)
			}
		}
		if tasks {
			stage(importModels.ChunkProjectTasks, key, func(c *importModels.Chunk) error {
				return processTaskChunk(work, oldJiraImport{}, j, importProvider.JobOptions{}, c, importer)
			})
			for _, task := range commented[key] {
				stage(importModels.ChunkTaskComments, task, func(c *importModels.Chunk) error {
					return processTaskCommentChunk(work, oldJiraImport{}, j, importProvider.JobOptions{}, c, importer)
				})
			}
		}
		if _, err := env.PG.Exec(`UPDATE import_jobs SET status = $2 WHERE id = $1`, j.Id, status); err != nil {
			t.Fatal(err)
		}
		return j
	}
	job := importOf("HR", hr, 3, importModels.StatusCompleted, true)
	opsJob := importOf("OPS", ops, 3, importModels.StatusCompleted, true)
	finJob := importOf("FIN", fin, 3, importModels.StatusCompleted, true)
	made := map[string]uuid.UUID{}
	for _, j := range []*importModels.Job{job, opsJob, finJob} {
		for _, kind := range []string{importModels.EntityTask, importModels.EntityComment} {
			m, err := importModels.IdMapOf(ctx, j.Id, kind)
			if err != nil {
				t.Fatal(err)
			}
			for src, id := range m {
				made[src[strings.LastIndex(src, ":")+1:]] = id
			}
		}
	}
	for _, src := range []string{"HR-1", "HR-2", "HR-3", "HR-4", "HR-5", "HR-6", "OPS-1", "FIN-1", "c-1", "c-2", "c-3", "c-4", "c-5", "c-6", "c-7"} {
		if made[src] == uuid.Nil {
			t.Fatalf("%s wasn't imported: %v", src, made)
		}
	}
	// FIN's import kept a file of FIN-1's and one of its internal note's, as
	// the importer writes them.
	keepFile := func(kind string, holder uuid.UUID, name string) {
		t.Helper()
		at := time.Now()
		p := &attachmentParentInfo{Kind: kind, ParentUUID: holder, TaskUUID: made["FIN-1"], ProjectUUID: fin}
		if err := linkKeptFile(ctx, p, &dgraphStruct.DgraphAttachment{Uuid: uuid.NewString(), FileName: name, ObjectKey: "import/" + name,
			CreatedAt: &at}, importer); err != nil {
			t.Fatalf("FIN's import keeping %s: %v", name, err)
		}
	}
	keepFile("task", made["FIN-1"], "Xylophone ledger.xlsx")
	keepFile("comment", made["c-5"], "Quince price sheet.pdf")

	call := func(who string, h http.HandlerFunc, params map[string]string, body any) (int, string) {
		t.Helper()
		return callAs(t, ctx, people[who], h, params, "", body)
	}
	restrict := func(task string, to ...string) {
		t.Helper()
		var ids []string
		for _, n := range to {
			ids = append(ids, people[n].String())
		}
		if code, out := call("ada", restrictionController.SetRestriction, map[string]string{"task_ref": made[task].String()},
			map[string]any{"restricted": true, "people": ids}); code != http.StatusOK {
			t.Fatalf("Ada keeping %s to %v: %d %s", task, to, code, out)
		}
	}
	// Ada kept HR-3 to Ben by hand before the re-check.
	restrict("HR-3", "ben")

	// Jira's fix runs here now, after the old imports; then an import first
	// runs after it, and an old one is rolled back.
	if err := ClaimImportFixes(ctx); err != nil {
		t.Fatal(err)
	}
	later := importOf("HR", hr, -1, importModels.StatusCompleted, false)
	rolledBack := importOf("OPS", ops, 3, importModels.StatusRolledBack, false)
	if prev := importProvider.Get(importModels.ProviderJira); prev != nil {
		t.Cleanup(func() { importProvider.Register(prev) })
	}
	importProvider.Register(jiraNow{})

	// Search has what the imports wrote once Ada finds it all.
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(30 * time.Second); !ok(); time.Sleep(250 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("waited 30 s for %s", what)
			}
		}
	}
	hits := func(who, text string) int {
		t.Helper()
		_, n := searchAs(t, ctx, people[who], text)
		return n
	}
	waitFor("Ada's search to find what the imports wrote", func() bool {
		return hits("ada", "zephyr") == 7 && hits("ada", "quokka") == 3 && hits("ada", "wombat") == 2 && hits("ada", "yarrow") == 3 &&
			hits("ada", "xylophone") == 1
	})
	// noteEntries is each search entry naming FIN-1's internal note: its own
	// and its file's.
	noteEntries := func() []searchEntry {
		t.Helper()
		entries, err := searchEntries(ctx, made["c-5"].String())
		if err != nil {
			t.Fatal(err)
		}
		return entries
	}
	waitFor("search to hold the note's file", func() bool { return len(noteEntries()) == 2 })

	asAda := &importer.UserDgraphInfo
	graphTask := func(task string) *dgraphStruct.DgraphTask {
		t.Helper()
		d, err := taskDomain.GetDgraphBasicTaskInfoByUUID(recheckContext(ctx), made[task].String(), uids["ada"])
		if err != nil || d == nil {
			t.Fatalf("read %s: %v", task, err)
		}
		return d
	}
	// The released importer put Pip, matched by his name alone, on HR-1, HR-6
	// and FIN-1; today's leaves such a person off (placed.go assigneeOf), so
	// the old imports' assignments are written as they left them.
	for _, task := range []string{"HR-1", "HR-6", "FIN-1"} {
		uid := graphTask(task).Uid
		dg.Mutate(t, map[string]any{"uid": uid, "task_assignee": map[string]any{"uid": uids["pip"], "user_tasks": []map[string]any{{"uid": uid}}}})
	}

	// HR-6 is in the trash when the re-check runs, and FIN is archived.
	trash := func(task string) {
		t.Helper()
		if err := taskBusiness.ArchiveTaskByTaskUUID(ctx, made[task], graphTask(task), asAda); err != nil {
			t.Fatalf("Ada trashing %s: %v", task, err)
		}
	}
	restore := func(task string) {
		t.Helper()
		if err := taskBusiness.UnArchiveTaskByTaskUUID(ctx, made[task], graphTask(task), asAda); err != nil {
			t.Fatalf("Ada restoring %s: %v", task, err)
		}
	}
	finAs := func(archive bool) {
		t.Helper()
		info, err := projectBusiness.GetDgraphProjectInfoAndTeamAdminFlag(ctx, fin.String(), uids["ada"])
		if err != nil || info == nil {
			t.Fatalf("read FIN: %v", err)
		}
		if archive {
			err = projectBusiness.ArchiveProjectByProjectUUID(ctx, fin, info)
		} else {
			err = projectBusiness.UnArchiveProjectUUID(ctx, fin, info)
		}
		if err != nil {
			t.Fatalf("Ada archiving FIN (%v): %v", archive, err)
		}
	}
	trash("HR-6")
	finAs(true)
	waitFor("HR-6 and FIN to leave search", func() bool {
		return hits("ada", "wombat") == 0 && hits("ada", "yarrow") == 0 && hits("ada", "xylophone") == 0
	})

	// The reviews: one of each gap for each project of each old import, an
	// archived one's too.
	if _, _, err := RecheckCandidates.Run(ctx); err != nil {
		t.Fatal(err)
	}
	reviews := func(j *importModels.Job) map[string]string {
		t.Helper()
		revs, err := reviewModel.AccessReviewsOf(ctx, j.Id)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, r := range revs {
			var s struct {
				Reason string `json:"reason"`
			}
			_ = json.Unmarshal(r.Summary, &s)
			out[r.Gap] = strings.TrimSuffix(r.State+" "+s.Reason, " ")
		}
		return out
	}
	both := func(state string) map[string]string {
		return map[string]string{reviewModel.GapJiraSecurity: state, reviewModel.GapJiraComments: state}
	}
	rechecking := both(reviewModel.StateRechecking)
	for name, c := range map[string]struct {
		j    *importModels.Job
		want map[string]string
	}{"HR's import": {job, rechecking}, "OPS's import": {opsJob, rechecking}, "FIN's import, its project archived": {finJob, rechecking},
		"the import after the fix": {later, map[string]string{}}, "the rolled-back import": {rolledBack, map[string]string{}}} {
		if got := reviews(c.j); !maps.Equal(got, c.want) {
			t.Errorf("%s's reviews: %v; want %v", name, got, c.want)
		}
	}
	summary := func(j *importModels.Job, gap string, v any) {
		t.Helper()
		var raw []byte
		if err := env.PG.QueryRow(`SELECT summary FROM import_access_reviews WHERE import_id = $1 AND gap = $2`, j.Id, gap).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, v); err != nil {
			t.Fatal(err)
		}
	}

	// The first pass can't restrict HR-5, nor hide HR-2's internal note: HR's
	// import waits, naming both, and the pass says why.
	heard := len(world.live())
	away := true
	restrictNow, hideNow := setRestriction, hideComment
	t.Cleanup(func() { setRestriction, hideComment = restrictNow, hideNow })
	setRestriction = func(ctx context.Context, task *dgraphStruct.DgraphTask, restrict bool, people []uuid.UUID, who taskBusiness.Changer, at time.Time) (bool, error) {
		if away && task.Uuid == made["HR-5"].String() {
			return false, errors.New("the graph is away")
		}
		return restrictNow(ctx, task, restrict, people, who, at)
	}
	hideComment = func(ctx context.Context, comment uuid.UUID, project string, at time.Time, by string) (bool, error) {
		if away && comment == made["c-3"] {
			return false, errors.New("search is away")
		}
		return hideNow(ctx, comment, project, at, by)
	}
	if rep, err := recheckPending(ctx); err == nil || rep.Waiting != 1 || !strings.Contains(err.Error(), job.Id.String()) ||
		!strings.Contains(err.Error(), "the graph is away") || !strings.Contains(err.Error(), "search is away") {
		t.Fatalf("the first pass: %v (%s); want HR's import waiting, with why", err, rep)
	}
	var counts recheckCounts
	summary(job, reviewModel.GapJiraSecurity, &counts)
	if got := reviews(job); !maps.Equal(got, rechecking) || !slices.Equal(counts.Failed, []uuid.UUID{made["HR-2"], made["HR-5"]}) || counts.Restricted != 2 {
		t.Errorf("HR's import after a pass that failed on HR-2 and HR-5: %v, %+v; want it re-checked again, both named", got, counts)
	}
	// An undo that stops while it puts a comment back (a search or graph step
	// failing after Postgres's, or its server stopping) doesn't record it, so
	// the comment is live with its change standing; an admin may then re-check
	// an import a pass gave up on and they contained. c-2, which the first
	// pass hid, comes back so: the pass that retries HR-2 hides it again where
	// the first did, and counts it once.
	hid, err := reviewModel.AccessChangesOf(ctx, job.Id)
	if err != nil {
		t.Fatal(err)
	}
	c2 := slices.IndexFunc(hid, func(c reviewModel.AccessChange) bool {
		return c.Kind == reviewModel.ChangeCommentHidden && c.Entity == made["c-2"]
	})
	if c2 < 0 {
		t.Fatal("the first pass recorded no change hiding c-2")
	}
	if shown, err := undoChange(ctx, &hid[c2], nil, taskBusiness.Changer{}, asAda); err != nil || !shown {
		t.Fatalf("putting c-2 back as an undo does: %v, %v", shown, err)
	}
	away = false
	if rep, err := recheckPending(ctx); err != nil {
		t.Fatalf("the next pass: %v (%s)", err, rep)
	}
	settledReviews := both(reviewModel.StateRechecked)
	for name, j := range map[string]*importModels.Job{"HR's import": job, "FIN's import": finJob} {
		if got := reviews(j); !maps.Equal(got, settledReviews) {
			t.Errorf("%s after the re-check: %v; want %v", name, got, settledReviews)
		}
	}
	otherSite := both("needs_admin other_source")
	if got := reviews(opsJob); !maps.Equal(got, otherSite) {
		t.Errorf("OPS's import, whose connection reaches another site: %v; want %v", got, otherSite)
	}
	counts = recheckCounts{}
	summary(job, reviewModel.GapJiraSecurity, &counts)
	if counts.Restricted != 3 || counts.Unassigned != 2 || counts.AlreadyRestricted != 1 || counts.CreatorOutside != 3 || counts.GoneOrHidden != 1 ||
		!slices.Equal(counts.GoneOrHiddenTasks, []uuid.UUID{made["HR-4"]}) || len(counts.Failed) != 0 || counts.Retries != 0 ||
		counts.AssigneeOutside != 0 || counts.OpenedSince != 0 {
		t.Errorf("what the re-check counts: %+v", counts)
	}
	var hidden, finCounts, finHidden recheckCounts
	summary(job, reviewModel.GapJiraComments, &hidden)
	summary(finJob, reviewModel.GapJiraSecurity, &finCounts)
	summary(finJob, reviewModel.GapJiraComments, &finHidden)
	if hidden.CommentsHidden != 3 || finHidden.CommentsHidden != 1 || finCounts.Restricted != 1 || finCounts.Unassigned != 1 || finCounts.CreatorOutside != 1 {
		t.Errorf("the comments the re-check counts: %+v and FIN's %+v, %+v", hidden, finCounts, finHidden)
	}

	// Who reads each task now, and its comments.
	sees := func(who, task string) (bool, string) {
		t.Helper()
		code, out := call(who, taskController.GetTaskInfo, map[string]string{"task_uuid": made[task].String()}, nil)
		return code == http.StatusOK && strings.Contains(out, made[task].String()), out
	}
	readers := func(leg string, want map[string][]string) {
		t.Helper()
		for task, seers := range want {
			for _, who := range names {
				if got, out := sees(who, task); got != slices.Contains(seers, who) {
					t.Errorf("%s: %s sees %s %v, want %v:\n%.300s", leg, who, task, got, !got, out)
				}
			}
		}
	}
	everyone := names
	assignee := func(task string) string {
		t.Helper()
		if d := graphTask(task); d.Assignee != nil {
			return d.Assignee.Uuid
		}
		return ""
	}
	// Ada restores HR-6 and FIN as the re-check left them: what it hid stays
	// out of sight, search included.
	restore("HR-6")
	finAs(false)
	waitFor("HR-6 and FIN to come back to search", func() bool {
		return hits("ada", "lunch") == 1 && hits("ada", "kickoff") == 1 && hits("ada", "xylophone") == 1
	})
	for what, text := range map[string]string{"HR-6's kept comment": "scores", "FIN-1's note": "supplier", "FIN-1's note's file": "quince"} {
		for _, who := range []string{"ada", "ben"} {
			if n := hits(who, text); n != 0 {
				t.Errorf("%s finds %s after its restore: %d", who, what, n)
			}
		}
	}
	// The hits can't see a hidden comment's mark: search drops a hit on a
	// comment whose row is deleted, though not one on its files. Every entry
	// naming HR-6's kept comment keeps the re-check's own mark through HR-6's
	// restore, which clears only the trash's.
	if entries, err := searchEntries(ctx, made["c-4"].String()); err != nil || len(entries) == 0 ||
		slices.ContainsFunc(entries, func(e searchEntry) bool { return e.By != hiddenBy }) {
		t.Errorf("HR-6's kept comment's search entries after HR-6's restore: %+v (%v); want each marked by %q", entries, err, hiddenBy)
	}
	// Nor can they see whether the note's file came back, as search reaches
	// an import's comment files through no project (linkKeptFile). FIN's
	// archive took the note's entries out by their ids before the re-check
	// hid the note, so they carry no mark; FIN's restore brings back only
	// what is live, so they stay out.
	if entries := noteEntries(); len(entries) != 2 || slices.ContainsFunc(entries, func(e searchEntry) bool { return !e.out() }) {
		t.Errorf("FIN-1's note's search entries, its file's too, after FIN's restore: %+v; want both out of search", entries)
	}
	readers("after the re-check", map[string][]string{
		"HR-6": {"ada", "rae", "cy"}, "FIN-1": {"ada", "rae", "cy"},
		"HR-1": {"ada", "rae", "cy"}, "HR-5": {"ada", "rae", "cy"}, "HR-3": {"ada", "ben", "cy"},
		"HR-2": everyone, "HR-4": everyone, "OPS-1": everyone,
	})
	for _, task := range []string{"HR-1", "HR-6", "FIN-1"} {
		if got := assignee(task); got != "" {
			t.Errorf("%s's assignee after the re-check: %s; want Pip off it", task, got)
		}
	}
	if _, out := sees("ada", "HR-6"); strings.Contains(out, "appraisal scores") || !strings.Contains(out, "lunch menu") {
		t.Errorf("HR-6's comments after the re-check, for Ada: want its open one alone:\n%.300s", out)
	}
	commentsSeen := func(leg string, want []string) {
		t.Helper()
		for _, who := range names {
			_, out := sees(who, "HR-2")
			for c, text := range map[string]string{"c-1": "desks arrive", "c-2": "salaries of the movers", "c-3": "vendor quote"} {
				if strings.Contains(out, text) != slices.Contains(want, c) {
					t.Errorf("%s: %s sees HR-2's %s %v, want %v", leg, who, c, !slices.Contains(want, c), slices.Contains(want, c))
				}
			}
		}
	}
	commentsSeen("after the re-check", []string{"c-1"})
	waitFor("search to drop what is kept", func() bool {
		return hits("ben", "zephyr") == 4 && hits("ben", "quokka") == 1 && hits("ada", "quokka") == 1 &&
			hits("ben", "yarrow") == 0 && hits("ada", "yarrow") == 2
	})
	for _, msg := range world.live()[heard:] {
		for _, text := range []string{"pay bands", "bonus pool", "salaries of the movers", "vendor quote", "appraisal scores", "yarrow budget", "supplier rates"} {
			if strings.Contains(strings.ToLower(msg), text) {
				t.Errorf("a realtime message tells of what is kept: %.300s", msg)
			}
		}
	}

	// A second pass changes nothing.
	countChanges := func() int {
		t.Helper()
		var n int
		if err := env.PG.QueryRow(`SELECT COUNT(*) FROM import_access_changes`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	changes := countChanges()
	if rep, err := recheckPending(ctx); err != nil || rep.Imports != 0 {
		t.Errorf("a second pass: %s, %v; want nothing to do", rep, err)
	}
	if n := countChanges(); n != changes {
		t.Errorf("a second pass made %d changes", n-changes)
	}

	// What waits for nothing refuses an admin's action.
	if err := UndoRecheck(ctx, later.Id, ada); !errors.Is(err, ErrNotRecheckable) {
		t.Errorf("undoing an import with no review: %v; want ErrNotRecheckable", err)
	}
	if err := ContainImport(ctx, job.Id, ada); !errors.Is(err, ErrNotRecheckable) {
		t.Errorf("containing a re-checked import: %v; want ErrNotRecheckable", err)
	}

	// Ada keeps OPS's tasks to its admins until it is re-checked, then undoes it.
	if err := ContainImport(ctx, opsJob.Id, ada); err != nil {
		t.Fatalf("Ada containing OPS's import: %v", err)
	}
	waitFor("OPS's import to be contained", func() bool { return reviews(opsJob)[reviewModel.GapJiraSecurity] == "contained other_source" })
	readers("contained", map[string][]string{"OPS-1": {"ada", "cy"}})
	if err := UndoRecheck(ctx, opsJob.Id, ada); err != nil {
		t.Fatalf("Ada undoing OPS's containment: %v", err)
	}
	waitFor("OPS's containment to be undone", func() bool { return strings.HasPrefix(reviews(opsJob)[reviewModel.GapJiraSecurity], "undone") })
	readers("its containment undone", map[string][]string{"OPS-1": everyone})

	// Ada archives FIN again and undoes its re-check: FIN-1's note comes back
	// out of sight with FIN, search included, until FIN's restore
	// brings it back with FIN-1, open, Pip on it again.
	finAs(true)
	waitFor("FIN to leave search again", func() bool {
		return hits("ada", "kickoff") == 0
	})
	if err := UndoRecheck(ctx, finJob.Id, ada); err != nil {
		t.Fatalf("Ada undoing FIN's re-check: %v", err)
	}
	waitFor("FIN's re-check to be undone", func() bool { return strings.HasPrefix(reviews(finJob)[reviewModel.GapJiraSecurity], "undone") })
	var undone struct {
		Undone RecheckUndone `json:"undone"`
	}
	summary(finJob, reviewModel.GapJiraSecurity, &undone)
	if want := (RecheckUndone{Opened: 1, Reassigned: 1, Shown: 1}); undone.Undone != want {
		t.Errorf("FIN's undo put back %+v; want %+v", undone.Undone, want)
	}
	if n := hits("ada", "supplier"); n != 0 {
		t.Errorf("FIN-1's note while FIN is archived after the undo: found %d; want out with FIN", n)
	}
	finAs(false)
	waitFor("FIN's restore to bring FIN-1's note back", func() bool {
		return hits("ben", "supplier") == 1
	})
	readers("FIN's re-check undone", map[string][]string{"FIN-1": everyone})
	if got := assignee("FIN-1"); got != people["pip"].String() {
		t.Errorf("FIN-1's assignee after the undo: %q; want Pip back", got)
	}

	// Ada keeps HR-1 and HR-5 to Rae and Ben by hand, Rae deletes the internal
	// note the re-check hid, and Ada puts HR-6 in the trash again; then she
	// undoes the re-check.
	restrict("HR-1", "rae", "ben")
	restrict("HR-5", "rae", "ben")
	if code, out := call("rae", taskController.DeleteTaskComment, nil,
		map[string]any{"task_comment_uuid": made["c-3"].String(), "task_uuid": made["HR-2"].String()}); code != http.StatusOK {
		t.Fatalf("Rae deleting her note: %d %s", code, out)
	}
	trash("HR-6")
	waitFor("HR-6 to leave search again", func() bool { return hits("ada", "lunch") == 0 })
	if err := UndoRecheck(ctx, job.Id, ada); err != nil {
		t.Fatalf("Ada undoing the re-check: %v", err)
	}
	waitFor("the re-check to be undone", func() bool { return strings.HasPrefix(reviews(job)[reviewModel.GapJiraSecurity], "undone") })
	summary(job, reviewModel.GapJiraSecurity, &undone)
	if want := (RecheckUndone{Opened: 1, Reassigned: 1, Shown: 2, LeftAlone: 4}); undone.Undone != want {
		t.Errorf("the undo put back %+v; want %+v", undone.Undone, want)
	}
	// HR-6's comment stays out of search while HR-6 is in the trash, and the
	// deleted note stays gone.
	for what, text := range map[string]string{"HR-6's comment in the trash": "scores", "Rae's deleted note": "vendor"} {
		if n := hits("ada", text); n != 0 {
			t.Errorf("Ada finds %s after the undo: %d", what, n)
		}
	}
	restore("HR-6")
	waitFor("HR-6's restore to bring its comments back", func() bool { return hits("ben", "lunch") == 1 && hits("ben", "scores") == 1 })
	readers("after the undo", map[string][]string{
		"HR-6": everyone, "HR-1": {"ada", "rae", "ben", "cy"}, "HR-5": {"ada", "rae", "ben", "cy"}, "HR-3": {"ada", "ben", "cy"}, "HR-2": everyone,
	})
	if got := assignee("HR-6"); got != people["pip"].String() {
		t.Errorf("HR-6's assignee after the undo: %q; want Pip back", got)
	}
	if got := assignee("HR-1"); got != "" {
		t.Errorf("HR-1's assignee after the undo, restricted by hand since: %q; want Pip kept off", got)
	}
	if _, out := sees("ben", "HR-6"); !strings.Contains(out, "appraisal scores") {
		t.Errorf("HR-6's kept comment after the undo, for Ben: want it back:\n%.300s", out)
	}
	commentsSeen("after the undo", []string{"c-1", "c-2"})
	waitFor("search to have the comments back", func() bool { return hits("ben", "quokka") == 2 })
	if n := hits("ada", "vendor"); n != 0 {
		t.Errorf("Ada finds Rae's deleted note after the undo: %d", n)
	}
}
