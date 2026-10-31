//go:build integration

package integration_test

// Tables S1 step 5: a change set's operation (the change_set_operations
// migration, its number a placeholder) goes down and up
// again, as a release that steps back would run it.

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/akashc777/OneCamp/tests/integration"
)

func TestChangeSetOperationsMigrationDownUp(t *testing.T) {
	_, here, _, _ := runtime.Caller(0)
	read := func(dir string) string {
		t.Helper()
		matches, _ := filepath.Glob(filepath.Join(filepath.Dir(here), "..", "..", "migrations", "*_change_set_operations."+dir+".sql"))
		if len(matches) != 1 {
			t.Fatalf("want one change_set_operations %s migration, found %v", dir, matches)
		}
		b, err := os.ReadFile(matches[0])
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	up, down := read("up"), read("down")
	env := integration.SetupEnv(t)
	has := func() bool {
		t.Helper()
		var ok bool
		if err := env.PG.QueryRow(`SELECT EXISTS (SELECT 1 FROM information_schema.columns
			WHERE table_name = 'data_table_change_sets' AND column_name = 'operation')`).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !has() {
		t.Fatal("data_table_change_sets has no operation after every migration ran")
	}
	for i, step := range []struct {
		sql  string
		want bool
	}{{down, false}, {up, true}, {down, false}, {up, true}} {
		if _, err := env.PG.Exec(step.sql); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if has() != step.want {
			t.Fatalf("step %d: the operation column exists = %v", i, !step.want)
		}
	}
}
