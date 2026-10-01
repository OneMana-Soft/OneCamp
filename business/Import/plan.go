package business

import (
	"context"
	"encoding/json"
	"fmt"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/google/uuid"
)

// BuildPlan delegates to the provider's Plan() and persists the result.
//
// Side-effects:
//   - import_jobs.plan stored as JSONB
//   - import_jobs.status → 'planned'
//   - import_chunks initial rows inserted (idempotent)
//
// The operator can call this multiple times before Run; we don't
// duplicate chunks because of the unique index. Status mappings the
// operator confirms come in via SetStatusMappings/SetPriorityMappings
// before Run starts.
func BuildPlan(ctx context.Context, jobId uuid.UUID) (*importProvider.Plan, error) {
	job, err := importModels.GetJob(ctx, jobId)
	if err != nil {
		return nil, err
	}
	if job == nil {
		return nil, fmt.Errorf("job not found")
	}
	prov := importProvider.Get(job.Provider)
	if prov == nil {
		return nil, fmt.Errorf("unknown provider %s", job.Provider)
	}

	opts := decodeOptions(job.Options)

	plan, chunks, err := prov.Plan(ctx, job, opts)
	if err != nil {
		return nil, err
	}
	if plan == nil {
		return nil, fmt.Errorf("provider returned nil plan")
	}

	if len(chunks) > 0 {
		if err := importModels.CreateChunks(ctx, chunks); err != nil {
			return nil, fmt.Errorf("create chunks: %w", err)
		}
	}

	planJSON, _ := json.Marshal(plan)
	if err := importModels.UpdatePlan(ctx, jobId, planJSON); err != nil {
		return nil, err
	}
	return plan, nil
}
