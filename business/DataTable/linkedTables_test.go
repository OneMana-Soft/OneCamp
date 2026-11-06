package business

import (
	"context"
	"fmt"
	"testing"

	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// A save's checks read each table they link to once, however many of a
// batch's items ask (linkedTables): rollups over the same relation, the chain
// past a rollup of a rollup and a relation to the same table all share one
// read, and a table the batch read up front (preload) is never read again.
func TestABatchsChecksReadEachLinkedTableOnce(t *testing.T) {
	ctx, me := context.Background(), Actor{UserID: uuid.New()}
	own, projects, tasks := uuid.New(), uuid.New(), uuid.New()
	table := func(id uuid.UUID) *model.DataTable {
		return &model.DataTable{Id: id, Name: "Table", Visibility: model.VisibilityWorkspace}
	}
	relation := func(in, to uuid.UUID, name string) *model.Field {
		return &model.Field{Id: uuid.New(), TableId: in, Name: name, Type: model.FieldRelation,
			Config: fmt.Sprintf(`{"relation_target":"table","table_id":%q}`, to)}
	}
	// own links to projects; projects link to tasks and add up their estimates.
	estimate := &model.Field{Id: uuid.New(), TableId: tasks, Name: "Estimate", Type: model.FieldNumber}
	toTasks := relation(projects, tasks, "Tasks")
	planned := &model.Field{Id: uuid.New(), TableId: projects, Name: "Planned", Type: model.FieldRollup,
		Config: fmt.Sprintf(`{"relation":%q,"field":%q,"aggregate":"sum"}`, toTasks.Id, estimate.Id)}
	toProjects := relation(own, projects, "Projects")
	stored := map[uuid.UUID]*linkedTable{
		projects: {table: table(projects), fields: []*model.Field{toTasks, planned}},
		tasks:    {table: table(tasks), fields: []*model.Field{estimate}},
	}
	rollup := func(name, how string, of *model.Field) FieldInput {
		cfg := map[string]interface{}{"relation": toProjects.Id.String(), "aggregate": how}
		if of != nil {
			cfg["field"] = of.Id.String()
		}
		return FieldInput{Name: name, Type: model.FieldRollup, Config: cfg}
	}
	items := []FieldInput{
		rollup("Projects", "count", nil),
		rollup("Planned in all", "sum", planned),
		rollup("Most planned", "max", planned),
		{Name: "Also in", Type: model.FieldRelation, Config: map[string]interface{}{"relation_target": "table", "table_id": projects.String()}},
	}
	fields := []*model.Field{toProjects}

	read := map[uuid.UUID]int{}
	linked := newLinked()
	linked.fetch = func(_ context.Context, id uuid.UUID) *linkedTable {
		read[id]++
		return stored[id]
	}
	for i := range items {
		if _, err := computedFieldConfig(ctx, me, table(own), fields, uuid.Nil, &items[i], linked); err != nil {
			t.Fatalf("item %d (%s): %v", i, items[i].Name, err)
		}
	}
	if len(read) != 2 || read[projects] != 1 || read[tasks] != 1 {
		t.Errorf("%d items read %v; want projects and tasks (down the rollup's chain) once each", len(items), read)
	}

	// As preload leaves a table: read already, so neither a preload naming it
	// again nor a check asking for it reads it.
	pre := newLinked()
	pre.opened[projects] = stored[projects]
	pre.fetch = func(_ context.Context, id uuid.UUID) *linkedTable {
		t.Errorf("read %s again; the batch had read it up front", id)
		return stored[id]
	}
	if err := pre.preload(ctx, []uuid.UUID{projects, projects}); err != nil {
		t.Fatalf("a preload of a table read already: %v", err)
	}
	for _, in := range []FieldInput{items[0], items[3]} {
		if _, err := computedFieldConfig(ctx, me, table(own), fields, uuid.Nil, &in, pre); err != nil {
			t.Fatalf("%s on a preloaded table: %v", in.Name, err)
		}
	}
}
