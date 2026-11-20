//go:build integration

package business

// DeleteTables (a table template's undo, gsheets-structure-05 item 2): the
// tables are read and checked at once, and go to the trash in one write, all
// of them or none.

import (
	"testing"

	"github.com/google/uuid"
)

func TestDeleteTablesTrashesThemAllOrNone(t *testing.T) {
	w := newRelationWorld(t)
	ctx := w.ctx
	a, _ := w.table("Undo A")
	b, _ := w.table("Undo B")
	theirs, err := CreateTable(ctx, TableInput{Name: "Theirs", Visibility: "workspace"}, w.member)
	if err != nil {
		t.Fatal(err)
	}
	live := func(ids ...uuid.UUID) int {
		return w.purgeCount(`SELECT count(*) FROM data_tables WHERE id = ANY($1) AND deleted_at IS NULL`, ids)
	}

	// One table that isn't the owner's to manage refuses them all.
	if err := DeleteTables(ctx, []uuid.UUID{a.Id, b.Id, theirs.Id}, w.me); !IsForbidden(err) {
		t.Errorf("with someone else's table: %v, want forbidden", err)
	}
	// One that isn't there refuses them all too.
	if err := DeleteTables(ctx, []uuid.UUID{a.Id, uuid.New()}, w.me); !IsNotFound(err) {
		t.Errorf("with a table that isn't there: %v, want not found", err)
	}
	if n := live(a.Id, b.Id, theirs.Id); n != 3 {
		t.Fatalf("refused, yet %d of 3 tables are live", n)
	}

	// The owner's own go together (a repeat is one), by the owner.
	if err := DeleteTables(ctx, []uuid.UUID{a.Id, b.Id, a.Id}, w.me); err != nil {
		t.Fatal(err)
	}
	if n := live(a.Id, b.Id); n != 0 {
		t.Errorf("%d of the two deleted tables are still live", n)
	}
	if n := w.purgeCount(`SELECT count(*) FROM data_tables WHERE id = ANY($1) AND deleted_by = $2`, []uuid.UUID{a.Id, b.Id}, w.me.UserID); n != 2 {
		t.Errorf("%d of 2 deleted tables name who deleted them", n)
	}
	if live(theirs.Id) != 1 {
		t.Error("a table not asked for was deleted")
	}
	// Already in the trash, a table isn't there to delete: as DeleteTable says.
	if err := DeleteTable(ctx, a.Id, w.me); !IsNotFound(err) {
		t.Errorf("deleting a table in the trash: %v, want not found", err)
	}
}
