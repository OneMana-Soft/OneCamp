package business

import (
	"encoding/json"
	"reflect"
	"testing"

	fieldTypes "github.com/akashc777/OneCamp/business/FieldTypes"
	formula "github.com/akashc777/OneCamp/business/Formula"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// Each rule of a type change (TAB-BUG-9): what a cell becomes, and whether it
// is kept, converted or cleared.
func TestConvertCell(t *testing.T) {
	cell := func(s string) interface{} {
		var v interface{}
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	const kept, converted, cleared, emptied = "kept", "converted", "cleared", "emptied"
	cases := []struct {
		to, in string
		want   interface{}
		how    string
		extra  int // values cleared past the one kept
	}{
		{model.FieldNumber, `12.5`, 12.5, kept, 0},
		{model.FieldNumber, `"1,200"`, 1200.0, converted, 0},
		{model.FieldNumber, `"twelve"`, nil, cleared, 0},
		{model.FieldNumber, `true`, nil, cleared, 0},
		{model.FieldCheckbox, `true`, true, kept, 0},
		{model.FieldCheckbox, `"Yes"`, true, converted, 0},
		{model.FieldCheckbox, `"✓"`, true, converted, 0},
		{model.FieldCheckbox, `1`, true, converted, 0},
		{model.FieldCheckbox, `"maybe"`, nil, cleared, 0},
		{model.FieldDate, `"2026-10-14"`, "2026-10-14", kept, 0},
		{model.FieldDate, `"2026-10-14T09:30:00+05:30"`, "2026-10-14", converted, 0},
		{model.FieldDate, `"next week"`, nil, cleared, 0},
		{model.FieldMultiSelect, `"Design"`, []interface{}{"Design"}, converted, 0},
		{model.FieldMultiSelect, `["Design","Ops"]`, []interface{}{"Design", "Ops"}, kept, 0},
		{model.FieldSelect, `["Design","Ops","Web"]`, "Design", converted, 2},
		{model.FieldSelect, `"In review"`, "In review", kept, 0},
		{model.FieldText, `["Design","Ops"]`, "Design, Ops", converted, 0},
		{model.FieldText, `1200`, "1200", converted, 0},
		{model.FieldText, `true`, "Yes", converted, 0},
		{model.FieldText, `false`, nil, emptied, 0},
		{model.FieldSelect, `true`, "Yes", converted, 0},
		{model.FieldMultiSelect, `false`, nil, emptied, 0},
		{model.FieldURL, `"https://onemana.dev"`, "https://onemana.dev", kept, 0},
	}
	for _, c := range cases {
		out, isKept, gone := fieldTypes.ConvertCell(c.to, cell(c.in), formula.PlainSigns)
		how := converted
		switch {
		case out == nil && gone == 0:
			how = emptied
		case out == nil:
			how, gone = cleared, gone-1
		case isKept:
			how = kept
		}
		if !reflect.DeepEqual(out, c.want) || how != c.how || gone != c.extra {
			t.Errorf("%s to %s: %#v %s (%d more cleared), want %#v %s (%d)", c.in, c.to, out, how, gone, c.want, c.how, c.extra)
		}
	}
	for from, to := range map[string]string{model.FieldFormula: model.FieldText, model.FieldText: model.FieldRollup, model.FieldRelation: model.FieldText} {
		if converts(from, to) {
			t.Errorf("a %s field's cells are converted to %s", from, to)
		}
	}
}

// Text becomes a number as whoever changes the type writes numbers, and what
// reads two ways, or isn't finite, is cleared, and counted (the tables
// audit's B3): the totals' reading dropped every comma, so "1,5" was 15 for
// good, and one "NaN" failed the whole change.
func TestTextBecomesNumbersInItsWritersSigns(t *testing.T) {
	de := formula.NumberSigns{Decimal: ",", Group: "."}
	for _, c := range []struct {
		in    string
		signs formula.NumberSigns
		want  interface{}
	}{
		{"1,5", de, 1.5},
		{"1.234,56", de, 1234.56},
		{"1,234.56", formula.PlainSigns, 1234.56},
		{"1,5", formula.PlainSigns, nil},
		{"1,5", formula.NumberSigns{}, nil},
		{"1.234,56", formula.PlainSigns, nil},
		{"NaN", de, nil},
		{"Infinity", formula.PlainSigns, nil},
		{"1e400", formula.PlainSigns, nil},
	} {
		out, kept, cleared := fieldTypes.ConvertCell(model.FieldNumber, c.in, c.signs)
		if out != c.want || kept || (out == nil) != (cleared == 1) {
			t.Errorf("%q in %+v became %v (kept %v, %d cleared), want %v", c.in, c.signs, out, kept, cleared, c.want)
		}
	}
	f := &model.Field{Type: model.FieldNumber, Config: "{}"}
	cells := map[uuid.UUID]json.RawMessage{uuid.New(): json.RawMessage(`"NaN"`), uuid.New(): json.RawMessage(`"Inf"`), uuid.New(): json.RawMessage(`"1,5"`)}
	out, counts, err := conversion(f, cells, de)
	if err != nil || counts != (ConvertCounts{Converted: 1, Cleared: 2}) || len(out) != 3 {
		t.Errorf("converting NaN, Inf and 1,5 gave %v, %+v, %v", out, counts, err)
	}
}

// A select or a multi-select made of other cells offers every label they
// became, and a checkbox's cells become what the grid showed: a multi-select
// added no options, so its cells held labels nothing offered, and unticked
// boxes became "false".
func TestConversionsOfferWhatCellsBecame(t *testing.T) {
	cells := map[uuid.UUID]json.RawMessage{uuid.New(): json.RawMessage(`"Design"`), uuid.New(): json.RawMessage(`"Ops"`)}
	ticks := map[uuid.UUID]json.RawMessage{uuid.New(): json.RawMessage(`true`), uuid.New(): json.RawMessage(`false`)}
	for _, c := range []struct {
		typ   string
		cells map[uuid.UUID]json.RawMessage
		want  []string
	}{
		{model.FieldSelect, cells, []string{"Design", "Ops"}},
		{model.FieldMultiSelect, cells, []string{"Design", "Ops"}},
		{model.FieldSelect, ticks, []string{"Yes"}},
		{model.FieldMultiSelect, ticks, []string{"Yes"}},
	} {
		f := &model.Field{Type: c.typ, Config: "{}"}
		out, counts, err := conversion(f, c.cells, formula.PlainSigns)
		var cfg map[string]interface{}
		_ = json.Unmarshal([]byte(f.Config), &cfg)
		var labels []string
		for _, o := range fieldTypes.Options(cfg) {
			if fieldTypes.OptionID(o) == "" {
				t.Errorf("%s: option %v has no id", c.typ, o)
			}
			labels = append(labels, fieldTypes.OptionLabel(o))
		}
		if err != nil || counts.Cleared != 0 || !reflect.DeepEqual(labels, c.want) {
			t.Errorf("%s of %v offers %v (%+v, %v); cells %v", c.typ, c.cells, labels, counts, err, out)
		}
	}
}

// A link's cells are named for each reader on every read, so a link can't
// become another kind of field; the update refused it while its dry run
// said it would change nothing, and the menu saved it at once and failed.
func TestLinksStayLinks(t *testing.T) {
	toTable := &model.Field{Type: model.FieldRelation, Config: `{"relation_target":"table","table_id":"` + uuid.NewString() + `"}`}
	toItems := &model.Field{Type: model.FieldRelation, Config: `{"relation_target":"doc"}`}
	text := &model.Field{Type: model.FieldText, Config: `{}`}
	for _, c := range []struct {
		was     *model.Field
		to      string
		refused bool
	}{
		{toTable, model.FieldText, true},
		{toItems, model.FieldSelect, true},
		{toItems, model.FieldRelation, false},
		{text, model.FieldNumber, false},
		{text, model.FieldRelation, false},
	} {
		if err := typeChangeProblem(c.was, c.to); (err != nil) != c.refused {
			t.Errorf("%s to %s: %v", c.was.Config, c.to, err)
		}
	}
}
