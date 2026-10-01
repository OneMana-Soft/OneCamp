package ai

// Content→memory deletion cascade, placed in services/AI so any content
// package (Post, Chat, Doc, ...) can call it WITHOUT importing business/AI
// (which would create an import cycle: business/AI already imports those
// content packages for its tool executors).
//
// When a user deletes source content, every workspace-memory item the
// extractor derived from it must also disappear — from Postgres (system of
// record) and from the OpenSearch projection. This is both a correctness
// guarantee (no stale facts) and a privacy guarantee (deleted content must
// not resurface via AI).

import (
	"context"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	memDgraph "github.com/akashc777/OneCamp/models/dgraph/WorkspaceMemory"
	memoryModels "github.com/akashc777/OneCamp/models/postgres/WorkspaceMemory"
	"github.com/google/uuid"
)

// DeleteMemoryProjectionsAsync drops the OpenSearch + Dgraph projections
// for a SINGLE memory item id (used when a user resolves/dismisses an item
// directly). Fire-and-forget, best-effort.
func DeleteMemoryProjectionsAsync(memID string) {
	if memID == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if derr := DeleteEmbedding(ctx, "memory", memID); derr != nil {
			helpers.LogErrorWithContext(ctx, "memory projection: drop embedding %s failed: %v", memID, derr)
		}
		if derr := memDgraph.DeleteMemoryItem(ctx, memID); derr != nil {
			helpers.LogErrorWithContext(ctx, "memory projection: drop graph node %s failed: %v", memID, derr)
		}
	}()
}

// InvalidateMemoryForEditedSourceAsync reconciles the workspace-memory
// items captured from a source message that was EDITED, given the message's
// NEW plain text. A captured item stores a point-in-time snapshot of the
// message text, so on edit we REFRESH that snapshot in place rather than
// drop the user's deliberate save:
//
//   - newPlainText non-empty: refresh the captured content across Postgres,
//     OpenSearch and Dgraph so the three stores stay consistent and no
//     stale/old text lingers (privacy preserved, staleness fixed, save kept).
//   - newPlainText empty (the edit removed all text): treat as a redaction
//     and fall back to the deletion cascade.
//
// A source_uuid match is by construction a manual capture (the batched
// worker keys items by scope, never by message uuid), so refreshing the
// verbatim captured content is exactly right. Fire-and-forget with its own
// bounded context so it never blocks the edit request. No-op when nothing
// was captured from the source.
func InvalidateMemoryForEditedSourceAsync(sourceType, sourceUUID, newPlainText string) {
	trimmed := strings.TrimSpace(newPlainText)
	if trimmed == "" {
		// Edit emptied the message → redaction semantics: drop captures.
		DeleteMemoryBySourceAsync(sourceType, sourceUUID)
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		refreshed, err := memoryModels.RefreshContentBySource(ctx, sourceType, sourceUUID, trimmed)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "memory edit refresh (%s:%s) failed: %v", sourceType, sourceUUID, err)
			return
		}
		for _, it := range refreshed {
			reprojectMemoryItem(ctx, it)
		}
		if len(refreshed) > 0 {
			helpers.LogInfoWithContext(ctx, "memory edit: refreshed %d item(s) for %s:%s", len(refreshed), sourceType, sourceUUID)
		}
	}()
}

