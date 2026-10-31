package refsBusiness

import domain "github.com/akashc777/OneCamp/domain/WorkGraph"

// HasProjector is whether k's nodes have a keeper, for the external guard
// (kinds_guard_test.go).
func HasProjector(k domain.Kind) bool {
	_, ok := projectorOf(k)
	return ok
}
