//go:build integration

package business

// What the tests that break one of a Slack import's reads run in (audit
// package 36): Postgres whose maps can be made unreadable for one kind of row
// (importDB), and for the import's workers, Dgraph, a quiet OpenSearch, the
// importing admin, a teammate and the #general the export's general came to
// (importWorld).

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	channelAdapter "github.com/akashc777/OneCamp/adapter/Channel"
	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	importModels "github.com/akashc777/OneCamp/models/postgres/SlackImport"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// importDB is the test's own Postgres, connected for the import's models.
// Each statement is planned for its own values, so unreadable's fault can
// leave out every read but one kind's. The helpers take the (sub)test they
// fail: a subtest can't stop its parent.
type importDB struct {
	ctx context.Context
	env *integration.Env
}

func newImportDB(t *testing.T) *importDB {
	t.Helper()
	db := &importDB{ctx: context.Background(), env: integration.SetupEnv(t)}
	var name string
	if err := db.env.PG.QueryRow(`SELECT current_database()`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	db.exec(t, `ALTER DATABASE "`+name+`" SET plan_cache_mode = force_custom_plan`)
	if err := postgresInit.ConnectPostgres(db.ctx, db.env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	db.exec(t, `CREATE EXTENSION IF NOT EXISTS file_fdw`)
	db.exec(t, `CREATE SERVER unreadable FOREIGN DATA WRAPPER file_fdw`)
	return db
}

func (db *importDB) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := db.env.PG.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}

func (db *importDB) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.env.PG.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// newJob starts a Slack import of workspace. A workspace runs one import at a
// time (uq_import_jobs_active_per_provider_workspace), so the one before it
// has finished.
func (db *importDB) newJob(t *testing.T, workspace string) uuid.UUID {
	t.Helper()
	db.exec(t, `UPDATE import_jobs SET status = 'completed'
		WHERE provider = 'slack' AND source_workspace_name = $1 AND status = 'running'`, workspace)
	job := uuid.New()
	db.exec(t, `INSERT INTO import_jobs (id, provider, source_workspace_name, source, status)
		VALUES ($1, 'slack', $2, 'export_zip', 'running')`, job, workspace)
	return job
}

// unreadable makes the reads of table's rows of entityType (of every row, for
// "") fail until restore, as a dropped connection or a timeout fails a read:
// they scan a child of the table that is a file that isn't there. The table's
// other reads, and its writes, go on.
func (db *importDB) unreadable(t *testing.T, table, entityType string) (restore func()) {
	t.Helper()
	db.exec(t, `CREATE FOREIGN TABLE `+table+`_unreadable () INHERITS (`+table+`)
		SERVER unreadable OPTIONS (filename '/nonexistent/`+table+`')`)
	if entityType != "" {
		db.exec(t, `ALTER FOREIGN TABLE `+table+`_unreadable
			ADD CONSTRAINT one_kind CHECK (entity_type = '`+entityType+`')`)
	}
	return func() { db.exec(t, `DROP FOREIGN TABLE `+table+`_unreadable`) }
}

// importWorld is importDB with what the import's workers write to.
type importWorld struct {
	*importDB
	// quiet is how the import's orchestrator runs its workers: no notices,
	// MQTT or AI.
	quiet      context.Context
	dg         *integration.DgraphEnv
	admin, ada *userModels.UserInfo
	general    uuid.UUID
}

func newImportWorld(t *testing.T) *importWorld {
	t.Helper()
	w := &importWorld{importDB: newImportDB(t)}
	w.quiet = context.WithValue(w.ctx, helpers.BulkImportContextKey, true)
	w.dg = integration.SetupDgraph(t)
	search := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		fmt.Fprint(rw, `{"took":1,"errors":false,"items":[]}`)
	}))
	t.Cleanup(search.Close)
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client

	w.admin, w.ada = w.person(t, "admin"), w.person(t, "ada")
	cerr, general := channelBusiness.CreateChannel(w.ctx, &channelAdapter.InputCreateChannel{ChannelName: "general"}, w.admin)
	if cerr != nil {
		t.Fatal(cerr)
	}
	w.general = general
	return w
}

// person is a workspace member named name, in Postgres and the graph.
func (w *importWorld) person(t *testing.T, name string) *userModels.UserInfo {
	t.Helper()
	id := uuid.New()
	w.exec(t, `INSERT INTO users (id, email_id, username) VALUES ($1, $2, $3)`, id, name+"@acme.test", name)
	w.dg.Mutate(t, []map[string]any{{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name}})
	pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(w.ctx, id)
	if err != nil || pg == nil {
		t.Fatalf("load %s: %v", name, err)
	}
	node, err := userDomain.GetDgraphUserInfoByUUID(w.ctx, id.String())
	if err != nil || node == nil {
		t.Fatalf("load %s's node: %v", name, err)
	}
	return &userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *node}
}

// newJob is importDB's, with what the import's earlier steps leave: the
// channel C1 (#general) and the people U1 (the admin) and U2 (ada) mapped.
func (w *importWorld) newJob(t *testing.T, workspace string) uuid.UUID {
	t.Helper()
	job := w.importDB.newJob(t, workspace)
	w.exec(t, `INSERT INTO import_id_map (import_id, entity_type, source_id, onecamp_uuid)
		VALUES ($1, $2, 'C1', $3), ($1, $4, 'U1', $5), ($1, $4, 'U2', $6)`,
		job, importModels.EntityChannel, w.general, importModels.EntityUser, w.admin.UserPostgresInfo.Id, w.ada.UserPostgresInfo.Id)
	return job
}

// dayFile is the export's day file of a conversation's messages.
func dayFile(conversation string) string { return conversation + "/2026-10-20.json" }

// exportWith is an export holding conversation's day of messages.
func exportWith(t *testing.T, conversation string, messages ...string) *Archive {
	t.Helper()
	_, zr := buildArchive(t, []zipTestEntry{{name: dayFile(conversation), body: "[" + strings.Join(messages, ",") + "]"}})
	return &Archive{Reader: zr}
}

// chunk is job's chunk of a conversation's day of messages.
func (w *importWorld) chunk(t *testing.T, job uuid.UUID, kind, conversation string) *importModels.Chunk {
	t.Helper()
	key := dayFile(conversation)
	c := &importModels.Chunk{Id: uuid.New(), ImportId: job, ChunkType: kind,
		ChannelSlackId: &conversation, ObjectKey: &key, Status: importModels.ChunkStatusInProgress, MaxAttempts: 3}
	if err := importModels.CreateChunks(w.ctx, []*importModels.Chunk{c}); err != nil {
		t.Fatal(err)
	}
	return c
}

// run works c, its day holding messages, as the import's orchestrator does.
func (w *importWorld) run(t *testing.T, c *importModels.Chunk, workspace string, messages ...string) {
	t.Helper()
	work := processMessageChunk
	if c.ChunkType == importModels.ChunkDMMessages {
		work = processDMChunk
	}
	if err := work(w.quiet, exportWith(t, *c.ChannelSlackId, messages...), c, true, w.admin, workspace, nil); err != nil {
		t.Fatal(err)
	}
}

// logged checks how many of each code job's import errors hold.
func (w *importWorld) logged(t *testing.T, job uuid.UUID, want map[string]int) {
	t.Helper()
	for code, n := range want {
		if got := w.count(t, `SELECT count(*) FROM import_errors WHERE import_id = $1 AND code = $2`, job, code); got != n {
			t.Errorf("%d %s logged, want %d", got, code, n)
		}
	}
}
