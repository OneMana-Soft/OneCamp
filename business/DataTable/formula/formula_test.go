package formula

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"
	"time"
)

// A launch tracker: what each formula below reads.
var tracker = []Field{
	{ID: "price", Name: "Price", Kind: KindNumber},
	{ID: "qty", Name: "Quantity", Kind: KindNumber},
	{ID: "name", Name: "Name", Kind: KindText},
	{ID: "status", Name: "Status", Kind: KindText},
	{ID: "due", Name: "Due", Kind: KindDate},
	{ID: "start", Name: "Start", Kind: KindDate},
	{ID: "done", Name: "Done", Kind: KindBool},
	{ID: "notes", Name: "Notes", Kind: KindText},
}

// IST, so "today" isn't UTC's: 8 Oct 2026, 20:00 UTC is already 9 Oct there.
var (
	ist, _ = time.LoadLocation("Asia/Kolkata")
	now    = time.Date(2026, 10, 8, 20, 0, 0, 0, time.UTC)
)

func day(s string) Value {
	v, _ := ParseDate(s)
	return v
}

var row = map[string]Value{
	"price":  Number(12.5),
	"qty":    Number(4),
	"name":   Text("Pricing page"),
	"status": Text("In review"),
	"due":    day("2026-10-12"),
	"start":  day("2026-10-05"),
	"done":   Bool(false),
}

// eval works out one formula over row, as a field called "f".
func eval(t *testing.T, src string) Value {
	t.Helper()
	fields := append(append([]Field{}, tracker...), Field{ID: "f", Name: "F", IsFormula: true, Formula: src})
	out := Compile(fields).Run(func(id string) (Value, int) { return row[id], 0 }, now, ist)
	return out["f"]
}

func TestNumbersAndPrecedence(t *testing.T) {
	cases := map[string]float64{
		"{Price} * {Quantity}":     50,
		"1 + 2 * 3":                7,
		"(1 + 2) * 3":              9,
		"-2 + 5":                   3,
		"10 / 4":                   2.5,
		"0.1 + 0.2":                0.3,
		"{Notes} + 1":              1, // a blank is 0
		"SUM({Price}, {Notes}, 3)": 15.5,
		"AVERAGE(2, {Notes}, 4)":   3, // blanks left out
		"MIN(3, -1, 2)":            -1,
		"MAX(3, -1, 2)":            3,
		"ROUND(2.675, 2)":          2.68,
		"ROUND(1234, -2)":          1200,
		"ROUNDUP(-1.2)":            -2,
		"ROUNDDOWN(-1.8)":          -1,
		"CEILING(7, 5)":            10,
		"FLOOR(7, 5)":              5,
		"MOD(-3, 5)":               2,
		"POWER(2, 10)":             1024,
		"ABS(-4)":                  4,
		"VALUE(\"$1,200\")":        1200,
		"LEN(\"héllo\")":           5,
		"FIND(\"view\", {Status})": 6,
		"FIND(\"View\", {Status})": 0, // FIND minds case
	}
	for src, want := range cases {
		got := eval(t, src)
		if got.Kind != KindNumber || got.JSON() != want {
			t.Errorf("%s = %#v, want %v", src, got, want)
		}
	}
}

func TestText(t *testing.T) {
	cases := map[string]string{
		`{Name} & " (" & {Quantity} & ")"`:     "Pricing page (4)",
		`1 + 2 & "x"`:                          "3x",
		`CONCATENATE("a", 1, TRUE)`:            "a1true",
		`UPPER(LEFT({Name}, 7))`:               "PRICING",
		`RIGHT({Name}, 4)`:                     "page",
		`MID({Name}, 9, 4)`:                    "page",
		`SUBSTITUTE({Status}, "review", "QA")`: "In QA",
		`TRIM("  x ")`:                         "x",
		`REPT("ab", 3)`:                        "ababab",
		`IF({Done}, "Shipped", "Open")`:        "Open",
		`SWITCH({Status}, "Done", 1, "In review", "Close", "Far")`: "Close",
		`SWITCH({Status}, "Done", "a", "b")`:                       "b",
	}
	for src, want := range cases {
		if got := eval(t, src); got.Kind != KindText || got.Str != want {
			t.Errorf("%s = %#v, want %q", src, got, want)
		}
	}
}

