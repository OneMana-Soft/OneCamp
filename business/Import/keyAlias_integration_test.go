//go:build integration

package business

// O-L2 (design/linear/ANALYSIS.md 7.1, option A): an imported task keeps the
// key it had in its source as an alias when its project's key differs.
// Linear's keys belong to teams, so ENG-7 lands in a project keyed MOB, and
// ENG-7 still finds it in a link, a search or the API. Keys it had before in
// the source find it too. A project can't later take ENG from under it, a key
// a project here uses stays that project's, and rolling the import back
// frees its keys with its tasks.
// Run: PKGS=./business/Import/ integ_rel.sh <wt> <log> 'TestAnImportedTaskAnswersToItsSourceKey'

import (
	"context"
	"errors"
	"testing"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	taskKeys "github.com/akashc777/OneCamp/business/TaskKeys"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	taskKeyModel "github.com/akashc777/OneCamp/models/postgres/TaskKey"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestAnImportedTaskAnswersToItsSourceKey(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// A rollback takes an import's tasks off the graph as whoever ran it.
	by, mobile, ops := uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO users (id, email_id, created_at, updated_at) VALUES ($1, 'alias@example.com', NOW(), NOW())`, by)
	integration.SetupDgraph(t).Mutate(t, []map[string]any{{"uid": "_:by", "dgraph.type": "User", "user_uuid": by.String(), "user_name": "Ana"}})
	newJob := func() *importModels.Job {
		t.Helper()
		j := &importModels.Job{Id: uuid.New(), Provider: "linear", SourceWorkspaceName: "w", Source: "api", Status: importModels.StatusCompleted, TriggeredBy: &by}
		if err := importModels.CreateJob(ctx, j); err != nil {
			t.Fatal(err)
		}
		return j
	}
	job := newJob()
	exec(`INSERT INTO projects (id, project_name, created_by) VALUES ($1, 'Mobile app', $3), ($2, 'Ops', $3)`, mobile, ops, by)
	for p, key := range map[uuid.UUID]string{mobile: "MOB", ops: "OPS"} {
		if _, err := taskKeys.Ensure(ctx, p, ""); err != nil {
			t.Fatal(err)
		}
		if _, _, err := taskKeys.SetKey(ctx, p, key, by); err != nil {
			t.Fatal(err)
		}
	}
	task := func(project uuid.UUID, n int) uuid.UUID {
		t.Helper()
		id := uuid.New()
		if _, err := taskKeyModel.InsertTaskNumbered(ctx, id, project, by, time.Now(), n, nil); err != nil {
			t.Fatal(err)
		}
		return id
	}
	// imported records a task as one an import made, as the task stage does,
	// so rolling that import back puts it in the trash.
	imported := func(j *importModels.Job, src string, id uuid.UUID) uuid.UUID {
		t.Helper()
		if err := importModels.RecordMapping(ctx, j.Id, j.Provider, j.SourceWorkspaceName, importModels.EntityTask, src, id, nil, nil, true); err != nil {
			t.Fatal(err)
		}
		return id
	}
	finds := func(raw string, want uuid.UUID) {
		t.Helper()
		got, err := taskKeys.Resolve(ctx, raw)
		if want == uuid.Nil {
			if !errors.Is(err, taskKeys.ErrNoTask) {
				t.Errorf("%s finds %s (%v); want nothing", raw, got, err)
			}
			return
		}
		if err != nil || got != want {
			t.Errorf("%s finds %s (%v); want %s", raw, got, err, want)
		}
	}
	notKept := func(j *importModels.Job) int {
		return count(`SELECT count(*) FROM import_errors WHERE import_id = $1 AND code = 'KEY_ALIAS_NOT_KEPT'`, j.Id)
	}

	// ENG-7, once DES-2 before it moved teams, landed in Mobile as MOB-7.
	t7 := imported(job, "i7", task(mobile, 7))
	keepSourceAliases(ctx, job, &importProvider.SourceTask{SourceID: "i7", SourceKey: "ENG-7", FormerKeys: []string{"DES-2"}}, t7)
	for _, raw := range []string{"ENG-7", "eng-7", "DES-2", "MOB-7"} {
		finds(raw, t7)
	}

	// A task whose own key is the one it had keeps no alias.
	t3 := imported(job, "i3", task(mobile, 3))
	keepSourceAliases(ctx, job, &importProvider.SourceTask{SourceID: "i3", SourceKey: "MOB-3"}, t3)
	if n := count(`SELECT count(*) FROM task_key_aliases WHERE task_id = $1`, t3); n != 0 {
		t.Errorf("a task keyed as it was has %d aliases; want none", n)
	}

	// A key a project here uses stays that project's, and the import says so.
	tOps := task(ops, 1)
	t9 := imported(job, "i9", task(mobile, 9))
	keepSourceAliases(ctx, job, &importProvider.SourceTask{SourceID: "i9", SourceKey: "OPS-1"}, t9)
	finds("OPS-1", tOps)
	if n := notKept(job); n != 1 {
		t.Errorf("a project's key as an alias said %d times; want once", n)
	}

	// A project can't take ENG while a task elsewhere answers to it, and a
	// key made from a name passes it by.
	if _, _, err := taskKeys.SetKey(ctx, ops, "ENG", by); !errors.Is(err, taskKeys.ErrKeyTaken) {
		t.Errorf("a project took ENG from under ENG-7: %v", err)
	}
	if busy, err := taskKeyModel.Taken(ctx, ops, []string{"ENG", "DES", "ZZZ"}); err != nil || !busy["ENG"] || !busy["DES"] || busy["ZZZ"] {
		t.Errorf("taken keys %v (%v); want ENG and DES", busy, err)
	}

	// Another import's ENG-7 leaves the first's, and says so.
	again := newJob()
	keepSourceAliases(ctx, again, &importProvider.SourceTask{SourceID: "j7", SourceKey: "ENG-7"}, imported(again, "j7", task(mobile, 10)))
	finds("ENG-7", t7)
	if n := notKept(again); n != 1 {
		t.Errorf("a second ENG-7 said %d times; want once", n)
	}

	// Rolling the import back puts its tasks in the trash, and their keys
	// with them: ENG-7 finds nothing, and ENG is free again.
	if err := RollbackImport(ctx, job.Id); err != nil {
		t.Fatal(err)
	}
	finds("ENG-7", uuid.Nil)
	finds("DES-2", uuid.Nil)
	if _, _, err := taskKeys.SetKey(ctx, ops, "ENG", by); err != nil {
		t.Errorf("ENG after the rollback: %v", err)
	}
}
