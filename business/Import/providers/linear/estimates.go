package linear

import importProvider "github.com/akashc777/OneCamp/business/Import/provider"

// Estimates (linear-05): a team estimates in points on one of Linear's four
// scales, which its projects keep here (business/EstimateScale), and each
// issue's estimate comes over as the task's points.

// linearScales names Linear's scales as OneCamp does; notUsed, a team that
// doesn't estimate, isn't one.
var linearScales = map[string]string{"exponential": "exponential", "fibonacci": "fibonacci", "linear": "linear", "tShirt": "tshirt"}

// estimateScales is each team's scale, nil for a team that doesn't estimate.
// A sub-team that inherits its estimates takes its parent's, up the tree.
func estimateScales(teams []linearTeam) map[string]*importProvider.SourceEstimateScale {
	byID := make(map[string]linearTeam, len(teams))
	for _, t := range teams {
		byID[t.ID] = t
	}
	out := make(map[string]*importProvider.SourceEstimateScale, len(teams))
	for _, t := range teams {
		own := t
		// At most one step a team: an answer whose parents ran in a circle
		// would otherwise never end.
		for range teams {
			parent, ok := byID[own.ParentID]
			if !own.InheritEstimation || !ok {
				break
			}
			own = parent
		}
		out[t.ID] = estimateScale(own)
	}
	return out
}

// estimateScale is a team's own scale. Linear counts an unestimated issue as
// its default estimate, one unless the team turned that off (zero).
func estimateScale(t linearTeam) *importProvider.SourceEstimateScale {
	kind, ok := linearScales[t.EstimationType]
	if !ok {
		return nil
	}
	return &importProvider.SourceEstimateScale{
		Kind: kind, Extended: t.EstimationExtended, AllowZero: t.EstimationAllowZero, CountUnestimated: t.DefaultEstimate > 0,
	}
}