func TestComparisonsAndLogic(t *testing.T) {
	cases := map[string]bool{
		`{Status} = "in review"`:                    true, // case doesn't matter
		`{Status} != "Done"`:                        true,
		`{Status} <> "In review"`:                   false,
		`5 > "3"`:                                   true, // a number against text that's a number
		`"10" < "9"`:                                true, // text against text is text
		`{Notes} = ""`:                              true,
		`{Notes} = BLANK()`:                         true,
		`{Notes} = 0`:                               true, // a blank is 0 against a number
		`{Due} > {Start}`:                           true,
		`{Due} = "2026-10-12"`:                      true,
		`{Notes} < {Due}`:                           true, // a blank comes before any date
		`AND({Price} > 10, NOT({Done}))`:            true,
		`OR({Done}, {Quantity} > 10)`:               false,
		`ISBLANK({Notes})`:                          true,
		`ISBLANK({Name})`:                           false,
		`ISERROR(1 / 0)`:                            true,
		`ISERROR({Price})`:                          false,
		`IF({Notes}, TRUE, FALSE)`:                  false,
		`{Price} * {Quantity} >= 50`:                true,
		`ROUND(0.1 + 0.2, 10) = 0.3`:                true,
		`0.1 + 0.2 = 0.3`:                           true, // no float noise
		`TODAY() = "2026-10-09"`:                    true, // today where the workspace is
		`WEEKDAY(TODAY()) = 5`:                      true, // a Friday
		`DATEADD({Due}, 1, "month") = "2026-11-12"`: true,
	}
	for src, want := range cases {
		if got := eval(t, src); got.Kind != KindBool || got.Bool != want {
			t.Errorf("%s = %#v, want %v", src, got, want)
		}
	}
}

func TestDates(t *testing.T) {
	cases := map[string]interface{}{
		`{Due} + 7`:                                   "2026-10-19",
		`7 + {Due}`:                                   "2026-10-19",
		`{Due} - 12`:                                  "2026-09-30",
		`{Due} - {Start}`:                             float64(7),
		`DATETIME_DIFF({Due}, {Start})`:               float64(7),
		`DATETIME_DIFF({Start}, {Due}, "days")`:       float64(-7),
		`DATETIME_DIFF({Due}, {Start}, "weeks")`:      float64(1),
		`DATETIME_DIFF("2026-12-11", {Due}, "month")`: float64(1), // a day short of two
		`DATETIME_DIFF("2027-10-12", {Due}, "years")`: float64(1),
		`WORKDAY_DIFF("2026-10-05", "2026-10-09")`:    float64(5), // Monday to Friday
		`WORKDAY_DIFF("2026-10-09", "2026-10-12")`:    float64(2), // Friday to Monday
		`WORKDAY_DIFF("2026-10-12", "2026-10-09")`:    float64(-2),
		`WORKDAY_DIFF({Start}, {Due})`:                float64(6),
		`TODAY()`:                                     "2026-10-09",
		`DATEADD({Due}, -1, "year")`:                  "2025-10-12",
		`DATEADD({Due}, 2, "weeks")`:                  "2026-10-26",
		`YEAR({Due}) * 100 + MONTH({Due})`:            float64(202610),
		`DATETIME_DIFF({Due}, {Notes})`:               nil, // blank in, blank out
	}
	for src, want := range cases {
		if got := eval(t, src).JSON(); got != want {
			t.Errorf("%s = %#v, want %#v", src, got, want)
		}
	}
}

func TestErrorsInARow(t *testing.T) {
	cases := map[string]string{
		`{Price} / ({Quantity} - 4)`:     "Divided by zero",
		`SQRT(-1)`:                       "SQRT needs a number that isn't negative",
		`{Name} * 2`:                     `"Pricing page" isn't a number`,
		`SUM({Price}, {Name})`:           `SUM needs numbers, and "Pricing page" isn't one`,
		`YEAR({Name})`:                   `YEAR needs a date, and "Pricing page" isn't one`,
		`DATEADD({Due}, 1, "fortnight")`: `DATEADD counts in years, months, weeks, days, hours or minutes, not "fortnight"`,
		`IF(1 / 0, "a", "b")`:            "Divided by zero", // an error in the test is the answer
		`MID({Name}, 0, 2)`:              "MID counts from 1",
		`REPT("x", 20000)`:               "REPT would make more than 10000 characters",
	}
	for src, want := range cases {
		got := eval(t, src)
		if got.Kind != KindError || got.Str != want {
			t.Errorf("%s = %#v, want the error %q", src, got, want)
		}
		if m, ok := got.JSON().(map[string]interface{}); !ok || m["error"] != want {
			t.Errorf("%s is sent as %#v", src, got.JSON())
		}
	}
	// IF only works out the branch it takes.
	if got := eval(t, `IF({Done}, 1 / 0, "fine")`); got.Str != "fine" {
		t.Errorf("IF worked out the branch it didn't take: %#v", got)
	}
}

