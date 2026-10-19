package business

import (
	"encoding/json"
	"strings"
	"testing"

	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

func field(kind, name string) *model.Field {
	return &model.Field{Id: uuid.New(), Name: name, Type: kind, Config: "{}"}
}

// A new row gets each field's default it doesn't name, as the field's kind
// stores it; a row being edited never does, and a cell a new row names
// (even empty) keeps what it names.
func TestDefaultsFillNewRowsOnly(t *testing.T) {
	stage, amount := field(model.FieldText, "Stage"), field(model.FieldNumber, "Amount")
	stage.Default, amount.Default = json.RawMessage(`"Lead"`), json.RawMessage(`10`)
	fields := []*model.Field{stage, amount}
	got, err := Kinds.Check(fields, map[string]any{amount.Id.String(): ""}, Input{New: true})
	if err != nil || got[stage.Id.String()] != "Lead" || got[amount.Id.String()] != "" {
		t.Errorf("a new row: %v %v", got, err)
	}
	edit, err := Kinds.Check(fields, map[string]any{}, Input{})
	if err != nil || len(edit) != 0 {
		t.Errorf("an edit got defaults: %v %v", edit, err)
	}
}

// A complete write (the API, an AI tool, a form) must give every required
// field; the grid's blank new row needn't; a default counts as given.
func TestRequiredFieldsOnCompleteWrites(t *testing.T) {
	name, email := field(model.FieldText, "Name"), field(model.FieldEmail, "Email")
	name.Required, email.Required = true, true
	email.Default = json.RawMessage(`"team@example.com"`)
	fields := []*model.Field{name, email}
	_, err := Kinds.Check(fields, map[string]any{name.Id.String(): "  "}, Input{New: true, Complete: true})
	cells, ok := err.(CellErrors)
	if !ok || len(cells) != 1 || cells[0].Field != name.Id.String() || !strings.Contains(cells[0].Msg, `"Name" is required`) {
		t.Errorf("a blank required field: %v", err)
	}
	if _, err := Kinds.Check(fields, map[string]any{}, Input{New: true}); err != nil {
		t.Errorf("the grid's blank new row was refused: %v", err)
	}
	if _, err := Kinds.Check(fields, map[string]any{name.Id.String(): "Maya"}, Input{New: true, Complete: true}); err != nil {
		t.Errorf("a complete row was refused: %v", err)
	}
}

// Every cell a write can't store is said, each naming its field.
func TestEveryRefusedCellIsSaid(t *testing.T) {
	a, b, c := field(model.FieldNumber, "A"), field(model.FieldDate, "B"), field(model.FieldText, "C")
	_, err := Kinds.Check([]*model.Field{a, b, c}, map[string]any{a.Id.String(): "x", b.Id.String(): "y", c.Id.String(): "fine"}, Input{})
	cells, _ := err.(CellErrors)
	if len(cells) != 2 || err.Error() != cells[0].Msg {
		t.Errorf("refusals %v", cells)
	}
}

// A whole row's cells as they're stored pass unchecked: a value stored
// before writes were checked doesn't make an edit of another cell fail.
func TestAStoredCellPassesUnchecked(t *testing.T) {
	amount, notes := field(model.FieldNumber, "Amount"), field(model.FieldText, "Notes")
	stored := map[string]any{amount.Id.String(): "about 12"}
	write := map[string]any{amount.Id.String(): "about 12", notes.Id.String(): "new"}
	got, err := Kinds.Check([]*model.Field{amount, notes}, write, Input{Unchanged: stored})
	if err != nil || got[amount.Id.String()] != "about 12" || got[notes.Id.String()] != "new" {
		t.Errorf("an edit beside an old cell: %v %v", got, err)
	}
	write[amount.Id.String()] = "about 13"
	if _, err := Kinds.Check([]*model.Field{amount, notes}, write, Input{Unchanged: stored}); err == nil {
		t.Error("a changed cell wasn't checked")
	}
}

// A default is stored as its kind stores a write of it; a kind no new row
// starts with has none.
func TestDefaults(t *testing.T) {
	n := field(model.FieldNumber, "Amount")
	if v, err := Kinds.CheckDefault(n, "1,5"); err == nil {
		t.Errorf("a default that isn't a number: %v", v)
	}
	if v, err := Kinds.CheckDefault(n, "12"); err != nil || v != 12.0 {
		t.Errorf("a number's default: %v %v", v, err)
	}
	if v, err := Kinds.CheckDefault(n, ""); err != nil || v != nil {
		t.Errorf("no default: %v %v", v, err)
	}
	for _, kind := range []string{model.FieldRelation, model.FieldFormula, model.FieldRollup} {
		if _, err := Kinds.CheckDefault(field(kind, "X"), "a"); err == nil {
			t.Errorf("a %s has a default", kind)
		}
	}
}

// A writer in the server may hand over typed values (an import's []string,
// an int): they're checked as the same values in a request's body are.
func TestTypedValuesFromTheServerAreReadAsJSON(t *testing.T) {
	multi := field(model.FieldMultiSelect, "Tags")
	multi.Config = `{"options": [{"label": "a"}, {"label": "b"}]}`
	n := field(model.FieldNumber, "N")
	got, err := Kinds.Check([]*model.Field{multi, n}, map[string]any{multi.Id.String(): []string{"b", "a"}, n.Id.String(): 3}, Input{})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(got[multi.Id.String()]); string(b) != `["b","a"]` || got[n.Id.String()] != 3.0 {
		t.Errorf("typed values stored as %s and %#v", b, got[n.Id.String()])
	}
}
