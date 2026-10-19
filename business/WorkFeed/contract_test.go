package business

import (
	"context"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
)

// Nothing is logged yet: a reader catches up on nothing and keeps their
// place, and publishing reaches nobody.
func TestTheFeedHasNothingYet(t *testing.T) {
	got, next, err := Changes.Since(context.Background(), accessBusiness.Subject{UserUUID: "someone"}, "41", 100)
	if err != nil || len(got) != 0 || next != "41" {
		t.Errorf("%v %q %v", got, next, err)
	}
	Changes.Publish(context.Background(), []Entry{{Sequence: 1}})
}
