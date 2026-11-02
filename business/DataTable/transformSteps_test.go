package business

import (
	"strings"
	"testing"

	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

func textField(kind string) *model.Field {
	return &model.Field{Id: uuid.New(), Name: "Name", Type: kind, Config: "{}"}
}

func mustFind(t *testing.T, text string, matchCase, wholeCell, regex bool) *finder {
	t.Helper()
	f, err := compileFind(text, matchCase, wholeCell, regex)
	if err != nil {
		t.Fatalf("compileFind(%q): %v", text, err)
	}
	return f
}

// A find matches as typed: any case unless asked, inside a cell unless the
// whole cell is asked for, and a pattern only when it is asked to be one.
func TestAFindMatchesAsTyped(t *testing.T) {
	cases := []struct {
		find                        string
		matchCase, wholeCell, regex bool
		cell                        string
		want                        bool
	}{
		{"acme", false, false, false, "ACME Corp", true},
		{"acme", true, false, false, "ACME Corp", false},
		{"acme", false, true, false, "Acme", true},
		{"acme", false, true, false, "Acme Corp", false},
		{"a.c", false, false, false, "abc", false},
		{"a.c", false, false, false, "a.c", true},
		{"a.c", false, false, true, "abc", true},
		{"^a.*e$", false, false, true, "acme", true},
		{"co|inc", false, true, true, "Inc", true},
		{"co|inc", false, true, true, "Acme Inc", false},
		{"ünï", false, false, false, "BÜNÏON", true},
	}
	for _, c := range cases {
		f := mustFind(t, c.find, c.matchCase, c.wholeCell, c.regex)
		if got := f.matches(c.cell); got != c.want {
			t.Errorf("find %q (case %v, whole %v, regex %v) in %q = %v, want %v", c.find, c.matchCase, c.wholeCell, c.regex, c.cell, got, c.want)
		}
	}
}

// What can't be found is refused in words before any row is read. Spaces
// alone are a find: two spaces, to replace with one.
func TestAFindThatCantMatchIsRefused(t *testing.T) {
	for _, c := range []struct {
		find  string
		regex bool
		says  string
	}{
		{"", false, "what to find"},
		{"(", true, "regular expression"},
		{"x*", true, "empty cell"},
		{strings.Repeat("a", maxFindText+1), false, "at most"},
	} {
		_, err := compileFind(c.find, false, false, c.regex)
		if err == nil || !strings.Contains(err.Error(), c.says) {
			t.Errorf("compileFind(%q, regex %v) = %v, want an error saying %q", c.find, c.regex, err, c.says)
		}
	}
	if _, err := compileFind("  ", false, false, false); err != nil {
		t.Errorf("a find of two spaces was refused: %v", err)
	}
}

// Replace changes every match in a cell; the whole cell when asked; a
// pattern's groups only for a pattern.
func TestAReplaceChangesEveryMatchInACell(t *testing.T) {
	cases := []struct {
		find, with                  string
		matchCase, wholeCell, regex bool
		cell, want                  string
	}{
		{"acme", "Beta", false, false, false, "Acme and acme", "Beta and Beta"},
		{"acme", "Beta", true, false, false, "Acme and acme", "Acme and Beta"},
		{"acme", "Beta", false, true, false, "acme", "Beta"},
		{"acme", "Beta", false, true, false, "acme co", "acme co"},
		{`(\w+)@old\.com`, "$1@new.com", false, false, true, "ann@old.com, bo@old.com", "ann@new.com, bo@new.com"},
		{"old", "$1", false, false, false, "old", "$1"},
		{"x", "", false, false, false, "xax", "a"},
	}
	for _, c := range cases {
		f := mustFind(t, c.find, c.matchCase, c.wholeCell, c.regex)
		if got := f.replace(c.cell, c.with); got != c.want {
			t.Errorf("replace %q with %q in %q = %q, want %q", c.find, c.with, c.cell, got, c.want)
		}
	}
}

// Find reads a cell as a person reads it: text, numbers written plainly,
// lists by their names; never a tick, an error or a link it can't open.
func TestFindReadsACellAsAPersonReadsIt(t *testing.T) {
	f := textField(model.FieldText)
	cases := []struct {
		v    any
		want string
		ok   bool
	}{
		{"Acme", "Acme", true},
		{1234.5, "1234.5", true},
		{float64(3), "3", true},
		{true, "", false},
		{nil, "", false},
		{[]any{"Red", "Blue"}, "Red, Blue", true},
		{map[string]any{"error": "#DIV/0!"}, "", false},
		{[]any{map[string]any{"id": uuid.NewString(), "hidden": true}}, "", false},
	}
	for _, c := range cases {
		got, ok := findText(f, c.v)
		if got != c.want || ok != c.ok {
			t.Errorf("findText(%#v) = %q, %v; want %q, %v", c.v, got, ok, c.want, c.ok)
		}
	}
}

// Trim takes spaces off both ends and runs of them inside down to one,
// no-break spaces too; a long text keeps its lines.
func TestTrimTidiesSpaces(t *testing.T) {
	cases := []struct {
		in        string
		multiline bool
		want      string
	}{
		{"  Acme   Corp \t", false, "Acme Corp"},
		{"Acme  Corp", false, "Acme Corp"},
		{"line one\nline two", false, "line one line two"},
		{"\n  first  line \r\n\r\n second\tline  \n\n", true, "first line\n\nsecond line"},
		{"tidy", false, "tidy"},
		{"   ", false, ""},
	}
	for _, c := range cases {
		if got := trimText(c.in, c.multiline); got != c.want {
			t.Errorf("trimText(%q, %v) = %q, want %q", c.in, c.multiline, got, c.want)
		}
	}
}

// Rows are duplicates when every compared cell is the same as stored; an
// empty cell, "" and no cell are the same.
func TestDuplicateRowsShareAKey(t *testing.T) {
	a, b := textField(model.FieldText), textField(model.FieldNumber)
	fields := []*model.Field{a, b}
	key := func(v map[string]any) [32]byte { return dedupeKey(v, fields) }
	ida, idb := a.Id.String(), b.Id.String()
	same := []map[string]any{
		{ida: "Acme", idb: 3.0},
		{ida: "Acme", idb: float64(3), "other": "ignored"},
	}
	if key(same[0]) != key(same[1]) {
		t.Errorf("rows equal on every compared field have different keys")
	}
	if key(map[string]any{ida: "", idb: nil}) != key(map[string]any{}) {
		t.Errorf("empty, \"\" and no cell should be the same")
	}
	for _, other := range []map[string]any{
		{ida: "acme", idb: 3.0},
		{ida: "Acme ", idb: 3.0},
		{ida: "Acme", idb: 4.0},
		{ida: "Acme"},
	} {
		if key(other) == key(same[0]) {
			t.Errorf("%v has the key of %v", other, same[0])
		}
	}
	// The fields' order is the key's: values never run into each other.
	if key(map[string]any{ida: "ab", idb: nil}) == key(map[string]any{ida: "a", idb: "b"}) {
		t.Errorf("values ran into each other")
	}
}

// A split gives each target its part, trimmed; the last target takes the
// rest; a target past the parts is cleared.
func TestASplitGivesEachTargetItsPart(t *testing.T) {
	cases := []struct {
		in, sep string
		n       int
		want    []any
	}{
		{"Ann Lee", " ", 2, []any{"Ann", "Lee"}},
		{"a, b, c, d", ",", 2, []any{"a", "b, c, d"}},
		{"a,,c", ",", 3, []any{"a", nil, "c"}},
		{"Madonna", " ", 2, []any{"Madonna", nil}},
		{"x\ny", "\n", 2, []any{"x", "y"}},
		{"Ann  Lee", " ", 2, []any{"Ann", "Lee"}},
		{"Ann \t Lee", " ", 3, []any{"Ann", "Lee", nil}},
	}
	for _, c := range cases {
		got := splitInto(c.in, c.sep, c.n)
		if len(got) != len(c.want) {
			t.Errorf("splitInto(%q, %q, %d) = %#v, want %#v", c.in, c.sep, c.n, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("splitInto(%q, %q, %d)[%d] = %#v, want %#v", c.in, c.sep, c.n, i, got[i], c.want[i])
			}
		}
	}
	if n := countParts("a;b;;c", ";"); n != 4 {
		t.Errorf("countParts = %d, want 4", n)
	}
}

// A transform's step changes only the cells it should, and says where it
// found its text.
func TestAStepChangesOnlyWhatItShould(t *testing.T) {
	name, notes, num := textField(model.FieldText), textField(model.FieldLongText), textField(model.FieldNumber)
	row := map[string]any{name.Id.String(): " Acme  Corp ", notes.Id.String(): "acme\n acme ", num.Id.String(): 7.0}

	find := &transform{step: StepFind, fields: []*model.Field{name, notes, num}, find: mustFind(t, "acme", false, false, false)}
	patch, found := find.cells(row)
	if len(patch) != 0 || len(found) != 2 {
		t.Errorf("find: patch %v, found %d; want no patch and 2 cells", patch, len(found))
	}

	replace := &transform{step: StepReplace, fields: []*model.Field{name, notes}, find: mustFind(t, "acme", false, false, false), with: "Beta"}
	patch, _ = replace.cells(row)
	if patch[name.Id.String()] != " Beta  Corp " || patch[notes.Id.String()] != "Beta\n Beta " {
		t.Errorf("replace: patch %v", patch)
	}

	trim := &transform{step: StepTrim, fields: []*model.Field{name, notes}}
	patch, _ = trim.cells(row)
	if patch[name.Id.String()] != "Acme Corp" || patch[notes.Id.String()] != "acme\nacme" {
		t.Errorf("trim: patch %v", patch)
	}
	if patch, _ = trim.cells(map[string]any{name.Id.String(): "tidy"}); len(patch) != 0 {
		t.Errorf("trim wrote a tidy cell: %v", patch)
	}

	first, last := textField(model.FieldText), textField(model.FieldText)
	split := &transform{step: StepSplit, fields: []*model.Field{name}, sep: " ", into: []*model.Field{first, last}}
	patch, _ = split.cells(map[string]any{name.Id.String(): "Ann Lee", last.Id.String(): "Lee"})
	if len(patch) != 1 || patch[first.Id.String()] != "Ann" {
		t.Errorf("split: patch %v; want only the first name written", patch)
	}
	if patch, _ = split.cells(map[string]any{name.Id.String(): "  ", first.Id.String(): "kept"}); len(patch) != 0 {
		t.Errorf("split of an empty cell wrote %v", patch)
	}
}
