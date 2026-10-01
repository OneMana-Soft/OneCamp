package postgresInit

// "Have these rows changed since I last looked?", answered in one cheap query.
//
// This is what the config reconcilers (helpers.ConfigReconciler) poll. It has
// to be cheap because every replica runs it every interval for every cache, and
// it has to be complete because a change it does not see is a replica that
// stays stale. count(*) catches an insert or a hard delete; max(updated_at)
// catches an edit, a toggle, a re-introspection and a soft delete, because every
// write path on the watched tables sets updated_at (checked when this was
// written; a new write path that forgets to is the one way this goes wrong).

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// identifier is what a table or column name is allowed to look like here. The
// names are compile-time constants at every call site, never input, and this
// keeps that true by construction: a name that is not a plain identifier is a
// programming error, refused before it reaches the query.
var identifier = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// RowsFingerprint returns a string that changes whenever any row in any of the
// named tables is inserted, deleted or has its updated_at bumped.
//
// The value is opaque and only ever compared for equality; its shape is not a
// contract.
func RowsFingerprint(ctx context.Context, tables ...string) (string, error) {
	if len(tables) == 0 {
		return "", fmt.Errorf("rows fingerprint: no tables named")
	}
	parts := make([]string, 0, len(tables))
	for _, t := range tables {
		if !identifier.MatchString(t) {
			return "", fmt.Errorf("rows fingerprint: %q is not a plain table name", t)
		}
		parts = append(parts, fmt.Sprintf(
			"(SELECT count(*)::text || '@' || coalesce(max(updated_at)::text, '') FROM %s)", t))
	}
	q := "SELECT " + strings.Join(parts, " || '|' || ")

	if DBConn == nil || DBConn.SqlDB == nil {
		return "", fmt.Errorf("rows fingerprint: no database connection")
	}
	dbCtx, cancel := context.WithTimeout(ctx, DBConn.DBTimeout)
	defer cancel()
	var fp string
	if err := DBConn.SqlDB.QueryRowContext(dbCtx, q).Scan(&fp); err != nil {
		return "", fmt.Errorf("rows fingerprint: %w", err)
	}
	return fp, nil
}
