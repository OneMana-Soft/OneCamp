package business

import (
	"context"
	"slices"
	"testing"

	fieldTypes "github.com/akashc777/OneCamp/business/FieldTypes"
	formula "github.com/akashc777/OneCamp/business/Formula"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// The gate (Serves) opens a value's reach only through the tables its plan
// opened as the reader (airtable-03). A table in the reach that the plan
// never opened, one that is gone, and one the reader can't open each fail
// it, so a gap between reachOf and the plan is a refusal, never a kept value.
func TestTheGateFailsATableItsPlanNeverOpened(t *testing.T) {
	open, gone, shut, never := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	c := &computed{reach: &reach{tables: map[uuid.UUID]*opened{
		open: {table: &model.DataTable{Id: open}, visible: true},
		gone: {},
		shut: {table: &model.DataTable{Id: shut}},
	}}}
	for _, leg := range []struct {
		name   string
		tables []uuid.UUID
		opens  bool
	}{
		{"no table", nil, true},
		{"a table it opened", []uuid.UUID{open}, true},
		{"a table it never opened", []uuid.UUID{never}, false},
		{"one it opened and one it never did", []uuid.UUID{open, never}, false},
		{"a table that is gone", []uuid.UUID{gone}, false},
		{"a table the reader can't open", []uuid.UUID{shut}, false},
	} {
		in := map[uuid.UUID]bool{}
		for _, id := range leg.tables {
			in[id] = true
		}
		if got := c.opensEvery(in); got != leg.opens {
			t.Errorf("the gate over %s: opens %v, where it opens %v", leg.name, got, leg.opens)
		}
	}
}

// A link to OneCamp items reads as what ITS reader may open (itemLinks.go
// nameItems names docs, tasks, boards and projects by the reader), so no one
// stored value serves every reader: a formula or a rollup over one is never
// kept, and each reader's own read works it out (CR-92 M1). People are named
// alike for everyone (peopleNames), so one over people is kept.
func TestAValueOverItemsNamedPerReaderIsNeverKept(t *testing.T) {
	field := func(name, typ, cfg string) *model.Field {
		return &model.Field{Id: uuid.New(), Name: name, Type: typ, Config: cfg}
	}
	links := func(kind string) string { return `{"relation_target":"` + kind + `"}` }
	docs := field("Docs", model.FieldRelation, links(fieldTypes.ItemDoc))
	tasks := field("Tasks", model.FieldRelation, links(fieldTypes.ItemTask))
	things := field("Things", model.FieldRelation, links(fieldTypes.ItemAny))
	owners := field("Owners", model.FieldPeople, `{}`)
	amount := field("Amount", model.FieldNumber, `{}`)
	byDocs := field("Doc names", model.FieldFormula, `{"formula":"LEN({Docs})"}`)
	byTasks := field("Task names", model.FieldFormula, `{"formula":"UPPER({Tasks})"}`)
	byThings := field("Thing names", model.FieldFormula, `{"formula":"{Things}"}`)
	overByDocs := field("Doc names twice", model.FieldFormula, `{"formula":"{Doc names} * 2"}`)
	byOwners := field("Owner names", model.FieldFormula, `{"formula":"LEN({Owners})"}`)
	byAmount := field("Double", model.FieldFormula, `{"formula":"{Amount} * 2"}`)
	fields := []*model.Field{docs, tasks, things, owners, amount, byDocs, byTasks, byThings, overByDocs, byOwners, byAmount}
	kinds := keptKinds(planComputed(context.Background(), fields), fields)
	for _, f := range []*model.Field{byDocs, byTasks, byThings, overByDocs} {
		if _, kept := kinds[f.Id.String()]; kept {
			t.Errorf("%s is kept, where its cells are named for each reader", f.Name)
		}
	}
	for _, f := range []*model.Field{byOwners, byAmount} {
		if _, kept := kinds[f.Id.String()]; !kept {
			t.Errorf("%s isn't kept, where it reads the same for everyone", f.Name)
		}
	}
	// A rollup over a linked table's items link (a lookup of {Link}.{Docs}) is
	// named per reader the same way.
	c := &computed{problems: map[string]string{}}
	for _, leg := range []struct {
		of   *model.Field
		kept bool
	}{{docs, false}, {tasks, false}, {things, false}, {owners, true}, {amount, true}} {
		r := &rollupField{field: field("Of "+leg.of.Name, model.FieldRollup, `{}`), of: leg.of}
		if got := c.rollupKept(r); got != leg.kept {
			t.Errorf("a rollup of %s: kept %v, where it is %v", leg.of.Name, got, leg.kept)
		}
	}
}

// A {Link}.{Field} the plan has no read for still reaches its link's table
// (CR-92 S1), so the gate fails closed on it, never open. No plan gets there
// today: a formula reading {Ledger}.{Amount} reads {Ledger} too (parse
// records both), so even Loop, which reads itself and whose linked reads
// Compile leaves out, reaches Ledger. reachOf's own rule is asked as well,
// for the {Link}.{Field} alone.
func TestALinkedReadThePlanNeverPlannedReachesItsTable(t *testing.T) {
	ledger := uuid.New()
	link := &model.Field{Id: uuid.New(), Name: "Ledger", Type: model.FieldRelation, Config: `{"relation_target":"table","table_id":"` + ledger.String() + `"}`}
	amount := &model.Field{Id: uuid.New(), TableId: ledger, Name: "Amount", Type: model.FieldNumber, Config: `{}`}
	loop := &model.Field{Id: uuid.New(), Name: "Loop", Type: model.FieldFormula, Config: `{"formula":"{Loop} + SUM({Ledger}.{Amount})"}`}
	fields := []*model.Field{link, loop}
	in := withLinked(formulaInputs(fields), fields, func(uuid.UUID) []*model.Field { return []*model.Field{amount} })
	c := &computed{fields: fields, program: formula.Compile(in)}
	ref := formula.LinkedRef(link.Id.String(), amount.Id.String())
	if reads, _ := c.program.Reads(loop.Id.String()); !slices.Contains(reads, ref) || slices.Contains(c.program.LinkedRefs(), ref) {
		t.Fatalf("Loop reads %v and the plan's linked reads are %v, where it reads {Ledger}.{Amount} and the plan reads none", reads, c.program.LinkedRefs())
	}
	reach := c.reachFn()
	for _, leg := range []struct{ name, ref string }{{"Loop", loop.Id.String()}, {"{Ledger}.{Amount} alone", ref}} {
		if got := reach(leg.ref); !got[ledger] {
			t.Errorf("%s reaches %v, where its {Ledger}.{Amount} reaches Ledger", leg.name, got)
		}
	}
}
