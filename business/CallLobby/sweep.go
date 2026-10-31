package business

import (
	"context"
	"time"

	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	model "github.com/akashc777/OneCamp/models/postgres/CallState"
)

// knockLasts is how long someone waits in a lobby nobody answers before their
// knock lapses: their page says so, and the next host doesn't find them.
const knockLasts = 30 * time.Minute

func init() {
	sweepBusiness.Jobs.Register(sweepBusiness.Job{Name: "calls.lobby.expire", Every: time.Minute, Run: ExpireKnocks})
}

// ExpireKnocks lets lapse every knock nobody answered within knockLasts.
func ExpireKnocks(ctx context.Context) error {
	rooms, err := model.ExpireKnocks(ctx, time.Now().Add(-knockLasts))
	for _, room := range rooms {
		callStateBusiness.Publish(ctx, room)
	}
	return err
}
