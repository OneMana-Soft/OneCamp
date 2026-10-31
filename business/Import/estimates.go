package business

import (
	"context"

	estimateScale "github.com/akashc777/OneCamp/business/EstimateScale"
	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/google/uuid"
)

// keepEstimateScale puts the project on the scale its team estimated on in
// the source (linear-05), before its tasks come over with their points. One
// it can't keep is said in the report: the project estimates in hours, and
// its tasks' points stay, unread until it counts in points.
func keepEstimateScale(ctx context.Context, job *importModels.Job, sp *importProvider.SourceProject, project, by uuid.UUID) {
	s := sp.EstimateScale
	if s == nil {
		return
	}
	in := estimateScale.Scale{Kind: s.Kind, Extended: s.Extended, AllowZero: s.AllowZero, CountUnestimated: s.CountUnestimated}
	if _, err := estimateScale.Set(ctx, project, in, by); err != nil {
		importModels.LogImportError(ctx, job.Id, nil, importModels.EntityProject, sp.SourceID,
			importModels.SeverityWarning, "ESTIMATE_SCALE_NOT_KEPT", "It estimates in hours here: "+err.Error(), nil)
	}
}