// reprojectMemoryItem re-projects a memory item into OpenSearch (semantic
// recall) and Dgraph (GraphRAG) so all three stores carry the current
// content. Used by the edit-refresh path (node exists) and the archive
// RESTORE path (node was dropped on archive and must be recreated), so it
// does a full Dgraph UPSERT rather than a content-only patch — idempotent
// and correct whether the node currently exists or not. Best-effort; the
// graph/vector copies are indexes, not the source of truth.
func reprojectMemoryItem(ctx context.Context, it *memoryModels.MemoryItem) {
	if it == nil {
		return
	}
	var chUUID, prUUID, ownerUUID string
	if it.ChannelUUID != nil {
		chUUID = it.ChannelUUID.String()
	}
	if it.ProjectUUID != nil {
		prUUID = it.ProjectUUID.String()
	}
	if it.OwnerID != nil {
		ownerUUID = it.OwnerID.String()
	} else if it.CreatedBy != nil {
		ownerUUID = it.CreatedBy.String()
	}

	// OpenSearch: re-embed the kind-prefixed fact (upsert by memory id).
	embedText := memoryEmbedText(it.Kind, it.Content)
	EmbedMemoryContent(embedText, it.ID.String(), chUUID, prUUID, it.ChatGrpID, ownerUUID)

	// Dgraph: full upsert so the node + owner/channel/project edges are
	// (re)created if missing (restore) or updated in place (edit).
	node := &dgraphStruct.DgraphMemoryItem{
		Uuid:       it.ID.String(),
		Kind:       it.Kind,
		Content:    it.Content,
		Status:     it.Status,
		Confidence: it.Confidence,
		DueAt:      it.DueAt,
		GrpID:      it.ChatGrpID,
		CreatedAt:  &it.CreatedAt,
		UpdatedAt:  &it.UpdatedAt,
	}
	if derr := memDgraph.UpsertMemoryItem(ctx, node, ownerUUID, chUUID, prUUID); derr != nil {
		helpers.LogErrorWithContext(ctx, "memory reproject: graph upsert %s failed: %v", it.ID, derr)
	}
}

// memoryEmbedText renders the kind-prefixed embedding text for a memory
// item, matching the extractor's projection format so refreshed embeddings
// read identically to freshly-extracted ones.
func memoryEmbedText(kind, content string) string {
	if kind == "" {
		return content
	}
	return strings.ToUpper(kind[:1]) + kind[1:] + ": " + content
}

// DeleteMemoryBySourceAsync soft-deletes all memory items derived from
// (sourceType, sourceUUID) and drops their embeddings. Fire-and-forget
// with its own bounded context so it never blocks the deletion request.
// No-op when nothing was extracted from the source.
func DeleteMemoryBySourceAsync(sourceType, sourceUUID string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		ids, err := memoryModels.DeleteBySource(ctx, sourceType, sourceUUID)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "memory cascade delete (%s:%s) failed: %v", sourceType, sourceUUID, err)
			return
		}
		for _, id := range ids {
			// Drop the OpenSearch semantic projection.
			if derr := DeleteEmbedding(ctx, "memory", id.String()); derr != nil {
				helpers.LogErrorWithContext(ctx, "memory cascade: drop embedding %s failed: %v", id, derr)
			}
			// Drop the Dgraph GraphRAG projection (best-effort; graph is an
			// index, not the source of truth).
			if derr := memDgraph.DeleteMemoryItem(ctx, id.String()); derr != nil {
				helpers.LogErrorWithContext(ctx, "memory cascade: drop graph node %s failed: %v", id, derr)
			}
		}
		if len(ids) > 0 {
			helpers.LogInfoWithContext(ctx, "memory cascade: removed %d item(s) for %s:%s", len(ids), sourceType, sourceUUID)
		}
	}()
}

// ArchiveMemoryByScope reversibly soft-deletes (reason='archive') every
// memory item belonging to a scope and drops their projections — the
// scope-level twin of ArchiveMemoryBySources, revived by
// RestoreMemoryByScope. Synchronous + idempotent. Returns the count archived.
func ArchiveMemoryByScope(ctx context.Context, scope memoryModels.ScopeRef) int {
	if scope == (memoryModels.ScopeRef{}) {
		return 0
	}
	ids, err := memoryModels.ArchiveByScope(ctx, scope)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "memory scope archive (%+v) failed: %v", scope, err)
		return 0
	}
	dropMemoryProjections(ctx, ids)
	if len(ids) > 0 {
		helpers.LogInfoWithContext(ctx, "memory scope archive cascade: removed %d item(s) for scope %+v", len(ids), scope)
	}
	return len(ids)
}

