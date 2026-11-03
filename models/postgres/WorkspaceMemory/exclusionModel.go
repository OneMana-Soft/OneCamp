package models

// Per-scope AI memory exclusions (migration 67). A scope with a live row in
// this table is opted OUT of the memory layer — never extracted, captured, or
// surfaced. This is the trust/control knob for sensitive channels. A lifted
// exclusion stays, marked by who lifted it and when (V-184 head 4a).

import (
	"context"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	softDelete "github.com/akashc777/OneCamp/models/postgres/SoftDelete"
	"github.com/google/uuid"
)

// Exclusion scope types — keep aligned with the CHECK constraint.
const (
	ExclusionChannel = "channel"
	ExclusionProject = "project"
	ExclusionChatGrp = "chat_grp"
)

// IsScopeExcluded reports whether a single scope is opted out. Best-effort:
// on DB error it returns false (fail-open to "included") so a transient
// hiccup never silently drops memory the user expected to keep — the
// exclusion is a deliberate opt-out, and the caller logs the error.
func IsScopeExcluded(ctx context.Context, scopeType, scopeID string) (bool, error) {
	if scopeType == "" || scopeID == "" {
		return false, nil
	}
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	var exists bool
	err := postgresInit.DBConn.SqlDB.QueryRowContext(cctx,
		`SELECT EXISTS(SELECT 1 FROM ai_memory_exclusions WHERE scope_type = $1 AND scope_id = $2 AND `+softDelete.Live("")+`)`,
		scopeType, scopeID).Scan(&exists)
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/WorkspaceMemory IsScopeExcluded failed: %+v", err)
		return false, err
	}
	return exists, nil
}

// SetScopeExcluded excludes a scope, or lifts its exclusion, as who; both are
// idempotent. Lifting keeps the row, marked by who lifted it (V-184 head 4a),
// so who turned memory off and who turned it back on, and when, stay on
// record; excluding the scope again brings the row back, live, by who.
func SetScopeExcluded(ctx context.Context, scopeType, scopeID string, excludedByID *uuid.UUID, excluded bool) error {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	if excluded {
		_, err := postgresInit.DBConn.SqlDB.ExecContext(cctx,
			`INSERT INTO ai_memory_exclusions (scope_type, scope_id, excluded_by)
			 VALUES ($1, $2, $3)
			 ON CONFLICT (scope_type, scope_id) DO UPDATE SET excluded_by = EXCLUDED.excluded_by, created_at = NOW(), deleted_at = NULL, deleted_by = NULL
			 WHERE ai_memory_exclusions.deleted_at IS NOT NULL`,
			scopeType, scopeID, excludedByID)
		if err != nil {
			helpers.LogErrorWithContext(cctx, "models/WorkspaceMemory SetScopeExcluded(add) failed: %+v", err)
		}
		return err
	}
	_, err := postgresInit.DBConn.SqlDB.ExecContext(cctx,
		`UPDATE ai_memory_exclusions SET deleted_at = NOW(), deleted_by = $3 WHERE scope_type = $1 AND scope_id = $2 AND `+softDelete.Live(""),
		scopeType, scopeID, liftedBy(excludedByID))
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/WorkspaceMemory SetScopeExcluded(remove) failed: %+v", err)
	}
	return err
}

// liftedBy is the deleted_by a lift writes: who lifted the exclusion, or
// NULL for nobody (softDelete.By).
func liftedBy(who *uuid.UUID) any {
	if who == nil {
		return nil
	}
	return softDelete.By(*who)
}
