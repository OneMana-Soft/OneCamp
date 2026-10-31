//go:build integration

package business

// An imported value outside a select's options becomes one of them, as
// Airtable's and Notion's own imports make it, so the value stays in its
// cell: always in a table the import makes, and in a table already there only
// when the person importing may change its fields (the field-edit API's own
// check), the cell otherwise left out and said as before. Each value is added
// once a field, however many rows carry it, as the app adds one by hand (an
// id, its label, no colour), and the import's notes say so.
// Run: go test -tags=integration ./business/DataTable/ -run TestAnImportedValueOutsideASelectsOptionsBecomesOne -v

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	rowLedger "github.com/akashc777/OneCamp/business/RowLedger"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// intoTable runs an import's rows step over a table already there, as an
// import into one will (tables B9's): the source's fields are the table's,
// by name.
func intoTable(ctx context.Context, actor Actor, tb *model.DataTable, rows []ImportRow) (*ImportedBase, error) {
	actor.Kind = rowLedger.ActorImport
	t := &ImportTable{Key: "t", Name: tb.Name, Rows: func(page func([]ImportRow) error) error { return page(rows) }}
	im := newImporter(ctx, actor, ImportBase{})
	im.out.Tables["t"], im.tables["t"] = tb.Id, t
	fields, err := model.ListFields(ctx, tb.Id)
	if err != nil {
		return nil, err
	}
	for _, f := range fields {
		t.Fields = append(t.Fields, ImportField{Key: f.Name, Name: f.Name, Type: f.Type})
		im.out.Fields["t/"+f.Name] = f.Id
		im.stored["t/"+f.Name] = true
	}
	err = im.makeRows(t)
	return im.out, err
}

// optionsOfField is a field's options as stored, by name.
func optionsOfField(t *testing.T, ctx context.Context, table uuid.UUID, name string) []map[string]any {
	t.Helper()
	fields, err := model.ListFields(ctx, table)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fields {
		if f.Name == name {
			var cfg struct {
				Options []map[string]any `json:"options"`
			}
			if err := json.Unmarshal([]byte(f.Config), &cfg); err != nil {
				t.Fatal(err)
			}
			return cfg.Options
		}
	}
	t.Fatalf("no field %q", name)
	return nil
}

// labelsOf is options' labels in their order, and checks that each added one
// (not in was) is as the app makes one by hand: an "o_" id of its own and no
// colour.
func labelsOf(t *testing.T, opts []map[string]any, was ...string) string {
	t.Helper()
	old := map[string]bool{}
	for _, l := range was {
		old[l] = true
	}
	var out []string
	for _, o := range opts {
		label, _ := o["label"].(string)
		out = append(out, label)
		if old[label] {
			continue
		}
		if id, _ := o["id"].(string); !strings.HasPrefix(id, "o_") || len(id) != 12 {
			t.Errorf("the option %q was added with the id %q, not one as the app makes", label, id)
		}
		if _, coloured := o["color"]; coloured || len(o) != 2 {
			t.Errorf("the option %q was added as %v, not as the app adds one (an id and its label)", label, o)
		}
	}
	return strings.Join(out, ", ")
}

// listOf is a multi-select cell's labels, in their order.
func listOf(cell any) string {
	list, _ := cell.([]any)
	var out []string
	for _, it := range list {
		out = append(out, fmt.Sprint(it))
	}
	return strings.Join(out, ", ")
}