func TestFormulasThatCantBeRead(t *testing.T) {
	cases := map[string]string{
		``:           "Write a formula",
		`{Price} *`:  "The formula ends too soon",
		`(1 + 2`:     "A ( has no closing )",
		`{Price`:     "A field name has no closing }",
		`"abc`:       "This text has no closing \"",
		`{Cost} * 2`: `There's no field called "Cost"`,
		`PRICE * 2`:  "PRICE isn't a function; a field's name goes in braces, like {PRICE}",
		`SUMM(1, 2)`: "There's no function called SUMM",
		`IF(TRUE)`:   "IF takes 2 to 3 values",
		`TODAY(1)`:   "TODAY takes no values",
		`ROUND()`:    "ROUND takes 1 to 2 values",
		`1 2`:        `"2" doesn't belong here`,
		`1 # 2`:      `"#" can't be used here`,
		`{}`:         "{} needs a field's name inside it",
		strings.Repeat("(", 80) + "1" + strings.Repeat(")", 80): "This formula is nested too deeply",
		strings.Repeat("1+", 1001) + "1":                        "A formula can be at most 2000 characters",
	}
	for src, want := range cases {
		_, err := Check(src, tracker, "f")
		var se *SyntaxError
		if !errors.As(err, &se) || se.Msg != want {
			t.Errorf("%q: got %v, want %q", src, err, want)
		}
	}
	// Where: a character people can find.
	if _, err := Check(`{Price} * {Cost}`, tracker, "f"); err == nil || !strings.Contains(err.Error(), "(at character 11)") {
		t.Errorf("the error doesn't say where: %v", err)
	}
	// One that can't be read is an error in every row, and the others work.
	fields := append(append([]Field{}, tracker...),
		Field{ID: "bad", Name: "Bad", IsFormula: true, Formula: "{Cost} * 2"},
		Field{ID: "good", Name: "Good", IsFormula: true, Formula: "{Price} * 2"})
	out := Compile(fields).Run(func(id string) (Value, int) { return row[id], 0 }, now, ist)
	if out["bad"].Kind != KindError || out["good"].JSON() != float64(25) {
		t.Errorf("bad = %#v, good = %#v", out["bad"], out["good"])
	}
}

func TestFormulasReadingFormulas(t *testing.T) {
	fields := append(append([]Field{}, tracker...),
		// Listed before what it reads: the order is worked out.
		Field{ID: "label", Name: "Label", IsFormula: true, Formula: `IF({Total} > 40, "Big", "Small")`},
		Field{ID: "total", Name: "Total", IsFormula: true, Formula: `{Price} * {Quantity}`},
		Field{ID: "a", Name: "A", IsFormula: true, Formula: `{B} + 1`},
		Field{ID: "b", Name: "B", IsFormula: true, Formula: `{A} + 1`},
		Field{ID: "me", Name: "Me", IsFormula: true, Formula: `{Me} + 1`},
		Field{ID: "empty", Name: "Empty", IsFormula: true},
	)
	p := Compile(fields)
	out := p.Run(func(id string) (Value, int) { return row[id], 0 }, now, ist)
	if out["label"].Str != "Big" || out["total"].JSON() != float64(50) {
		t.Errorf("label = %#v, total = %#v", out["label"], out["total"])
	}
	if out["a"].Str != ErrCycle.Error() || out["b"].Str != ErrCycle.Error() {
		t.Errorf("a loop: a = %#v, b = %#v", out["a"], out["b"])
	}
	if out["me"].Str != "A formula can't read its own value" {
		t.Errorf("reading itself: %#v", out["me"])
	}
	if out["empty"].Kind != KindBlank {
		t.Errorf("a formula not written yet is %#v", out["empty"])
	}
	if p.Kind("total") != KindNumber || p.Kind("label") != KindText {
		t.Errorf("kinds: total %v, label %v", p.Kind("total"), p.Kind("label"))
	}
}

func TestKinds(t *testing.T) {
	cases := map[string]Kind{
		`{Price} * {Quantity}`:                     KindNumber,
		`{Name} & "!"`:                             KindText,
		`{Price} > 10`:                             KindBool,
		`{Due} + 7`:                                KindDate,
		`{Due} - {Start}`:                          KindNumber,
		`IF({Done}, 1, 2)`:                         KindNumber,
		`IF({Done}, {Price}, BLANK())`:             KindNumber,
		`IF({Done}, 1, "no")`:                      KindText,
		`SWITCH({Status}, "Done", {Due}, {Start})`: KindDate,
		`TODAY()`:                                  KindDate,
		`{Name}`:                                   KindText,
	}
	for src, want := range cases {
		got, err := Check(src, tracker, "f")
		if err != nil || got != want {
			t.Errorf("%s: kind %v (%v), want %v", src, got, err, want)
		}
	}
}

