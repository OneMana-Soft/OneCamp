package business

import (
	"encoding/json"
	"testing"

	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
)

// A column's change leaves its entries as they were unless it changes what an
// entry is made from: a change to how the column looks walks no row, options
// only moved move their places alone (V-455, CR-212 row 2500 F4).
func TestWhatAColumnsChangeLeavesItsEntriesNeeding(t *testing.T) {
	state := func(typ, config string) *model.ColumnState {
		return &model.ColumnState{Name: "Stage", Type: typ, Config: json.RawMessage(config), Position: 2}
	}
	const (
		two     = `{"options":[{"id":"o1","label":"Open","color":"blue"},{"id":"o2","label":"Won","color":"green","group":"Done"}]}`
		related = `{"relation_target":"table","inverse_of":"5b0f2c9e-6a43-4f38-9a7e-2a3c1d9e8b11"}`
	)
	choice := state(model.FieldSelect, two)
	trashed := *choice
	trashed.Deleted = true
	cases := []struct {
		name     string
		was, now *model.ColumnState
		want     Reindex
	}{
		{"made", nil, choice, MakeEntries},
		{"brought back", &trashed, choice, MakeEntries},
		{"sent to the trash", choice, &trashed, KeepEntries},
		{"renamed", choice, &model.ColumnState{Name: "Phase", Type: model.FieldSelect, Config: json.RawMessage(two), Position: 2}, KeepEntries},
		{"an option recoloured", choice, state(model.FieldSelect, `{"options":[{"id":"o1","label":"Open","color":"red"},{"id":"o2","label":"Won","color":"green","group":"Done"}]}`), KeepEntries},
		{"an option's group changed", choice, state(model.FieldSelect, `{"options":[{"id":"o1","label":"Open","color":"blue"},{"id":"o2","label":"Won","color":"green","group":"Closed"}]}`), KeepEntries},
		{"an option's id changed", choice, state(model.FieldSelect, `{"options":[{"id":"o9","label":"Open","color":"blue"},{"id":"o2","label":"Won","color":"green","group":"Done"}]}`), KeepEntries},
		{"a label's case and spaces", choice, state(model.FieldSelect, `{"options":[{"id":"o1","label":" open ","color":"blue"},{"id":"o2","label":"Won","color":"green","group":"Done"}]}`), KeepEntries},
		{"a text's wrap", state(model.FieldText, `{}`), state(model.FieldText, `{"wrap":true}`), KeepEntries},
		{"a number's format", state(model.FieldNumber, `{"format":"number"}`), state(model.FieldNumber, `{"format":"currency","decimals":2}`), KeepEntries},
		{"a date's format", state(model.FieldDate, `{}`), state(model.FieldDate, `{"format":"long","include_time":true}`), KeepEntries},
		{"a formula changed", state(model.FieldFormula, `{"formula":"{Amount} * 2"}`), state(model.FieldFormula, `{"formula":"{Amount} * 3"}`), KeepEntries},
		{"select to status", choice, state(model.FieldStatus, two), KeepEntries},
		{"options reordered", choice, state(model.FieldSelect, `{"options":[{"id":"o2","label":"Won","color":"green","group":"Done"},{"id":"o1","label":"Open","color":"blue"}]}`), PlaceOptions},
		{"an option added", choice, state(model.FieldSelect, `{"options":[{"id":"o1","label":"Open","color":"blue"},{"id":"o2","label":"Won","color":"green","group":"Done"},{"id":"o3","label":"Lost"}]}`), PlaceOptions},
		{"an option relabelled", choice, state(model.FieldSelect, `{"options":[{"id":"o1","label":"New","color":"blue"},{"id":"o2","label":"Won","color":"green","group":"Done"}]}`), PlaceOptions},
		{"retyped: text to date", state(model.FieldText, `{}`), state(model.FieldDate, `{}`), MakeEntries},
		{"retyped: select to text", choice, state(model.FieldText, two), MakeEntries},
		{"retyped: select to multiple", choice, state(model.FieldMultiSelect, two), MakeEntries},
		{"retyped: number to created time", state(model.FieldNumber, `{}`), state(model.FieldCreatedTime, `{}`), MakeEntries},
		{"a relation's target", state(model.FieldRelation, `{}`), state(model.FieldRelation, related), MakeEntries},
		{"a relation's side", state(model.FieldRelation, `{"relation_target":"table"}`), state(model.FieldRelation, related), MakeEntries},
	}
	for _, c := range cases {
		if got := EntriesAfter(c.was, c.now); got != c.want {
			t.Errorf("a column %s: %d, want %d", c.name, got, c.want)
		}
	}
	if got := EntriesAfter(nil, nil); got != KeepEntries {
		t.Errorf("no column before or after: %d, want none", got)
	}
}
