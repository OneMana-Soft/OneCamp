//go:build integration

package business

// What a Slack import maps, it saves, or the item fails (audit package 16).
// Seven of SlackImport's map writes dropped their errors: a DM or group found
// in the workspace's map, or made by the import, went on unrecorded, and a
// file's placeholder finished its chunk unmapped, so the next import of the
// workspace made each again. Here a trigger refuses the rows of the ids it
// names, as a full disk or a dropped connection would.
//
// Run: go test -tags=integration ./business/SlackImport/ -run TestWhatASlackImportCannotMapFailsItsItem -v

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	importModels "github.com/akashc777/OneCamp/models/postgres/SlackImport"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestWhatASlackImportCannotMapFailsItsItem(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := env.PG.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	job := uuid.New()
	exec(`INSERT INTO import_jobs (id, provider, source_workspace_name, source, status)
		VALUES ($1, 'slack', 'Acme', 'export_zip', 'running')`, job)
	// The import's own map refuses the ids that start "here-", the
	// workspace's map those that start "there-".
	exec(`CREATE FUNCTION refuse_mapping() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.source_id LIKE (TG_ARGV[0] || '%') THEN
				RAISE EXCEPTION 'mapping refused';
			END IF;
			RETURN NEW;
		END $$`)
	exec(`CREATE TRIGGER refuse_here BEFORE INSERT ON import_id_map
		FOR EACH ROW EXECUTE FUNCTION refuse_mapping('here-')`)
	exec(`CREATE TRIGGER refuse_there BEFORE INSERT ON import_workspace_id_map
		FOR EACH ROW EXECUTE FUNCTION refuse_mapping('there-')`)

	for _, slackID := range []string{"U1", "U2", "U3"} {
		exec(`INSERT INTO import_id_map (import_id, entity_type, source_id, onecamp_uuid)
			VALUES ($1, $2, $3, $4)`, job, importModels.EntityUser, slackID, uuid.New())
	}
	// What an earlier import of the workspace made.
	for _, found := range []struct{ kind, slackID string }{{importModels.EntityDM, "here-D1"}, {importModels.EntityMPIM, "here-G1"}} {
		exec(`INSERT INTO import_workspace_id_map (provider, source_workspace_name, entity_type, source_id, onecamp_uuid)
			VALUES ('slack', 'Acme', $1, $2, $3)`, found.kind, found.slackID, uuid.New())
	}

	refused := func(what string, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), "mapping refused") {
			t.Errorf("%s: the refused mapping ended in %v, want its error", what, err)
		}
	}
	two, three := []string{"U1", "U2"}, []string{"U1", "U2", "U3"}
	_, err := resolveDMs(ctx, job, "Acme", []SlackChannel{{ID: "here-D1", Members: two}}, nil, nil)
	refused("a DM found in the workspace's map", err)
	_, err = resolveDMs(ctx, job, "Acme", nil, []SlackChannel{{ID: "here-G1", Members: three}}, nil)
	refused("a group found in the workspace's map", err)
	_, err = resolveDMs(ctx, job, "Acme", []SlackChannel{{ID: "there-D2", Members: two}}, nil, nil)
	refused("a DM the import made", err)
	_, err = resolveDMs(ctx, job, "Acme", nil, []SlackChannel{{ID: "there-G2", Members: three}}, nil)
	refused("a group the import made", err)
	err = filePlaceholder(ctx, &importModels.Chunk{Id: uuid.New(), ImportId: job},
		&fileChunkMeta{FileID: "here-F1"}, map[string]interface{}{"reason": "not_read"})
	refused("a file's placeholder", err)
}