func TestStoredWithIDs(t *testing.T) {
	stored, err := Canonical(`IF({due} < TODAY(), "Late: " & {Name}, "")`, tracker)
	if err != nil {
		t.Fatal(err)
	}
	if stored != `IF({#due} < TODAY(), "Late: " & {#name}, "")` {
		t.Fatalf("stored as %s", stored)
	}
	// Renamed since: shown under the new name, and still works.
	renamed := append([]Field{}, tracker...)
	renamed[4].Name = "Deadline {final}"
	if shown := Display(stored, renamed); shown != `IF({Deadline {final\}} < TODAY(), "Late: " & {Name}, "")` {
		t.Errorf("shown as %s", shown)
	}
	again, err := Canonical(Display(stored, renamed), renamed)
	if err != nil || again != stored {
		t.Errorf("round trip: %s, %v", again, err)
	}
	// Deleted since: the formula says so.
	gone := append([]Field{}, tracker[:4]...)
	gone = append(gone, tracker[5:]...)
	if _, err := Check(stored, gone, "f"); err == nil || !strings.Contains(err.Error(), "has been deleted") {
		t.Errorf("a deleted field: %v", err)
	}
	if shown := Display(stored, gone); shown != `IF({Deleted field} < TODAY(), "Late: " & {Name}, "")` {
		t.Errorf("a deleted field is shown as %s", shown)
	}
	// A row's error says what, not where: positions count the stored form.
	fields := append(append([]Field{}, gone...), Field{ID: "f", Name: "F", IsFormula: true, Formula: stored})
	if v := Compile(fields).Run(func(string) (Value, int) { return Blank(), 0 }, now, ist)["f"]; v.Str != "A field this formula reads has been deleted" {
		t.Errorf("a row's error: %q", v.Str)
	}
	if _, err := Canonical(`{Cost}`, tracker); err == nil {
		t.Error("a field that doesn't exist was stored")
	}
}

func TestValuesAsSent(t *testing.T) {
	cases := []struct {
		v    Value
		want interface{}
	}{
		{Number(0.1 + 0.2), 0.3},
		{Text("x"), "x"},
		{Text(""), nil},
		{Bool(true), true},
		{day("2026-10-08"), "2026-10-08"},
		{Date(time.Date(2026, 10, 8, 14, 30, 0, 0, ist), false), "2026-10-08T09:00:00Z"},
		{Blank(), nil},
	}
	for _, c := range cases {
		if got := c.v.JSON(); got != c.want {
			t.Errorf("%#v is sent as %#v, want %#v", c.v, got, c.want)
		}
	}
}

// The web app lists these in its formula editor (lib/tables/formula.ts, whose
// test has the same list); change both when one changes. CONCAT is
// CONCATENATE's other name, which the editor doesn't list.
func TestFunctionsTheEditorLists(t *testing.T) {
	want := []string{
		"ABS", "AND", "AVERAGE", "BLANK", "CEILING", "CONCAT", "CONCATENATE", "DATEADD", "DATETIME_DIFF", "DAY", "FALSE", "FIND", "FLOOR",
		"IF", "ISBLANK", "ISERROR", "LEFT", "LEN", "LOWER", "MAX", "MID", "MIN", "MOD", "MONTH", "NOT", "NOW", "OR",
		"POWER", "REPT", "RIGHT", "ROUND", "ROUNDDOWN", "ROUNDUP", "SQRT", "SUBSTITUTE", "SUM", "SWITCH", "TODAY",
		"TRIM", "TRUE", "UPPER", "VALUE", "WEEKDAY", "WORKDAY_DIFF", "YEAR",
	}
	var got []string
	for name := range functions {
		got = append(got, name)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("functions are %v", got)
	}
}

// No formula can build text without end: one SUBSTITUTE of a REPT, 47
// characters, made 100 MB a row before there was a limit.
func TestTextHasALimit(t *testing.T) {
	cases := []string{
		`SUBSTITUTE(REPT("a",10000),"a",REPT("b",10000))`,
		`REPT("ab", 4000) & REPT("cd", 4000)`,
		`CONCATENATE(REPT("x",9000), REPT("y",9000))`,
		`UPPER(REPT("x",5000) & REPT("y",5001))`,
	}
	for _, src := range cases {
		start := time.Now()
		got := eval(t, src)
		if got.Kind != KindError || got.Str != "The text would be longer than 10000 characters" {
			t.Errorf("%s = %.80v", src, got)
		}
		if d := time.Since(start); d > 200*time.Millisecond {
			t.Errorf("%s took %v", src, d)
		}
	}
	if got := eval(t, `LEN(REPT("é", 10000))`); got.JSON() != float64(10000) {
		t.Errorf("text right at the limit: %#v", got)
	}
}

