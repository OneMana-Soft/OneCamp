package business

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	formula "github.com/akashc777/OneCamp/business/Formula"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

func fieldOf(kind string, cfg map[string]any) *model.Field {
	b, _ := json.Marshal(cfg)
	return &model.Field{Id: uuid.New(), Name: "Field", Type: kind, Config: string(b)}
}

func checkOne(t *testing.T, f *model.Field, v any) (any, error) {
	t.Helper()
	out, err := Kinds.Check([]*model.Field{f}, map[string]any{f.Id.String(): v}, Input{})
	if err != nil {
		return nil, err
	}
	return out[f.Id.String()], nil
}

// A status saved with no options starts with the three a new one has, each
// in its group; options saved without a group are to do, and a status whose
// options were all taken out keeps none.
func TestAStatusKeepsItsOptionsInGroups(t *testing.T) {
	cfg, err := Kinds.ConfigFor(model.FieldStatus, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, o := range Options(cfg) {
		got = append(got, OptionLabel(o)+"/"+StatusGroup(o))
	}
	if strings.Join(got, ",") != "Not started/todo,In progress/doing,Done/done" {
		t.Errorf("a new status starts with %v", got)
	}
	cfg, _ = Kinds.ConfigFor(model.FieldStatus, map[string]any{"options": []any{
		map[string]any{"label": "Waiting"}, map[string]any{"label": "Shipped", "group": "done"}, map[string]any{"label": "Odd", "group": "later"},
	}})
	got = nil
	for _, o := range Options(cfg) {
		got = append(got, OptionLabel(o)+"/"+StatusGroup(o))
	}
	if strings.Join(got, ",") != "Waiting/todo,Shipped/done,Odd/todo" {
		t.Errorf("groups kept as %v", got)
	}
	if cfg, _ := Kinds.ConfigFor(model.FieldStatus, map[string]any{"options": []any{}}); len(Options(cfg)) != 0 {
		t.Errorf("a status with every option taken out has %v", Options(cfg))
	}
	f := fieldOf(model.FieldStatus, map[string]any{"options": []any{map[string]any{"id": "o_1", "label": "Doing", "group": "doing"}}})
	if v, err := checkOne(t, f, "o_1"); err != nil || v != "Doing" {
		t.Errorf("a status by its option's id: %v %v", v, err)
	}
	if _, err := checkOne(t, f, "Later"); err == nil {
		t.Error("a status that isn't one of the field's options was stored")
	}
}

// People are members by their ids, each once, kept as a link to people is;
// a name or an email is no one (D-TB-4 matches those), and a field that
// takes one person takes one.
func TestPeopleAreMembersByTheirIDs(t *testing.T) {
	a, b := uuid.NewString(), uuid.NewString()
	f := fieldOf(model.FieldPeople, nil)
	v, err := checkOne(t, f, []any{a, map[string]any{"id": b}, map[string]any{"id": a, "type": "user"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []ItemRef{{ID: a, Type: ItemUser}, {ID: b, Type: ItemUser}}
	if !reflect.DeepEqual(v, want) {
		t.Errorf("people %#v, want %#v", v, want)
	}
	for _, bad := range []any{"Priya Shah", "priya@example.com", []any{map[string]any{"id": a, "type": "doc"}}} {
		if _, err := checkOne(t, f, bad); err == nil {
			t.Errorf("%v was taken as people", bad)
		}
	}
	one := fieldOf(model.FieldPeople, map[string]any{"one": true})
	if _, err := checkOne(t, one, []any{a, b}); err == nil {
		t.Error("two people in a field that takes one")
	}
	if _, err := Kinds.ConfigFor(model.FieldPeople, map[string]any{"one": "yes"}); err == nil {
		t.Error("a people field's 'one' took text")
	}
}

// A phone number is kept as written, when it is one: digits with the marks
// numbers are written with, and an extension.
func TestAPhoneNumberIsKeptAsWritten(t *testing.T) {
	f := fieldOf(model.FieldPhone, nil)
	for in, want := range map[any]string{"+91 98765 43210": "+91 98765 43210", " (020) 7946-0958 ": "(020) 7946-0958", "555-0100 ext 12": "555-0100 ext 12", 9876543210.0: "9876543210"} {
		if v, err := checkOne(t, f, in); err != nil || v != want {
			t.Errorf("%v: %v %v, want %q", in, v, err, want)
		}
	}
	for _, bad := range []any{"call me", "12", "+91 98765 43210 then press 2", strings.Repeat("1", 41), 12.5} {
		if _, err := checkOne(t, f, bad); err == nil {
			t.Errorf("%v was taken as a phone number", bad)
		}
	}
}

// A phone number names a row by what tells it apart (an import matching rows
// by a phone column), whatever spaces, brackets, dots or dashes it's written
// with: a country's code however it's written, and an extension.
func TestAPhoneNumberIsTheSameNumberHoweverItsWritten(t *testing.T) {
	for _, c := range []struct{ in, key string }{
		{"+1 555 0100", "+15550100"},
		{"(+1) 555-0100", "+15550100"},
		{"+44 20 7946 0958", "+442079460958"},
		{"0044 20 7946 0958", "+442079460958"},
		{"(020) 7946-0958", "02079460958"},
		{"1 555 0100", "15550100"},
		{"555-0100 ext. 12", "5550100x12"},
		{"555.0100 x12", "5550100x12"},
		{" +1 (555) 010-0199 extension 12 ", "+15550100199x12"},
		{"555+0100", "5550100"},
		{"", ""},
	} {
		if got := PhoneKey(c.in); got != c.key {
			t.Errorf("PhoneKey(%q) = %q, want %q", c.in, got, c.key)
		}
	}
}

// A duration is kept in seconds, written as a clock, in units, as hours with
// their minutes, or as minutes alone; a writer's JSON number is seconds.
func TestADurationIsKeptInSeconds(t *testing.T) {
	for in, want := range map[string]float64{"1:30": 5400, "1:30:15": 5415, "0:45": 2700, "1h 30m": 5400, "90m": 5400, "45s": 45, "1.5h": 5400, "2d": 172800, "2 hours": 7200, "90": 5400, "1.5": 90, "1h30": 5400, "2 hrs 15": 8100} {
		if got, ok := ParseDuration(in); !ok || got != want {
			t.Errorf("%q: %v %v, want %v", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "soon", "1:75", "h", "-5m"} {
		if got, ok := ParseDuration(bad); ok {
			t.Errorf("%q read as %v", bad, got)
		}
	}
	f := fieldOf(model.FieldDuration, nil)
	if v, err := checkOne(t, f, 90.0); err != nil || v != 90.0 {
		t.Errorf("seconds as given: %v %v", v, err)
	}
	if v, err := checkOne(t, f, "90"); err != nil || v != 5400.0 {
		t.Errorf("minutes as written: %v %v", v, err)
	}
	german := Input{Signs: formula.NumberSigns{Decimal: ",", Group: "."}}
	if out, err := Kinds.Check([]*model.Field{f}, map[string]any{f.Id.String(): "1,5"}, german); err != nil || out[f.Id.String()] != 90.0 {
		t.Errorf("minutes in German signs: %v %v", out, err)
	}
	if _, err := checkOne(t, f, -1.0); err == nil {
		t.Error("a negative duration was stored")
	}
	if _, err := Kinds.ConfigFor(model.FieldDuration, map[string]any{"format": "mm:ss"}); err == nil {
		t.Error("a duration shown as mm:ss")
	}
}

// A rating is a whole number of stars, 1 to the field's most (5 unless it
// says up to 10).
func TestARatingIsWholeStarsUpToTheMost(t *testing.T) {
	f := fieldOf(model.FieldRating, nil)
	if v, err := checkOne(t, f, 3.0); err != nil || v != 3.0 {
		t.Errorf("3 stars: %v %v", v, err)
	}
	for _, bad := range []any{6.0, 0.0, 2.5, "lots"} {
		if _, err := checkOne(t, f, bad); err == nil {
			t.Errorf("%v stars were stored", bad)
		}
	}
	ten := fieldOf(model.FieldRating, map[string]any{"max": 10.0})
	if v, err := checkOne(t, ten, 9.0); err != nil || v != 9.0 {
		t.Errorf("9 of 10: %v %v", v, err)
	}
	for _, bad := range []any{0.0, 11.0, 2.5, "five"} {
		if _, err := Kinds.ConfigFor(model.FieldRating, map[string]any{"max": bad}); err == nil {
			t.Errorf("a rating up to %v", bad)
		}
	}
}

// An id, when a row was made and changed and by whom are the row's own, read
// from it; nobody writes them, and the last change is whoever made the row
// until someone changes it.
func TestWhatIsReadFromTheRow(t *testing.T) {
	maya, bo := uuid.New(), uuid.New()
	made := time.Date(2026, 10, 17, 9, 0, 0, 0, time.FixedZone("IST", 19800))
	changed := made.Add(3 * time.Hour)
	row := &model.Row{RowNo: 12, CreatedAt: made, UpdatedAt: changed, CreatedBy: &maya}
	read := func(kind string) any {
		k, _ := Kinds.Lookup(kind)
		return k.FromRow(fieldOf(kind, nil), row)
	}
	if read(model.FieldAutonumber) != 12.0 {
		t.Errorf("the row's number: %v", read(model.FieldAutonumber))
	}
	if read(model.FieldCreatedTime) != "2026-10-17T03:30:00Z" || read(model.FieldModifiedTime) != "2026-10-17T06:30:00Z" {
		t.Errorf("made %v, changed %v", read(model.FieldCreatedTime), read(model.FieldModifiedTime))
	}
	want := []ItemRef{{ID: maya.String(), Type: ItemUser}}
	if !reflect.DeepEqual(read(model.FieldCreatedBy), want) || !reflect.DeepEqual(read(model.FieldModifiedBy), want) {
		t.Errorf("made by %v, changed by %v", read(model.FieldCreatedBy), read(model.FieldModifiedBy))
	}
	row.UpdatedBy = &bo
	if got := read(model.FieldModifiedBy); !reflect.DeepEqual(got, []ItemRef{{ID: bo.String(), Type: ItemUser}}) {
		t.Errorf("changed by %v", got)
	}
	row.RowNo = 0
	if read(model.FieldAutonumber) != nil {
		t.Error("a row with no number yet has one")
	}
	// None of them is written: a write naming them leaves them out.
	f := fieldOf(model.FieldCreatedBy, nil)
	out, err := Kinds.Check([]*model.Field{f}, map[string]any{f.Id.String(): "someone"}, Input{})
	if err != nil || out[f.Id.String()] != "someone" {
		t.Fatalf("not checked here (the caller strips what isn't stored): %v %v", out, err)
	}
	if _, err := Kinds.CheckDefault(f, "someone"); err == nil {
		t.Error("a row's stamp took a default")
	}
	// An id's prefix: up to 12 letters, digits, _ or -.
	if cfg, err := Kinds.ConfigFor(model.FieldAutonumber, map[string]any{"prefix": " DEAL "}); err != nil || cfg["prefix"] != "DEAL" {
		t.Errorf("the prefix kept as %v, %v", cfg, err)
	}
	if _, err := Kinds.ConfigFor(model.FieldAutonumber, map[string]any{"prefix": "DEAL #"}); err == nil {
		t.Error("a prefix with a space and a #")
	}
	if p := IDPrefix(fieldOf(model.FieldAutonumber, map[string]any{"prefix": "OPS"})); p != "OPS" {
		t.Errorf("the prefix read as %q", p)
	}
}

// A files cell holds files of its table, each once, by id, with its name,
// size and type; a file of another place, or one a writer can't ask about,
// is refused, and a name is cleaned.
func TestFilesHoldTheirTablesFiles(t *testing.T) {
	f := &model.Field{Name: "Contracts", Type: model.FieldAttachment}
	mine, other := "0a6c1f0e-0000-4000-8000-000000000001", "0a6c1f0e-0000-4000-8000-000000000002"
	ours := Input{Files: func(ids []string) (map[string]bool, error) {
		out := map[string]bool{}
		for _, id := range ids {
			out[id] = id == mine
		}
		return out, nil
	}}
	got, err := coerceFiles(f, []any{
		map[string]any{"id": mine, "name": "plan\x07.pdf", "size": 2048.0, "mime": "application/pdf"},
		map[string]any{"id": strings.ToUpper(mine), "name": "again"},
	}, ours)
	if err != nil || !reflect.DeepEqual(got, []FileRef{{ID: mine, Name: "plan.pdf", Size: 2048, Mime: "application/pdf"}}) {
		t.Errorf("a file of the table: %#v %v", got, err)
	}
	if _, err := coerceFiles(f, []any{map[string]any{"id": other}}, ours); err == nil {
		t.Error("another place's file was taken")
	}
	if _, err := coerceFiles(f, []any{map[string]any{"id": mine}}, Input{}); err == nil {
		t.Error("a writer that can't ask took a file")
	}
	if _, err := coerceFiles(f, []any{"plan.pdf"}, ours); err == nil {
		t.Error("a name for a file was taken")
	}
	if got, err := coerceFiles(f, []any{}, ours); err != nil || len(got.([]FileRef)) != 0 {
		t.Errorf("no files: %v %v", got, err)
	}
}

// A box is ticked or not as people write it, in any case: yes and no, true
// and false, y and n, 1 and 0, ✓ and ✔ for ticked, ✗ and ✘ for not, and
// nothing for not.
func TestATickIsReadAsPeopleWriteIt(t *testing.T) {
	for in, want := range map[string]bool{"yes": true, "No": false, "TRUE": true, "false": false, "Y": true, "n": false,
		"1": true, "0": false, "✓": true, "✔": true, "✗": false, "✘": false, " ": false} {
		if ticked, ok := Truth(in); !ok || ticked != want {
			t.Errorf("%q reads as %v (%v), want %v", in, ticked, ok, want)
		}
	}
	for _, s := range []string{"maybe", "2", "ok"} {
		if _, ok := Truth(s); ok {
			t.Errorf("%q read as a tick", s)
		}
	}
}
