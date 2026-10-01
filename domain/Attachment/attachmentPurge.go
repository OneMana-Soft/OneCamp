package Domain

// The reads and writes behind permanent removal of archived attachments.
// See business/Archive/purge.go for the rule; this file only knows the tables.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	postDgraphModels "github.com/akashc777/OneCamp/models/dgraph/Post"
	OpenSearchModels "github.com/akashc777/OneCamp/models/openSearch/Attachment"
	"github.com/google/uuid"
)

// ArchivedAttachment is the little a purge needs to know about a row.
type ArchivedAttachment struct {
	Id     uuid.UUID
	ObjKey string
}

// ListAttachmentsArchivedBefore returns attachments archived before the cutoff,
// oldest first, at most limit of them.
func ListAttachmentsArchivedBefore(ctx context.Context, cutoff time.Time, limit int) ([]ArchivedAttachment, error) {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbCtx, `
		SELECT id, obj_key FROM attachments
		WHERE deleted_at IS NOT NULL AND deleted_at < $1
		ORDER BY deleted_at ASC
		LIMIT $2`, cutoff, limit)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/ListAttachmentsArchivedBefore failed: %v", err)
		return nil, err
	}
	defer rows.Close()
	var out []ArchivedAttachment
	for rows.Next() {
		var a ArchivedAttachment
		if err := rows.Scan(&a.Id, &a.ObjKey); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ObjKeyHeldElsewhere says whether any row still shows the same file: one
// upload sent to several channels is several rows with one obj_key, and the
// bytes may only go when every one of them is past the cutoff.
func ObjKeyHeldElsewhere(ctx context.Context, objKey string, cutoff time.Time) (bool, error) {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	var held bool
	err := postgresInit.DBConn.SqlDB.QueryRowContext(dbCtx, `
		SELECT EXISTS (
			SELECT 1 FROM attachments
			WHERE obj_key = $1 AND (deleted_at IS NULL OR deleted_at >= $2)
		)`, objKey, cutoff).Scan(&held)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/ObjKeyHeldElsewhere failed: %v", err)
		return true, err
	}
	return held, nil
}

// DeleteArchivedAttachmentRows removes every row for the object that is past
// the cutoff and returns their ids, so the graph and the index can follow.
func DeleteArchivedAttachmentRows(ctx context.Context, objKey string, cutoff time.Time) ([]uuid.UUID, error) {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbCtx, `
		DELETE FROM attachments
		WHERE obj_key = $1 AND deleted_at IS NOT NULL AND deleted_at < $2
		RETURNING id`, objKey, cutoff)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/DeleteArchivedAttachmentRows failed: %v", err)
		return nil, err
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// DeleteAttachmentsEverywhere removes the attachment nodes from the graph and
// the documents from search. Errors are logged, not returned: the rows and the
// bytes are already gone, and a node that outlives them is invisible to every
// query (they all filter on the row), so this is tidying, not correctness.
func DeleteAttachmentsEverywhere(ctx context.Context, ids []uuid.UUID) {
	if len(ids) == 0 {
		return
	}
	var q strings.Builder
	q.WriteString("query {\n")
	var del []string
	for i, id := range ids {
		fmt.Fprintf(&q, "  at%d as var(func: eq(attachment_uuid, %q))\n", i, id.String())
		del = append(del, fmt.Sprintf(`{"uid": "uid(at%d)"}`, i))
	}
	q.WriteString("}")
	if err := postDgraphModels.DeleteByUpsert(ctx, q.String(), "["+strings.Join(del, ",")+"]"); err != nil {
		helpers.LogErrorWithContext(ctx, "domain/DeleteAttachmentsEverywhere dgraph: %v", err)
	}
	for _, id := range ids {
		if err := OpenSearchModels.DeleteAttachmentInOpenSearch(ctx, id.String()); err != nil {
			helpers.LogErrorWithContext(ctx, "domain/DeleteAttachmentsEverywhere opensearch %s: %v", id, err)
		}
	}
}
