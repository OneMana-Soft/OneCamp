//go:build integration

package business

// A base's notes go to the import's report in statements that don't grow
// with them (tables T-AT): the importer notes each cell it leaves out, and
// the tables stage wrote each note in a statement of its own, so a messy
// base ran thousands.
// Run: go test -tags=integration ./business/Import/ -run TestABasesNotesTakeAFixedNumberOfStatements -v

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestABasesNotesTakeAFixedNumberOfStatements(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	admin := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, 'ada@example.test', 'ada', 'Ada', NOW(), NOW())`, admin); err != nil {
		t.Fatal(err)
	}
	job := &importModels.Job{Id: uuid.New(), Provider: "airtable", SourceWorkspaceName: "Acme", Source: "api", Status: "running", TriggeredBy: &admin}
	if err := importModels.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	reads := 0
	prov := baseFrom{reads: &reads, make: func() dataTableBusiness.ImportBase {
		return dataTableBusiness.ImportBase{Tables: []dataTableBusiness.ImportTable{{Key: "tblC", Name: "Cells"}}}
	}}
	// The importer as a messy base leaves it: a table, and a note for each
	// cell it left out.
	notes := 0
	saved := importBase
	t.Cleanup(func() { importBase = saved })
	importBase = func(context.Context, dataTableBusiness.Actor, dataTableBusiness.ImportBase) (*dataTableBusiness.ImportedBase, error) {
		made := &dataTableBusiness.ImportedBase{Tables: map[string]uuid.UUID{"tblC": uuid.New()}}
		for i := 0; i < notes; i++ {
			made.Notes = append(made.Notes, dataTableBusiness.ImportNote{Table: "tblC", Field: "fState", Row: fmt.Sprintf("rec%d", i),
				Code: "CELL_LEFT_OUT", Msg: fmt.Sprintf(`"State" has no option "v%d": its options are Reading, Read`, i)})
		}
		return made, nil
	}
	counter := integration.CountPostgres(t, env)
	// statements imports a base of n notes, and answers how many statements
	// it sent; every note is in the report, in the order it was noted.
	statements := func(n int) int {
		t.Helper()
		notes = n
		base := fmt.Sprintf("app%d", n)
		chunk := &importModels.Chunk{Id: uuid.New(), ImportId: job.Id, ChunkType: importModels.ChunkTableBase, ParentSourceId: &base,
			Status: importModels.ChunkStatusInProgress, MaxAttempts: 5}
		if err := importModels.CreateChunks(ctx, []*importModels.Chunk{chunk}); err != nil {
			t.Fatal(err)
		}
		counter.Reset()
		if err := processTableChunk(counter.Context(ctx), prov, job, importProvider.JobOptions{}, chunk, &userModels.UserInfo{}); err != nil {
			t.Fatalf("a base of %d notes: %v", n, err)
		}
		sent := counter.Count()
		rows, err := env.PG.Query(`SELECT source_id, entity_type, severity, code, message FROM import_errors
			WHERE chunk_id = $1 AND code = 'CELL_LEFT_OUT' ORDER BY created_at`, chunk.Id)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var kept []string
		for rows.Next() {
			var source, entity, severity, code, msg string
			if err := rows.Scan(&source, &entity, &severity, &code, &msg); err != nil {
				t.Fatal(err)
			}
			if entity != importModels.EntityTable || severity != importModels.SeverityWarning || msg != fmt.Sprintf(`"State" has no option "v%d": its options are Reading, Read`, len(kept)) {
				t.Fatalf("note %d of %d reads %s %s %s %q", len(kept), n, entity, severity, code, msg)
			}
			kept = append(kept, source)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if len(kept) != n || (n > 0 && (kept[0] != "tblC/rec0" || kept[n-1] != fmt.Sprintf("tblC/rec%d", n-1))) {
			t.Fatalf("the report holds %d of the base's %d notes (first %v)", len(kept), n, kept[:min(len(kept), 3)])
		}
		return sent
	}
	few, many, most := statements(10), statements(900), statements(2500)
	t.Logf("statements: %d for a base of 10 notes, %d for 900, %d for 2,500", few, many, most)
	if many != few {
		t.Errorf("a base of 900 notes took %d statements and one of 10 took %d: its notes cost a statement each", many, few)
	}
	// A statement holds a thousand notes.
	if most != few+2 {
		t.Errorf("a base of 2,500 notes took %d statements, want %d: three for its notes", most, few+2)
	}
}
