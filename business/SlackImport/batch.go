package business

// Messages are made a batch at a time (backend audit U1a): a batch's
// mappings are read in a few statements before its messages are made in
// their order. Each message read its own mapping, its workspace mapping,
// its thread's parent and its author's mapping (a statement apiece), and
// wrote its two mapping rows in two more.

import (
	"archive/zip"
	"context"
	"encoding/json"

	importModels "github.com/akashc777/OneCamp/models/postgres/SlackImport"
	"github.com/google/uuid"
)

// messageBatch is how many messages a chunk reads its mappings for at once.
const messageBatch = 200

// iterBatches hands each message of a daily file after resumeCursor to
// handle, in order, a batch at a time, after prefetch has read the batch's
// mappings. handle answering false stops it, as IterMessages' callback does.
func iterBatches(ctx context.Context, arc *Archive, zf *zip.File, resumeCursor string,
	prefetch func([]*SlackMessage), handle func(*SlackMessage) bool) error {
	batch := make([]*SlackMessage, 0, messageBatch)
	flush := func() bool {
		if len(batch) == 0 {
			return true
		}
		prefetch(batch)
		for _, m := range batch {
			if !handle(m) {
				return false
			}
		}
		batch = batch[:0]
		return true
	}
	stopped := false
	err := IterMessages(ctx, arc, zf, func(m *SlackMessage) bool {
		if resumeCursor != "" && tsLE(m.Ts, resumeCursor) {
			return true
		}
		batch = append(batch, m)
		if len(batch) < messageBatch {
			return true
		}
		stopped = !flush()
		return !stopped
	})
	if err == nil && !stopped {
		flush()
	}
	return err
}

// messageMaps holds the mappings a chunk's current batch needs. A map is
// nil when its batch read failed, and each mapping is then read alone.
type messageMaps struct {
	importId      uuid.UUID
	workspaceName string

	ownType                 string
	own, workspace, parents map[string]uuid.UUID

	// Slack user id -> OneCamp uuid (uuid.Nil for none), for authors and
	// mentions: a person is read once a chunk, not once a message.
	userIds map[string]uuid.UUID
}

func newMessageMaps(importId uuid.UUID, workspaceName string) *messageMaps {
	return &messageMaps{importId: importId, workspaceName: workspaceName, userIds: make(map[string]uuid.UUID, 32)}
}

// prefetch reads, for a batch made as entityType, this import's mappings,
// the workspace's from earlier imports, the thread parents' and the
// authors' not yet known: at most four statements.
func (mm *messageMaps) prefetch(ctx context.Context, entityType string, batch []*SlackMessage) {
	ts := make([]string, 0, len(batch))
	var threads, authors []string
	for _, m := range batch {
		ts = append(ts, m.Ts)
		if m.ThreadTs != "" && m.ThreadTs != m.Ts {
			threads = append(threads, m.ThreadTs)
		}
		if _, known := mm.userIds[m.User]; m.User != "" && !known {
			mm.userIds[m.User] = uuid.Nil // asked once, below
			authors = append(authors, m.User)
		}
	}
	var err error
	mm.ownType = entityType
	if mm.own, err = importModels.LookupIdMappingsBatch(ctx, mm.importId, entityType, ts); err != nil {
		mm.own = nil
	}
	mm.workspace = nil
	if mm.workspaceName != "" {
		if mm.workspace, err = importModels.LookupWorkspaceMappingsBatch(ctx, mm.workspaceName, entityType, ts); err != nil {
			mm.workspace = nil
		}
	}
	mm.parents = map[string]uuid.UUID{}
	if len(threads) > 0 {
		if mm.parents, err = importModels.LookupIdMappingsBatch(ctx, mm.importId, importModels.EntityMessage, threads); err != nil {
			mm.parents = nil
		}
	}
	if len(authors) > 0 {
		found, err := importModels.LookupIdMappingsBatch(ctx, mm.importId, importModels.EntityUser, authors)
		for _, a := range authors {
			if err != nil {
				delete(mm.userIds, a) // read alone when asked
			} else {
				mm.userIds[a] = found[a]
			}
		}
	}
}

// ownMapping is what this import made for a message, or uuid.Nil.
func (mm *messageMaps) ownMapping(ctx context.Context, entityType, ts string) uuid.UUID {
	if mm.own != nil && mm.ownType == entityType {
		return mm.own[ts]
	}
	id, _ := importModels.LookupIdMapping(ctx, mm.importId, entityType, ts)
	return id
}

// workspaceMapping is what an earlier import of the workspace made for a message.
func (mm *messageMaps) workspaceMapping(ctx context.Context, entityType, ts string) uuid.UUID {
	if mm.workspace != nil && mm.ownType == entityType {
		return mm.workspace[ts]
	}
	id, _ := importModels.LookupWorkspaceMapping(ctx, mm.workspaceName, entityType, ts)
	return id
}

// parentMapping is the message a thread reply answers, or uuid.Nil.
func (mm *messageMaps) parentMapping(ctx context.Context, threadTs string) (uuid.UUID, error) {
	if mm.parents != nil {
		if id, ok := mm.parents[threadTs]; ok {
			return id, nil
		}
		if mm.ownType == importModels.EntityMessage && mm.own != nil {
			if id, ok := mm.own[threadTs]; ok {
				return id, nil // made earlier in this batch
			}
		}
	}
	return importModels.LookupIdMapping(ctx, mm.importId, importModels.EntityMessage, threadTs)
}

// userId is the OneCamp person a Slack user came across as, or uuid.Nil.
func (mm *messageMaps) userId(ctx context.Context, slackId string) (uuid.UUID, error) {
	if id, ok := mm.userIds[slackId]; ok {
		return id, nil
	}
	id, err := importModels.LookupIdMapping(ctx, mm.importId, importModels.EntityUser, slackId)
	if err != nil {
		return uuid.Nil, err
	}
	mm.userIds[slackId] = id
	return id, nil
}

// userIdsOf is userId for several Slack users: those not yet known read in
// one statement and kept. A user who didn't come across is uuid.Nil.
func (mm *messageMaps) userIdsOf(ctx context.Context, slackIds []string) (map[string]uuid.UUID, error) {
	out := make(map[string]uuid.UUID, len(slackIds))
	var missing []string
	for _, id := range slackIds {
		if v, ok := mm.userIds[id]; ok {
			out[id] = v
		} else {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return out, nil
	}
	found, err := importModels.LookupIdMappingsBatch(ctx, mm.importId, importModels.EntityUser, missing)
	if err != nil {
		return out, err
	}
	for _, id := range missing {
		out[id] = found[id]
		mm.userIds[id] = found[id]
	}
	return out, nil
}

// recordCreated records a message this import made, in its map and the
// workspace's (one statement), and remembers it for the rest of the batch.
func (mm *messageMaps) recordCreated(ctx context.Context, entityType, ts string, id uuid.UUID, parent *string, metadata json.RawMessage) error {
	if err := importModels.RecordCreated(ctx, mm.importId, entityType, ts, id, parent, metadata, mm.workspaceName); err != nil {
		return err
	}
	if mm.own != nil && mm.ownType == entityType {
		mm.own[ts] = id
	}
	return nil
}
