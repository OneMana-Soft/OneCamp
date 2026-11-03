package business

import (
	"context"
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/google/uuid"
)

// A write is Recoverable exactly when it names the ops that take it back, and
// those are published beside it, one of them a write.
func TestARecoverableOpNamesItsUndoPublishedBesideIt(t *testing.T) {
	undoneBy := func(op Op, names ...string) Op { op.UndoneBy = names; return op }
	for name, op := range map[string]Op{
		"Recoverable with no undo":   thing("things.a", Behaviour{Destructive: true, Recoverable: true}, okServe),
		"an undo, not Recoverable":   undoneBy(thing("things.b", Behaviour{Destructive: true}, okServe), "things.restore"),
		"a read that is Recoverable": undoneBy(thing("things.c", Behaviour{ReadOnly: true, Recoverable: true}, okServe), "things.restore"),
	} {
		opsForTest(t)
		if msg := panics(func() { RegisterOps(Resource{Name: "things", Owner: "test"}, []Op{op}) }); !strings.Contains(msg, "Recoverable") {
			t.Errorf("%s: registered (panic %q); want it refused", name, msg)
		}
	}

	restore := thing("things.restore", Behaviour{Idempotent: true}, okServe)
	look := thing("things.look", Behaviour{ReadOnly: true}, okServe)
	drop := undoneBy(thing("things.drop", Behaviour{Destructive: true, Recoverable: true}, okServe), "things.look", "things.restore")
	if err := CheckUndo([]Op{drop, look, restore}); err != nil {
		t.Errorf("a delete whose restore is published beside it: %v", err)
	}
	if err := CheckUndo([]Op{drop, look}); err == nil || !strings.Contains(err.Error(), "things.restore") {
		t.Errorf("a delete whose restore isn't published beside it answered %v; want it refused, naming things.restore", err)
	}
	if err := CheckUndo([]Op{undoneBy(drop, "things.look"), look, restore}); err == nil {
		t.Error("a delete undone by reads alone was accepted")
	}
}

// A Recoverable write's audit row names the change its write makes, and the
// first change set the call makes takes that id; a write that isn't
// Recoverable names none.
func TestARecoverableWritesAuditRowNamesTheChangeItMakes(t *testing.T) {
	var metas []map[string]any
	savedAudit, savedCharge := audit, chargeRows
	audit = func(_ context.Context, _ *uuid.UUID, _, _, _, _, _ string, meta map[string]interface{}) error {
		metas = append(metas, meta)
		return nil
	}
	chargeRows = func(context.Context, string, bool, int) error { return nil }
	t.Cleanup(func() { audit, chargeRows = savedAudit, savedCharge })

	var took []uuid.UUID
	serve := func(ctx context.Context, _ Call) (any, error) {
		took = append(took, helpers.TakeCallChange(ctx), helpers.TakeCallChange(ctx))
		return map[string]any{}, nil
	}
	drop := thing("things.drop", Behaviour{Destructive: true, Recoverable: true}, serve)
	drop.UndoneBy = []string{"things.restore"}
	change := thing("things.change", Behaviour{Idempotent: true}, serve)
	ctx := context.WithValue(context.Background(), helpers.ApiTokenIDContextKey, uuid.NewString())
	for _, op := range []*Op{&drop, &change} {
		if _, err := invoke(ctx, op, "token", map[string]any{"id": "t1"}); err != nil {
			t.Fatalf("%s: %v", op.Name, err)
		}
	}
	if len(metas) != 2 || len(took) != 4 {
		t.Fatalf("recorded %d calls and took %d ids; want 2 and 4", len(metas), len(took))
	}
	if named, _ := metas[0]["change_id"].(string); named == "" || named != took[0].String() || took[1] != uuid.Nil {
		t.Errorf("the Recoverable write recorded change %q; its first change set took %s and its second %s; want that id, then none",
			named, took[0], took[1])
	}
	if named, ok := metas[1]["change_id"]; ok || took[2] != uuid.Nil {
		t.Errorf("a write that isn't Recoverable recorded change %v and took %s; want neither", named, took[2])
	}
}
