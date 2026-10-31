//go:build integration

package business

// The tables stage (tables_worker.go) against Postgres: a provider's base
// made into tables, people in its cells matched to members by email (one who
// isn't left out, and said), the base noted in the import's id map so a
// retry makes nothing twice, and a roll back taking its tables away.
// Run: go test -tags=integration ./business/Import/ -run 'TestAProvidersBase' -v

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dataTableModels "github.com/akashc777/OneCamp/models/postgres/DataTable"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// baseFrom is a provider that brings one fixed base (CapTables), counting
// how often it's read.
type baseFrom struct {
	importProvider.Provider
	reads *int
	make  func() dataTableBusiness.ImportBase
}

func (p baseFrom) Name() string { return "airtable" }

func (p baseFrom) BaseOf(context.Context, *importModels.Job, importProvider.JobOptions, string, func(int)) (dataTableBusiness.ImportBase, error) {
	*p.reads++
	return p.make(), nil
}

func TestAProvidersBaseBecomesTablesAndRollsBack(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	// A table's people cells are named from Dgraph as rows are read.
	integration.SetupDgraph(t)
	admin, maya := uuid.New(), uuid.New()
	for id, email := range map[uuid.UUID]string{admin: "ada@example.test", maya: "maya@example.test"} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, email, email, email); err != nil {
			t.Fatal(err)
		}
	}
	reads := 0
	prov := baseFrom{reads: &reads, make: func() dataTableBusiness.ImportBase {
		return dataTableBusiness.ImportBase{Tables: []dataTableBusiness.ImportTable{{
			Key: "tblT", Name: "Tasks",
			Fields: []dataTableBusiness.ImportField{
				{Key: "fName", Name: "Name", Type: dataTableModels.FieldText},
				{Key: "fOwner", Name: "Owner", Type: dataTableModels.FieldPeople},
			},
			Rows: func(page func([]dataTableBusiness.ImportRow) error) error {
				return page([]dataTableBusiness.ImportRow{
					{Key: "rec1", Cells: map[string]any{"fName": "Booth", "fOwner": []any{importProvider.PersonRef{Email: "maya@example.test"}}}},
					{Key: "rec2", Cells: map[string]any{"fName": "Banners", "fOwner": []any{importProvider.PersonRef{Email: "someone@else.test"}}}},
				})
			},
		}}}
	}}
	job := &importModels.Job{Id: uuid.New(), Provider: "airtable", SourceWorkspaceName: "Acme", Source: "api", Status: "running", TriggeredBy: &admin}
	if err := importModels.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	base := "appBase"
	chunk := &importModels.Chunk{Id: uuid.New(), ImportId: job.Id, ChunkType: importModels.ChunkTableBase, ParentSourceId: &base, Status: importModels.ChunkStatusPending, MaxAttempts: 5}
	if err := importModels.CreateChunks(ctx, []*importModels.Chunk{chunk}); err != nil {
		t.Fatal(err)
	}
	importing := &userModels.UserInfo{}
	importing.UserPostgresInfo.Id = admin
	if err := processTableChunk(ctx, prov, job, importProvider.JobOptions{}, chunk, importing); err != nil {
		t.Fatalf("the base: %v", err)
	}
	tableID, err := importModels.LookupIdMapping(ctx, job.Id, importModels.EntityTable, baseTableKey(base, "tblT"))
	if err != nil || tableID == uuid.Nil {
		t.Fatalf("the table isn't noted: %v", err)
	}
	table, _ := dataTableModels.GetTableByID(ctx, tableID)
	if table == nil || table.Visibility != dataTableModels.VisibilityPrivate {
		t.Fatalf("the table: %+v", table)
	}
	rows, err := dataTableModels.ListRows(ctx, tableID, 10, 0)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows: %v %d", err, len(rows))
	}
	fields, _ := dataTableModels.ListFields(ctx, tableID)
	var owner string
	for _, f := range fields {
		if f.Name == "Owner" {
			owner = f.Id.String()
		}
	}
	got := map[string]any{}
	for _, r := range rows {
		v := map[string]any{}
		_ = jsonUnmarshal(r.Values, &v)
		got[r.Id.String()] = v[owner]
	}
	matched, left := 0, 0
	for _, v := range got {
		switch list := v.(type) {
		case []any:
			if len(list) == 1 && refID(list[0]) == maya.String() {
				matched++
			}
		case nil:
			left++
		}
	}
	if matched != 1 || left != 1 {
		t.Errorf("owners: %v", got)
	}
	warnings, _ := importModels.ListErrors(ctx, job.Id, importModels.SeverityWarning, 10, 0)
	said := false
	for _, w := range warnings {
		code, _ := w["code"].(*string) // a code is nullable: the row's own
		said = said || (code != nil && *code == "PEOPLE_NOT_MEMBERS")
	}
	if !said {
		t.Errorf("the person who isn't a member wasn't said: %v", warnings)
	}

	// Tried again (as by the reaper): nothing is made twice.
	if err := processTableChunk(ctx, prov, job, importProvider.JobOptions{}, chunk, importing); err != nil || reads != 1 {
		t.Errorf("a retry: %v, read %d times", err, reads)
	}

	// Rolled back: its tables go.
	if _, err := env.PG.Exec(`UPDATE import_jobs SET status = 'completed' WHERE id = $1`, job.Id); err != nil {
		t.Fatal(err)
	}
	if err := RollbackImport(ctx, job.Id); err != nil {
		t.Fatalf("roll back: %v", err)
	}
	if after, _ := dataTableModels.GetTableByID(ctx, tableID); after != nil && after.DeletedAt == nil {
		t.Error("a rolled back import left its table")
	}
}

func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }

// refID is a people cell's item's id: a ref's, or the id itself.
func refID(v any) string {
	if m, ok := v.(map[string]any); ok {
		s, _ := m["id"].(string)
		return s
	}
	s, _ := v.(string)
	return s
}