// RestoreMemoryByScope revives the archive-removed memory items for a scope
// (never user-deleted ones) and re-projects them. Synchronous + idempotent.
// Returns the count revived.
func RestoreMemoryByScope(ctx context.Context, scope memoryModels.ScopeRef) int {
	if scope == (memoryModels.ScopeRef{}) {
		return 0
	}
	items, err := memoryModels.RestoreByScope(ctx, scope)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "memory scope restore (%+v) failed: %v", scope, err)
		return 0
	}
	for _, it := range items {
		reprojectMemoryItem(ctx, it)
	}
	if len(items) > 0 {
		helpers.LogInfoWithContext(ctx, "memory scope restore cascade: revived %d item(s) for scope %+v", len(items), scope)
	}
	return len(items)
}

// dropMemoryProjections removes the OpenSearch + Dgraph projections for a
// set of memory ids. Best-effort per id so one failure never stalls the
// rest. Shared by the source- and scope-level delete/archive cascades.
func dropMemoryProjections(ctx context.Context, ids []uuid.UUID) {
	for _, id := range ids {
		if derr := DeleteEmbedding(ctx, "memory", id.String()); derr != nil {
			helpers.LogErrorWithContext(ctx, "memory cascade: drop embedding %s failed: %v", id, derr)
		}
		if derr := memDgraph.DeleteMemoryItem(ctx, id.String()); derr != nil {
			helpers.LogErrorWithContext(ctx, "memory cascade: drop graph node %s failed: %v", id, derr)
		}
	}
}

// it reversibly soft-deletes (reason='archive') every memory item captured
// from the given source ids and drops their OpenSearch + Dgraph projections,
// so archived content stops surfacing as AI-queryable facts. Synchronous
// (the archive job already runs in its own goroutine) and idempotent. Only
// manual captures carry a per-message source_uuid, so worker-extracted
// (scope-keyed) items are intentionally untouched — see memoryWorker.go.
//
// Returns the number of memory items archived. Errors are logged and
// swallowed per projection so one bad id never stalls the archive job.
func ArchiveMemoryBySources(ctx context.Context, sourceType string, sourceUUIDs []string) int {
	if sourceType == "" || len(sourceUUIDs) == 0 {
		return 0
	}
	ids, err := memoryModels.ArchiveBySource(ctx, sourceType, sourceUUIDs)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "memory archive cascade (%s, %d sources) failed: %v", sourceType, len(sourceUUIDs), err)
		return 0
	}
	for _, id := range ids {
		if derr := DeleteEmbedding(ctx, "memory", id.String()); derr != nil {
			helpers.LogErrorWithContext(ctx, "memory archive cascade: drop embedding %s failed: %v", id, derr)
		}
		if derr := memDgraph.DeleteMemoryItem(ctx, id.String()); derr != nil {
			helpers.LogErrorWithContext(ctx, "memory archive cascade: drop graph node %s failed: %v", id, derr)
		}
	}
	if len(ids) > 0 {
		helpers.LogInfoWithContext(ctx, "memory archive cascade: removed %d item(s) for %d %s source(s)", len(ids), len(sourceUUIDs), sourceType)
	}
	return len(ids)
}

// RestoreMemoryBySources is the inverse of ArchiveMemoryBySources: it
// revives the memory items the archive cascade removed for the given source
// ids (never items a user hard-deleted) and re-projects them to OpenSearch
// + Dgraph. Synchronous + idempotent. Returns the number revived.
func RestoreMemoryBySources(ctx context.Context, sourceType string, sourceUUIDs []string) int {
	if sourceType == "" || len(sourceUUIDs) == 0 {
		return 0
	}
	items, err := memoryModels.RestoreBySource(ctx, sourceType, sourceUUIDs)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "memory restore cascade (%s, %d sources) failed: %v", sourceType, len(sourceUUIDs), err)
		return 0
	}
	for _, it := range items {
		reprojectMemoryItem(ctx, it)
	}
	if len(items) > 0 {
		helpers.LogInfoWithContext(ctx, "memory restore cascade: revived %d item(s) for %d %s source(s)", len(items), len(sourceUUIDs), sourceType)
	}
	return len(items)
}
