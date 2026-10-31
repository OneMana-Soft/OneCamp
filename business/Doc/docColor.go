package businness

// A node's colour (docs F1, D-31): a camp hue's name, set on many nodes at
// once (a selection in the tree, coloured in bulk), or none.

import (
	"context"
	"time"

	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	domain "github.com/akashc777/OneCamp/domain/Doc"
)

// ColorCap is the most docs one colouring takes.
const ColorCap = domain.ColorCap

// SetColors colours docs for by, who may edit each (the caller's to have
// checked): color, a camp hue, or none (""), in one write. Each doc it
// changed is told as restyled, as a change of its look is. It answers how
// many it changed: a doc in the trash meanwhile is left as it is.
func SetColors(ctx context.Context, docUUIDs []string, color string, by Actor) (int, error) {
	changed, err := domain.WriteDocColors(ctx, docUUIDs, color)
	if err != nil {
		return 0, err
	}
	now := time.Now()
	for _, id := range changed {
		resourceEventBusiness.Bus.Emit(ctx, resourceEventBusiness.Event{
			Kind: resourceEventBusiness.DocRestyled, ResourceKind: "doc", ResourceUUID: id, Actor: by, At: now,
			Data: map[string]string{"changed": "color"},
		})
	}
	return len(changed), nil
}
