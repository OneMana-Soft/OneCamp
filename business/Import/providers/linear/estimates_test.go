package linear

import (
	"testing"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

// linear-05: each team's scale comes over one to one, a sub-team that
// inherits takes its parent's, and a team that doesn't estimate has none.
func TestEachTeamsScaleComesOverOneToOne(t *testing.T) {
	teams := []linearTeam{
		{ID: "eng", EstimationType: "fibonacci", EstimationExtended: true, DefaultEstimate: 1},
		{ID: "design", EstimationType: "tShirt", EstimationAllowZero: true},
		{ID: "ops", EstimationType: "notUsed"},
		{ID: "web", ParentID: "eng", InheritEstimation: true, EstimationType: "linear"},
		{ID: "web-perf", ParentID: "web", InheritEstimation: true},
		{ID: "mobile", ParentID: "eng", EstimationType: "exponential", DefaultEstimate: 1},
		// Parents in a circle end at the bound, on a team's own settings.
		{ID: "x", ParentID: "y", InheritEstimation: true, EstimationType: "linear"},
		{ID: "y", ParentID: "x", InheritEstimation: true, EstimationType: "linear"},
	}
	got := estimateScales(teams)
	fib := importProvider.SourceEstimateScale{Kind: "fibonacci", Extended: true, CountUnestimated: true}
	for team, want := range map[string]*importProvider.SourceEstimateScale{
		"eng":      &fib,
		"design":   {Kind: "tshirt", AllowZero: true},
		"ops":      nil,
		"web":      &fib,
		"web-perf": &fib,
		"mobile":   {Kind: "exponential", CountUnestimated: true},
		"x":        {Kind: "linear"},
	} {
		switch g := got[team]; {
		case want == nil && g != nil:
			t.Errorf("%s: %+v, want none", team, *g)
		case want != nil && (g == nil || *g != *want):
			t.Errorf("%s: %+v, want %+v", team, g, *want)
		}
	}
}
