package business

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
)

const (
	// linkUpkeepFirst is when the tables mostly added to are first looked at
	// after the server starts, and linkUpkeepEvery how often after that.
	// linkUpkeepAfter is how many rows of one must have been added or removed
	// since it was last vacuumed for it to be vacuumed again.
	linkUpkeepFirst = time.Minute
	linkUpkeepEvery = 10 * time.Minute
	linkUpkeepAfter = 10000
)

// StartLinkUpkeep keeps the tables' tables that are mostly added to quick to
// count and read (model.InsertMostly: the links between rows, and the row
// ledger's change sets and changes). Postgres 12 never vacuums a table that's
// mostly added to, and until it does, a count reads each row as well as the
// index: ten times the work, enough for a page with many links, or a row's
// history, to take seconds. So it vacuums each itself, soon after it grows.
//
// At its first look it also gives a place to any row made without one: by a
// server from before row keys (migration 301), between the migration and
// this server starting (model.KeyRowsWithout).
func StartLinkUpkeep(ctx context.Context) {
	helpers.GoSafeLoop("business/DataTable/linkUpkeep:StartLinkUpkeep", func() {
		first := time.NewTimer(linkUpkeepFirst)
		defer first.Stop()
		ticker := time.NewTicker(linkUpkeepEvery)
		defer ticker.Stop()
		last := map[string]int64{}
		for _, t := range model.InsertMostly {
			last[t] = -linkUpkeepAfter // vacuumed at the first look
		}
		look := func() {
			for _, t := range model.InsertMostly {
				// Fewer than at the last vacuum: Postgres's counts were reset.
				changed, err := model.TuplesChanged(ctx, t)
				if err != nil || (changed >= last[t] && changed-last[t] < linkUpkeepAfter) {
					continue
				}
				if err := model.Vacuum(ctx, t); err != nil {
					helpers.LogErrorWithContext(ctx, "StartLinkUpkeep %s: %v", t, err)
					continue
				}
				last[t] = changed
			}
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-first.C:
				if n, err := model.KeyRowsWithout(ctx, keys); err != nil {
					helpers.LogErrorWithContext(ctx, "StartLinkUpkeep keys: %v", err)
				} else if n > 0 {
					helpers.LogInfoWithContext(ctx, "StartLinkUpkeep: gave %d rows a place", n)
				}
				look()
			case <-ticker.C:
				look()
			}
		}
	})
}
