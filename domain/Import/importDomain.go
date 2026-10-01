package domain

// Import-pipeline reads — domain layer.
//
// Follows the repo convention: the QUERY is built here (domain) and EXECUTED by
// the model (models/postgres/Import). These are the dynamic reads whose SQL was
// previously assembled inline in the model: the job list (optional provider
// filter), the runnable-chunk existence check (optional chunk-type filter), and
// the two batch id-map resolvers (dynamic source_id IN(...) sets). The
// scan-coupled job projection (JobSelectColumns) stays in the model and is
// composed here. Bulk inserts and the transactional chunk claim remain in the
// model, as writes/claims do.

import (
	"context"
	"fmt"
	"strings"

	model "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/google/uuid"
)

// ListJobs returns up to limit jobs ordered by created_at desc. providerFilter
// empty means all providers.
func ListJobs(ctx context.Context, providerFilter string, limit int) ([]*model.Job, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	query := `SELECT ` + model.JobSelectColumns + ` FROM import_jobs `
	args := []interface{}{}
	if providerFilter != "" {
		args = append(args, providerFilter)
		query += `WHERE provider = $1 `
	}
	args = append(args, limit)
	query += fmt.Sprintf(`ORDER BY created_at DESC LIMIT $%d`, len(args))
	return model.ExecJobs(ctx, query, args)
}

// HasPendingWork returns true if any chunk for the job (optionally filtered by
// type) is still runnable.
func HasPendingWork(ctx context.Context, importId uuid.UUID, chunkTypes []string) (bool, error) {
	args := []interface{}{importId}
	clause := ""
	if len(chunkTypes) > 0 {
		ps := make([]string, 0, len(chunkTypes))
		for _, t := range chunkTypes {
			args = append(args, t)
			ps = append(ps, fmt.Sprintf("$%d", len(args)))
		}
		clause = " AND chunk_type IN (" + strings.Join(ps, ",") + ")"
	}
	query := fmt.Sprintf(`
		SELECT COUNT(*) FROM import_chunks
		WHERE import_id = $1
		  AND status IN ('pending','in_progress','failed')
		  AND attempts < max_attempts
		  %s`, clause)
	n, err := model.ExecCount(ctx, query, args)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// LookupIdMappingsBatch resolves N source ids (within one import) in one
// round-trip to a source_id -> OneCamp uuid map.
func LookupIdMappingsBatch(ctx context.Context, importId uuid.UUID, entityType string, sourceIds []string) (map[string]uuid.UUID, error) {
	if len(sourceIds) == 0 {
		return map[string]uuid.UUID{}, nil
	}
	args := make([]interface{}, 0, len(sourceIds)+2)
	args = append(args, importId, entityType)
	ps := make([]string, 0, len(sourceIds))
	for _, s := range sourceIds {
		args = append(args, s)
		ps = append(ps, fmt.Sprintf("$%d", len(args)))
	}
	query := fmt.Sprintf(`
		SELECT source_id, onecamp_uuid FROM import_id_map
		WHERE import_id = $1 AND entity_type = $2 AND source_id IN (%s)`,
		strings.Join(ps, ","))
	return model.ExecSourceIdMap(ctx, query, args, len(sourceIds))
}

// LookupWorkspaceMappingsBatch is the cross-import batch resolver: it resolves N
// source ids for a (provider, workspace, entityType) to a source_id -> OneCamp
// uuid map in one round-trip.
func LookupWorkspaceMappingsBatch(ctx context.Context, provider, workspace, entityType string, sourceIds []string) (map[string]uuid.UUID, error) {
	if len(sourceIds) == 0 {
		return map[string]uuid.UUID{}, nil
	}
	args := make([]interface{}, 0, len(sourceIds)+3)
	args = append(args, provider, workspace, entityType)
	ps := make([]string, 0, len(sourceIds))
	for _, s := range sourceIds {
		args = append(args, s)
		ps = append(ps, fmt.Sprintf("$%d", len(args)))
	}
	query := fmt.Sprintf(`
		SELECT source_id, onecamp_uuid FROM import_workspace_id_map
		WHERE provider = $1 AND source_workspace_name = $2 AND entity_type = $3
		  AND source_id IN (%s)`, strings.Join(ps, ","))
	return model.ExecSourceIdMap(ctx, query, args, len(sourceIds))
}
