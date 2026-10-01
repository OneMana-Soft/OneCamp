package models

import (
	"context"
	"encoding/json"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// RouteTarget is the allowlisted model one kind of background work runs on.
type RouteTarget struct {
	ProviderID uuid.UUID `json:"provider_id"`
	Model      string    `json:"model"`
}

// GetModelRouting returns the routing table (migration 172). An empty map means
// every purpose uses the workspace default.
func GetModelRouting(ctx context.Context) (map[string]RouteTarget, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	var raw []byte
	if err := postgresInit.DBConn.SqlDB.QueryRowContext(cctx,
		`SELECT model_routing FROM ai_settings WHERE id = 1`).Scan(&raw); err != nil {
		return nil, err
	}
	out := map[string]RouteTarget{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// SetModelRouting replaces the routing table.
func SetModelRouting(ctx context.Context, routes map[string]RouteTarget) error {
	if routes == nil {
		routes = map[string]RouteTarget{}
	}
	b, err := json.Marshal(routes)
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(cctx,
		`UPDATE ai_settings SET model_routing = $1, updated_at = NOW() WHERE id = 1`, b)
	return err
}
