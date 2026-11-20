package business

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	fieldTypes "github.com/akashc777/OneCamp/business/FieldTypes"
	tableDomain "github.com/akashc777/OneCamp/domain/DataTable"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// formulaRef is a field a formula names, {Name}.
var formulaRef = regexp.MustCompile(`\{([^}]+)\}`)

var rollupAggregates = map[string]bool{"count": true, "count_values": true, "unique_count": true, "sum": true,
	"average": true, "min": true, "max": true, "checked": true, "earliest": true, "latest": true, "list": true, "unique": true}

func templateField(tb *TemplateTable, key string) dataTableBusiness.ImportField {
	if tb != nil {
		for _, f := range tb.Fields {
			if f.Key == key {
				return f
			}
		}
	}
	return dataTableBusiness.ImportField{}
}

// Each built-in table template is whole: fixed ids, every link names a table
// of the template and its other side names it back, every view names its
// fields, every rollup names a link and a field of the table it links to,
// every formula names its own table's fields, every row's cells name fields
// rows store and its links name rows of the linked table, and the people in
// it are example addresses.
func TestBuiltinTableTemplatesAreWholeAndStable(t *testing.T) {
	want := []string{"CRM", "Hiring", "Content calendar", "Stock", "Budget"}
	if len(BuiltinTableTemplates) != len(want) {
		t.Fatalf("%d built-in table templates, want %d", len(BuiltinTableTemplates), len(want))
	}
	for i, b := range BuiltinTableTemplates {
		if id := uuid.MustParse(fmt.Sprintf("b5d0c000-0000-4000-8000-0000000001%02x", i+1)); b.ID != id || b.Name != want[i] {
			t.Errorf("template %d is %s %s, want %s %s (the web asks for it by id)", i, b.Name, b.ID, want[i], id)
		}
		if _, clash := builtinByID(b.ID); clash {
			t.Errorf("%s shares a doc template's id", b.Name)
		}
		if !helpers.IsCampHue(b.Hue) || b.Glyph == "" || b.Category == "" || b.Description == "" {
			t.Errorf("%s is missing something: %q %q %q %q", b.Name, b.Hue, b.Glyph, b.Category, b.Description)
		}
		if n := len(b.Tables); n < 2 || n > 3 {
			t.Errorf("%s makes %d tables, want 2 or 3", b.Name, n)
		}
		tables := map[string]*TemplateTable{}
		for j := range b.Tables {
			tables[b.Tables[j].Key] = &b.Tables[j]
		}
		made := map[string]bool{} // tables whose formulas an install has made, in its order
		for ti := range b.Tables {
			tb := &b.Tables[ti]
			where := b.Name + "/" + tb.Name
			fields := map[string]dataTableBusiness.ImportField{}
			names := map[string]bool{}
			for _, f := range tb.Fields {
				if _, twice := fields[f.Key]; twice {
					t.Errorf("%s: field key %s twice", where, f.Key)
				}
				fields[f.Key], names[strings.ToLower(f.Name)] = f, true
			}
			if len(tb.Fields) == 0 || tb.Fields[0].Type != model.FieldText || tb.Name == "" || tb.Description == "" {
				t.Errorf("%s needs a name, a line and a text name field first", where)
			}
			for _, f := range tb.Fields {
				switch f.Type {
				case model.FieldRelation:
					back := templateField(tables[f.LinksTo], f.Pair)
					if tables[f.LinksTo] == nil || back.Type != model.FieldRelation || back.LinksTo != tb.Key || back.Pair != f.Key {
						t.Errorf("%s.%s links to %q, whose %q doesn't link back", where, f.Name, f.LinksTo, f.Pair)
					}
				case model.FieldRollup:
					through := fields[f.Through]
					of := templateField(tables[through.LinksTo], f.Of)
					switch {
					case through.Type != model.FieldRelation:
						t.Errorf("%s.%s adds up through %q, not a link", where, f.Name, f.Through)
					case of.Key == "":
						t.Errorf("%s.%s adds up %q, not a field of %s", where, f.Name, f.Of, through.LinksTo)
					case of.Type == model.FieldFormula && !made[through.LinksTo]:
						t.Errorf("%s.%s adds up %s's formula before it's made: list that table first", where, f.Name, through.LinksTo)
					}
					if !rollupAggregates[f.Aggregate] {
						t.Errorf("%s.%s adds up as %q", where, f.Name, f.Aggregate)
					}
				case model.FieldFormula:
					for _, m := range formulaRef.FindAllStringSubmatch(f.Formula, -1) {
						if !names[strings.ToLower(m[1])] {
							t.Errorf("%s.%s reads {%s}, not a field of its table", where, f.Name, m[1])
						}
					}
				}
			}
			made[tb.Key] = true
			if len(tb.Views) == 0 {
				t.Errorf("%s has no view", where)
			}
			for _, v := range tb.Views {
				if !tableDomain.ValidViewType(v.Type) || v.Name == "" {
					t.Errorf("%s: view %q is a %q", where, v.Name, v.Type)
				}
				if (v.Type == tableDomain.ViewBoard) != (v.GroupBy != "") || (v.GroupBy != "" && fields[v.GroupBy].Type != model.FieldSelect) {
					t.Errorf("%s: view %q groups by %q: a board names a select, and only a board", where, v.Name, v.GroupBy)
				}
			}
			if len(tb.Rows) < 3 {
				t.Errorf("%s has %d rows, want a few that show its links", where, len(tb.Rows))
			}
			for _, r := range tb.Rows {
				for k, v := range r.Cells {
					f, ok := fields[k]
					if !ok || f.Type == model.FieldRelation || f.Type == model.FieldRollup || f.Type == model.FieldFormula {
						t.Errorf("%s row %s: %q is no field a row stores", where, r.Key, k)
					}
					s, _ := v.(string)
					if strings.Contains(s, "@") && !strings.HasSuffix(s, "@example.com") && !strings.HasSuffix(s, "@example.org") {
						t.Errorf("%s row %s: %q isn't an example address", where, r.Key, s)
					}
					if f.Type == model.FieldSelect {
						found := false
						for _, o := range fieldTypes.Options(f.Config) {
							found = found || fieldTypes.OptionLabel(o) == s
						}
						if !found {
							t.Errorf("%s row %s: %q isn't one of %s's options", where, r.Key, s, f.Name)
						}
					}
				}
				for k, keys := range r.Links {
					f := fields[k]
					linked := tables[f.LinksTo]
					for _, key := range keys {
						ok := false
						if linked != nil {
							for _, lr := range linked.Rows {
								ok = ok || lr.Key == key
							}
						}
						if f.Type != model.FieldRelation || !ok {
							t.Errorf("%s row %s: %s links to %q, no row of %q", where, r.Key, k, key, f.LinksTo)
						}
					}
				}
			}
		}
	}
	// The content calendar opens on its calendar.
	if b := BuiltinTableTemplates; len(b) > 2 && (len(b[2].Tables) == 0 || len(b[2].Tables[0].Views) == 0 || b[2].Tables[0].Views[0].Type != tableDomain.ViewCalendar) {
		t.Errorf("the content calendar doesn't open on a calendar")
	}
}
