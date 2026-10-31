package refsBusiness_test

import (
	"testing"

	_ "github.com/akashc777/OneCamp/business/Calendar" // registers the meetings' projector
	refsBusiness "github.com/akashc777/OneCamp/business/Refs"
	domain "github.com/akashc777/OneCamp/domain/WorkGraph"
)

// TestEveryProjectedKindHasAKeeper: a projected kind's nodes are written by
// its projector alone and repaired by its nightly pass, so a kind without
// one would never reach the graph. A new kind's package registers its
// projector, and is imported above.
func TestEveryProjectedKindHasAKeeper(t *testing.T) {
	for _, k := range domain.ProjectedKinds() {
		if !refsBusiness.HasProjector(k) {
			t.Errorf("the work graph's %s has no projector", k)
		}
	}
}
