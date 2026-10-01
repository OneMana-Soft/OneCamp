package domain

// Marketplace template reads — domain layer.
//
// Follows the repo convention: the QUERY is built here (domain) and EXECUTED by
// the model (models/postgres/Marketplace). The template list read takes an
// optional kind filter — the one dynamic decision — so it belongs here. The
// scan-coupled projection (ListColumns/ListJoins) stays defined in the model
// and is composed here, keeping a single source of truth.

import (
	"context"
	"strings"

	model "github.com/akashc777/OneCamp/models/postgres/Marketplace"
)

// ListTemplates returns listed templates, optionally filtered by kind (""=all),
// newest first.
func ListTemplates(ctx context.Context, kind string) ([]*model.Template, error) {
	query := `SELECT ` + model.ListColumns + ` FROM marketplace_templates t` + model.ListJoins + `
		WHERE t.deleted_at IS NULL`
	args := []any{}
	if strings.TrimSpace(kind) != "" {
		query += ` AND t.kind=$1`
		args = append(args, kind)
	}
	query += ` ORDER BY t.created_at DESC LIMIT 1000`
	return model.ExecTemplates(ctx, query, args)
}
