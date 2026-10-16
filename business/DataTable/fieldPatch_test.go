package business

import (
	"reflect"
	"strconv"
	"testing"

	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
)

// A field save names what it changes; what it leaves out keeps its stored
// value. A save sent its page's copy whole, and one without a place moved
// the column to the front.
func TestAFieldSaveKeepsWhatItLeavesOut(t *testing.T) {
	f := &model.Field{Name: "Status", Type: model.FieldSelect, Config: `{"options":[{"id":"a","label":"Todo"}],"tinted":true}`, Position: 3}
	in := FieldPatch{}.onto(f)
	if in.Name != "Status" || in.Type != model.FieldSelect || in.Position != 3 || len(optionsIn(in.Config)) != 1 || in.Config["tinted"] != true {
		t.Errorf("a save naming nothing leaves %+v", in)
	}
	front, blank := 0.0, " "
	in = FieldPatch{Name: optStr("State"), Type: &blank, Position: &front, Config: map[string]interface{}{}}.onto(f)
	if in.Name != "State" || in.Type != model.FieldSelect || in.Position != 0 || len(in.Config) != 0 {
		t.Errorf("a save naming a name, a place and a config leaves %+v", in)
	}
}

// A field save naming its name, type and place together is a whole field, as
// the web released before patches sends every save; the web since names only
// what it changes.
func TestAWholeFieldIsNamedWhole(t *testing.T) {
	name, typ, at := "Status", model.FieldSelect, 2.0
	if !(FieldPatch{Name: &name, Type: &typ, Position: &at}).whole() {
		t.Error("a save naming the name, type and place isn't whole")
	}
	for _, p := range []FieldPatch{{Name: &name}, {Type: &typ, Config: map[string]interface{}{}}, {Position: &at}, {Name: &name, Type: &typ}} {
		if p.whole() {
			t.Errorf("%+v is read as a whole field", p)
		}
	}
}

// A header naming both the name and who can see the table is the web's from
// before patches (app/app/tables/[table-id]/page.tsx's saveTable, and the
// list's rename, in v2.72.2), which sends its copy of the header whole with
// each save: read for the one change it makes, against the table as stored.
func TestAWholeHeaderIsReadForItsOneChange(t *testing.T) {
	draft := "Draft"
	stored := &model.DataTable{Name: "Plan", Description: &draft, Visibility: model.VisibilityWorkspace}
	s := func(v string) *string { return &v }
	cases := []struct {
		name string
		sent TablePatch
		want TablePatch
		err  bool
	}{
		{name: "a rename", sent: TablePatch{Name: s("Plan Q4"), Description: s("Draft"), Visibility: s("workspace")}, want: TablePatch{Name: s("Plan Q4")}},
		{name: "a description", sent: TablePatch{Name: s("Plan"), Description: s("New"), Visibility: s("workspace")}, want: TablePatch{Description: s("New")}},
		{name: "a description cleared, which that page leaves out", sent: TablePatch{Name: s("Plan"), Visibility: s("workspace")}, want: TablePatch{Description: s("")}},
		{name: "Make private", sent: TablePatch{Name: s("Plan"), Description: s("Draft"), Visibility: s("private")}, want: TablePatch{Visibility: s("private")}},
		{name: "nothing", sent: TablePatch{Name: s("Plan"), Description: s("Draft"), Visibility: s("workspace")}, want: TablePatch{Name: s("Plan")}},
		{name: "the copy's icon is no change", sent: TablePatch{Name: s("Plan Q4"), Description: s("Draft"), Icon: s("📈"), Visibility: s("workspace")}, want: TablePatch{Name: s("Plan Q4")}},
		{name: "a rename from a copy read before the table was made shown", sent: TablePatch{Name: s("Plan Q4"), Description: s("Draft"), Visibility: s("private")}, err: true},
		{name: "a description from such a copy", sent: TablePatch{Name: s("Plan"), Description: s("New"), Visibility: s("private")}, err: true},
		{name: "a rename from a copy read before the description changed", sent: TablePatch{Name: s("Plan Q4"), Description: s("Old"), Visibility: s("workspace")}, err: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := wholeHeader(stored, c.sent)
			if c.err {
				if err != errStaleHeader {
					t.Errorf("answered %+v, %v; want errStaleHeader", got, err)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, c.want) {
				t.Errorf("read as %s, %v; want %s", headerString(got), err, headerString(c.want))
			}
		})
	}
}

// headerString writes a header patch out, with what each field names.
func headerString(p TablePatch) string {
	v := func(s *string) string {
		if s == nil {
			return "-"
		}
		return strconv.Quote(*s)
	}
	return "{name " + v(p.Name) + " description " + v(p.Description) + " icon " + v(p.Icon) + " visibility " + v(p.Visibility) + "}"
}