func TestReviewedEdges(t *testing.T) {
	cases := map[string]interface{}{
		// Month ends, as Airtable and EDATE have them.
		`DATEADD("2026-01-31", 1, "month")`:    "2026-02-28",
		`DATEADD("2024-02-29", 1, "year")`:     "2025-02-28",
		`DATEADD("2026-03-31", -1, "month")`:   "2026-02-28",
		`DATEADD("2026-01-15", -13, "months")`: "2024-12-15",
		`FIND("é", "café")`:                    float64(4),
		`FIND("a", "banana", 3)`:               float64(4),
		`TRUE() = TRUE`:                        true,
		`IF(FALSE(), 1, 2)`:                    float64(2),
		// A moment's day is the reader's: 23:30 UTC on the 8th is the 9th in India.
		`"2026-10-08T23:30:00Z" = TODAY()`: true,
		`DAY("2026-10-08T23:30:00Z")`:      float64(9),
	}
	for src, want := range cases {
		if got := eval(t, src).JSON(); got != want {
			t.Errorf("%s = %#v, want %#v", src, got, want)
		}
	}
	// Never "-0".
	for _, src := range []string{`{Notes} * -1`, `ROUND(-0.4)`, `DATETIME_DIFF("2026-01-01", "2026-02-01", "years")`} {
		if v, ok := eval(t, src).JSON().(float64); !ok || v != 0 || math.Signbit(v) {
			t.Errorf("%s = %#v", src, eval(t, src).JSON())
		}
	}
	if got := eval(t, `"Due in " & ({Notes} * -1) & " days"`); got.Str != "Due in 0 days" {
		t.Errorf("text with a negative zero: %q", got.Str)
	}
	// Spans of any length: a time.Duration overflowed past 292 years.
	want := float64((time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC).Unix() - time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC).Unix()) / 86400)
	if got := eval(t, `DATETIME_DIFF("9999-12-31", "0001-01-01")`).JSON(); got != want {
		t.Errorf("ten thousand years: %v, want %v", got, want)
	}
	// "" is no value, so the column still gives a number.
	if kind, err := Check(`IF({Done}, 5, "")`, tracker, "f"); err != nil || kind != KindNumber {
		t.Errorf(`IF({Done}, 5, ""): %v %v`, kind, err)
	}
}

// A field whose name starts with # is found by its name.
func TestHashNames(t *testing.T) {
	fields := []Field{{ID: "seats", Name: "# of seats", Kind: KindNumber}}
	stored, err := Canonical(`{# of seats} * 2`, fields)
	if err != nil || stored != `{#seats} * 2` {
		t.Fatalf("stored as %q, %v", stored, err)
	}
	if shown := Display(stored, fields); shown != `{# of seats} * 2` {
		t.Errorf("shown as %q", shown)
	}
	all := append(fields, Field{ID: "f", Name: "F", IsFormula: true, Formula: stored})
	out := Compile(all).Run(func(string) (Value, int) { return Number(3), 0 }, now, ist)
	if out["f"].JSON() != float64(6) {
		t.Errorf("worked out as %#v", out["f"])
	}
}

// A row's formulas have a budget, however many there are.
func TestWorkHasALimit(t *testing.T) {
	long := strings.TrimSuffix(strings.Repeat("1+", 999), "+")
	var fields []Field
	for i := 0; i < 600; i++ {
		fields = append(fields, Field{ID: fmt.Sprint("f", i), Name: fmt.Sprint("F", i), IsFormula: true, Formula: long})
	}
	out := Compile(fields).Run(func(string) (Value, int) { return Blank(), 0 }, now, ist)
	if out["f0"].JSON() != float64(999) {
		t.Errorf("the first formula: %#v", out["f0"])
	}
	if out["f599"].Str != "This formula takes too much working out" {
		t.Errorf("the last formula: %#v", out["f599"])
	}
}

// runOn works out formulas over one row of cells, as fields f0, f1….
func runOn(p *Program, cells map[string]Value) map[string]Value {
	return p.Run(func(id string) (Value, int) { return cells[id], 0 }, now, ist)
}

