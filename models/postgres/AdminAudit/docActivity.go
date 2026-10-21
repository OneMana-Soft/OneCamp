package models

// One doc's rows in the record: "AI activity on this doc" (docs programme B9).
//
// Doc AI writes its rows with the doc's uuid in the metadata (business/AI
// docAIRecord.go), under actions that start with "ai.doc.". Migration 213
// indexes exactly that pair, so a doc's history is read without walking the
// whole log.

import (
	"context"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/models/pgexec"
)

// ListDocAIEntries is a doc's rows whose action starts with prefix (which must
// be "ai.doc." for the index to answer), newest first.
func ListDocAIEntries(ctx context.Context, docUUID, prefix string, limit int) ([]*AuditEntry, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := `SELECT ` + auditCols + ` FROM admin_audit_log
		WHERE action LIKE 'ai.doc.%' AND action LIKE $1 AND (metadata ->> 'doc_uuid') = $2
		ORDER BY seq DESC LIMIT $3`
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, q, prefix+"%", docUUID, limit)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/AdminAudit.ListDocAIEntries err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var out []*AuditEntry
	for rows.Next() {
		e, scanErr := scanEntry(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListDocEvents is a doc's newest rows in the "docs" record (business/Follow
// audit.go: shared, locked, published, restored...), newest first, at most
// limit; a row retention redacted is left out. Migration 470's index answers
// it.
func ListDocEvents(ctx context.Context, docUUID string, limit int) ([]*AuditEntry, error) {
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	q := `SELECT ` + auditCols + ` FROM admin_audit_log
		WHERE category = '` + CategoryDocs + `' AND (metadata ->> 'resource_uuid') = $1 AND redacted_at IS NULL
		ORDER BY seq DESC LIMIT $2`
	out, err := pgexec.Query(ctx, q, func(r pgexec.Row) (*AuditEntry, error) { return scanEntry(r) }, docUUID, limit)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/AdminAudit.ListDocEvents err: %+v", err)
		return nil, err
	}
	return out, nil
}