// noteLines is notes as "code field: message", sorted.
func noteLines(notes []ImportNote) string {
	var out []string
	for _, n := range notes {
		out = append(out, fmt.Sprintf("%s %s: %s", n.Code, n.Field, n.Msg))
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

func TestAnImportedValueOutsideASelectsOptionsBecomesOne(t *testing.T) {
	w := newRelationWorld(t)
	ctx := w.ctx

	t.Run("in a table the import makes, a select's and a multi-select's", func(t *testing.T) {
		books := ImportTable{Key: "tbl", Name: "Reading list", Fields: []ImportField{
			{Key: "fTitle", Name: "Title", Type: model.FieldText},
			{Key: "fState", Name: "State", Type: model.FieldSelect, Config: map[string]any{"options": []map[string]any{{"id": "selR", "label": "Reading"}, {"id": "selD", "label": "Read"}}}},
			{Key: "fTags", Name: "Tags", Type: model.FieldMultiSelect, Config: map[string]any{"options": []map[string]any{{"label": "Classic"}}}},
		}}
		// Two pages: what the first adds is an option by the second.
		books.Rows = func(page func([]ImportRow) error) error {
			if err := page([]ImportRow{
				{Key: "r1", Cells: map[string]any{"fTitle": "Dune", "fState": "Read", "fTags": []string{"Classic", "Sci-fi"}}},
				{Key: "r2", Cells: map[string]any{"fTitle": "Emma", "fState": "Abandoned", "fTags": []string{"Sci-fi", "Romance"}}},
			}); err != nil {
				return err
			}
			return page([]ImportRow{{Key: "r3", Cells: map[string]any{"fTitle": "Ulysses", "fState": "Abandoned", "fTags": []string{"Romance"}}}})
		}
		made, err := ImportBaseAs(ctx, w.me, ImportBase{Tables: []ImportTable{books}})
		if err != nil {
			t.Fatalf("import: %v", err)
		}
		want := strings.Join([]string{
			`OPTION_ADDED fState: "State" gained the option "Abandoned"`,
			`OPTION_ADDED fTags: "Tags" gained the option "Romance"`,
			`OPTION_ADDED fTags: "Tags" gained the option "Sci-fi"`,
		}, "\n")
		if got := noteLines(made.Notes); got != want {
			t.Errorf("the notes are\n%s\nwant\n%s", got, want)
		}
		tb, _ := model.GetTableByID(ctx, made.Tables["tbl"])
		if got := labelsOf(t, optionsOfField(t, ctx, tb.Id, "State"), "Reading", "Read"); got != "Reading, Read, Abandoned" {
			t.Errorf("State's options are %s", got)
		}
		if got := labelsOf(t, optionsOfField(t, ctx, tb.Id, "Tags"), "Classic"); got != "Classic, Sci-fi, Romance" {
			t.Errorf("Tags' options are %s", got)
		}
		cells := w.cells(tb, &w.me)
		state, tags := made.Fields["tbl/fState"].String(), made.Fields["tbl/fTags"].String()
		for row, want := range map[string]string{"r1": "Read Classic, Sci-fi", "r2": "Abandoned Sci-fi, Romance", "r3": "Abandoned Romance"} {
			c := cells[made.Rows["tbl/"+row]]
			if got := fmt.Sprintf("%v %s", c[state], listOf(c[tags])); got != want {
				t.Errorf("%s holds %s, want %s", row, got, want)
			}
		}
	})

	// A table already there, its rows brought by an import into it.
	shelf := func(t *testing.T) (*model.DataTable, *model.Field) {
		t.Helper()
		tb, name := w.table("Shelf")
		w.field(tb, "State", model.FieldSelect, map[string]interface{}{"options": []map[string]interface{}{{"label": "Reading"}, {"label": "Read"}}})
		w.field(tb, "Tags", model.FieldMultiSelect, map[string]interface{}{"options": []map[string]interface{}{{"label": "Classic"}}})
		return tb, name
	}
	rows := []ImportRow{
		{Key: "r1", Cells: map[string]any{"Name": "Emma", "State": "Abandoned", "Tags": []string{"Classic", "Sci-fi"}}},
		{Key: "r2", Cells: map[string]any{"Name": "Dune", "State": "Read", "Tags": []string{"Classic"}}},
	}

	t.Run("in a table already there, by someone who may change its fields", func(t *testing.T) {
		tb, _ := shelf(t)
		made, err := intoTable(ctx, w.me, tb, rows)
		if err != nil {
			t.Fatalf("import: %v", err)
		}
		want := `OPTION_ADDED State: "State" gained the option "Abandoned"` + "\n" + `OPTION_ADDED Tags: "Tags" gained the option "Sci-fi"`
		if got := noteLines(made.Notes); got != want {
			t.Errorf("the notes are\n%s\nwant\n%s", got, want)
		}
		if got := labelsOf(t, optionsOfField(t, ctx, tb.Id, "State"), "Reading", "Read"); got != "Reading, Read, Abandoned" {
			t.Errorf("State's options are %s", got)
		}
		if got := labelsOf(t, optionsOfField(t, ctx, tb.Id, "Tags"), "Classic"); got != "Classic, Sci-fi" {
			t.Errorf("Tags' options are %s", got)
		}
		c := w.cells(tb, &w.me)[made.Rows["t/r1"]]
		if got := fmt.Sprintf("%v %s", c[made.Fields["t/State"].String()], listOf(c[made.Fields["t/Tags"].String()])); got != "Abandoned Classic, Sci-fi" {
			t.Errorf("Emma's row holds %s", got)
		}
	})

	t.Run("in a table already there, by someone who may not change its fields", func(t *testing.T) {
		tb, _ := shelf(t)
		made, err := intoTable(ctx, w.member, tb, rows)
		if err != nil {
			t.Fatalf("import: %v", err)
		}
		want := `CELL_LEFT_OUT State: "State" has no option "Abandoned": its options are Reading, Read` + "\n" +
			`CELL_LEFT_OUT Tags: "Tags" has no option "Sci-fi": its options are Classic`
		if got := noteLines(made.Notes); got != want {
			t.Errorf("the notes are\n%s\nwant\n%s", got, want)
		}
		for _, n := range made.Notes {
			if n.Row != "r1" {
				t.Errorf("a note names the row %q, want r1", n.Row)
			}
		}
		if got := labelsOf(t, optionsOfField(t, ctx, tb.Id, "State"), "Reading", "Read"); got != "Reading, Read" {
			t.Errorf("State's options are %s", got)
		}
		if got := labelsOf(t, optionsOfField(t, ctx, tb.Id, "Tags"), "Classic"); got != "Classic" {
			t.Errorf("Tags' options are %s", got)
		}
		cells := w.cells(tb, &w.me)
		emma := cells[made.Rows["t/r1"]]
		if emma[made.Fields["t/Name"].String()] != "Emma" || emma[made.Fields["t/State"].String()] != nil || emma[made.Fields["t/Tags"].String()] != nil {
			t.Errorf("Emma's row holds %v, want her name and the two cells left out", emma)
		}
		dune := cells[made.Rows["t/r2"]]
		if dune[made.Fields["t/State"].String()] != "Read" || listOf(dune[made.Fields["t/Tags"].String()]) != "Classic" {
			t.Errorf("Dune's row holds %v", dune)
		}
	})
}