// many is n formula fields, f0 to f(n-1), each src, reading a text field
// called Notes.
func many(n int, src string) *Program {
	fields := []Field{{ID: "notes", Name: "Notes", Kind: KindText}}
	for i := 0; i < n; i++ {
		fields = append(fields, Field{ID: fmt.Sprint("f", i), Name: fmt.Sprint("F", i), IsFormula: true, Formula: src})
	}
	return Compile(fields)
}

// A row can hold 100 KB, and a formula reading a cell that long, or many
// formulas giving long text, cost the server in proportion.
func TestLongCellsAreCheap(t *testing.T) {
	long := map[string]Value{"notes": Text(strings.Repeat("a", 100<<10-200) + "b")}

	// 120 formulas each searching the cell took 18 s a row; the row's
	// budget now stops them partway, in milliseconds.
	for _, src := range []string{`FIND("b", {Notes}) + LEN({Notes})`, `LEN(UPPER({Notes})) + LEN(LOWER({Notes}))`} {
		start := time.Now()
		out := runOn(many(120, src), long)
		if d := time.Since(start); d > time.Second {
			t.Errorf("%s 120 times took %v", src, d)
		}
		if out["f0"].Kind != KindNumber || out["f119"] != tooMuch {
			t.Errorf("%s: the first %#v, the last %#v", src, out["f0"], out["f119"])
		}
	}
	if want := float64(2 * (100<<10 - 199)); runOn(many(1, `FIND("b", {Notes}) + LEN({Notes})`), long)["f0"].JSON() != want {
		t.Errorf("searching a long cell once doesn't give %v", want)
	}

	// Formulas passing a long cell on, or each building long text, give no
	// more than a row holds between them.
	for _, src := range []string{`{Notes}`, `REPT("😀", 2000)`, `UPPER({Notes})`} {
		out := runOn(many(50, src), long)
		total := 0
		for _, v := range out {
			if v != tooMuchText {
				total += len(v.Str)
			}
		}
		if out["f0"].Kind != KindText || out["f49"] != tooMuchText || total > maxRowText {
			t.Errorf("%s: the first %v, the last %#v, %d bytes in all", src, out["f0"].Kind, out["f49"], total)
		}
	}

	// A message quotes a little of a value, not the whole cell.
	msg := runOn(many(1, `{Notes} * 2`), long)["f0"].Str
	if want := `"` + strings.Repeat("a", quoteLen) + `…" isn't a number`; msg != want {
		t.Errorf("message: %.80q", msg)
	}
}

// Every row a read works out shares a budget, so a page of rows can't hold
// the server either; and a formula can't hide the budget running out.
func TestAReadHasABudget(t *testing.T) {
	cells := map[string]Value{"notes": Text(strings.Repeat("x", 64<<10))}
	p := many(1, `LEN(UPPER({Notes}))`)
	start := time.Now()
	var got []Value
	for i := 0; i < 2500; i++ {
		got = append(got, runOn(p, cells)["f0"])
	}
	// Generous: it takes a tenth of this alone, and the suite runs packages
	// side by side.
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("2500 rows took %v", d)
	}
	if got[0].JSON() != float64(64<<10) || got[2499] != tooMuchRead {
		t.Errorf("the first row %#v, the last %#v", got[0], got[2499])
	}
	// Once it's out, it stays out.
	for i := 1; i < len(got); i++ {
		if got[i-1] == tooMuchRead && got[i] != tooMuchRead {
			t.Fatalf("row %d was worked out after the budget ran out", i)
		}
	}

	// ISERROR sees an error when the row's budget runs out partway, but the
	// formula gives that, not an answer.
	sum := strings.TrimSuffix(strings.Repeat("LEN(UPPER({Notes}))+", 80), "+")
	out := runOn(many(1, `IF(ISERROR(`+sum+`), "fine", "fine")`), cells)
	if out["f0"] != tooMuch {
		t.Errorf("a budget run out behind ISERROR: %#v", out["f0"])
	}
}

