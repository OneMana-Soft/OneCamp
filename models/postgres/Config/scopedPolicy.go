package models

import (
	"context"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// Policy values set for one thing beside the workspace (a project), which beat
// the workspace's own (system_configs) for that thing: business/Policy's scoped
// policies. Migration 338.

// ScopedValues is every policy value set for one scope, by key.
func ScopedValues(ctx context.Context, kind string, id uuid.UUID) (map[string]string, error) {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(c,
		`SELECT key, value FROM policy_scoped_values WHERE scope_kind = $1 AND scope_id = $2`, kind, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// ScopedValuesMany is every policy value set for each of ids (scopes of one
// kind), by id, in one statement; an id with none is missing.
func ScopedValuesMany(ctx context.Context, kind string, ids []uuid.UUID) (map[uuid.UUID]map[string]string, error) {
	out := map[uuid.UUID]map[string]string{}
	if len(ids) == 0 {
		return out, nil
	}
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(c,
		`SELECT scope_id, key, value FROM policy_scoped_values WHERE scope_kind = $1 AND scope_id = ANY($2::uuid[])`, kind, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var k, v string
		if err := rows.Scan(&id, &k, &v); err != nil {
			return nil, err
		}
		if out[id] == nil {
			out[id] = map[string]string{}
		}
		out[id][k] = v
	}
	return out, rows.Err()
}

// SetScopedValue sets a policy's value for one scope, by whom (uuid.Nil for
// nobody in particular).
func SetScopedValue(ctx context.Context, kind string, id uuid.UUID, key, value string, by uuid.UUID) error {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	var setBy any
	if by != uuid.Nil {
		setBy = by
	}
	_, err := postgresInit.DBConn.SqlDB.ExecContext(c, `
		INSERT INTO policy_scoped_values (scope_kind, scope_id, key, value, set_by, set_at)
		VALUES ($1, $2, $3, $4, $5, NOW())
		ON CONFLICT (scope_kind, scope_id, key) DO UPDATE SET value = EXCLUDED.value, set_by = EXCLUDED.set_by, set_at = NOW()`,
		kind, id, key, value, setBy)
	return err
}

// ClearScopedValue takes a policy's value for one scope away, so the
// workspace's applies there again.
func ClearScopedValue(ctx context.Context, kind string, id uuid.UUID, key string) error {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(c,
		`DELETE FROM policy_scoped_values WHERE scope_kind = $1 AND scope_id = $2 AND key = $3`, kind, id, key)
	return err
}
