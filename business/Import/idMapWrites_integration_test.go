//go:build integration

package business

// C6 (the 30-day audit): the importers ignored the error on 28 of their 51
// id-map writes, and wrote an item's own row and its workspace row in two
// statements. A write that failed left an item the import counted as come
// across, which a rollback can't find and a re-run makes again, or one row
// without the other. Every "made or found" write is now one statement
// (importModels.RecordMapping), and its failure fails the item.
// Run: PKGS=./business/Import/ integ_rel.sh <wt> <log> '^(TestAFailedIdMapWriteFailsItsItem|TestAPageOrCommentThatCantBeRememberedIsFailed)$'

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// refuseMappings makes Postgres refuse to write these source ids' rows in
// table (import_id_map or import_workspace_id_map), as a dropped connection
// would refuse any write.
func refuseMappings(t *testing.T, db *sql.DB, table string, sourceIDs ...string) {
	t.Helper()
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS refused_mappings (tbl text NOT NULL, source_id text NOT NULL)`,
		`CREATE OR REPLACE FUNCTION refuse_mapping() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF EXISTS (SELECT 1 FROM refused_mappings WHERE tbl = TG_TABLE_NAME AND source_id = NEW.source_id) THEN
				RAISE EXCEPTION 'refused to map %', NEW.source_id;
			END IF;
			RETURN NEW;
		END $$`,
		`DROP TRIGGER IF EXISTS refuse_mapping ON ` + table,
		`CREATE TRIGGER refuse_mapping BEFORE INSERT ON ` + table + ` FOR EACH ROW EXECUTE FUNCTION refuse_mapping()`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range sourceIDs {
		if _, err := db.Exec(`INSERT INTO refused_mappings (tbl, source_id) VALUES ($1, $2)`, table, id); err != nil {
			t.Fatal(err)
		}
	}
}

// importRows is how many rows this import's id map has for sourceID.
func importRows(t *testing.T, db *sql.DB, job uuid.UUID, sourceID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM import_id_map WHERE import_id = $1 AND source_id = $2`, job, sourceID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// failedAs is the code the import noted sourceID's item as failed with, or "".
func failedAs(t *testing.T, db *sql.DB, job uuid.UUID, sourceID string) string {
	t.Helper()
	var code string
	err := db.QueryRow(`SELECT code FROM import_errors WHERE import_id = $1 AND source_id = $2 AND severity = $3`,
		job, sourceID, importModels.SeverityError).Scan(&code)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	return code
}

// An item matched from an earlier import of the workspace, or by its address,
// is failed when the import can't remember it, and the import's row isn't
// left behind without the workspace's.
func TestAFailedIdMapWriteFailsItsItem(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}

	earlier := &importModels.Job{Id: uuid.New(), Provider: "asana", SourceWorkspaceName: "Acme", Source: "api", Status: importModels.StatusCompleted}
	job := &importModels.Job{Id: uuid.New(), Provider: "asana", SourceWorkspaceName: "Acme", Source: "api", Status: importModels.StatusRunning}
	for _, j := range []*importModels.Job{earlier, job} {
		if err := importModels.CreateJob(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	for src, kind := range map[string]string{"team-1": importModels.EntityTeam, "project-1": importModels.EntityProject,
		"task-1": importModels.EntityTask, "user-1": importModels.EntityUser} {
		if err := importModels.RecordMapping(ctx, earlier.Id, job.Provider, job.SourceWorkspaceName, kind, src, uuid.New(), nil, nil, true); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, created_at, updated_at) VALUES ($1, 'bo@acme.test', NOW(), NOW())`, uuid.New()); err != nil {
		t.Fatal(err)
	}
	refuseMappings(t, env.PG, "import_id_map", "team-1", "project-1", "task-1", "user-1")
	refuseMappings(t, env.PG, "import_workspace_id_map", "user-2")
	importer := &userModels.UserInfo{}

	for _, c := range []struct {
		name, src, entity string
		item              func() error
	}{
		{"a team matched before", "team-1", importModels.EntityTeam, func() error {
			return resolveOneTeam(ctx, job, &importProvider.SourceTeam{SourceID: "team-1", Name: "Ops"}, importer)
		}},
		{"a project matched before", "project-1", importModels.EntityProject, func() error {
			return resolveOneProject(ctx, job, &importProvider.SourceProject{SourceID: "project-1", Name: "Launch"}, importer)
		}},
		{"a task matched before", "task-1", importModels.EntityTask, func() error {
			return importOneTask(ctx, job, &importProvider.SourceTask{SourceID: "task-1", Name: "Ship it"}, uuid.Nil, nil,
				nil, nil, nil, nil, importer, nil, nil, nil, nil, nil)
		}},
		{"a person matched before", "user-1", importModels.EntityUser, func() error {
			return resolveOneUser(ctx, job, &importProvider.SourceUser{SourceID: "user-1"}, &UserResolveStats{}, &memberNames{})
		}},
		{"a person matched by address, whose workspace row is refused", "user-2", importModels.EntityUser, func() error {
			return resolveOneUser(ctx, job, &importProvider.SourceUser{SourceID: "user-2", Email: "bo@acme.test"}, &UserResolveStats{}, &memberNames{})
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := c.item()
			if want := "remember " + c.entity + " " + c.src + ":"; err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("the item answered %v, want it failed, saying %q", err, want)
			}
			if n := importRows(t, env.PG, job.Id, c.src); n != 0 {
				t.Fatalf("%d of the import's rows were left for %s, want none", n, c.src)
			}
		})
	}
}

// A page made, or a page's comment written, that the import can't remember
// is noted as failed, the import's row isn't left without the workspace's,
// and the rest of the tree still comes across.
func TestAPageOrCommentThatCantBeRememberedIsFailed(t *testing.T) {
	ctx := context.Background()
	w := newPagesWorld(t)
	job, chunk := w.job(t)
	refuseMappings(t, w.env.PG, "import_workspace_id_map", treeChild)
	refuseMappings(t, w.env.PG, "import_id_map", "cm-refused")

	withComments := pagesFrom{pages: []importProvider.SourcePage{
		{SourceID: treeRoot, Title: "Engineering", HTML: `<p>Ship on Friday.</p>`, Comments: []importProvider.SourceComment{
			{SourceID: "cm-kept", Body: "<p>Friday works.</p>"},
			{SourceID: "cm-refused", Body: "<p>After the review.</p>"},
		}},
		{SourceID: treeChild, ParentSourceID: treeRoot, Title: "Runbooks", HTML: `<p>Restart the queue.</p>`},
	}}
	if err := processPageChunk(ctx, withComments, job, importProvider.JobOptions{}, chunk, w.importing); err != nil {
		t.Fatal(err)
	}
	for src, want := range map[string]string{treeChild: "PAGE_NOT_RECORDED", "cm-refused": "COMMENT_IMPORT_FAILED"} {
		if got := failedAs(t, w.env.PG, job.Id, src); got != want {
			t.Errorf("%s was noted as %q, want %q", src, got, want)
		}
	}
	if n := importRows(t, w.env.PG, job.Id, treeChild); n != 0 {
		t.Errorf("%d of the import's rows were left for the page whose workspace row was refused, want none", n)
	}
	for _, src := range []string{treeRoot, "cm-kept"} {
		if n := importRows(t, w.env.PG, job.Id, src); n != 1 {
			t.Errorf("%s has %d of the import's rows, want 1", src, n)
		}
	}
}
