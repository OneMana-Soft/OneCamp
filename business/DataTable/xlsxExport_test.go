package business

import (
	"fmt"
	"slices"
	"testing"
	"time"

	contentIOBusiness "github.com/akashc777/OneCamp/business/ContentIO"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// An export's cell is what the grid shows, as a workbook holds it: numbers
// with their currency, percent or length of time, an id with its prefix, a
// day or a moment on the reader's clock, a checkbox unticked as FALSE, a
// formula that failed as #VALUE!, and a list by its names, with what the
// reader can't open as one phrase.
func TestAnExportsCellIsWhatTheGridShows(t *testing.T) {
	field := func(typ, config string) *model.Field {
		return &model.Field{Id: uuid.New(), Type: typ, Config: config}
	}
	when := field(model.FieldFormula, `{}`)
	e := &TableExport{zone: time.FixedZone("IST", 5*3600+1800), kinds: map[string]string{when.Id.String(): model.FieldDate}}
	day := func(y int, m time.Month, d, h, min int) contentIOBusiness.Cell {
		return contentIOBusiness.DayCell(time.Date(y, m, d, h, min, 0, 0, time.UTC))
	}
	for _, c := range []struct {
		what string
		f    *model.Field
		v    any
		want contentIOBusiness.Cell
	}{
		{"an amount in dollars", field(model.FieldNumber, `{"format":{"currency":"USD","decimals":2}}`), 1250.5,
			contentIOBusiness.Cell{Kind: contentIOBusiness.CellNumber, Number: 1250.5, Currency: "USD"}},
		{"a percent", field(model.FieldNumber, `{"format":{"percent":true}}`), 12.5,
			contentIOBusiness.Cell{Kind: contentIOBusiness.CellNumber, Number: 12.5, Percent: true}},
		{"a duration's seconds", field(model.FieldDuration, `{"format":"h:mm:ss"}`), 5415.0, contentIOBusiness.DurationCell(5415)},
		{"a formula's number shown as a duration", field(model.FieldFormula, `{"format":{"duration":"h:mm"}}`), 5400.0, contentIOBusiness.DurationCell(5400)},
		{"an id with its prefix", field(model.FieldAutonumber, `{"prefix":"DEAL"}`), 12.0, contentIOBusiness.Cell{Text: "DEAL-12"}},
		{"an id without one", field(model.FieldAutonumber, `{}`), 12.0, contentIOBusiness.Cell{Kind: contentIOBusiness.CellNumber, Number: 12}},
		{"a day", field(model.FieldDate, `{}`), "2026-10-23", day(2026, 10, 23, 0, 0)},
		{"a moment, on the reader's clock", field(model.FieldCreatedTime, `{}`), "2026-10-23T20:00:00Z", day(2026, 10, 24, 1, 30)},
		{"a formula's date", when, "2026-10-23T08:15:00Z", day(2026, 10, 23, 13, 45)},
		{"text that isn't a day", field(model.FieldDate, `{}`), "soon", contentIOBusiness.Cell{Text: "soon"}},
		{"a ticked checkbox", field(model.FieldCheckbox, `{}`), true, contentIOBusiness.BoolCell(true)},
		{"an unticked one", field(model.FieldCheckbox, `{}`), nil, contentIOBusiness.BoolCell(false)},
		{"an empty cell", field(model.FieldText, `{}`), nil, contentIOBusiness.Cell{}},
		{"text like a formula", field(model.FieldText, `{}`), "=SUM(A1:A9)", contentIOBusiness.Cell{Text: "=SUM(A1:A9)"}},
		{"a formula that failed", field(model.FieldFormula, `{}`), map[string]any{"error": "#DIV/0!"},
			contentIOBusiness.Cell{Kind: contentIOBusiness.CellError, Text: "#VALUE!"}},
		{"a person", field(model.FieldCreatedBy, `{}`), map[string]any{"id": "u1", "name": "Asha"}, contentIOBusiness.Cell{Text: "Asha"}},
		{"a person without a name", field(model.FieldCreatedBy, `{}`), map[string]any{"id": "u2"}, contentIOBusiness.Cell{Text: "Something you can't open"}},
		{"a list by its names, one hidden", field(model.FieldRelation, `{}`),
			[]any{map[string]any{"id": "r1", "label": "Acme"}, map[string]any{"id": "r2", "hidden": true}, map[string]any{"type": "more", "label": "2 more"}},
			contentIOBusiness.Cell{Text: "Acme, 2 more, Something you can't open"}},
		{"a list with three hidden", field(model.FieldPeople, `{}`),
			[]any{map[string]any{"hidden": true}, map[string]any{"hidden": true}, map[string]any{"hidden": true}},
			contentIOBusiness.Cell{Text: "3 things you can't open"}},
		{"a multi-select", field(model.FieldMultiSelect, `{}`), []any{"Hot", "Big"}, contentIOBusiness.Cell{Text: "Hot, Big"}},
	} {
		if got := e.cell(c.f, c.v); fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", c.want) {
			t.Errorf("%s: %+v; want %+v", c.what, got, c.want)
		}
	}
}

// An export's column is the field's name, its width from the view's pixels,
// its fixed decimal places when the field sets them (0 to 6), a length of
// time's seconds, and a select's options for its dropdown.
func TestAnExportsColumnIsTheFieldsAsShown(t *testing.T) {
	e := &TableExport{}
	stage := &model.Field{Id: uuid.New(), Name: "Stage", Type: model.FieldSelect, Config: `{"options":[{"id":"o_1","label":"Won"},"Lost"]}`}
	if got := e.column(stage, 140); got.Name != "Stage" || got.Width != 20 || !slices.Equal(got.Choices, []string{"Won", "Lost"}) {
		t.Errorf("a select's column: %+v", got)
	}
	if got := e.column(&model.Field{Type: model.FieldNumber, Config: `{"format":{"decimals":2}}`}, 0); !got.Fixed || got.Places != 2 || got.Width != 0 {
		t.Errorf("a number with two places: %+v", got)
	}
	if got := e.column(&model.Field{Type: model.FieldNumber, Config: `{"format":{"decimals":9}}`}, 0); got.Fixed {
		t.Errorf("a number with places past six keeps each number's own: %+v", got)
	}
	if got := e.column(&model.Field{Type: model.FieldDuration, Config: `{"format":"h:mm:ss"}`}, 0); !got.Seconds {
		t.Errorf("a duration with seconds: %+v", got)
	}
}

// A table the reader can't open reads as one that isn't there, at the
// export's check and at its rows' (RowsByID checks again, so access lost
// between the two is said the same way); any other failure is itself.
func TestATableTheReaderCantOpenIsNotThereAtEitherCheck(t *testing.T) {
	if got := unseen(errForbidden); got != errNotFound {
		t.Errorf("a table the reader can't open reads as %v, want not found", got)
	}
	if got := unseen(errRowsUnavailable); got != errRowsUnavailable {
		t.Errorf("rows that couldn't be read read as %v, want themselves", got)
	}
}
