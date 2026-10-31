//go:build integration

package business

// What an import made goes in its own map, and in its workspace's map so the
// next import of that workspace skips it. The workspace's map is keyed by the
// workspace's name, so an import that names none keeps its rows to itself:
// under "" one unnamed import's ids would stand for another's.
// Run: PKGS=./business/Import/ integ_rel.sh <wt> <log> 'TestAnImportThatNamesNoWorkspaceKeepsItsMappingsToItself'

import (
	"context"
	"testing"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestAnImportThatNamesNoWorkspaceKeepsItsMappingsToItself(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for _, c := range []struct {
		name, workspace string
		inWorkspaceMap  int
	}{
		{"named", "acme", 2},
		{"unnamed", "", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			j := &importModels.Job{Id: uuid.New(), Provider: "linear", SourceWorkspaceName: c.workspace, Source: "api", Status: importModels.StatusCompleted}
			if err := importModels.CreateJob(ctx, j); err != nil {
				t.Fatal(err)
			}
			// One row by each way in: a task alone, and a cycle in a batch.
			if err := importModels.RecordMapping(ctx, j.Id, j.Provider, j.SourceWorkspaceName, importModels.EntityTask, "ENG-1", uuid.New(), nil, nil, true); err != nil {
				t.Fatal(err)
			}
			if err := importModels.RecordMappings(ctx, j.Id, j.Provider, j.SourceWorkspaceName, importModels.EntityCycle,
				[]importModels.Mapped{{SourceID: "cycle-1", OnecampUUID: uuid.New(), CreatedHere: true}}); err != nil {
				t.Fatal(err)
			}
			if got := count(`SELECT count(*) FROM import_id_map WHERE import_id = $1`, j.Id); got != 2 {
				t.Errorf("the import's own map holds %d rows, want 2", got)
			}
			if got := count(`SELECT count(*) FROM import_workspace_id_map WHERE origin_import_id = $1`, j.Id); got != c.inWorkspaceMap {
				t.Errorf("the workspace's map holds %d of its rows, want %d", got, c.inWorkspaceMap)
			}
		})
	}
}
