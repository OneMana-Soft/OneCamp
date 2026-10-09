package business

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
)

const (
	// linkUpkeepFirst is when links between tables' rows are first looked at
	// after the server starts, and linkUpkeepEvery how often after that.
	// linkUpkeepAfter is how many must have been made or removed since they
	// were last vacuumed for them to be vacuumed again.
	linkUpkeepFirst = time.Minute
	linkUpkeepEvery = 10 * time.Minute
	linkUpkeepAfter = 10000
)

// StartLinkUpkeep keeps the links between tables' rows quick to count and
// read. Postgres 12 never vacuums a table that's mostly added to, and until
// it does, a count reads each link's row as well as the index: ten times the
// work, enough for a page with many links to take seconds. So it vacuums
// data_table_links itself, soon after links change in number.
func StartLinkUpkeep(ctx context.Context) {
	go func() {
		first := time.NewTimer(linkUpkeepFirst)
		defer first.Stop()
		ticker := time.NewTicker(linkUpkeepEvery)
		defer ticker.Stop()
		last := int64(-linkUpkeepAfter) // vacuumed at the first look
		look := func() {
			// Fewer than at the last vacuum: Postgres's counts were reset.
			changed, err := model.LinksChanged(ctx)
			if err != nil || (changed >= last && changed-last < linkUpkeepAfter) {
				return
			}
			if err := model.VacuumLinks(ctx); err != nil {
				helpers.LogErrorWithContext(ctx, "StartLinkUpkeep: %v", err)
				return
			}
			last = changed
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-first.C:
				look()
			case <-ticker.C:
				look()
			}
		}
	}()
}