// Text that's only cut, trimmed or changed in case can be as long as its cell;
// only text a formula builds has the 10,000-character limit.
func TestLongTextThatDoesntGrow(t *testing.T) {
	notes := strings.Repeat("ab", 10000) // 20,000 characters
	cells := map[string]Value{"notes": Text(notes)}
	cases := map[string]interface{}{
		`LEN({Notes})`: float64(20000),
		`LEN(TRIM(" " & LEFT({Notes}, 9000))) = 9000`: true,
		`LEN(UPPER({Notes}))`:                         float64(20000),
		`LEN(LOWER({Notes}))`:                         float64(20000),
		`LEN(TRIM({Notes}))`:                          float64(20000),
		`LEN(LEFT({Notes}, 15000))`:                   float64(15000),
		`LEN(RIGHT({Notes}, 15000))`:                  float64(15000),
		`LEN(MID({Notes}, 2, 19998))`:                 float64(19998),
		`LEN(SUBSTITUTE({Notes}, "a", "c"))`:          float64(20000),
		`FIND("ba", {Notes}, 19998)`:                  float64(19998),
		`{Notes} = UPPER({Notes})`:                    true,
	}
	for src, want := range cases {
		if got := runOn(many(1, src), cells)["f0"].JSON(); got != want {
			t.Errorf("%s = %#v, want %#v", src, got, want)
		}
	}
	for _, src := range []string{`{Notes} & "!"`, `SUBSTITUTE({Notes}, "a", "aa")`, `CONCATENATE({Notes}, "")`} {
		if got := runOn(many(1, src), cells)["f0"]; got.Str != "The text would be longer than 10000 characters" {
			t.Errorf("%s = %.80v", src, got)
		}
	}
}

// LEFT, RIGHT, MID and FIND count characters without copying the text into
// runes; they give what counting runes gave.
func TestCharactersCountedInPlace(t *testing.T) {
	var read int
	e := &env{read: &read}
	texts := []string{"", "a", "café", "naïve façade", "😀a😀b", "日本語のテキスト", "a\xffb"}
	for _, s := range texts {
		rs := []rune(s)
		for n := 0; n <= len(rs)+2; n++ {
			if got, want := fnLeft(e, []Value{Text(s), Number(float64(n))}).text(), string(rs[:min(n, len(rs))]); got != want && !strings.Contains(s, "\xff") {
				t.Errorf("LEFT(%q, %d) = %q, want %q", s, n, got, want)
			}
			if got, want := fnRight(e, []Value{Text(s), Number(float64(n))}).text(), string(rs[len(rs)-min(n, len(rs)):]); got != want && !strings.Contains(s, "\xff") {
				t.Errorf("RIGHT(%q, %d) = %q, want %q", s, n, got, want)
			}
			for c := 0; c <= len(rs)+1; c++ {
				want := ""
				if n >= 1 && n <= len(rs) {
					want = string(rs[n-1 : min(n-1+c, len(rs))])
				}
				if n >= 1 {
					if got := fnMid(e, []Value{Text(s), Number(float64(n)), Number(float64(c))}).text(); got != want && !strings.Contains(s, "\xff") {
						t.Errorf("MID(%q, %d, %d) = %q, want %q", s, n, c, got, want)
					}
				}
			}
			for _, what := range []string{"", "a", "é", "😀", "テ", "zz"} {
				want := 0
				if from := max(n, 1); from <= len(rs)+1 {
					if i := strings.Index(string(rs[from-1:]), what); i >= 0 {
						want = from + len([]rune(string(rs[from-1:])[:i]))
					}
				}
				if got := fnFind(e, []Value{Text(what), Text(s), Number(float64(n))}).JSON(); got != float64(want) {
					t.Errorf("FIND(%q, %q, %d) = %v, want %d", what, s, n, got, want)
				}
			}
		}
	}
}

// Text compares ignoring case as lowercasing it did, without the copies.
func TestCompareFold(t *testing.T) {
	texts := []string{"", "a", "A", "ab", "aB", "b", "Á", "á", "ä", "Z", "zeta", "ΣΑΣ", "σας", "İ", "i", "\xff", "�", "ǅ", "ǆ"}
	sign := func(n int) int {
		switch {
		case n < 0:
			return -1
		case n > 0:
			return 1
		}
		return 0
	}
	for _, a := range texts {
		for _, b := range texts {
			if got, _ := compareFold(a, b); got != sign(strings.Compare(strings.ToLower(a), strings.ToLower(b))) {
				t.Errorf("compareFold(%q, %q) = %d", a, b, got)
			}
		}
	}
}

func TestLongValuesArentNumbersOrDates(t *testing.T) {
	if _, ok := ParseNumber(strings.Repeat("1", maxReadLen+1)); ok {
		t.Error("a long run of digits read as a number")
	}
	if _, ok := ParseNumber(" ₹ 1,00,00,000.50 "); !ok {
		t.Error("a rupee amount wasn't read")
	}
	if _, ok := ParseDate("2026-10-08" + strings.Repeat(" ", maxReadLen) + "x"); ok {
		t.Error("a long text read as a date")
	}
	if got := eval(t, `DATEADD("2026-10-08", 1, "`+strings.Repeat("day", 30)+`")`).Str; len(got) > 150 || !strings.HasSuffix(got, `…"`) {
		t.Errorf("a long unit: %q", got)
	}
}

