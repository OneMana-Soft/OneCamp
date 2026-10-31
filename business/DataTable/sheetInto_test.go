package business

import (
	"fmt"
	"strings"
	"testing"
	"time"

	contentIOBusiness "github.com/akashc777/OneCamp/business/ContentIO"
	formula "github.com/akashc777/OneCamp/business/Formula"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// intoDeals is a table of the person's, Deals, for a sheet to come into:
// Name, Amount, Due, Owner (a person, which a sheet's cells can't fill) and
// Notes.
func intoDeals() (map[uuid.UUID]targetTable, *model.DataTable, map[string]*model.Field) {
	table := &model.DataTable{Id: uuid.New(), Name: "Deals"}
	fields := map[string]*model.Field{}
	var list []*model.Field
	for _, f := range [][2]string{{"Name", model.FieldText}, {"Amount", model.FieldNumber}, {"Due", model.FieldDate}, {"Owner", model.FieldPerson}, {"Notes", model.FieldLongText}} {
		fields[f[0]] = &model.Field{Id: uuid.New(), TableId: table.Id, Name: f[0], Type: f[1]}
		list = append(list, fields[f[0]])
	}
	return map[uuid.UUID]targetTable{table.Id: {table: table, fields: list}}, table, fields
}

// A sheet brought into a table of the person's: each column goes into the
// field of its name, ignoring case, or the one chosen for it, its cells read
// as that field's kind. A column with no such field, one whose field an
// earlier column fills, and one whose field a sheet's cells can't fill, is
// left out and said, by its name (a second "Name" is "Name 2", as the preview
// shows it), as no field of the import has its key. The table keeps its name
// and fields.
func TestAColumnGoesIntoTheFieldOfItsName(t *testing.T) {
	into, table, fields := intoDeals()
	sheet := sheetOfRows([]string{"name", "AMOUNT", "Owner", "Extra", "Due", "Remarks", "Name"},
		[]string{"Ada", "1,200.50", "ada@example.com", "x", "23/10/2026", "first", "again"})
	chosen := []sheetChoice{{Index: 0, Into: &table.Id, MatchBy: "c1", Fields: []fieldChoice{{Key: "c6", Field: fields["Notes"].Id.String()}}}}
	base, err := sheetBase([]contentIOBusiness.Sheet{sheet}, SheetOptions{Signs: formula.PlainSigns, DayFirst: true, Zone: time.UTC}, chosen, into, func(int, int) {})
	if err != nil || len(base.Tables) != 1 {
		t.Fatalf("%d tables, %v", len(base.Tables), err)
	}
	got := base.Tables[0]
	if got.Into != table.Id || got.Name != "Deals" || got.MatchBy != "c1" {
		t.Errorf("the sheet comes into %v as %q, by %q", got.Into, got.Name, got.MatchBy)
	}
	var goes []string
	for _, f := range got.Fields {
		itsField := fields[f.Name] != nil && f.Into == fields[f.Name].Id
		goes = append(goes, fmt.Sprintf("%s>%s:%s:%t", f.Key, f.Name, f.Type, itsField))
	}
	if want := "[c1>Name:text:true c2>Amount:number:true c5>Due:date:true c6>Notes:long_text:true]"; fmt.Sprint(goes) != want {
		t.Errorf("the columns go into %v, want %s", goes, want)
	}
	var notes []string
	for _, n := range base.Notes {
		notes = append(notes, n.Table+"/"+n.Field+":"+n.Code+" "+n.Msg)
	}
	if want := `[Sheet/Owner:COLUMN_LEFT_OUT "Owner" is a person field, which a sheet's cells can't fill, so the column isn't brought. ` +
		`Sheet/Extra:COLUMN_LEFT_OUT The table has no field named "Extra", so the column isn't brought. ` +
		`Sheet/Name 2:COLUMN_LEFT_OUT An earlier column fills "Name", so this one isn't brought.]`; fmt.Sprint(notes) != want {
		t.Errorf("the notes are %v, want %s", notes, want)
	}
	var rows []string
	if err := got.Rows(func(page []ImportRow) error {
		for _, r := range page {
			rows = append(rows, fmt.Sprint(r.Cells))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if want := "[map[c1:Ada c2:1200.5 c5:2026-10-23 c6:first]]"; fmt.Sprint(rows) != want {
		t.Errorf("the rows are %v, want %s", rows, want)
	}
}

// Choices for a sheet coming into a table that can't be followed are said:
// a field the table doesn't have, a column's own name or kind (it takes its
// field's), and rows named by a column that doesn't come or whose field
// can't name a row.
func TestChoicesIntoATableThatCantBeFollowedAreSaid(t *testing.T) {
	into, table, fields := intoDeals()
	sheet := sheetOfRows([]string{"Name", "Extra", "Notes", "Due"}, []string{"Ada", "x", "long", "23/10/2026"})
	for _, c := range []struct {
		ch   sheetChoice
		says string
	}{
		{sheetChoice{Fields: []fieldChoice{{Key: "c1", Field: uuid.NewString()}}}, "a field the table doesn't have"},
		{sheetChoice{Fields: []fieldChoice{{Key: "c1", Type: model.FieldNumber}}}, "takes its field's name and kind"},
		{sheetChoice{Fields: []fieldChoice{{Key: "c1", Name: "Title"}}}, "takes its field's name and kind"},
		{sheetChoice{MatchBy: "c2"}, "a column that doesn't come"},
		{sheetChoice{MatchBy: "c9"}, "a column that doesn't come"},
		{sheetChoice{MatchBy: "c3", Fields: []fieldChoice{{Key: "c3", Field: fields["Notes"].Id.String()}}}, "can't name a row"},
	} {
		c.ch.Into = &table.Id
		if _, err := sheetBase([]contentIOBusiness.Sheet{sheet}, SheetOptions{Signs: formula.PlainSigns, DayFirst: true, Zone: time.UTC}, []sheetChoice{c.ch}, into, func(int, int) {}); err == nil || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%+v: %v, want it said: %s", c.ch, err, c.says)
		}
	}
}

// A preview shows each sheet as the person's choices make it. One chosen into
// a table shows that table, the column whose values name its rows, and each
// column in the field it goes into, with that field's name and kind and none
// of the settings guessed for the column, or left out and why. One of its
// own shows the names and kinds chosen, and what a kind leaves out.
func TestAPreviewShowsEachSheetAsChosen(t *testing.T) {
	into, table, fields := intoDeals()
	deals := sheetOfRows([]string{"name", "AMOUNT", "Owner", "Extra", "Due", "Remarks", "Name"},
		[]string{"Ada", "1,200.50", "ada@example.com", "x", "23/10/2026", "first", "again"})
	prices := sheetOfRows([]string{"Item", "Price"}, []string{"Tea", "1.50"}, []string{"Cake", "a lot"})
	chosen := []sheetChoice{
		{Index: 0, Into: &table.Id, MatchBy: "c1", Fields: []fieldChoice{{Key: "c6", Field: fields["Notes"].Id.String()}}},
		{Index: 1, Fields: []fieldChoice{{Key: "c1", Name: "Cake or tea"}, {Key: "c2", Type: model.FieldNumber}}},
	}
	p, err := previewOf([]contentIOBusiness.Sheet{deals, prices}, SheetOptions{Signs: formula.PlainSigns, DayFirst: true, Zone: time.UTC}, chosen, into)
	if err != nil || len(p.Sheets) != 2 {
		t.Fatalf("%d sheets, %v", len(p.Sheets), err)
	}
	if s := p.Sheets[0]; s.Into == nil || *s.Into != table.Id || s.IntoName != "Deals" || s.MatchBy != "c1" {
		t.Errorf("the first sheet shows it comes into %v (%q) by %q", s.Into, s.IntoName, s.MatchBy)
	}
	if s := p.Sheets[1]; s.Into != nil || s.IntoName != "" || s.MatchBy != "" {
		t.Errorf("the second sheet shows it comes into %v (%q) by %q", s.Into, s.IntoName, s.MatchBy)
	}
	named := map[string]string{}
	for name, f := range fields {
		named[f.Id.String()] = name
	}
	var shown, notes []string
	for i, s := range p.Sheets {
		for _, f := range s.Fields {
			col := fmt.Sprintf("%d/%s>%s:%s", i, f.Key, f.Name, f.Type)
			if f.Field != "" {
				col += "@" + named[f.Field]
				if f.Config != nil {
					col += "+guessed"
				}
			}
			if f.Leave {
				col += " out"
			}
			shown = append(shown, col)
		}
		for _, n := range s.Notes {
			notes = append(notes, fmt.Sprintf("%d/%s:%s %s", i, n.Field, n.Code, n.Msg))
		}
	}
	if want := "[0/c1>Name:text@Name 0/c2>Amount:number@Amount 0/c3>Owner:email out 0/c4>Extra:text out 0/c5>Due:date@Due 0/c6>Notes:long_text@Notes 0/c7>Name 2:text out " +
		"1/c1>Cake or tea:text 1/c2>Price:number]"; fmt.Sprint(shown) != want {
		t.Errorf("the preview shows\n%v,\nwant\n%s", shown, want)
	}
	if want := `[0/c3:COLUMN_LEFT_OUT "Owner" is a person field, which a sheet's cells can't fill, so the column isn't brought. ` +
		`0/c4:COLUMN_LEFT_OUT The table has no field named "Extra", so the column isn't brought. ` +
		`0/c7:COLUMN_LEFT_OUT An earlier column fills "Name", so this one isn't brought. ` +
		`1/c2:CELL_LEFT_OUT A cell isn't a number, so it's left empty.]`; fmt.Sprint(notes) != want {
		t.Errorf("the preview's notes are\n%v,\nwant\n%s", notes, want)
	}
}
