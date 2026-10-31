package business

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	fieldTypes "github.com/akashc777/OneCamp/business/FieldTypes"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// A read works out tables B1's kinds for its reader: people named as links
// to people are, a row's id, when it was made and last changed and by whom
// from the row itself, and a formula reading them sees them as numbers and
// dates, not text.
func TestEachReadWorksOutPeopleAndTheRowsStamps(t *testing.T) {
	w := newItemsWorld(t)
	table := uuid.New()
	maya, bo := uuid.New(), uuid.New()
	w.may("", fieldTypes.ItemUser, maya.String(), "Maya Iyer")
	w.may("", fieldTypes.ItemUser, bo.String(), "Bo Chen")
	field := func(name, kind, cfg string) *model.Field {
		return &model.Field{Id: uuid.New(), TableId: table, Name: name, Type: kind, Config: cfg}
	}
	people := field("Owners", model.FieldPeople, `{}`)
	id := field("ID", model.FieldAutonumber, `{"prefix":"DEAL"}`)
	made := field("Made", model.FieldCreatedTime, `{}`)
	madeBy := field("Made by", model.FieldCreatedBy, `{}`)
	changed := field("Changed", model.FieldModifiedTime, `{}`)
	changedBy := field("Changed by", model.FieldModifiedBy, `{}`)
	next := field("Next id", model.FieldFormula, fmt.Sprintf(`{"formula":"{#%s} + 1"}`, id.Id))
	year := field("Year made", model.FieldFormula, fmt.Sprintf(`{"formula":"YEAR({#%s})"}`, made.Id))
	fields := []*model.Field{people, id, made, madeBy, changed, changedBy, next, year}

	at := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	row := &model.Row{
		Id: uuid.New(), RowNo: 12, CreatedAt: at, UpdatedAt: at.Add(time.Hour), CreatedBy: &maya, UpdatedBy: &bo,
		// A forged label stored with a person, and stamps a writer tried to
		// keep: neither is read.
		Values: fmt.Sprintf(`{%q:[{"id":%q,"type":"user","label":"Forged"}],%q:"Nobody",%q:99}`, people.Id, bo, madeBy.Id, id.Id),
	}
	withComputed(asViewer(context.Background(), Actor{UserID: maya}), fields, []*model.Row{row})
	var cells map[string]json.RawMessage
	if err := json.Unmarshal([]byte(row.Values), &cells); err != nil {
		t.Fatalf("the row read back isn't JSON: %v", err)
	}
	cell := func(f *model.Field) string { return string(cells[f.Id.String()]) }
	if got := cell(people); !strings.Contains(got, `"label":"Bo Chen"`) || strings.Contains(got, "Forged") {
		t.Errorf("the people cell: %s", got)
	}
	if cell(id) != "12" || cell(next) != "13" {
		t.Errorf("the id %s and the formula over it %s", cell(id), cell(next))
	}
	if cell(made) != `"2026-10-17T09:00:00Z"` || cell(changed) != `"2026-10-17T10:00:00Z"` || cell(year) != "2026" {
		t.Errorf("made %s, changed %s, the year it was made %s", cell(made), cell(changed), cell(year))
	}
	if got := cell(madeBy); !strings.Contains(got, `"label":"Maya Iyer"`) || strings.Contains(got, "Nobody") {
		t.Errorf("made by: %s", got)
	}
	if got := cell(changedBy); !strings.Contains(got, `"label":"Bo Chen"`) {
		t.Errorf("changed by: %s", got)
	}
}

// A field's kind keeps its own settings as it is saved: a status saved with
// no options starts with three in their groups, and a rating's most stars
// must be a whole number from 1 to 10.
func TestAFieldsKindKeepsItsSettings(t *testing.T) {
	f, _, err := buildField(uuid.New(), FieldInput{Name: "Stage", Type: model.FieldStatus}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	_ = json.Unmarshal([]byte(f.Config), &cfg)
	opts := fieldTypes.Options(cfg)
	if len(opts) != 3 || fieldTypes.OptionID(opts[0]) == "" || fieldTypes.StatusGroup(opts[2]) != fieldTypes.StatusDone {
		t.Errorf("a new status's options: %v", opts)
	}
	if _, _, err := buildField(uuid.New(), FieldInput{Name: "Stars", Type: model.FieldRating, Config: map[string]interface{}{"max": 12.0}}, nil, false); err == nil {
		t.Error("a rating up to 12 stars was saved")
	}
}
