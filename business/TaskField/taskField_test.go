package business

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	model "github.com/akashc777/OneCamp/models/postgres/TaskField"
	"github.com/google/uuid"
)

func TestCheckAField(t *testing.T) {
	f, err := Check(Input{Name: "  Launch   channel ", Options: []OptionInput{
		{Label: "Blog"}, {ID: "0a1b2c3d", Label: " Email ", Color: "violet"}, {ID: "not-an-id", Label: "Social", Color: "chartreuse"},
	}, OnCard: true}, TypeSelect)
	if err != nil {
		t.Fatal(err)
	}
	if f.Name != "Launch channel" || !f.OnCard || len(f.Options) != 3 {
		t.Fatalf("%+v", f)
	}
	if !isOptionID(f.Options[0].ID) || f.Options[1].ID != "0a1b2c3d" || f.Options[1].Label != "Email" || f.Options[1].Color != "violet" {
		t.Errorf("options keep the ids they have and get ones they don't: %+v", f.Options)
	}
	if !isOptionID(f.Options[2].ID) || f.Options[2].Color == "chartreuse" {
		t.Errorf("a made-up id and colour are replaced: %+v", f.Options[2])
	}
	bad := []struct {
		in  Input
		typ string
		say string
	}{
		{Input{Name: " "}, TypeText, "name"},
		{Input{Name: strings.Repeat("a", 41)}, TypeText, "name"},
		{Input{Name: "Size"}, "slider", "kind"},
		{Input{Name: "Size", Options: []OptionInput{{Label: "S"}, {Label: "s"}}}, TypeSelect, "twice"},
		{Input{Name: "Size", Options: []OptionInput{{Label: ""}}}, TypeMultiSelect, "option"},
		{Input{Name: "Budget", Currency: "rupees"}, TypeMoney, "three-letter"},
	}
	for _, b := range bad {
		var ie *InputError
		if _, err := Check(b.in, b.typ); !errors.As(err, &ie) || !strings.Contains(err.Error(), b.say) {
			t.Errorf("%+v as %s: %v, want a message about %q", b.in, b.typ, err, b.say)
		}
	}
	if f, err := Check(Input{Name: "Budget", Currency: " inr "}, TypeMoney); err != nil || f.Currency != "INR" {
		t.Errorf("currency: %+v %v", f, err)
	}
	if f, _ := Check(Input{Name: "Notes", Options: []OptionInput{{Label: "x"}}, Currency: "EUR"}, TypeText); len(f.Options) != 0 || f.Currency != "" {
		t.Errorf("only choice fields keep options, only money a currency: %+v", f)
	}
}

func TestNormalizeValues(t *testing.T) {
	field := func(typ string) *model.Field {
		return &model.Field{Name: "F", Type: typ, Options: []model.Option{{ID: "aaaa1111", Label: "One"}, {ID: "bbbb2222", Label: "Two"}}}
	}
	person := uuid.NewString()
	ok := []struct {
		typ, in, want string
	}{
		{TypeText, `"  hello "`, `"hello"`},
		{TypeText, `""`, ``},
		{TypeURL, `"https://example.com/a"`, `"https://example.com/a"`},
		{TypeNumber, `12.5`, `12.5`},
		{TypeNumber, `-3`, `-3`},
		{TypeMoney, `125000`, `125000`},
		{TypeDate, `"2026-10-31"`, `"2026-10-31"`},
		{TypeSelect, `"bbbb2222"`, `"bbbb2222"`},
		{TypeMultiSelect, `["bbbb2222","aaaa1111","bbbb2222"]`, `["bbbb2222","aaaa1111"]`},
		{TypeMultiSelect, `[]`, ``},
		// An option taken away is left out rather than refused.
		{TypeMultiSelect, `["aaaa1111","cccc3333"]`, `["aaaa1111"]`},
		{TypeMultiSelect, `["cccc3333"]`, ``},
		{TypePerson, `"` + strings.ToUpper(person) + `"`, `"` + person + `"`},
		{TypeCheckbox, `true`, `true`},
		{TypeCheckbox, `false`, ``},
		{TypeSelect, `null`, ``},
	}
	for _, c := range ok {
		got, err := Normalize(field(c.typ), json.RawMessage(c.in))
		if err != nil || string(got) != c.want {
			t.Errorf("%s %s: %s %v, want %s", c.typ, c.in, got, err, c.want)
		}
	}
	bad := []struct{ typ, in string }{
		{TypeURL, `"javascript:alert(1)"`},
		{TypeURL, `"example.com"`},
		{TypeNumber, `"12"`},
		{TypeNumber, `1e16`},
		{TypeMoney, `12.5`},
		{TypeDate, `"31/10/2026"`},
		{TypeSelect, `"cccc3333"`},
		{TypePerson, `"someone"`},
		{TypeCheckbox, `"yes"`},
		{TypeText, `"` + strings.Repeat("a", MaxTextLength+1) + `"`},
	}
	for _, c := range bad {
		var ie *InputError
		if _, err := Normalize(field(c.typ), json.RawMessage(c.in)); !errors.As(err, &ie) {
			t.Errorf("%s %s: %v, want a message for the person", c.typ, c.in, err)
		}
	}
}

func TestFilterIDs(t *testing.T) {
	id := uuid.New()
	fid := FilterID(id)
	if len(fid) != 38 || strings.Contains(fid, "-") {
		t.Fatalf("%s: a filter id is field_ and 32 hex digits", fid)
	}
	if got, ok := FieldOfFilter(fid); !ok || got != id {
		t.Errorf("read back %v %v", got, ok)
	}
	if _, ok := FieldOfFilter("task_cycle"); ok {
		t.Error("task_cycle isn't a field")
	}
	if _, ok := FieldOfFilter("field_nothex"); ok {
		t.Error("not an id")
	}
	if inClause(nil) != matchNothing || inClause([]string{"a"}) != `eq(task_uuid, ["a"])` {
		t.Errorf("%s / %s", inClause(nil), inClause([]string{"a"}))
	}
}

func TestSameValue(t *testing.T) {
	if !sameValue(json.RawMessage(`["a", "b"]`), json.RawMessage(`["a","b"]`)) || sameValue(json.RawMessage(`["a"]`), nil) || !sameValue(nil, nil) {
		t.Error("values compare by what they hold")
	}
	if sameValue(json.RawMessage(`["a","b"]`), json.RawMessage(`["b","a"]`)) {
		t.Error("a multi-select's order is its own")
	}
}
