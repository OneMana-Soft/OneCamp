package business

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// An export of every table asks each listed table again by loadViewable's own
// rule, in one read: of a table the list names, it keeps one that is there
// and its reader opens, in the list's order, and leaves out one in the trash,
// one gone, and one private to someone else, which an admin opens, as one
// table's export lets them. A read that fails is the store's failure.
func TestEveryListedTableIsAskedAgainByLoadViewablesRule(t *testing.T) {
	me, other := uuid.New(), uuid.New()
	at := time.Now()
	table := func(name, visibility string, by uuid.UUID, deleted bool) *model.DataTable {
		tb := &model.DataTable{Id: uuid.New(), Name: name, Visibility: visibility, CreatedBy: by}
		if deleted {
			tb.DeletedAt = &at
		}
		return tb
	}
	shared := table("Shared", model.VisibilityWorkspace, other, false)
	mine := table("Mine", model.VisibilityPrivate, me, false)
	theirs := table("Theirs", model.VisibilityPrivate, other, false)
	trashed := table("Trashed", model.VisibilityWorkspace, me, true)
	gone := uuid.New()
	was := tablesByIDs
	t.Cleanup(func() { tablesByIDs = was })
	tablesByIDs = func(_ context.Context, ids []uuid.UUID) ([]*model.DataTable, error) {
		return []*model.DataTable{trashed, theirs, mine, shared}, nil
	}
	ids := []uuid.UUID{mine.Id, theirs.Id, gone, trashed.Id, shared.Id}
	names := func(a Actor) []string {
		got, err := openedAmong(context.Background(), ids, a)
		if err != nil {
			t.Fatalf("asked again: %v", err)
		}
		var out []string
		for _, tb := range got {
			out = append(out, tb.Name)
		}
		return out
	}
	if got := names(Actor{UserID: me}); !slices.Equal(got, []string{"Mine", "Shared"}) {
		t.Errorf("a member's tables: %q; want Mine and Shared, in the list's order", got)
	}
	if got := names(Actor{UserID: me, IsAdmin: true}); !slices.Equal(got, []string{"Mine", "Theirs", "Shared"}) {
		t.Errorf("an admin's tables: %q; want every live table, in the list's order", got)
	}
	tablesByIDs = func(context.Context, []uuid.UUID) ([]*model.DataTable, error) { return nil, errors.New("down") }
	if got, err := openedAmong(context.Background(), ids, Actor{UserID: me}); got != nil || err == nil || IsNotFound(err) {
		t.Errorf("a failed read: %v, %v; want the store's failure, and no table", got, err)
	}
}
