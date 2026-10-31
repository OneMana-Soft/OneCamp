//go:build integration

package business_test

// A table whose mark is taken away while IndexTable walks it stays unmarked
// (V-36). The walk goes a batch of rows at a time, and once it ended it
// marked the table whole whatever had happened meanwhile. A field made while
// the table was walked, whose entries couldn't be written for the rows
// already there, takes the mark away (MarkCellIndexStale: CreateField,
// UpdateField's options, a field brought back from the trash) so that the
// repair sweep makes the index whole again; the walk's mark, landing after
// it, put the mark back, and Row Query answered from an index without that
// field's entries for the rows walked before it.
//
// Run: integ_rel.sh <worktree> <log> '^(TestATableMarkedStaleWhileItIsWalkedStaysUnmarked)$' with PKGS='./business/RowQuery/'

import (
	"context"
	"sync"
	"testing"
	"time"

	dataTable "github.com/akashc777/OneCamp/business/DataTable"
	rowQuery "github.com/akashc777/OneCamp/business/RowQuery"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

func TestATableMarkedStaleWhileItIsWalkedStaysUnmarked(t *testing.T) {
	ctx, owner := tableOwner(t)
	tb, err := dataTable.CreateTable(ctx, dataTable.TableInput{Name: "Walked", Visibility: "workspace"}, owner)
	if err != nil {
		t.Fatal(err)
	}
	item, err := dataTable.CreateField(ctx, tb.Id, dataTable.FieldInput{Name: "Item", Type: model.FieldText}, owner)
	if err != nil {
		t.Fatal(err)
	}
	// A table just made is indexed in the background (indexSoon): that walk
	// would meet the seam below too, so it ends first.
	if busy := helpers.SettleBackground(30 * time.Second); len(busy) > 0 {
		t.Fatalf("background work still running 30 s on: %v", busy)
	}
	// Two batches' worth of rows (a batch is 500), written as an import
	// writes them: the table's mark taken away.
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `INSERT INTO data_table_rows (id, table_id, "values", position, position_key, row_no, created_by, created_at, updated_at)
		SELECT md5($4::text || g)::uuid, $1::uuid, jsonb_build_object($2::text, 'Row ' || g), g, 'p' || lpad(g::text, 5, '0'), g, $3::uuid, NOW(), NOW()
		FROM generate_series(1, 600) g`, tb.Id, item.Id.String(), owner.UserID, tb.Id.String()); err != nil {
		t.Fatal(err)
	}
	if err := model.MarkCellIndexStale(ctx, tb.Id); err != nil {
		t.Fatal(err)
	}
	whole := func() bool {
		t.Helper()
		ok, _, err := model.CellIndexState(ctx, tb.Id)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}

	// After the walk's first batch, a write takes the table's mark away, as
	// CreateField does when a new field's entries couldn't be written.
	var mu sync.Mutex
	batches := 0
	restore := rowQuery.AfterIndexBatch(func(ctx context.Context, table uuid.UUID) {
		if table != tb.Id {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		batches++
		if batches == 1 {
			if err := model.MarkCellIndexStale(ctx, table); err != nil {
				t.Error(err)
			}
		}
	})
	t.Cleanup(restore)
	if err := rowQuery.IndexTable(ctx, tb.Id); err != nil {
		t.Fatal(err)
	}
	restore()
	if batches != 2 {
		t.Fatalf("the walk went %d batches, want 2 (600 rows)", batches)
	}
	if whole() {
		t.Fatal("the table is marked whole, although its mark was taken away during the walk: the repair sweep would leave it as it is")
	}

	// The next walk, with nothing taking the mark away, makes it whole.
	if err := rowQuery.IndexTable(ctx, tb.Id); err != nil {
		t.Fatal(err)
	}
	if !whole() {
		t.Fatal("a walk with no write during it left the table unmarked")
	}
}
