//go:build integration

package business

// A ledger write runs without parallel workers, against Postgres 12 with
// every migration. Run: go test -tags=integration ./business/DataTable/ -run TestALedgerWrite -v

import (
	"context"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/models/pgexec"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// A ledger write's transaction reads too (a batch finds its rows, their
// versions and links), and Postgres may give such a read parallel workers,
// whose hashes live in /dev/shm. At the harness's 64M, 4 of 19,051 writes
// beside 3,990 window reads were refused with 53100 in models/ApplyBatch
// (V-583). A write is never run again, so its transaction runs without
// workers from its first statement.
func TestALedgerWriteRunsWithoutParallelWorkers(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("connect: %v", err)
	}
	owner := uuid.New()
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `
		INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, 'owner@example.test', 'owner', 'Owner', NOW(), NOW())`, owner); err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	actor := Actor{UserID: owner, IsAdmin: true}
	table, err := CreateTable(ctx, TableInput{Name: "Accounts", Visibility: "workspace"}, actor)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	f, err := CreateField(ctx, table.Id, FieldInput{Name: "Stage", Type: model.FieldText}, actor)
	if err != nil {
		t.Fatalf("create field: %v", err)
	}
	stage := f.Id.String()
	first, err := CreateRow(ctx, table.Id, RowInput{Values: map[string]interface{}{stage: "Lead"}}, actor)
	if err != nil {
		t.Fatalf("create row: %v", err)
	}

	// Every write to a row runs out of shared memory while its transaction
	// may have parallel workers: Postgres at a small /dev/shm.
	for _, s := range []string{
		`CREATE FUNCTION shm_write() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF current_setting('max_parallel_workers_per_gather') <> '0' THEN
				RAISE EXCEPTION 'could not resize shared memory segment "/PostgreSQL.1" to 8388608 bytes: No space left on device' USING ERRCODE = '53100';
			END IF;
			RETURN NEW;
		END $$`,
		`CREATE TRIGGER shm_write BEFORE INSERT OR UPDATE ON data_table_rows FOR EACH ROW EXECUTE FUNCTION shm_write()`,
	} {
		if _, err := pgexec.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	// The trap bites a write outside the ledger.
	if _, err := pgexec.Exec(ctx, `UPDATE data_table_rows SET position_key = position_key WHERE id = $1`, first.Id); !helpers.IsDiskFull(err) {
		t.Fatalf("a write with workers: %v; want 53100 (the trap must bite)", err)
	}

	stored := func(id uuid.UUID) string {
		t.Helper()
		r, err := model.GetRowByID(ctx, table.Id, id)
		if err != nil || r == nil {
			t.Fatalf("read row %s: %v %v", id, r, err)
		}
		v, _ := parseRowValues(r.Values)[stage].(string)
		return v
	}

	// Each kind of ledger write is made, none refused.
	row, err := CreateRow(ctx, table.Id, RowInput{Values: map[string]interface{}{stage: "Lead"}}, actor)
	if err != nil {
		t.Fatalf("create a row: %v; want it made without workers", err)
	}
	if _, err := PatchRow(ctx, table.Id, row.Id, map[string]interface{}{stage: "Won"}, actor); err != nil {
		t.Errorf("patch a row: %v; want it made without workers", err)
	} else if v := stored(row.Id); v != "Won" {
		t.Errorf("after the patch the cell is %q; want Won", v)
	}
	if _, err := UpdateRow(ctx, table.Id, first.Id, RowInput{Values: map[string]interface{}{stage: "Lost"}}, actor); err != nil {
		t.Errorf("update a row: %v; want it made without workers", err)
	} else if v := stored(first.Id); v != "Lost" {
		t.Errorf("after the update the cell is %q; want Lost", v)
	}
	if _, err := DeleteRow(ctx, table.Id, row.Id, actor); err != nil {
		t.Errorf("delete a row: %v; want it made without workers", err)
	}

	// The setting is the write's transaction's: the pool's sessions keep
	// their own.
	for i := 0; i < 8; i++ {
		if w, err := pgexec.Scalar[string](ctx, `SELECT current_setting('max_parallel_workers_per_gather')`); err != nil || w == "0" {
			t.Fatalf("a session after a ledger write has max_parallel_workers_per_gather %q, %v; want the server's own", w, err)
		}
	}
}
