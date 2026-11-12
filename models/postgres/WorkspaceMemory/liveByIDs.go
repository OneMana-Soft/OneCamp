package models

import (
	"context"

	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// LiveByIDs is those of these items not deleted, by id: one read for the
// nudge engine to check the commitments behind its open nudges (V-581). An id
// missing from the answer is deleted or never was.
func LiveByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]*MemoryItem, error) {
	out := make(map[uuid.UUID]*MemoryItem, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	items, err := pgexec.Query(ctx, `
		SELECT id, kind, content, status, owner_user_id, due_at,
		       channel_uuid, project_uuid, chat_grp_id, team_uuid,
		       source_type, source_uuid, confidence, created_by_user_id,
		       created_at, updated_at
		FROM workspace_memory_items
		WHERE id = ANY($1) AND deleted_at IS NULL`, func(r pgexec.Row) (*MemoryItem, error) { return scanItem(r) }, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	for _, it := range items {
		out[it.ID] = it
	}
	return out, nil
}
