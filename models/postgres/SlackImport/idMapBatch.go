package models

// The message workers' mapping reads and writes, a batch of messages at a
// time (backend audit U1a): each message looked up its own mapping and its
// workspace mapping, and wrote the two rows in two statements.

import (
	"context"
	"encoding/json"

	"github.com/akashc777/OneCamp/models/pgexec"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// LookupWorkspaceMappingsBatch is LookupWorkspaceMapping for many Slack ids
// at once: the OneCamp uuid a previous import of the workspace made for each
// one it made.
func LookupWorkspaceMappingsBatch(ctx context.Context, workspaceName, entityType string, slackIds []string) (map[string]uuid.UUID, error) {
	if len(slackIds) == 0 || workspaceName == "" {
		return map[string]uuid.UUID{}, nil
	}
	return pgexec.Pairs[string, uuid.UUID](ctx, `
		SELECT source_id, onecamp_uuid FROM import_workspace_id_map
		WHERE provider = 'slack' AND source_workspace_name = $1 AND entity_type = $2
		  AND source_id = ANY($3)`, workspaceName, entityType, pq.Array(slackIds))
}

// RecordCreated records what this import made for a Slack id, in this
// import's map (as made by it) and, when workspaceName is set, in the
// workspace's map, so the next import of the workspace skips it: one
// statement, where UpsertIdMappingWithOwnership and UpsertWorkspaceMapping
// were two. Both are idempotent.
func RecordCreated(ctx context.Context, importId uuid.UUID, entityType, slackId string,
	onecampUUID uuid.UUID, parentSlackId *string, metadata json.RawMessage, workspaceName string) error {
	return RecordMapping(ctx, importId, entityType, slackId, onecampUUID, parentSlackId, metadata, true, workspaceName)
}

// RecordMapping is RecordCreated for something this import made
// (createdHere) or found already there, such as a member matched by address.
func RecordMapping(ctx context.Context, importId uuid.UUID, entityType, slackId string,
	onecampUUID uuid.UUID, parentSlackId *string, metadata json.RawMessage, createdHere bool, workspaceName string) error {
	_, err := pgexec.Exec(ctx, `
		WITH mapped AS (
			INSERT INTO import_id_map
			    (import_id, entity_type, source_id, onecamp_uuid, parent_source_id, metadata, created_by_this_import)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (import_id, entity_type, source_id) DO NOTHING
		)
		INSERT INTO import_workspace_id_map
		    (provider, source_workspace_name, entity_type, source_id, onecamp_uuid, origin_import_id)
		SELECT 'slack', $8::varchar, $2, $3, $4, $1 WHERE $8::varchar <> ''
		ON CONFLICT (provider, source_workspace_name, entity_type, source_id) DO NOTHING`,
		importId, entityType, slackId, onecampUUID, parentSlackId, metadata, createdHere, workspaceName)
	return err
}

// LookupIdMappingsMany is LookupIdMappingsBatch for any number of ids, a
// thousand at a time (a statement holds at most 65,535 parameters).
func LookupIdMappingsMany(ctx context.Context, importId uuid.UUID, entityType string, slackIds []string) (map[string]uuid.UUID, error) {
	out := make(map[string]uuid.UUID, len(slackIds))
	for len(slackIds) > 0 {
		n := min(len(slackIds), 1000)
		part, err := pgexec.Pairs[string, uuid.UUID](ctx, `
			SELECT source_id, onecamp_uuid FROM import_id_map
			WHERE import_id = $1 AND entity_type = $2 AND source_id = ANY($3)`,
			importId, entityType, pq.Array(slackIds[:n]))
		if err != nil {
			return nil, err
		}
		for k, v := range part {
			out[k] = v
		}
		slackIds = slackIds[n:]
	}
	return out, nil
}

// IdMapMetadataMany is GetIdMapMetadata for several Slack ids in one
// statement: each one's metadata, by its id (an id with none is left out).
func IdMapMetadataMany(ctx context.Context, importId uuid.UUID, entityType string, slackIds []string) (map[string]json.RawMessage, error) {
	if len(slackIds) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	return pgexec.Pairs[string, json.RawMessage](ctx, `
		SELECT source_id, metadata FROM import_id_map
		WHERE import_id = $1 AND entity_type = $2 AND source_id = ANY($3) AND metadata IS NOT NULL`,
		importId, entityType, pq.Array(slackIds))
}

// JobCount is what the jobs list shows of an import's progress (the
// generic import's, over the same tables).
type JobCount = importModels.JobCount

// JobCounts is each import's chunk, item and error counts, in one statement.
func JobCounts(ctx context.Context, jobIds []uuid.UUID) (map[uuid.UUID]JobCount, error) {
	return importModels.JobCounts(ctx, jobIds)
}
