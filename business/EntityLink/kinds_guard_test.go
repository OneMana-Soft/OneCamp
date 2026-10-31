package entityLinkBusiness_test

import (
	"testing"

	_ "github.com/akashc777/OneCamp/business/Calendar" // registers the event's reader
	entityLinkBusiness "github.com/akashc777/OneCamp/business/EntityLink"
	domain "github.com/akashc777/OneCamp/domain/WorkGraph"
)

// TestEveryKindOfTheWorkGraphHasAReader: each node the work graph reads is
// decided as the reader by its kind's reader, so a kind without one would
// read as hidden to everyone, silently. A new kind's package registers its
// reader, and is imported above.
func TestEveryKindOfTheWorkGraphHasAReader(t *testing.T) {
	for _, k := range domain.Kinds() {
		if !entityLinkBusiness.HasReader(string(k)) {
			t.Errorf("the work graph's %s has no EntityLink reader", k)
		}
	}
}
