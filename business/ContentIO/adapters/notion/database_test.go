package notion

import (
	"reflect"
	"testing"

	dataTableModels "github.com/akashc777/OneCamp/models/postgres/DataTable"
)

// A Notion property becomes the column kind nearest it, and anything no kind
// holds comes across as text.
func TestNotionPropertiesBecomeTheNearestColumnKind(t *testing.T) {
	for notion, want := range map[string]string{
		"title": dataTableModels.FieldText, "rich_text": dataTableModels.FieldText, "number": dataTableModels.FieldNumber,
		"select": dataTableModels.FieldSelect, "status": dataTableModels.FieldSelect, "multi_select": dataTableModels.FieldMultiSelect,
		"date": dataTableModels.FieldDate, "created_time": dataTableModels.FieldDate, "checkbox": dataTableModels.FieldCheckbox,
		"url": dataTableModels.FieldURL, "email": dataTableModels.FieldEmail, "formula": dataTableModels.FieldText,
		"relation": dataTableModels.FieldText, "people": dataTableModels.FieldText,
	} {
		if got := columnKind(notion); got != want {
			t.Errorf("%s: %s, want %s", notion, got, want)
		}
	}
}

// A value is kept as its column keeps it: a multi-select's single choice as a
// list, a list in a text column as its words, and nothing as nothing.
func TestCellsAreKeptAsTheirColumnKeepsThem(t *testing.T) {
	cases := []struct {
		kind string
		in   any
		want any
	}{
		{dataTableModels.FieldMultiSelect, "web", []string{"web"}},
		{dataTableModels.FieldMultiSelect, []string{"web", "ios"}, []string{"web", "ios"}},
		{dataTableModels.FieldText, []string{"Maya", "Ada"}, "Maya, Ada"},
		{dataTableModels.FieldText, "  ", nil},
		{dataTableModels.FieldMultiSelect, []string{}, nil},
		{dataTableModels.FieldNumber, 42.5, 42.5},
		{dataTableModels.FieldCheckbox, false, false},
		{dataTableModels.FieldText, nil, nil},
	}
	for _, c := range cases {
		if got := cellValue(c.kind, c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s %#v: %#v, want %#v", c.kind, c.in, got, c.want)
		}
	}
}
