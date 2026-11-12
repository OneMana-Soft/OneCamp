package business

import (
	"context"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
)

// A move of data that tells nobody (QuietMove: an import, a sync, the demo's
// seed, an import's re-check) is quiet to every teller, as a bulk import's
// writes are; a DataMove alone (a template's apply, a person's act told in
// its one push) and a person's write tell (NOTIF-H6).
func TestAQuietMoveTellsNobodyAndADataMoveTells(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		ctx   context.Context
		quiet bool
	}{
		"a person's write":            {ctx, false},
		"a template's apply":          {DataMove(ctx), false},
		"an import, a sync, the seed": {QuietMove(ctx), true},
		"a bulk import":               {context.WithValue(ctx, helpers.BulkImportContextKey, true), true},
	} {
		if got := helpers.Quiet(c.ctx); got != c.quiet {
			t.Errorf("%s: quiet %v, want %v", name, got, c.quiet)
		}
	}
	if q := QuietMove(ctx); q.Value(dataMoveKey{}) == nil {
		t.Error("a quiet move isn't a data move: the status gate would ask of its first statuses")
	}
}
