package business

// J-F (jira-01): formula task fields on the Field Types registry, without a
// database: the registry holds TaskField's kinds (FieldTypes/contract.go:7),
// a formula field is checked as it's made, its values are never written,
// and an export reads them as text. Inputs are built from JSON so this file
// compiles against today's Input, which has no formula yet.

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	fieldTypes "github.com/akashc777/OneCamp/business/FieldTypes"
	formula "github.com/akashc777/OneCamp/business/Formula"
	model "github.com/akashc777/OneCamp/models/postgres/TaskField"
	"github.com/google/uuid"
)

// jfInput is a field as the app sends one.
func jfInput(t *testing.T, fields map[string]any) Input {
	t.Helper()
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	var in Input
	if err := json.Unmarshal(b, &in); err != nil {
		t.Fatal(err)
	}
	return in
}

// One registry: every kind a task field may be is a FieldTypes kind, money
// (a task field's only) and formula included, and a table can't take money.
func TestTaskFieldTypesAreFieldTypesKinds(t *testing.T) {
	want := []string{"text", "number", "money", "date", "select", "multi_select", "person", "checkbox", "url", "formula"}
	for _, id := range want {
		if !slices.Contains(Types, id) {
			t.Errorf("a task field can't be %q", id)
		}
		if _, ok := fieldTypes.Kinds.Lookup(id); !ok {
			t.Errorf("FieldTypes doesn't register the task field kind %q", id)
		}
	}
	if len(Types) != len(want) {
		t.Errorf("task field types %v, want %v", Types, want)
	}
	if fieldTypes.Kinds.Valid("money") {
		t.Error("a table's field may be money; only a task field may")
	}
	if k, ok := fieldTypes.Kinds.Lookup("formula"); !ok || k.Stored {
		t.Errorf("formula registered %v, stored %v: its values are worked out on each read", ok, k.Stored)
	}
	if k, _ := fieldTypes.Kinds.Lookup("money"); !k.Stored {
		t.Error("money's values are stored")
	}
}

// A formula field is made with its formula, which it keeps (a template
// keeps it as InputOf gives it); one with none, or too long, is refused in
// words that say so.
func TestCheckTakesAFormula(t *testing.T) {
	f, err := Check(jfInput(t, map[string]any{"name": "Margin", "type": "formula", "formula": "{Budget} - {Spent}"}), "formula")
	if err != nil || f == nil || f.Type != "formula" {
		t.Fatalf("a formula field: %+v, %v", f, err)
	}
	b, _ := json.Marshal(InputOf(f))
	var kept map[string]any
	_ = json.Unmarshal(b, &kept)
	if kept["formula"] != "{Budget} - {Spent}" {
		t.Errorf("the field as a template keeps it: %s", b)
	}
	for _, src := range []string{"", "   ", strings.Repeat("1+", 1001) + "1"} {
		_, err := Check(jfInput(t, map[string]any{"name": "Margin", "type": "formula", "formula": src}), "formula")
		var in *InputError
		if !errors.As(err, &in) || !strings.Contains(strings.ToLower(err.Error()), "formula") {
			t.Errorf("a formula of %d characters: %v", len(src), err)
		}
	}
	// A formula on a field of another kind is not kept.
	g, err := Check(jfInput(t, map[string]any{"name": "Notes", "type": "text", "formula": "1"}), "text")
	if err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal(InputOf(g))
	if strings.Contains(string(b), `"formula":"1"`) {
		t.Errorf("a text field kept a formula: %s", b)
	}
}

// Nothing writes a formula's value: not a value, not taking one off. Every
// writer (the panel, forms, imports, templates, the Task Writer) goes
// through Normalize, and SetValue answers its InputError as a 400.
func TestAFormulaValueIsNeverWritten(t *testing.T) {
	f := &model.Field{Name: "Margin", Type: "formula"}
	for _, raw := range []string{`5`, `"five"`, `true`, `null`} {
		_, err := Normalize(f, json.RawMessage(raw))
		var in *InputError
		if !errors.As(err, &in) || !strings.Contains(err.Error(), "worked out") {
			t.Errorf("setting Margin to %s: %v", raw, err)
		}
	}
}

// An export writes a formula's value as its cell reads: a number as
// written, text, a checkbox Yes or No, a day, and an error as Error.
func TestTextOfAFormulaValue(t *testing.T) {
	f := &model.Field{Name: "Margin", Type: "formula"}
	for raw, want := range map[string]string{
		`12.5`:          "12.5",
		`-3`:            "-3",
		`"Late"`:        "Late",
		`true`:          "Yes",
		`false`:         "No",
		`"2026-11-06"`:  "2026-11-06",
		`{"error":"x"}`: "Error",
		`null`:          "",
	} {
		if got := Text(f, json.RawMessage(raw), nil); got != want {
			t.Errorf("Text(%s) = %q, want %q", raw, got, want)
		}
	}
}

// Money is for project admins only, and so is every formula that reads it,
// directly or through other formulas (V-698's one rule, Hidden): nobody (a
// bare context) reads none of them; the server reads them all; a formula
// that reads no money is anyone's.
func TestAFormulaOverMoneyIsHiddenAsMoneyIs(t *testing.T) {
	project := uuid.New()
	made := func(sent map[string]any) *model.Field {
		t.Helper()
		f, err := Check(jfInput(t, sent), sent["type"].(string))
		if err != nil {
			t.Fatalf("make %v: %v", sent["name"], err)
		}
		f.ID, f.ProjectID = uuid.New(), project
		return f
	}
	hours := made(map[string]any{"name": "Hours", "type": "number"})
	budget := made(map[string]any{"name": "Budget", "type": "money", "currency": "USD"})
	spend := made(map[string]any{"name": "Spend", "type": "formula", "formula": "{Budget} * 2"})
	spendMore := made(map[string]any{"name": "Spend more", "type": "formula", "formula": "{Spend} + {Hours}"})
	double := made(map[string]any{"name": "Double", "type": "formula", "formula": "{Hours} * 2"})
	fields := []*model.Field{hours, budget, spend, spendMore, double}

	hidden := Hidden(context.Background(), Reader{}, fields)
	for _, f := range []*model.Field{budget, spend, spendMore} {
		if !hidden[f.ID.String()] {
			t.Errorf("%s isn't hidden from nobody", f.Name)
		}
	}
	for _, f := range []*model.Field{hours, double} {
		if hidden[f.ID.String()] {
			t.Errorf("%s reads no money, yet it's hidden", f.Name)
		}
	}
	if got := Readable(context.Background(), Reader{}, fields); len(got) != 2 {
		t.Errorf("nobody's columns: %d, want Hours and Double", len(got))
	}
	if all := Hidden(context.Background(), Reader{System: true}, fields); len(all) != 0 {
		t.Errorf("the server is kept from %v", all)
	}
}

// One budget message for tasks and tables alike (RULINGS Q4).
func TestTheBudgetSaysOneThingForTasksAndTables(t *testing.T) {
	if got := formula.RanOut(); got.Kind != formula.KindError || got.Str != "These formulas take too much working out" {
		t.Errorf("a read past its budget says %q", got.Str)
	}
}
