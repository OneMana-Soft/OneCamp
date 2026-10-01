package models

// Postgres access for the Workspace Memory Layer (migration 65).
//
// Memory items are structured facts (decisions, commitments, open
// questions, glossary terms) extracted from workspace content by AI
// agents. This package is the single gateway to the table; retrieval is
// always permission-filtered by the caller's accessible channels/projects
// so memory never crosses an access boundary.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// ErrTombstoned is returned by Upsert when an identical fact was previously
// hard-deleted by a user (a tombstone). Callers treat it as a benign skip:
// the user deliberately removed this fact, so re-extraction must not revive
// it. It is NOT a failure.
var ErrTombstoned = errors.New("memory item tombstoned by prior user delete")

// DedupHash is the canonical, stable hash over (kind + normalized content +
// scope) used to make re-extraction idempotent (one row per logical fact)
// and to recompute the key when a captured item's content is refreshed on
// edit. It lives here so the model is the single source of truth for the
// dedup key; the business extractor delegates to it. Normalization:
// case-folded + whitespace-collapsed content; scope = channel|project|grp.
func DedupHash(kind, content, channelUUID, projectUUID, chatGrpID string) string {
	norm := strings.ToLower(strings.Join(strings.Fields(content), " "))
	scopeKey := channelUUID + "|" + projectUUID + "|" + chatGrpID
	sum := sha256.Sum256([]byte(kind + "\x00" + norm + "\x00" + scopeKey))
	return hex.EncodeToString(sum[:])
}

// Kinds — keep aligned with the CHECK constraint in migration 65.
const (
	KindDecision   = "decision"
	KindCommitment = "commitment"
	KindQuestion   = "question"
	KindGlossary   = "glossary"
)

// Statuses — keep aligned with the CHECK constraint.
const (
	StatusOpen       = "open"
	StatusResolved   = "resolved"
	StatusSuperseded = "superseded"
	StatusDismissed  = "dismissed"
)

// Source types.
const (
	SourcePost       = "post"
	SourceChat       = "chat"
	SourceDoc        = "doc"
	SourceTranscript = "transcript"
	SourceRecap      = "recap"
)

// Soft-delete reasons (deleted_reason column). Distinguishes a permanent
// user delete from a reversible archive sweep, so restore can revive ONLY
// archive-removed items. Empty/NULL == user/legacy delete.
const (
	DeleteReasonArchive = "archive"
)

