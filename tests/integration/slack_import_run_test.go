//go:build integration
// +build integration

package integration_test

// A Slack import starts once. Run read the job's status and then wrote
// "running" over whatever it had become, so two clicks (or a retried request)
// started two runs of the same import: every channel made twice, and Cancel
// stopping only one of them. A run that has failed starts again only once it
// has ended, and so do a retry, planning it again, a rollback and a staged
// file's delete; a run refused for it saves nothing it was sent. A rollback
// takes away all of what the import made, or, when a step fails, none of it.
// And a stage that writes "running" after the admin's cancel no longer carries
// the import on.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestASlackImportStartsOnce|TestARetryWaitsForTheRunToEnd|TestARollbackWaitsForTheRunToEnd|TestARollbackThatFailsTakesNothingAway|TestARefusedRunSavesNothing|TestAPlanAgainWaitsForTheRunToEnd|TestARunLeaseNeverOutlivesItsRun' -v

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	importRun "github.com/akashc777/OneCamp/business/ImportRun"
	slackBusiness "github.com/akashc777/OneCamp/business/SlackImport"
	importController "github.com/akashc777/OneCamp/controllers/Import"
	slackController "github.com/akashc777/OneCamp/controllers/SlackImport"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	slackModels "github.com/akashc777/OneCamp/models/postgres/SlackImport"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestASlackImportStartsOnce(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)

	admin, job, late := uuid.New(), uuid.New(), uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username) VALUES ($1, 'admin@acme.test', 'admin')`, admin); err != nil {
		t.Fatal(err)
	}
	// Planned, and (like any test job) with no export behind it: the one run
	// that starts fails at once, saying so.
	if _, err := env.PG.Exec(`INSERT INTO import_jobs (id, provider, source_workspace_name, source, status, triggered_by)
		VALUES ($1, 'slack', 'Acme', 'export_zip', 'planned', $3), ($2, 'slack', 'Beta', 'export_zip', 'planned', $3)`,
		job, late, admin); err != nil {
		t.Fatal(err)
	}
	asAdmin := context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{
		UserPostgresInfo: userModels.User{Id: admin, EmailID: "admin@acme.test", IsAdmin: true},
	})
	run := func() int {
		rc := chi.NewRouteContext()
		rc.URLParams.Add("jobId", job.String())
		rec := httptest.NewRecorder()
		slackController.HandleRun(rec, httptest.NewRequest(http.MethodPost, "/", nil).WithContext(context.WithValue(asAdmin, chi.RouteCtxKey, rc)))
		return rec.Code
	}

	// The run that starts fails at once, and its goroutine is held as if it
	// were still cleaning up after writing "failed": a click in that window
	// used to start a second run beside it (the burst under load once answered
	// [202 202 409 409 409]).
	letGo := importRun.HoldRunsForTest()
	defer letGo()

	// Four clicks at once (the run limit is five a window).
	codes := make([]int, 4)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			codes[i] = run()
		}(i)
	}
	close(start)
	wg.Wait()
	started := 0
	for _, code := range codes {
		switch code {
		case http.StatusAccepted:
			started++
		case http.StatusConflict:
		default:
			t.Errorf("a click answered %d", code)
		}
	}
	if started != 1 {
		t.Errorf("%d runs started from %v, want one", started, codes)
	}
	waitFor(t, "the run to fail", func() bool {
		j, err := slackModels.GetJob(ctx, job)
		return err == nil && j.Status == slackModels.StatusFailed
	})
	// Failed, but its run not over: a fifth click starts nothing.
	if code := run(); code != http.StatusConflict {
		t.Errorf("a click while the failed run was still ending answered %d, want 409", code)
	}
	// Once it has ended, the failed import can be run again.
	letGo()
	waitFor(t, "the failed import to start again", func() bool {
		ok, err := importRun.Start(ctx, job, []string{slackModels.StatusFailed}, func() {})
		return err == nil && ok
	})

	// A late cancel: the admin cancels a running import, and a stage that was
	// already under way writes "running" afterwards.
	if ok, err := slackModels.StartRunning(ctx, late, []string{slackModels.StatusPlanned, slackModels.StatusFailed}); err != nil || !ok {
		t.Fatalf("start: %v %v", ok, err)
	}
	if err := slackBusiness.CancelImport(ctx, late); err != nil {
		t.Fatal(err)
	}
	stage := "messages"
	if err := slackModels.UpdateStatus(ctx, late, slackModels.StatusRunning, &stage, nil); err != nil {
		t.Fatal(err)
	}
	if j, err := slackModels.GetJob(ctx, late); err != nil || j.Status != slackModels.StatusCancelled {
		t.Errorf("after a late stage write: %+v %v, want cancelled", j, err)
	}
	if ok, err := slackModels.StartRunning(ctx, late, []string{slackModels.StatusPlanned, slackModels.StatusFailed}); err != nil || ok {
		t.Errorf("a cancelled import was started again: %v %v", ok, err)
	}
}

// A retry waits for the run before it to end. Cancel writes "cancelled" at
// once, and the run it stops ends a little later, its workers finishing the
// chunk they hold. A retry in between put the failed chunks back to pending
// under the old run's workers and started a second run beside it.
func TestARetryWaitsForTheRunToEnd(t *testing.T) {
	ctx := context.Background()
	e := newRaceEnv(t, nil)
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	job := e.job(t, "racesource", "Retry", "planned", "staged/retry.json")
	if _, err := e.PG.Exec(`INSERT INTO import_chunks (import_id, chunk_type, status) VALUES ($1, 'projects', 'failed')`, job); err != nil {
		t.Fatal(err)
	}

	// A run, cancelled, still ending.
	ending := make(chan struct{})
	ended := make(chan struct{})
	if ok, err := importRun.Start(ctx, job, []string{importModels.StatusPlanned}, func() {
		defer close(ended)
		<-ending
	}); err != nil || !ok {
		t.Fatalf("start: %v %v", ok, err)
	}
	if _, err := e.PG.Exec(`UPDATE import_jobs SET status = 'cancelled' WHERE id = $1`, job); err != nil {
		t.Fatal(err)
	}

	code, out := e.call(importController.HandleRetryFailedChunks, map[string]string{"jobId": job.String()})
	if code != http.StatusConflict || out["code"] != "run_alive" {
		t.Errorf("a retry while the cancelled run was ending answered %d %v, want 409 run_alive", code, out)
	}
	var pending int
	if err := e.PG.QueryRow(`SELECT count(*) FROM import_chunks WHERE import_id = $1 AND status = 'pending'`, job).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Errorf("%d chunks were put back to pending under the run still ending", pending)
	}
	if status, _ := e.state(t, job); status != importModels.StatusCancelled {
		t.Errorf("the job is %s, want cancelled", status)
	}

	// Once it has ended, the lease is free for the retry.
	close(ending)
	<-ended
	waitFor(t, "the run's lease to be given back", func() bool { return leaseFree(job) })
}

// leaseFree reports whether no run of the job holds its lease, giving the
// lease straight back.
func leaseFree(job uuid.UUID) bool {
	l, err := importRun.Take(job)
	if err != nil {
		return false
	}
	l.Release()
	return true
}

// A rollback, and a staged file's delete, wait for the run before them to end.
// Cancel writes "cancelled" at once, and the run it stops ends a little later,
// its workers finishing the chunk they hold: a rollback in between swept what
// the import had made while they were still adding to it, and what they wrote
// after the sweep survived it.
func TestARollbackWaitsForTheRunToEnd(t *testing.T) {
	ctx := context.Background()
	e := newRaceEnv(t, nil)
	integration.SetupRedis(t)
	integration.StubMqttClient(t)

	// An import of each kind with what it made.
	tasks := e.job(t, "racesource", "Rollback", "planned", "staged/rollback.json")
	tasksMade := importMade(t, e, tasks, "racesource", "Rollback")
	messages := e.job(t, "slack", "Rollback Slack", "planned", "staged/rollback.zip")
	slackMade := slackImportMade(t, e, messages, "Rollback Slack")

	// Both cancelled while they ran, their runs still ending.
	letGo := importRun.HoldRunsForTest()
	defer letGo()
	for _, job := range []uuid.UUID{tasks, messages} {
		if ok, err := importRun.Start(ctx, job, []string{importModels.StatusPlanned}, func() {}); err != nil || !ok {
			t.Fatalf("start: %v %v", ok, err)
		}
		if _, err := e.PG.Exec(`UPDATE import_jobs SET status = 'cancelled' WHERE id = $1`, job); err != nil {
			t.Fatal(err)
		}
	}

	type imported struct {
		name             string
		job              uuid.UUID
		made             map[string]uuid.UUID // what it made, by table
		rollback, delete http.HandlerFunc
	}
	imports := []imported{
		{"a Jira-like import", tasks, tasksMade, importController.HandleRollback, importController.HandleDeleteStagedZip},
		{"a Slack import", messages, slackMade, slackController.HandleRollback, slackController.HandleDeleteStagedZip},
	}
	state := func(c imported) (string, bool, []string) { return e.rollbackState(t, c.job, c.made) }

	for _, c := range imports {
		params := map[string]string{"jobId": c.job.String()}
		if code, out := e.call(c.rollback, params); code != http.StatusConflict || out["code"] != "run_alive" {
			t.Errorf("%s: a rollback while the cancelled run was ending answered %d %v, want 409 run_alive", c.name, code, out)
		}
		if code, out := e.call(c.delete, params); code != http.StatusConflict || out["code"] != "run_alive" {
			t.Errorf("%s: deleting its file while the cancelled run was ending answered %d %v, want 409 run_alive", c.name, code, out)
		}
		if status, staged, kept := state(c); status != importModels.StatusCancelled || !staged || len(kept) != len(c.made) {
			t.Errorf("%s: left %s, its file staged %v and what it made kept in %v; want cancelled with all of it", c.name, status, staged, kept)
		}
	}

	// Once the runs have ended, both go ahead.
	letGo()
	for _, c := range imports {
		waitFor(t, "the run's lease to be given back", func() bool { return leaseFree(c.job) })
		params := map[string]string{"jobId": c.job.String()}
		if code, out := e.call(c.delete, params); code != http.StatusOK {
			t.Errorf("%s: deleting its file once the run had ended answered %d %v", c.name, code, out)
		}
		if _, staged, _ := state(c); staged {
			t.Errorf("%s: its file is still staged after the delete", c.name)
		}
		if code, out := e.call(c.rollback, params); code != http.StatusOK {
			t.Errorf("%s: a rollback once the run had ended answered %d %v", c.name, code, out)
		}
		if status, _, kept := state(c); status != importModels.StatusRolledBack || len(kept) > 0 {
			t.Errorf("%s: rolled back, it is %s with what it made kept in %v; want rolled_back with none of it", c.name, status, kept)
		}
	}
}

// A rollback that fails takes nothing away. It used to stop at the step that
// failed: what it had taken away by then was gone, and the import showed as
// before with the rest of it still there. Here the database refuses a step
// that comes after the import's tasks or messages, and its files.
func TestARollbackThatFailsTakesNothingAway(t *testing.T) {
	e := newRaceEnv(t, nil)
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	if _, err := e.PG.Exec(`CREATE FUNCTION refuse() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'the disk is full'; END $$`); err != nil {
		t.Fatal(err)
	}
	tasks := e.job(t, "racesource", "Halfway", "cancelled", "staged/halfway.json")
	messages := e.job(t, "slack", "Halfway Slack", "cancelled", "staged/halfway.zip")

	for _, c := range []struct {
		name     string
		job      uuid.UUID
		made     map[string]uuid.UUID
		rollback http.HandlerFunc
		refused  string // the table whose step the database refuses
	}{
		{"a Jira-like import", tasks, importMade(t, e, tasks, "racesource", "Halfway"), importController.HandleRollback, "teams"},
		{"a Slack import", messages, slackImportMade(t, e, messages, "Halfway Slack"), slackController.HandleRollback, "channels"},
	} {
		if _, err := e.PG.Exec(`CREATE TRIGGER refuse BEFORE UPDATE ON ` + c.refused + ` FOR EACH ROW EXECUTE FUNCTION refuse()`); err != nil {
			t.Fatal(err)
		}
		params := map[string]string{"jobId": c.job.String()}
		code, out := e.call(c.rollback, params)
		if msg, _ := out["error"].(string); code != http.StatusBadRequest || !strings.Contains(msg, "took nothing away") {
			t.Errorf("%s: a rollback the database refused half way answered %d %v, want 400 saying it took nothing away", c.name, code, out)
		}
		if status, staged, kept := e.rollbackState(t, c.job, c.made); status != importModels.StatusCancelled || !staged || len(kept) != len(c.made) {
			t.Errorf("%s: a rollback that failed left it %s, its file staged %v and what it made kept in %v; want cancelled with all of it",
				c.name, status, staged, kept)
		}

		// Once the database takes it, the rollback goes through.
		if _, err := e.PG.Exec(`DROP TRIGGER refuse ON ` + c.refused); err != nil {
			t.Fatal(err)
		}
		if code, out := e.call(c.rollback, params); code != http.StatusOK {
			t.Errorf("%s: the rollback, tried again, answered %d %v", c.name, code, out)
		}
		if status, staged, kept := e.rollbackState(t, c.job, c.made); status != importModels.StatusRolledBack || staged || len(kept) > 0 {
			t.Errorf("%s: rolled back, it is %s, its file staged %v and what it made kept in %v; want rolled_back with none of it",
				c.name, status, staged, kept)
		}
	}
}

// slackImportMade records, as the Slack importer records them, what a Slack
// import made: a channel, an external person, her message in it and its
// file. It returns them by table.
func slackImportMade(t *testing.T, e *raceEnv, job uuid.UUID, workspace string) map[string]uuid.UUID {
	t.Helper()
	ctx := context.Background()
	channel, ana, post, file := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	tag := job.String()[:8] // names are unique
	for _, s := range []struct {
		stmt string
		args []any
	}{
		{`INSERT INTO users (id, email_id, username, is_external) VALUES ($1, $2, $3, true)`,
			[]any{ana, "slack-import-" + tag + "-u1@no-reply.local", "slack-ana-" + tag}},
		{`INSERT INTO channels (id, ch_name, ch_private, created_by) VALUES ($1, $2, false, $3)`, []any{channel, "imported-" + tag, e.admin}},
		{`INSERT INTO posts (id, post_channel, created_by) VALUES ($1, $2, $3)`, []any{post, channel, ana}},
		{`INSERT INTO attachments (id, obj_key, src_value, src_key, created_by) VALUES ($1, $2, $3, 'channel', $4)`,
			[]any{file, "slackImport/" + tag + "/plan.pdf", channel.String(), e.admin}},
	} {
		if _, err := e.PG.Exec(s.stmt, s.args...); err != nil {
			t.Fatal(err)
		}
	}
	// The file worker records its file with the chunk it finishes, owned.
	if err := slackModels.UpsertIdMapping(ctx, job, slackModels.EntityFile, "F1", file, nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct {
		kind, source string
		id           uuid.UUID
	}{{slackModels.EntityUser, "U1", ana}, {slackModels.EntityChannel, "C1", channel}, {slackModels.EntityMessage, "1700000000.000100", post}} {
		if err := slackModels.UpsertIdMappingWithOwnership(ctx, job, m.kind, m.source, m.id, nil, nil, true); err != nil {
			t.Fatal(err)
		}
		if err := slackModels.UpsertWorkspaceMapping(ctx, workspace, m.kind, m.source, m.id, job); err != nil {
			t.Fatal(err)
		}
	}
	return map[string]uuid.UUID{"posts": post, "channels": channel, "users": ana, "attachments": file}
}

// importMade records, as the task importers record them, what an import made:
// a team, its project with a custom field, a task with a value of that field,
// its subtask, a comment and a file on it, and the external person who wrote
// them. It returns them by table ("table:what" where a table has two).
func importMade(t *testing.T, e *raceEnv, job uuid.UUID, provider, workspace string) map[string]uuid.UUID {
	t.Helper()
	ctx := context.Background()
	ravi, team, project, task, subtask := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	comment, file, field := uuid.New(), uuid.New(), uuid.New()
	tag := job.String()[:8] // names are unique
	for _, s := range []struct {
		stmt string
		args []any
	}{
		{`INSERT INTO users (id, email_id, username, is_external) VALUES ($1, $2, $3, true)`,
			[]any{ravi, "ravi-" + tag + "@tracker.example", "ravi-" + tag}},
		{`INSERT INTO teams (id, team_name, created_by) VALUES ($1, $2, $3)`, []any{team, "Imported " + tag, e.admin}},
		{`INSERT INTO projects (id, project_name, team_id, created_by) VALUES ($1, $2, $3, $4)`, []any{project, "Imported " + tag, team, e.admin}},
		{`INSERT INTO tasks (id, project_id, created_by) VALUES ($1, $3, $4), ($2, $3, $4)`, []any{task, subtask, project, ravi}},
		{`INSERT INTO comments (id, created_by) VALUES ($1, $2)`, []any{comment, ravi}},
		{`INSERT INTO attachments (id, obj_key, src_value, src_key, created_by) VALUES ($1, $2, $3, 'task', $4)`,
			[]any{file, "imports/" + tag + "/spec.pdf", task.String(), e.admin}},
		{`INSERT INTO task_fields (id, project_id, name, type) VALUES ($1, $2, 'Estimate', 'number')`, []any{field, project}},
		{`INSERT INTO task_field_values (task_uuid, field_id, value) VALUES ($1, $2, '3')`, []any{task, field}},
	} {
		if _, err := e.PG.Exec(s.stmt, s.args...); err != nil {
			t.Fatal(err)
		}
	}
	// The attachment worker records its file with the chunk it finishes, owned.
	if err := importModels.UpsertIdMapping(ctx, job, importModels.EntityFile, "A-1", file, nil, nil); err != nil {
		t.Fatal(err)
	}
	parent := "K-1"
	for _, m := range []struct {
		kind, source string
		id           uuid.UUID
		parent       *string
	}{
		{importModels.EntityUser, "U-1", ravi, nil}, {importModels.EntityTeam, "T-1", team, nil},
		{importModels.EntityProject, "P-1", project, nil}, {importModels.EntityTask, "K-1", task, nil},
		{importModels.EntitySubtask, "K-2", subtask, &parent}, {importModels.EntityComment, "C-1", comment, &parent},
		{importModels.EntityField, "F-1", field, nil},
	} {
		if err := importModels.UpsertIdMappingWithOwnership(ctx, job, m.kind, m.source, m.id, m.parent, nil, true); err != nil {
			t.Fatal(err)
		}
		if err := importModels.UpsertWorkspaceMapping(ctx, provider, workspace, m.kind, m.source, m.id, job); err != nil {
			t.Fatal(err)
		}
	}
	return map[string]uuid.UUID{"users": ravi, "teams": team, "projects": project, "tasks": task, "tasks:subtask": subtask,
		"comments": comment, "attachments": file, "task_fields": field}
}

// rollbackState is an import's status, whether its file is still staged, and
// what it made (by table, as importMade and slackImportMade return it) that
// is still there.
func (e *raceEnv) rollbackState(t *testing.T, job uuid.UUID, made map[string]uuid.UUID) (status string, staged bool, kept []string) {
	t.Helper()
	if err := e.PG.QueryRow(`SELECT status, raw_object_key IS NOT NULL FROM import_jobs WHERE id = $1`, job).Scan(&status, &staged); err != nil {
		t.Fatal(err)
	}
	for key, id := range made {
		table, _, _ := strings.Cut(key, ":")
		there := "deleted_at IS NULL"
		if table == "task_fields" {
			there = "TRUE" // a field is deleted, not marked
		}
		var n int
		if err := e.PG.QueryRow(`SELECT count(*) FROM `+table+` WHERE id = $1 AND `+there, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			kept = append(kept, key)
		}
	}
	slices.Sort(kept)
	return status, staged, kept
}

// A run refused because the last one is still ending leaves the import as it
// was. The options and mappings sent with it were saved before the lease was
// checked, so a refused run had overwritten them all the same.
func TestARefusedRunSavesNothing(t *testing.T) {
	ctx := context.Background()
	e := newRaceEnv(t, nil)
	integration.SetupRedis(t)
	integration.StubMqttClient(t)

	// A Slack import through each Run (one with no export behind it fails at
	// once), each failed with options of its own, its run still ending.
	letGo := importRun.HoldRunsForTest()
	defer letGo()
	imports := []struct {
		name string
		job  uuid.UUID
		run  http.HandlerFunc
	}{
		{"the imports' Run", e.job(t, "slack", "Options", "planned", "staged/options.zip"), importController.HandleRun},
		{"the Slack import's Run", e.job(t, "slack", "Options Slack", "planned", "staged/options-slack.zip"), slackController.HandleRun},
	}
	for _, c := range imports {
		if _, err := e.PG.Exec(`UPDATE import_jobs SET options = '{"channel_prefix": "old-"}' WHERE id = $1`, c.job); err != nil {
			t.Fatal(err)
		}
		if ok, err := importRun.Start(ctx, c.job, []string{importModels.StatusPlanned}, func() {}); err != nil || !ok {
			t.Fatalf("start: %v %v", ok, err)
		}
		if _, err := e.PG.Exec(`UPDATE import_jobs SET status = 'failed' WHERE id = $1`, c.job); err != nil {
			t.Fatal(err)
		}
	}
	const body = `{"options": {"channel_prefix": "new-"}, "status_mappings": {"to do": "done"}}`
	// saved is the channel prefix in the import's options, and how many status
	// mappings it has.
	saved := func(job uuid.UUID) (prefix string, mappings int) {
		t.Helper()
		if err := e.PG.QueryRow(`SELECT options->>'channel_prefix', (SELECT count(*) FROM import_status_mappings WHERE import_id = $1)
			FROM import_jobs WHERE id = $1`, job).Scan(&prefix, &mappings); err != nil {
			t.Fatal(err)
		}
		return prefix, mappings
	}

	for _, c := range imports {
		if code, out := e.send(c.run, map[string]string{"jobId": c.job.String()}, body); code != http.StatusConflict || out["code"] != "run_alive" {
			t.Errorf("%s while the failed run was ending answered %d %v, want 409 run_alive", c.name, code, out)
		}
		if prefix, mappings := saved(c.job); prefix != "old-" || mappings != 0 {
			t.Errorf("%s, refused, saved what it was sent: prefix %q and %d status mappings, want \"old-\" and none", c.name, prefix, mappings)
		}
	}

	// Once the runs have ended, a run saves what it was sent.
	letGo()
	for _, c := range imports {
		waitFor(t, "the run's lease to be given back", func() bool { return leaseFree(c.job) })
		if code, out := e.send(c.run, map[string]string{"jobId": c.job.String()}, body); code != http.StatusAccepted {
			t.Errorf("%s once the failed run had ended answered %d %v, want 202", c.name, code, out)
		}
		if prefix, _ := saved(c.job); prefix != "new-" {
			t.Errorf("%s, started, left the prefix %q, want \"new-\"", c.name, prefix)
		}
		waitFor(t, "the run, with no export behind it, to end", func() bool { return leaseFree(c.job) })
	}
}

// A failed import planned again waits for its run to end. Planning it moved it
// back to waiting and wrote the options and chunks it was sent while the run
// that failed was still ending, its workers finishing the chunk they hold.
func TestAPlanAgainWaitsForTheRunToEnd(t *testing.T) {
	ctx := context.Background()
	e := newRaceEnv(t, map[string][]byte{"raw.zip": slackExport(t)})
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	const key = "slackImport/x/raw.zip"

	// An import of each kind, failed with options of its own and a chunk of
	// its run, the run still ending.
	letGo := importRun.HoldRunsForTest()
	defer letGo()
	imports := []struct {
		name string
		job  uuid.UUID
		plan http.HandlerFunc
	}{
		{"a Jira-like import", e.job(t, "racesource", "Again", "planned", key), importController.HandlePlan},
		{"a Slack import", e.job(t, "slack", "Again Slack", "planned", key), slackController.HandlePlan},
	}
	for _, c := range imports {
		for _, stmt := range []string{
			`UPDATE import_jobs SET options = '{"channel_prefix": "old-"}' WHERE id = $1`,
			`INSERT INTO import_chunks (import_id, chunk_type, status) VALUES ($1, 'messages', 'failed')`,
		} {
			if _, err := e.PG.Exec(stmt, c.job); err != nil {
				t.Fatal(err)
			}
		}
		if ok, err := importRun.Start(ctx, c.job, []string{importModels.StatusPlanned}, func() {}); err != nil || !ok {
			t.Fatalf("start: %v %v", ok, err)
		}
		if _, err := e.PG.Exec(`UPDATE import_jobs SET status = 'failed' WHERE id = $1`, c.job); err != nil {
			t.Fatal(err)
		}
	}
	// state is the import's status, the channel prefix in its options and how
	// many chunks it has.
	state := func(job uuid.UUID) (status, prefix string, chunks int) {
		t.Helper()
		if err := e.PG.QueryRow(`SELECT status, COALESCE(options->>'channel_prefix', ''), (SELECT count(*) FROM import_chunks WHERE import_id = $1)
			FROM import_jobs WHERE id = $1`, job).Scan(&status, &prefix, &chunks); err != nil {
			t.Fatal(err)
		}
		return status, prefix, chunks
	}
	const body = `{"options": {"channel_prefix": "new-"}}`

	for _, c := range imports {
		if code, out := e.send(c.plan, map[string]string{"jobId": c.job.String()}, body); code != http.StatusConflict || out["code"] != "run_alive" {
			t.Errorf("%s: planned again while the failed run was ending, answered %d %v, want 409 run_alive", c.name, code, out)
		}
		if status, prefix, chunks := state(c.job); status != importModels.StatusFailed || prefix != "old-" || chunks != 1 {
			t.Errorf("%s: a refused plan left it %s with prefix %q and %d chunks; want failed, \"old-\" and its run's one", c.name, status, prefix, chunks)
		}
	}

	// Once the runs have ended, each is planned again.
	letGo()
	for _, c := range imports {
		waitFor(t, "the run's lease to be given back", func() bool { return leaseFree(c.job) })
		if code, out := e.send(c.plan, map[string]string{"jobId": c.job.String()}, body); code != http.StatusOK {
			t.Errorf("%s: planned again once the run had ended, answered %d %v", c.name, code, out)
		}
		if status, prefix, chunks := state(c.job); status != importModels.StatusPlanned || prefix != "new-" || chunks < 2 {
			t.Errorf("%s: planned again, it is %s with prefix %q and %d chunks; want planned, \"new-\" and the plan's chunks beside its run's",
				c.name, status, prefix, chunks)
		}
	}
}

// A run's lease never outlives it. A run that panics gives it back, and the
// job it left running is failed, so it can be run again (the panic used to end
// the server). A job that does not start gives it back at once, and so does a
// start that errs. And the Release deferred where a lease was taken leaves a
// started run's lease alone.
func TestARunLeaseNeverOutlivesItsRun(t *testing.T) {
	ctx := context.Background()
	e := newRaceEnv(t, nil)
	integration.SetupRedis(t)
	integration.StubMqttClient(t)

	// A run that panics.
	job := e.job(t, "racesource", "Panics", "planned", "staged/panics.json")
	if ok, err := importRun.Start(ctx, job, []string{importModels.StatusPlanned}, func() { panic("boom") }); err != nil || !ok {
		t.Fatalf("start: %v %v", ok, err)
	}
	waitFor(t, "the panicked run's lease to be given back", func() bool { return leaseFree(job) })
	var status, msg string
	if err := e.PG.QueryRow(`SELECT status, COALESCE(error_message, '') FROM import_jobs WHERE id = $1`, job).Scan(&status, &msg); err != nil {
		t.Fatal(err)
	}
	if status != importModels.StatusFailed || msg != "panic: boom" {
		t.Errorf("after the panic the job is %s (%q), want failed (\"panic: boom\")", status, msg)
	}
	if ok, err := importRun.Start(ctx, job, []string{importModels.StatusFailed}, func() {}); err != nil || !ok {
		t.Errorf("the panicked import could not be run again: %v %v", ok, err)
	}

	// A job in none of the statuses it starts from.
	done := e.job(t, "racesource", "Done", "completed", "staged/done.json")
	if ok, err := importRun.Start(ctx, done, []string{importModels.StatusPlanned}, func() { t.Error("a completed import ran") }); err != nil || ok {
		t.Errorf("a completed import started: %v %v", ok, err)
	}
	if !leaseFree(done) {
		t.Error("a job that did not start kept its lease")
	}

	// A start that errs (its request gone).
	gone, cancel := context.WithCancel(ctx)
	cancel()
	errs := e.job(t, "racesource", "Errs", "planned", "staged/errs.json")
	if ok, err := importRun.Start(gone, errs, []string{importModels.StatusPlanned}, func() { t.Error("an import ran after its start failed") }); err == nil || ok {
		t.Errorf("a start without its request: %v %v, want an error", ok, err)
	}
	if !leaseFree(errs) {
		t.Error("a start that erred kept its lease")
	}

	// A started run keeps its lease past the Release deferred where it was
	// taken, and gives it back when it ends.
	held := e.job(t, "racesource", "Held", "planned", "staged/held.json")
	ending := make(chan struct{})
	l, err := importRun.Take(held)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := l.Start(ctx, []string{importModels.StatusPlanned}, func() { <-ending }); err != nil || !ok {
		t.Fatalf("start: %v %v", ok, err)
	}
	l.Release()
	if leaseFree(held) {
		t.Error("a Release after the run started gave its lease back")
	}
	if ok, err := l.Start(ctx, []string{importModels.StatusRunning}, func() {}); ok || !errors.Is(err, importRun.ErrRunAlive) {
		t.Errorf("one lease started a second run: %v %v", ok, err)
	}
	close(ending)
	waitFor(t, "the ended run's lease to be given back", func() bool { return leaseFree(held) })
}
