//go:build integration

package business

// An old Linear import, run before Linear's fix, re-checked against Linear
// (recheck.go, P-IMP-5D). The import brought two Eng issues in for everyone in
// the project: i-1 "Quasar payroll" belongs to a private team, of whom Rae is
// the member here; i-2 "Quasar roadmap" and i-3 "Quasar hiring plan" to a
// public one, and i-3 has moved to another Linear project since. A moved
// issue still proves the connection reaches the import's own workspace. The
// re-check records one review of Linear's gap for the project, and keeps i-1
// to Rae (its assignee), Cy who made it, and Ada, who ran the import and runs
// the project; Ben, in the project but not the team, no longer reads it. i-2
// and i-3 stay open to everyone.
// Run: PKGS=./business/Import/ integ_rel.sh <wt> <log> '^TestAnOldLinearImportIsRecheckedAgainstLinear$'

import (
	"context"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	taskController "github.com/akashc777/OneCamp/controllers/Task"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	reviewModel "github.com/akashc777/OneCamp/models/postgres/ImportReview"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// oldLinearImport streams Eng's issues as the released importer brought them:
// nothing kept to anyone.
type oldLinearImport struct{ importProvider.Provider }

func (oldLinearImport) DefaultStatusMap() map[string]string   { return map[string]string{} }
func (oldLinearImport) DefaultPriorityMap() map[string]string { return map[string]string{} }

func (oldLinearImport) IterTasksOfProject(context.Context, *importModels.Job, importProvider.JobOptions, string) (<-chan importProvider.SourceTask, <-chan error) {
	return keptStream(
		importProvider.SourceTask{SourceID: "i-1", ProjectSourceID: "p-eng", Name: "Quasar payroll", Status: "Todo", CreatedBy: "u-cy", AssigneeIds: []string{"u-rae"}},
		importProvider.SourceTask{SourceID: "i-2", ProjectSourceID: "p-eng", Name: "Quasar roadmap", Status: "Todo", CreatedBy: "u-cy"},
		importProvider.SourceTask{SourceID: "i-3", ProjectSourceID: "p-eng", Name: "Quasar hiring plan", Status: "Todo", CreatedBy: "u-cy"})
}

func (oldLinearImport) IterSubtasksOfTask(context.Context, *importModels.Job, importProvider.JobOptions, string) (<-chan importProvider.SourceTask, <-chan error) {
	return keptStream()
}

// linearNow is Linear as it has the issues now: i-1's team is private, with
// Rae its member here, and i-3 is in another project.
type linearNow struct{ importProvider.Provider }

func (linearNow) Name() string { return importModels.ProviderLinear }

func (linearNow) Recheck(_ context.Context, _ uuid.UUID, tasks []string, _ map[string][]string) (map[string]importProvider.RecheckedTask, error) {
	now := map[string]importProvider.RecheckedTask{
		"i-1": {Task: importProvider.SourceTask{ProjectSourceID: "p-eng", CreatedBy: "u-cy", AssigneeIds: []string{"u-rae"},
			Restricted: "a private team", Audience: []string{"u-rae"}}},
		"i-2": {Task: importProvider.SourceTask{ProjectSourceID: "p-eng", CreatedBy: "u-cy"}},
		"i-3": {Task: importProvider.SourceTask{ProjectSourceID: "p-ops", CreatedBy: "u-cy"}},
	}
	out := map[string]importProvider.RecheckedTask{}
	for _, k := range tasks {
		out[k] = now[k]
	}
	return out, nil
}

func TestAnOldLinearImportIsRecheckedAgainstLinear(t *testing.T) {
	ctx := context.Background()
	world := seenWorld(t)
	env, dg := world.env, world.dg
	names := []string{"ada", "ben", "rae", "cy"}
	people := map[string]uuid.UUID{}
	for _, n := range names {
		people[n] = uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@acme.test', $2, $2, NOW(), NOW())`, people[n], n); err != nil {
			t.Fatal(err)
		}
	}
	ada := people["ada"]
	eng, team := uuid.New(), uuid.New()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO admin_users (email_id) VALUES ('ada@acme.test')`, nil},
		{`INSERT INTO teams (id, team_name, created_by) VALUES ($1, 'Builders', $2)`, []any{team, ada}},
		{`INSERT INTO projects (id, project_name, team_id, created_by) VALUES ($1, 'Eng', $2, $3)`, []any{eng, team, ada}},
		{`INSERT INTO import_oauth_tokens (provider, owner_user_id, access_token_enc) VALUES ('linear', $1, '\x00')`, []any{ada}},
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
	nodes := []map[string]any{{"uid": "_:eng", "dgraph.type": "Project", "project_uuid": eng.String(), "project_name": "Eng",
		"project_deleted_at": live, "project_members": in(names...), "project_admins": in("ada"), "project_created_by": map[string]any{"uid": "_:ada"},
		"project_team": map[string]any{"uid": "_:builders", "dgraph.type": "Team", "team_uuid": team.String(), "team_name": "Builders",
			"team_members": in(names...), "team_admins": in("ada"), "team_projects": in("eng")}}}
	for _, n := range names {
		nodes = append(nodes, map[string]any{"uid": "_:" + n, "dgraph.type": "User", "user_uuid": people[n].String(), "user_name": n,
			"user_deleted_at": live, "user_projects": in("eng"), "user_teams": in("builders")})
	}
	uids := dg.Mutate(t, nodes)
	importer := &userModels.UserInfo{UserPostgresInfo: userModels.User{Id: ada}}
	importer.UserDgraphInfo.Uid, importer.UserDgraphInfo.Uuid = uids["ada"], ada.String()

	job := &importModels.Job{Id: uuid.New(), Provider: importModels.ProviderLinear, SourceWorkspaceName: "acme", Source: "api",
		Status: importModels.StatusRunning, TriggeredBy: &ada}
	if err := importModels.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if _, err := env.PG.Exec(`UPDATE import_jobs SET started_at = NOW() - interval '3 hours', created_at = NOW() - interval '3 hours' WHERE id = $1`, job.Id); err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if err := importModels.UpsertIdMappingWithOwnership(ctx, job.Id, importModels.EntityUser, "u-"+n, people[n], nil,
			mustMarshal(map[string]any{"matched_by": "email"}), false); err != nil {
			t.Fatal(err)
		}
	}
	if err := importModels.UpsertIdMappingWithOwnership(ctx, job.Id, importModels.EntityProject, "p-eng", eng, nil, mustMarshal(map[string]any{}), true); err != nil {
		t.Fatal(err)
	}
	parent := "p-eng"
	c := &importModels.Chunk{Id: uuid.New(), ImportId: job.Id, ChunkType: importModels.ChunkProjectTasks, ParentSourceId: &parent,
		Status: importModels.ChunkStatusInProgress, MaxAttempts: 3}
	if err := importModels.CreateChunks(ctx, []*importModels.Chunk{c}); err != nil {
		t.Fatal(err)
	}
	if err := env.PG.QueryRow(`SELECT id FROM import_chunks WHERE import_id = $1 AND chunk_type = $2 AND channel_slack_id = $3`, job.Id, c.ChunkType, parent).Scan(&c.Id); err != nil {
		t.Fatal(err)
	}
	if err := processTaskChunk(importContext(ctx), oldLinearImport{}, job, importProvider.JobOptions{}, c, importer); err != nil {
		t.Fatalf("the project's issues: %v", err)
	}
	if _, err := env.PG.Exec(`UPDATE import_jobs SET status = 'completed' WHERE id = $1`, job.Id); err != nil {
		t.Fatal(err)
	}
	made := map[string]uuid.UUID{}
	m, err := importModels.IdMapOf(ctx, job.Id, importModels.EntityTask)
	if err != nil {
		t.Fatal(err)
	}
	for src, id := range m {
		made[src[strings.LastIndex(src, ":")+1:]] = id
	}
	if made["i-1"] == uuid.Nil || made["i-2"] == uuid.Nil || made["i-3"] == uuid.Nil {
		t.Fatalf("the issues weren't imported: %v", made)
	}
	if err := ClaimImportFixes(ctx); err != nil {
		t.Fatal(err)
	}
	if prev := importProvider.Get(importModels.ProviderLinear); prev != nil {
		t.Cleanup(func() { importProvider.Register(prev) })
	}
	importProvider.Register(linearNow{})

	if _, _, err := RecheckCandidates.Run(ctx); err != nil {
		t.Fatal(err)
	}
	states := func() map[string]string {
		t.Helper()
		revs, err := reviewModel.AccessReviewsOf(ctx, job.Id)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, r := range revs {
			out[r.Gap] = r.State
		}
		return out
	}
	if got, want := states(), map[string]string{reviewModel.GapLinearTeams: reviewModel.StateRechecking}; !maps.Equal(got, want) {
		t.Fatalf("the import's reviews: %v; want %v", got, want)
	}
	if rep, err := recheckPending(ctx); err != nil {
		t.Fatalf("the re-check: %v (%s)", err, rep)
	}
	if got, want := states(), map[string]string{reviewModel.GapLinearTeams: reviewModel.StateRechecked}; !maps.Equal(got, want) {
		t.Errorf("the import's reviews after the re-check: %v; want %v", got, want)
	}
	for task, seers := range map[string][]string{"i-1": {"ada", "rae", "cy"}, "i-2": names, "i-3": names} {
		for _, who := range names {
			code, out := callAs(t, ctx, people[who], taskController.GetTaskInfo, map[string]string{"task_uuid": made[task].String()}, "", nil)
			if sees := code == http.StatusOK && strings.Contains(out, made[task].String()); sees != slices.Contains(seers, who) {
				t.Errorf("%s sees %s %v, want %v", who, task, sees, !sees)
			}
		}
	}
}