// MemoryItem is the in-memory form of a workspace_memory_items row.
type MemoryItem struct {
	ID          uuid.UUID  `json:"id"`
	Kind        string     `json:"kind"`
	Content     string     `json:"content"`
	Status      string     `json:"status"`
	OwnerID     *uuid.UUID `json:"owner_user_id,omitempty"`
	DueAt       *time.Time `json:"due_at,omitempty"`
	ChannelUUID *uuid.UUID `json:"channel_uuid,omitempty"`
	ProjectUUID *uuid.UUID `json:"project_uuid,omitempty"`
	ChatGrpID   string     `json:"chat_grp_id,omitempty"`
	TeamUUID    *uuid.UUID `json:"team_uuid,omitempty"`
	SourceType  string     `json:"source_type"`
	SourceUUID  string     `json:"source_uuid,omitempty"`
	DedupHash   string     `json:"-"`
	Confidence  int        `json:"confidence"`
	CreatedBy   *uuid.UUID `json:"created_by_user_id,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// UpsertInput is the write shape for an extracted item. Scope pointers are
// nil when not applicable.
type UpsertInput struct {
	Kind        string
	Content     string
	Status      string
	OwnerID     *uuid.UUID
	DueAt       *time.Time
	ChannelUUID *uuid.UUID
	ProjectUUID *uuid.UUID
	ChatGrpID   string
	TeamUUID    *uuid.UUID
	SourceType  string
	SourceUUID  string
	DedupHash   string
	Confidence  int
	CreatedBy   *uuid.UUID
	// ForceRevive overrides the tombstone guard. Set ONLY for deliberate
	// user actions (manual "save to memory") where re-saving a fact the
	// user previously deleted is an explicit, intentional choice. The
	// automatic extractor/worker path leaves this false so it honors
	// tombstones.
	ForceRevive bool
}

// Upsert inserts a memory item or, on dedup_hash conflict, refreshes its
// mutable fields (content/status/owner/due/confidence/updated_at). This
// makes re-extraction of the same conversation idempotent.
func Upsert(ctx context.Context, in UpsertInput) (uuid.UUID, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	if in.Status == "" {
		in.Status = StatusOpen
	}
	if in.Confidence <= 0 {
		in.Confidence = 70
	}
	if in.Confidence > 100 {
		in.Confidence = 100
	}

	// Dedup-hash reconciliation. The unique index permits at most ONE live
	// row per hash but any number of soft-deleted ones, so we resolve the
	// hash to a single authoritative row, preferring a live row:
	//
	//   • a LIVE row exists  → fall through to INSERT…ON CONFLICT, which
	//     updates that row. (No tombstone logic — the fact is already active.)
	//   • only DELETED rows  → apply the tombstone rules:
	//       – user tombstone (non-archive) + automatic extraction →
	//         ErrTombstoned (honor the user's deliberate delete; don't revive).
	//       – archive tombstone, OR a deliberate manual re-capture
	//         (ForceRevive) → revive that row in place (keeps its id, clears
	//         the tombstone) rather than inserting a duplicate.
	if in.DedupHash != "" {
		id, isLive, reason, found, herr := findByHash(cctx, in.DedupHash)
		if herr == nil && found && !isLive {
			userTombstone := reason != DeleteReasonArchive
			if userTombstone && !in.ForceRevive {
				return uuid.Nil, ErrTombstoned
			}
			if rerr := reviveDeleted(cctx, id, in); rerr != nil {
				// A unique-violation here means a live row with this hash was
				// created concurrently; the fact is already active, so treat
				// the revive as satisfied rather than failing the caller.
				if isUniqueViolation(rerr) {
					if liveID, ok, _ := findLiveByHash(cctx, in.DedupHash); ok {
						return liveID, nil
					}
				}
				return uuid.Nil, rerr
			}
			return id, nil
		}
	}

	const q = `
		INSERT INTO workspace_memory_items (
			kind, content, status, owner_user_id, due_at,
			channel_uuid, project_uuid, chat_grp_id, team_uuid,
			source_type, source_uuid, dedup_hash, confidence, created_by_user_id
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		ON CONFLICT (dedup_hash) WHERE deleted_at IS NULL
		DO UPDATE SET
			content    = EXCLUDED.content,
			-- NEVER let re-extraction clobber a user-set lifecycle status.
			-- Extraction always proposes 'open'; if the user has resolved,
			-- dismissed, or superseded an item, re-seeing the same fact must
			-- NOT silently reopen it. Status is owned exclusively by explicit
			-- user action (UpdateStatus). This is the durability guarantee
			-- that makes Resolve/Dismiss stick across worker passes.
			status     = workspace_memory_items.status,
			owner_user_id = COALESCE(EXCLUDED.owner_user_id, workspace_memory_items.owner_user_id),
			due_at     = COALESCE(EXCLUDED.due_at, workspace_memory_items.due_at),
			confidence = EXCLUDED.confidence,
			updated_at = NOW()
		RETURNING id`

	var id uuid.UUID
	err := postgresInit.DBConn.SqlDB.QueryRowContext(cctx, q,
		in.Kind, in.Content, in.Status, in.OwnerID, in.DueAt,
		in.ChannelUUID, in.ProjectUUID, nullStr(in.ChatGrpID), in.TeamUUID,
		in.SourceType, nullStr(in.SourceUUID), in.DedupHash, in.Confidence, in.CreatedBy,
	).Scan(&id)
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/workspaceMemory Upsert failed: %+v", err)
		return uuid.Nil, err
	}
	return id, nil
}

// QueryFilter scopes a list query. Empty slices/strings mean "no
// constraint on that dimension". Permission filtering is the caller's
// responsibility: pass the user's accessible channel/project UUIDs.
type QueryFilter struct {
	Kinds              []string
	Statuses           []string
	AccessibleChannels []string
	AccessibleProjects []string
	AccessibleGrpIDs   []string
	OwnerID            *uuid.UUID
	Limit              int
}

// List returns memory items visible under the permission filter, newest
// first. An item is visible if it is scoped to one of the caller's
// accessible channels/projects/groups, OR owned by the caller. Items with
// no scope at all are treated as workspace-wide readable (rare; only
// glossary tends to be unscoped) — callers that want strict scoping should
// not request the glossary kind.
func List(ctx context.Context, f QueryFilter) ([]*MemoryItem, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}

	var where []string
	var args []any
	n := 1

	where = append(where, "deleted_at IS NULL")

	if len(f.Kinds) > 0 {
		ph, vals := inClause(&n, f.Kinds)
		where = append(where, "kind IN ("+ph+")")
		args = append(args, vals...)
	}
	if len(f.Statuses) > 0 {
		ph, vals := inClause(&n, f.Statuses)
		where = append(where, "status IN ("+ph+")")
		args = append(args, vals...)
	}

	// Permission OR-group: visible if scoped to an accessible resource,
	// or owned by the caller.
	var perm []string
	if len(f.AccessibleChannels) > 0 {
		ph, vals := inClause(&n, f.AccessibleChannels)
		perm = append(perm, "channel_uuid::text IN ("+ph+")")
		args = append(args, vals...)
	}
	if len(f.AccessibleProjects) > 0 {
		ph, vals := inClause(&n, f.AccessibleProjects)
		perm = append(perm, "project_uuid::text IN ("+ph+")")
		args = append(args, vals...)
	}
	if len(f.AccessibleGrpIDs) > 0 {
		ph, vals := inClause(&n, f.AccessibleGrpIDs)
		perm = append(perm, "chat_grp_id IN ("+ph+")")
		args = append(args, vals...)
	}
	if f.OwnerID != nil {
		perm = append(perm, fmt.Sprintf("owner_user_id = $%d", n))
		args = append(args, *f.OwnerID)
		n++
	}
	if len(perm) == 0 {
		// No accessible scope at all → return nothing rather than leak.
		return []*MemoryItem{}, nil
	}
	where = append(where, "("+strings.Join(perm, " OR ")+")")

	query := `
		SELECT id, kind, content, status, owner_user_id, due_at,
		       channel_uuid, project_uuid, chat_grp_id, team_uuid,
		       source_type, source_uuid, confidence, created_by_user_id,
		       created_at, updated_at
		FROM workspace_memory_items
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY created_at DESC
		LIMIT ` + fmt.Sprintf("%d", limit)

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(cctx, query, args...)
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/workspaceMemory List failed: %+v", err)
		return nil, err
	}
	defer rows.Close()

	var out []*MemoryItem
	for rows.Next() {
		item, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// UpdateStatus changes an item's lifecycle status (resolve/dismiss/etc).
func UpdateStatus(ctx context.Context, id uuid.UUID, status string) error {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	res, err := postgresInit.DBConn.SqlDB.ExecContext(cctx,
		`UPDATE workspace_memory_items SET status = $1, updated_at = NOW()
		 WHERE id = $2 AND deleted_at IS NULL`, status, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("memory item not found")
	}
	return nil
}

// UpdateDueDate sets (or clears, when dueAt is nil) a commitment's due date.
// A nil dueAt writes SQL NULL so a user can remove a deadline. updated_at is
// bumped so the staleness window restarts on an explicit edit.
func UpdateDueDate(ctx context.Context, id uuid.UUID, dueAt *time.Time) error {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	res, err := postgresInit.DBConn.SqlDB.ExecContext(cctx,
		`UPDATE workspace_memory_items SET due_at = $1, updated_at = NOW()
		 WHERE id = $2 AND deleted_at IS NULL`, dueAt, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("memory item not found")
	}
	return nil
}

// ClearOwner detaches a stale owner from an item (sets owner_user_id NULL).
// Used by the nudge engine when it detects the owner has left the item's
// scope: the item should no longer be addressed to them. Deliberately does
// NOT bump updated_at — clearing ownership is a system reconciliation, not
// user activity, and shouldn't reset the staleness window. Idempotent: a
// no-op (already NULL or missing) returns nil.
func ClearOwner(ctx context.Context, id uuid.UUID) error {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(cctx,
		`UPDATE workspace_memory_items SET owner_user_id = NULL
		 WHERE id = $1 AND owner_user_id IS NOT NULL AND deleted_at IS NULL`, id)
	return err
}

// SoftDelete dismisses an item (reversible audit-friendly delete).
func SoftDelete(ctx context.Context, id uuid.UUID) error {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	res, err := postgresInit.DBConn.SqlDB.ExecContext(cctx,
		`UPDATE workspace_memory_items SET deleted_at = NOW(), updated_at = NOW()
		 WHERE id = $1 AND deleted_at IS NULL`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("memory item not found")
	}
	return nil
}

// DeleteBySource soft-deletes all memory items derived from a given source
// (e.g. a post/chat/doc that the user deleted). Returns the ids removed so
// the caller can also drop their OpenSearch projections. This is the
// correctness + privacy cascade: deleted content must not linger in memory.
func DeleteBySource(ctx context.Context, sourceType, sourceUUID string) ([]uuid.UUID, error) {
	if sourceType == "" || sourceUUID == "" {
		return nil, nil
	}
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	const q = `
		UPDATE workspace_memory_items
		SET deleted_at = NOW(), updated_at = NOW()
		WHERE source_type = $1 AND source_uuid = $2 AND deleted_at IS NULL
		RETURNING id`

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(cctx, q, sourceType, sourceUUID)
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/workspaceMemory DeleteBySource failed: %+v", err)
		return nil, err
	}
	defer rows.Close()

	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return ids, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// findByHash resolves a dedup_hash to a single authoritative row, preferring
// a LIVE (deleted_at IS NULL) row over soft-deleted ones. Returns the row id,
// whether it's live, and (for a deleted row) its delete reason. The unique
// index guarantees at most one live row, so "prefer live, else most-recently
// deleted" is well-defined. Used by Upsert's dedup reconciliation.
func findByHash(ctx context.Context, dedupHash string) (id uuid.UUID, isLive bool, reason string, found bool, err error) {
	if dedupHash == "" {
		return uuid.Nil, false, "", false, nil
	}
	// Order so a live row (deleted_at IS NULL) sorts first; among deleted
	// rows, the most recently deleted wins.
	const q = `
		SELECT id, (deleted_at IS NULL) AS is_live, COALESCE(deleted_reason, '')
		FROM workspace_memory_items
		WHERE dedup_hash = $1
		ORDER BY (deleted_at IS NULL) DESC, deleted_at DESC NULLS FIRST
		LIMIT 1`
	row := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, q, dedupHash)
	if serr := row.Scan(&id, &isLive, &reason); serr != nil {
		if serr == sql.ErrNoRows {
			return uuid.Nil, false, "", false, nil
		}
		return uuid.Nil, false, "", false, serr
	}
	return id, isLive, reason, true, nil
}

// findLiveByHash returns the id of the single live row for a hash, if any.
// Used as a fallback when a revive races a concurrent insert.
func findLiveByHash(ctx context.Context, dedupHash string) (uuid.UUID, bool, error) {
	if dedupHash == "" {
		return uuid.Nil, false, nil
	}
	const q = `
		SELECT id FROM workspace_memory_items
		WHERE dedup_hash = $1 AND deleted_at IS NULL
		LIMIT 1`
	var id uuid.UUID
	if serr := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, q, dedupHash).Scan(&id); serr != nil {
		if serr == sql.ErrNoRows {
			return uuid.Nil, false, nil
		}
		return uuid.Nil, false, serr
	}
	return id, true, nil
}

// reviveDeleted clears the tombstone on a soft-deleted row and refreshes its
// mutable fields from the upsert input, re-opening it. Used when a user
// deliberately re-captures a previously-deleted fact (ForceRevive) or when an
// archive-tombstoned row is re-extracted. Guarded by the caller against the
// live-row case so it never collides with the partial unique index — except
// under a concurrent insert race, which the caller handles.
func reviveDeleted(ctx context.Context, id uuid.UUID, in UpsertInput) error {
	const q = `
		UPDATE workspace_memory_items
		SET deleted_at = NULL, deleted_reason = NULL,
		    content = $2, status = $3,
		    owner_user_id = COALESCE($4, owner_user_id),
		    due_at = COALESCE($5, due_at),
		    confidence = $6, updated_at = NOW()
		WHERE id = $1 AND deleted_at IS NOT NULL`
	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, q,
		id, in.Content, in.Status, in.OwnerID, in.DueAt, in.Confidence)
	if err != nil && !isUniqueViolation(err) {
		helpers.LogErrorWithContext(ctx, "models/workspaceMemory reviveDeleted failed: %+v", err)
	}
	return err
}

// ScopeRef identifies a permission scope a memory item can belong to. Used
// by the scope-level cascades so deleting/archiving an ENTIRE channel,
// project, or DM/group removes the facts derived from it — catching both
// manual captures (keyed by message uuid) AND worker-extracted items (keyed
// by scope), which a per-source_uuid cascade alone misses.
type ScopeRef struct {
	ChannelUUID string
	ProjectUUID string
	ChatGrpID   string
}

// scopeWhere builds the WHERE predicate + arg for a single scope dimension,
// starting placeholders at $start. Returns "" when the ref is empty.
func (s ScopeRef) scopeWhere(start int) (string, any) {
	switch {
	case s.ChannelUUID != "":
		return fmt.Sprintf("channel_uuid::text = $%d", start), s.ChannelUUID
	case s.ProjectUUID != "":
		return fmt.Sprintf("project_uuid::text = $%d", start), s.ProjectUUID
	case s.ChatGrpID != "":
		return fmt.Sprintf("chat_grp_id = $%d", start), s.ChatGrpID
	}
	return "", nil
}

// DeleteByScope soft-deletes (permanent, user-delete semantics) every live
// memory item belonging to a scope (channel/project/group). Returns the
// removed ids so the caller can drop projections. Used when an entire scope
// is deleted by a user. Unlike DeleteBySource this matches the scope COLUMN,
// so it removes worker-extracted (scope-keyed) facts too.
func DeleteByScope(ctx context.Context, scope ScopeRef) ([]uuid.UUID, error) {
	pred, arg := scope.scopeWhere(1)
	if pred == "" {
		return nil, nil
	}
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	q := `
		UPDATE workspace_memory_items
		SET deleted_at = NOW(), updated_at = NOW()
		WHERE ` + pred + ` AND deleted_at IS NULL
		RETURNING id`

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(cctx, q, arg)
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/workspaceMemory DeleteByScope failed: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if serr := rows.Scan(&id); serr != nil {
			return ids, serr
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ArchiveByScope reversibly soft-deletes (reason='archive') every live
// memory item belonging to a scope — the scope-level twin of
// ArchiveBySource, revived by RestoreByScope. Returns removed ids.
func ArchiveByScope(ctx context.Context, scope ScopeRef) ([]uuid.UUID, error) {
	pred, arg := scope.scopeWhere(2) // $1 reserved for reason
	if pred == "" {
		return nil, nil
	}
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout*10)
	defer cancel()

	q := `
		UPDATE workspace_memory_items
		SET deleted_at = NOW(), deleted_reason = $1, updated_at = NOW()
		WHERE ` + pred + ` AND deleted_at IS NULL
		RETURNING id`

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(cctx, q, DeleteReasonArchive, arg)
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/workspaceMemory ArchiveByScope failed: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if serr := rows.Scan(&id); serr != nil {
			return ids, serr
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// RestoreByScope revives the archive-removed (reason='archive') memory items
// for a scope — the inverse of ArchiveByScope. Never touches user-deleted
// (NULL-reason) rows. Returns the revived items so the caller can
// re-project them.
func RestoreByScope(ctx context.Context, scope ScopeRef) ([]*MemoryItem, error) {
	pred, arg := scope.scopeWhere(2) // $1 reserved for reason
	if pred == "" {
		return nil, nil
	}
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout*10)
	defer cancel()

	q := `
		UPDATE workspace_memory_items
		SET deleted_at = NULL, deleted_reason = NULL, updated_at = NOW()
		WHERE ` + pred + ` AND deleted_reason = $1 AND deleted_at IS NOT NULL
		RETURNING id, kind, content, status, owner_user_id, due_at,
		          channel_uuid, project_uuid, chat_grp_id, team_uuid,
		          source_type, source_uuid, confidence, created_by_user_id,
		          created_at, updated_at`

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(cctx, q, DeleteReasonArchive, arg)
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/workspaceMemory RestoreByScope failed: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var out []*MemoryItem
	for rows.Next() {
		it, serr := scanItem(rows)
		if serr != nil {
			return out, serr
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// item whose source is in sourceUUIDs for the given sourceType. Mirrors
// DeleteBySource but is REVERSIBLE via RestoreBySource: the reason marker
// lets restore revive only what the archive cascade removed, never an item
// a user independently hard-deleted. Returns the removed ids so the caller
// can drop their OpenSearch/Dgraph projections. Bulk-friendly (archive
// operates over many ids at once); a nil/empty input is a no-op. The id
// list is chunked so a huge archive job can't exceed PG's bind-param limit.
func ArchiveBySource(ctx context.Context, sourceType string, sourceUUIDs []string) ([]uuid.UUID, error) {
	if sourceType == "" || len(sourceUUIDs) == 0 {
		return nil, nil
	}

	var all []uuid.UUID
	err := chunkStrings(sourceUUIDs, memorySourceChunkSize, func(batch []string) error {
		cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout*10)
		defer cancel()

		// $1=sourceType, $2=reason, then the source_uuid IN-list from $3.
		ph, args := inClauseFrom(3, batch)
		args = append([]any{sourceType, DeleteReasonArchive}, args...)

		q := `
			UPDATE workspace_memory_items
			SET deleted_at = NOW(), deleted_reason = $2, updated_at = NOW()
			WHERE source_type = $1 AND source_uuid IN (` + ph + `) AND deleted_at IS NULL
			RETURNING id`

		rows, qerr := postgresInit.DBConn.SqlDB.QueryContext(cctx, q, args...)
		if qerr != nil {
			helpers.LogErrorWithContext(cctx, "models/workspaceMemory ArchiveBySource failed: %+v", qerr)
			return qerr
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			if serr := rows.Scan(&id); serr != nil {
				return serr
			}
			all = append(all, id)
		}
		return rows.Err()
	})
	return all, err
}

// RestoreBySource revives memory items previously removed by the ARCHIVE
// cascade (deleted_reason='archive') for the given source ids — the inverse
// of ArchiveBySource. Items hard-deleted by a user (NULL reason) are left
// untouched, so an undo can't resurrect content a user deliberately deleted.
// Clears the reason marker on revive. Returns the revived items so the
// caller can re-project them to OpenSearch + Dgraph. Chunked like the
// archive twin.
func RestoreBySource(ctx context.Context, sourceType string, sourceUUIDs []string) ([]*MemoryItem, error) {
	if sourceType == "" || len(sourceUUIDs) == 0 {
		return nil, nil
	}

	var out []*MemoryItem
	err := chunkStrings(sourceUUIDs, memorySourceChunkSize, func(batch []string) error {
		cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout*10)
		defer cancel()

		ph, args := inClauseFrom(3, batch)
		args = append([]any{sourceType, DeleteReasonArchive}, args...)

		q := `
			UPDATE workspace_memory_items
			SET deleted_at = NULL, deleted_reason = NULL, updated_at = NOW()
			WHERE source_type = $1 AND deleted_reason = $2
			  AND source_uuid IN (` + ph + `) AND deleted_at IS NOT NULL
			RETURNING id, kind, content, status, owner_user_id, due_at,
			          channel_uuid, project_uuid, chat_grp_id, team_uuid,
			          source_type, source_uuid, confidence, created_by_user_id,
			          created_at, updated_at`

		rows, qerr := postgresInit.DBConn.SqlDB.QueryContext(cctx, q, args...)
		if qerr != nil {
			helpers.LogErrorWithContext(cctx, "models/workspaceMemory RestoreBySource failed: %+v", qerr)
			return qerr
		}
		defer rows.Close()
		for rows.Next() {
			it, serr := scanItem(rows)
			if serr != nil {
				return serr
			}
			out = append(out, it)
		}
		return rows.Err()
	})
	return out, err
}

// memorySourceChunkSize bounds how many source ids go into a single IN-list
// so a large archive/restore can't exceed PG's bind-parameter limit.
const memorySourceChunkSize = 1000

// chunkStrings invokes fn over successive slices of s, each at most size
// elements. Stops and returns on the first error.
func chunkStrings(s []string, size int, fn func([]string) error) error {
	if size <= 0 {
		size = 1000
	}
	for i := 0; i < len(s); i += size {
		end := i + size
		if end > len(s) {
			end = len(s)
		}
		if err := fn(s[i:end]); err != nil {
			return err
		}
	}
	return nil
}

// inClauseFrom builds "$start,$start+1,..." placeholders for an IN list and
// returns the value slice, starting numbering at `start`. Used when leading
// positional args precede the IN-list.
func inClauseFrom(start int, vals []string) (string, []any) {
	ph := make([]string, len(vals))
	out := make([]any, len(vals))
	for i, v := range vals {
		ph[i] = fmt.Sprintf("$%d", start+i)
		out[i] = v
	}
	return strings.Join(ph, ","), out
}

// memory items linked to a manual-capture source (source_type, sourceUUID)
// to newContent, recomputing the dedup_hash so idempotency stays intact.
// Returns the refreshed items so the caller can re-project the OpenSearch +
// Dgraph copies (keeping all three stores consistent).
//
// Used on message EDIT: a captured item stores a point-in-time snapshot of
// the message text, so when the source is edited we refresh the snapshot in
// place rather than dropping the user's deliberate save. Privacy is
// preserved (the old text is overwritten everywhere); staleness is fixed.
// A source_uuid match is by construction a manual capture (the batched
// worker keys items by scope, never by a message uuid), so refreshing the
// verbatim content is exactly right.
//
// If recomputing the hash would collide with an existing live item (e.g.
// the edit makes this capture identical to another capture in the same
// scope), that row is skipped — the unique index protects against
// duplicates and the surviving row already carries the same fact.
func RefreshContentBySource(ctx context.Context, sourceType, sourceUUID, newContent string) ([]*MemoryItem, error) {
	if sourceType == "" || sourceUUID == "" {
		return nil, nil
	}
	newContent = strings.TrimSpace(newContent)
	if newContent == "" {
		// An edit that empties the captured text is treated as a redaction:
		// nothing to refresh to, so the caller should fall back to delete.
		return nil, nil
	}
	if len(newContent) > 1000 {
		newContent = newContent[:1000]
	}

	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	// Load the live items for this source so we can recompute each item's
	// scope-aware dedup hash (the hash depends on kind + content + scope).
	const sel = `
		SELECT id, kind, content, status, owner_user_id, due_at,
		       channel_uuid, project_uuid, chat_grp_id, team_uuid,
		       source_type, source_uuid, confidence, created_by_user_id,
		       created_at, updated_at
		FROM workspace_memory_items
		WHERE source_type = $1 AND source_uuid = $2 AND deleted_at IS NULL`

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(cctx, sel, sourceType, sourceUUID)
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/workspaceMemory RefreshContentBySource select failed: %+v", err)
		return nil, err
	}
	var items []*MemoryItem
	for rows.Next() {
		it, serr := scanItem(rows)
		if serr != nil {
			rows.Close()
			return nil, serr
		}
		items = append(items, it)
	}
	if cerr := rows.Err(); cerr != nil {
		rows.Close()
		return nil, cerr
	}
	rows.Close()

	if len(items) == 0 {
		return nil, nil
	}

	refreshed := make([]*MemoryItem, 0, len(items))
	for _, it := range items {
		// No-op if the captured content is already current (idempotent: an
		// edit that didn't change the captured text, or a re-fired event).
		if it.Content == newContent {
			refreshed = append(refreshed, it)
			continue
		}
		var chUUID, prUUID string
		if it.ChannelUUID != nil {
			chUUID = it.ChannelUUID.String()
		}
		if it.ProjectUUID != nil {
			prUUID = it.ProjectUUID.String()
		}
		newHash := DedupHash(it.Kind, newContent, chUUID, prUUID, it.ChatGrpID)

		res, uerr := postgresInit.DBConn.SqlDB.ExecContext(cctx,
			`UPDATE workspace_memory_items
			 SET content = $1, dedup_hash = $2, updated_at = NOW()
			 WHERE id = $3 AND deleted_at IS NULL`,
			newContent, newHash, it.ID)
		if uerr != nil {
			// Most likely a unique-index collision: the edited capture now
			// matches another live item in the same scope. Skip it; the
			// surviving row carries the same fact. Drop this redundant one
			// so the stale snapshot doesn't linger.
			if isUniqueViolation(uerr) {
				if _, derr := postgresInit.DBConn.SqlDB.ExecContext(cctx,
					`UPDATE workspace_memory_items SET deleted_at = NOW(), updated_at = NOW()
					 WHERE id = $1 AND deleted_at IS NULL`, it.ID); derr != nil {
					helpers.LogErrorWithContext(cctx, "models/workspaceMemory RefreshContentBySource collide-drop failed for %s: %+v", it.ID, derr)
				}
				continue
			}
			helpers.LogErrorWithContext(cctx, "models/workspaceMemory RefreshContentBySource update failed for %s: %+v", it.ID, uerr)
			return refreshed, uerr
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		it.Content = newContent
		it.DedupHash = newHash
		refreshed = append(refreshed, it)
	}
	return refreshed, nil
}

// isUniqueViolation detects PG error code 23505 portably (lib/pq surfaces
// the code or "duplicate key value" in the message), without importing a
// driver-specific type into this model.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "23505") || strings.Contains(msg, "duplicate key value")
}

// DigestItem is a memory item plus its owner, for the proactive digest.
type DigestItem struct {
	OwnerID uuid.UUID
	Item    *MemoryItem
}

// ListActionableForDigest returns OPEN memory items that warrant a proactive
// nudge, across ALL owners, in one query:
//   - commitments with an owner AND a due date that has passed
//     (overdueBefore), OR
//   - commitments/questions with an owner that have been open and untouched
//     since staleBefore (no movement → worth resurfacing).
//
// Only items with a non-null owner_user_id are returned (the digest is
// owner-addressed). The caller groups by OwnerID and applies per-recipient
// permission/scoping implicitly: an item's owner is, by construction, a
// participant of its scope. Bounded by limit to protect the digest worker.
func ListActionableForDigest(ctx context.Context, overdueBefore, staleBefore time.Time, limit int) ([]DigestItem, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	if limit <= 0 || limit > 5000 {
		limit = 2000
	}

	q := `
		SELECT id, kind, content, status, owner_user_id, due_at,
		       channel_uuid, project_uuid, chat_grp_id, team_uuid,
		       source_type, source_uuid, confidence, created_by_user_id,
		       created_at, updated_at
		FROM workspace_memory_items
		WHERE deleted_at IS NULL
		  AND status = $1
		  AND owner_user_id IS NOT NULL
		  AND (
		        (kind = $2 AND due_at IS NOT NULL AND due_at < $3)
		     OR (kind IN ($2, $4) AND updated_at < $5)
		  )
		ORDER BY owner_user_id, due_at NULLS LAST, created_at
		LIMIT ` + fmt.Sprintf("%d", limit)

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(cctx, q,
		StatusOpen, KindCommitment, overdueBefore, KindQuestion, staleBefore)
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/workspaceMemory ListActionableForDigest failed: %+v", err)
		return nil, err
	}
	defer rows.Close()

	var out []DigestItem
	for rows.Next() {
		item, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		if item.OwnerID == nil {
			continue
		}
		out = append(out, DigestItem{OwnerID: *item.OwnerID, Item: item})
	}
	return out, rows.Err()
}

// GetByID returns a single item (used to authorize status/delete actions).
func GetByID(ctx context.Context, id uuid.UUID) (*MemoryItem, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	const q = `
		SELECT id, kind, content, status, owner_user_id, due_at,
		       channel_uuid, project_uuid, chat_grp_id, team_uuid,
		       source_type, source_uuid, confidence, created_by_user_id,
		       created_at, updated_at
		FROM workspace_memory_items
		WHERE id = $1 AND deleted_at IS NULL`

	row := postgresInit.DBConn.SqlDB.QueryRowContext(cctx, q, id)
	item, err := scanItem(row)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("memory item not found")
	}
	return item, err
}

// CountByScope returns how many open items of each kind exist for a scope.
// Used by the "what does my workspace know" surface for headline counts.
func CountByScope(ctx context.Context, f QueryFilter) (map[string]int, error) {
	items, err := List(ctx, QueryFilter{
		Statuses:           []string{StatusOpen},
		AccessibleChannels: f.AccessibleChannels,
		AccessibleProjects: f.AccessibleProjects,
		AccessibleGrpIDs:   f.AccessibleGrpIDs,
		OwnerID:            f.OwnerID,
		Limit:              500,
	})
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, it := range items {
		counts[it.kind()]++
	}
	return counts, nil
}

func (m *MemoryItem) kind() string { return m.Kind }

// --- helpers ---

type scannable interface {
	Scan(dest ...any) error
}

func scanItem(s scannable) (*MemoryItem, error) {
	var m MemoryItem
	var ownerID, projectID, channelID, teamID uuid.NullUUID
	var createdBy uuid.NullUUID
	var dueAt sql.NullTime
	var grpID, sourceUUID sql.NullString
	if err := s.Scan(
		&m.ID, &m.Kind, &m.Content, &m.Status, &ownerID, &dueAt,
		&channelID, &projectID, &grpID, &teamID,
		&m.SourceType, &sourceUUID, &m.Confidence, &createdBy,
		&m.CreatedAt, &m.UpdatedAt,
	); err != nil {
		return nil, err
	}
	if ownerID.Valid {
		m.OwnerID = &ownerID.UUID
	}
	if projectID.Valid {
		m.ProjectUUID = &projectID.UUID
	}
	if channelID.Valid {
		m.ChannelUUID = &channelID.UUID
	}
	if teamID.Valid {
		m.TeamUUID = &teamID.UUID
	}
	if createdBy.Valid {
		m.CreatedBy = &createdBy.UUID
	}
	if dueAt.Valid {
		m.DueAt = &dueAt.Time
	}
	m.ChatGrpID = grpID.String
	m.SourceUUID = sourceUUID.String
	return &m, nil
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// inClause builds "$n,$n+1,..." placeholders for an IN list and returns
// the value slice, advancing *n.
func inClause(n *int, vals []string) (string, []any) {
	ph := make([]string, len(vals))
	out := make([]any, len(vals))
	for i, v := range vals {
		ph[i] = fmt.Sprintf("$%d", *n)
		out[i] = v
		*n++
	}
	return strings.Join(ph, ","), out
}