func TestRefs(t *testing.T) {
	if got := Refs(`{Price} * { Quantity } + IF({#abc}, {Price})`); fmt.Sprint(got) != "[Price Quantity #abc Price]" {
		t.Errorf("refs: %q", got)
	}
	if got := Refs(`{Price`); got != nil {
		t.Errorf("an unfinished formula: %q", got)
	}
}

// However many times formulas name a field, its cell is read once a row: a
// reader can be slow on a long cell.
func TestCellsAreReadOnceARow(t *testing.T) {
	sum := strings.TrimSuffix(strings.Repeat("{Notes}+", 200), "+")
	p := many(3, sum)
	reads := 0
	out := p.Run(func(id string) (Value, int) { reads++; return Number(1), 0 }, now, ist)
	if reads != 1 || out["f2"].JSON() != float64(200) {
		t.Errorf("the cell was read %d times, and f2 is %#v", reads, out["f2"])
	}
}

// A row's formula text is measured as it's sent: JSON writes < as six
// characters.
func TestRowTextIsCountedAsSent(t *testing.T) {
	out := runOn(many(12, `REPT("<", 10000)`), nil)
	sent := 0
	for _, v := range out {
		if v != tooMuchText {
			b, _ := json.Marshal(v.JSON())
			sent += len(b)
		}
	}
	if out["f0"].Kind != KindText || out["f11"] != tooMuchText || sent > maxRowText {
		t.Errorf("the first %v, the last %#v, %d bytes sent", out["f0"].Kind, out["f11"], sent)
	}
	for _, s := range []string{"", "plain", `"quoted" \ back`, "<b>&</b>", "line\nbreak\ttab\x01", "naïve 😀", "  ", "bad \xff byte"} {
		b, _ := json.Marshal(s)
		if got := sentLen(s); got < len(b) || (!strings.ContainsAny(s, "\n\t\x01") && got != len(b)) {
			t.Errorf("sentLen(%q) = %d, JSON has %d", s, got, len(b))
		}
	}
}

// Some work doesn't show in the text it gives, and is charged for itself: a
// long search through repeating text compares the whole search text at many
// places, and trimming spaces that aren't ASCII goes a character at a time.
func TestHiddenWorkIsCharged(t *testing.T) {
	block := "abcdefghijklmnop"
	fields := []Field{{ID: "h", Name: "H", Kind: KindText}, {ID: "n", Name: "N", Kind: KindText}, {ID: "f", Name: "F", IsFormula: true}}
	stepsFor := func(src string, cells map[string]Value) int {
		fields[2].Formula = src
		p := Compile(fields)
		runOn(p, cells)
		return p.read
	}
	repeating := map[string]Value{"h": Text(strings.Repeat(block, 3750)), "n": Text(strings.Repeat(block, 2499) + "abcdefghijklmnoX")}
	// 1,250 places each compared 40 KB: about 50 MB.
	if got := stepsFor(`FIND({N}, {H})`, repeating); got < 40000 {
		t.Errorf("a long search through repeating text cost %d steps", got)
	}
	if got := stepsFor(`SUBSTITUTE({H}, {N}, "")`, repeating); got < 100000 {
		t.Errorf("replacing a long text through repeating text cost %d steps", got)
	}
	// A short search costs only the text it reads.
	if got := stepsFor(`FIND("p", {H})`, repeating); got > 5000 {
		t.Errorf("a short search cost %d steps", got)
	}
	ideographic := map[string]Value{"h": Text(strings.Repeat("　", 30000) + "x")}
	for _, src := range []string{`ISBLANK({H})`, `IF({H}, 1, 0)`, `TRIM({H})`} {
		if got := stepsFor(src, ideographic); got < 90000/slowBytesPerStep {
			t.Errorf("%s over 90 KB of wide spaces cost %d steps", src, got)
		}
	}
	// Numbers and dates aren't looked for in long text, spaces and all.
	padded := strings.Repeat(" ", 4*maxReadLen) + "5"
	if _, ok := ParseNumber(padded); ok {
		t.Error("a number padded with 256 spaces was read")
	}
	if _, ok := ParseNumber("  5  "); !ok {
		t.Error("a number with a few spaces wasn't read")
	}
}

func TestHugeNumbersAsText(t *testing.T) {
	for f, want := range map[float64]string{1e21: "1e+21", -1.5e300: "-1.5e+300", 1e20: "100000000000000000000", 0.1 + 0.2: "0.3", 1500000: "1500000"} {
		if got := FormatNumber(f); got != want {
			t.Errorf("FormatNumber(%g) = %q, want %q", f, got, want)
		}
	}
}
