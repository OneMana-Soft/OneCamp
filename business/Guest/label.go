package business

// A sensitivity label with no_guest_links (business/Label) turns off every
// live external link to the items it goes on, as soon as it goes on them (put
// on one, or the label given the rule): their guests lose what they have open
// at once (RevokeGrants ends the live sessions). CreateResourceGrant refuses
// new links while it is on.

import (
	"context"
	"fmt"

	labelBusiness "github.com/akashc777/OneCamp/business/Label"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/google/uuid"
)

func init() { labelBusiness.OnApplied(labelBusiness.RuleNoGuestLinks, turnOffLinksOf) }

// liveLinksOf is the live links to these items; a seam for tests.
var liveLinksOf = guestModel.LiveGrantIDsOf

// turnOffLinksOf turns off the live links to these items of kind, and says
// how many, with their ids, for the label's row.
func turnOffLinksOf(ctx context.Context, kind string, items []uuid.UUID) (labelBusiness.Applied, error) {
	ids := make([]string, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.String())
	}
	live, err := liveLinksOf(ctx, kind, ids)
	if err != nil {
		return labelBusiness.Applied{}, err
	}
	if len(live) == 0 {
		return labelBusiness.Applied{}, nil
	}
	done := RevokeGrants(ctx, live)
	grants := make([]string, 0, len(done))
	for _, id := range done {
		grants = append(grants, id.String())
	}
	var err2 error
	if len(done) < len(live) {
		err2 = fmt.Errorf("%d of %d links couldn't be turned off", len(live)-len(done), len(live))
	}
	if len(done) == 0 {
		return labelBusiness.Applied{}, err2
	}
	words := "Turned off 1 external link"
	if len(done) > 1 {
		words = fmt.Sprintf("Turned off %d external links", len(done))
	}
	return labelBusiness.Applied{Words: words, Meta: map[string]interface{}{"grant_ids": grants}}, err2
}
