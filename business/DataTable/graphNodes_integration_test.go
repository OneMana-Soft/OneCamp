//go:build integration

package business

// The work graph hears of a table's rows that link work from the writes
// themselves (DG14): a row made, its links changed, its people set, each in
// the write's own transaction, and delivered once it commits; a text cell's
// change tells it nothing.
// Run: go test -tags=integration ./business/DataTable/ -run TestTheGraphHearsOfRowsFromTheirWrites -v

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/google/uuid"

	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	outboxModels "github.com/akashc777/OneCamp/models/postgres/Outbox"
)

// heardGraph records what the graph was told and delivered.
type heardGraph struct {
	mu        sync.Mutex
	added     map[int64][]string
	delivered []int64
}

func listenToGraph(t *testing.T) *heardGraph {
	h := &heardGraph{added: map[int64][]string{}}
	saved := GraphNodes
	t.Cleanup(func() { GraphNodes = saved })
	GraphNodes.InTx = func(_ context.Context, tx outboxModels.Execer, kind, _ string, ids []string) ([]int64, error) {
		if tx == nil || kind != GraphRow {
			t.Errorf("an entry of %q outside a transaction (%v)", kind, tx == nil)
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		id := int64(len(h.added) + 1)
		h.added[id] = ids
		return []int64{id}, nil
	}
	GraphNodes.Committed = func(ids []int64) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.delivered = append(h.delivered, ids...)
	}
	return h
}

// told is the rows the graph was told of and delivered since the last call, in order.
func (h *heardGraph) told() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, id := range h.delivered {
		out = append(out, h.added[id]...)
	}
	h.delivered = nil
	return out
}

func TestTheGraphHearsOfRowsFromTheirWrites(t *testing.T) {
	w := newRelationWorld(t)
	heard := listenToGraph(t)
	vendors, _ := w.table("Vendors")
	budget, name := w.table("Budget")
	vendor := w.field(budget, "Vendor", model.FieldRelation, map[string]interface{}{"relation_target": "table", "table_id": vendors.Id.String()})
	owner := w.field(budget, "Owner", model.FieldPeople, nil)
	acme := w.row(vendors, nil)
	heard.told()

	booth := w.row(budget, map[string]interface{}{name.Id.String(): "Booth"})
	if got := heard.told(); !slices.Equal(got, []string{booth.String()}) {
		t.Errorf("a row made told the graph %v, want the row", got)
	}
	if _, _, err := ChangeLinks(w.ctx, budget.Id, booth, vendor.Id, []uuid.UUID{acme}, nil, w.me); err != nil {
		t.Fatal(err)
	}
	if got := heard.told(); !slices.Contains(got, booth.String()) {
		t.Errorf("a link made told the graph %v, want the row it's on", got)
	}
	if _, err := PatchRow(w.ctx, budget.Id, booth, map[string]interface{}{owner.Id.String(): []interface{}{w.me.UserID.String()}}, w.me); err != nil {
		t.Fatal(err)
	}
	if got := heard.told(); !slices.Equal(got, []string{booth.String()}) {
		t.Errorf("a person set told the graph %v, want the row", got)
	}
	if _, err := PatchRow(w.ctx, budget.Id, booth, map[string]interface{}{name.Id.String(): "Booth, hall 2"}, w.me); err != nil {
		t.Fatal(err)
	}
	if got := heard.told(); len(got) != 0 {
		t.Errorf("a text cell's change told the graph %v", got)
	}
}
