package formula

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Tables B2: Airtable's library past the 45 there were, and five more of
// Notion's and a spreadsheet's. now is 8 Oct 2026, 20:00 UTC: 9 Oct, 01:30
// on the reader's clock (IST).
func TestTheLibraryPast45(t *testing.T) {
	cases := map[string]interface{}{
		// Text
		`SEARCH("view", {Status})`:                                   float64(6),
		`SEARCH("zz", {Status})`:                                     nil, // FIND says 0
		`REPLACE({Name}, 1, 7, "Landing")`:                           "Landing page",
		`T({Name})`:                                                  "Pricing page",
		`T({Price})`:                                                 nil,
		`ENCODE_URL_COMPONENT("a b&c/é(1)")`:                         "a%20b%26c%2F%C3%A9(1)",
		`REGEX_MATCH({Name}, "^Pri")`:                                true,
		`REGEX_MATCH({Name}, "^pri")`:                                false,
		`REGEX_MATCH({Name}, "(?i)^pri")`:                            true,
		`REGEX_EXTRACT("Order 4521 shipped", "\d+")`:                 "4521",
		`REGEX_EXTRACT("v=12;w=7", "w=(\d+)")`:                       "7",
		`REGEX_EXTRACT("none", "\d+")`:                               nil,
		`REGEX_REPLACE({Due} & "", "(\d+)-(\d+)-(\d+)", "$3/$2/$1")`: "12/10/2026",
		`REGEX_REPLACE("a  b   c", " +", " ")`:                       "a b c",
		// Numbers and logic
		`LOG(1000)`:                   float64(3),
		`LOG(8, 2)`:                   float64(3),
		`EXP(0)`:                      float64(1),
		`INT(-1.5)`:                   float64(-2),
		`EVEN(1.5)`:                   float64(2),
		`EVEN(-1)`:                    float64(-2),
		`EVEN(0)`:                     float64(0),
		`ODD(2)`:                      float64(3),
		`ODD(0)`:                      float64(1),
		`ODD(-1.5)`:                   float64(-3),
		`XOR(TRUE(), TRUE(), TRUE())`: true,
		`XOR({Done}, FALSE())`:        false,
		`ISERROR(ERROR())`:            true,
		`IF({Price} > 0, {Price}, ERROR("No price"))`:             12.5,
		`COUNT(1, "2", "x", BLANK(), TRUE())`:                     float64(2),
		`COUNTA(1, "x", BLANK())`:                                 float64(2),
		`COUNTALL(1, BLANK())`:                                    float64(2),
		`IFS({Price} > 100, "big", {Price} > 10, "mid", "small")`: "mid",
		`IFS({Price} > 100, "big")`:                               nil,
		`CONTAINS({Status}, "REVIEW")`:                            true,
		`LN(1)`:                                                   float64(0),
		`SIGN(-3) + SIGN(0) + SIGN(2)`:                            float64(0),
		`ROUND(PI(), 2)`:                                          3.14,
		// Dates
		`HOUR(NOW()) * 100 + MINUTE(NOW())`:     float64(130),
		`SECOND(NOW())`:                         float64(0),
		`WEEKNUM("2026-01-04")`:                 float64(2), // a Sunday starts week 2
		`WEEKNUM("2026-01-04", "Monday")`:       float64(1),
		`DATESTR({Due})`:                        "2026-10-12",
		`TIMESTR(NOW())`:                        "01:30:00",
		`DATETIME_FORMAT({Due}, "D MMMM YYYY")`: "12 October 2026",
		`DATETIME_FORMAT({Due}, "dddd Do MMM")`: "Monday 12th Oct",
		`DATETIME_FORMAT({Due}, "[Week] W, Q")`: "Week 42, 4",
		`DATETIME_FORMAT({Due})`:                "2026-10-12",
		`DATETIME_FORMAT(NOW(), "HH:mm A")`:     "01:30 AM",
		`DATETIME_FORMAT(NOW(), "h:mm a")`:      "1:30 am",
		`DATETIME_FORMAT(SET_TIMEZONE(NOW(), "America/New_York"), "YYYY-MM-DD HH:mm Z")`: "2026-10-08 16:00 -04:00",
		`HOUR(SET_TIMEZONE(NOW(), "UTC"))`:                                               float64(20),
		`DATETIME_FORMAT(SET_LOCALE({Due}, "fr"), "dddd D MMMM YYYY")`:                   "lundi 12 octobre 2026",
		`DATETIME_FORMAT(SET_LOCALE({Due}, "de"), "L")`:                                  "12.10.2026",
		`DATETIME_PARSE("12/10/2026", "DD/MM/YYYY")`:                                     "2026-10-12",
		`DATETIME_PARSE("8 October 2026")`:                                               "2026-10-08",
		`DATETIME_PARSE("October 8, 2026")`:                                              "2026-10-08",
		`DATETIME_PARSE("2026-10-12 09:15", "YYYY-MM-DD HH:mm")`:                         "2026-10-12T03:45:00Z", // 09:15 in IST
		`DATETIME_PARSE("2026-10-12T09:15+00:00", "YYYY-MM-DDTHH:mmZ")`:                  "2026-10-12T09:15:00Z",
		`DATETIME_PARSE("12 octobre 2026", "D MMMM YYYY", "fr")`:                         "2026-10-12",
		`DATETIME_PARSE("3:05 pm", "h:mm a") = DATETIME_PARSE("15:05", "HH:mm")`:         true,
		`WORKDAY({Due}, 5)`:                                      "2026-10-19",
		`WORKDAY("2026-10-09", 1)`:                               "2026-10-12", // Friday to Monday
		`WORKDAY({Due}, 5, "2026-10-14")`:                        "2026-10-20",
		`WORKDAY({Due}, -1)`:                                     "2026-10-09",
		`WORKDAY({Due}, 25)`:                                     "2026-11-16",
		`WORKDAY({Due}, -25)`:                                    "2026-09-07",
		`WORKDAY_DIFF({Start}, {Due}, "2026-10-07, 2026-10-10")`: float64(5), // the 10th is a Saturday
		`IS_SAME({Due}, "2026-10-14", "week")`:                   true,
		`IS_SAME({Due}, "2026-10-19", "week")`:                   false,
		`IS_SAME({Due}, "2026-10-12")`:                           true,
		`IS_SAME({Due}, "2026-12-12", "months")`:                 false,
		`IS_BEFORE({Start}, {Due})`:                              true,
		`IS_AFTER({Start}, {Due})`:                               false,
		`FROMNOW({Due})`:                                         float64(3),
		`TONOW({Start})`:                                         float64(4),
	}
	for src, want := range cases {
		if got := eval(t, src).JSON(); !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %#v, want %#v", src, got, want)
		}
	}
}

func TestTheLibrarysErrors(t *testing.T) {
	cases := map[string]string{
		`REGEX_MATCH("x", "(")`:                      "REGEX_MATCH's pattern isn't a regular expression: missing closing ): `(`",
		`REGEX_MATCH("x", REPT("[a-z]{1000}", 6))`:   "REGEX_MATCH's pattern is too complicated to work out",
		`REGEX_MATCH("x", "(a{1000}){1000}")`:        "REGEX_MATCH's pattern isn't a regular expression: invalid repeat count: `{1000}`",
		`REGEX_MATCH("x", REPT("a", 501))`:           "REGEX_MATCH's pattern is longer than 500 characters",
		`LOG(0)`:                                     "LOG needs a number above 0",
		`LOG(10, 1)`:                                 "LOG needs a base above 0 that isn't 1",
		`ERROR("Missing price")`:                     "Missing price",
		`ERROR()`:                                    "#ERROR",
		`DATETIME_PARSE("31/02/2026", "DD/MM/YYYY")`: `"31/02/2026" isn't a date written "DD/MM/YYYY"`,
		`DATETIME_PARSE("soon")`:                     `"soon" isn't a date DATETIME_PARSE can read; give it a format`,
		`SET_TIMEZONE(NOW(), "Mars/Olympus")`:        `SET_TIMEZONE doesn't know the zone "Mars/Olympus"`,
		`SET_TIMEZONE(NOW(), "../../etc/passwd")`:    `SET_TIMEZONE doesn't know the zone "../../etc/passwd"`,
		`SET_LOCALE({Due}, "xx")`:                    `SET_LOCALE doesn't know the locale "xx"; it knows en, en-gb, fr, de, es, it, pt, nl, sv, da, nb, pl and hi`,
		`WEEKNUM({Due}, "Friday")`:                   `WEEKNUM's week starts on "Sunday" or "Monday", not "Friday"`,
		`WORKDAY({Due}, 1, "someday")`:               `WORKDAY's holidays are dates, and "someday" isn't one`,
		`WORKDAY({Due}, 1e9)`:                        "WORKDAY counts at most 100000 weekdays",
		`IS_SAME({Due}, {Start}, "fortnight")`:       `IS_SAME compares by year, month, week, day, hour, minute or second, not "fortnight"`,
	}
	for src, want := range cases {
		got := eval(t, src)
		if got.Kind != KindError || got.Str != want {
			t.Errorf("%s = %#v, want the error %q", src, got, want)
		}
	}
}

// A list (a multi-select, a link, a lookup) is text to every function that
// was there before, and a list to the ARRAY functions, COUNT and CONTAINS.
func TestListsAreTextAndLists(t *testing.T) {
	fields := []Field{{ID: "tags", Name: "Tags", Kind: KindText}, {ID: "few", Name: "Few", Kind: KindText}}
	cells := map[string]Value{
		"tags": List([]Value{Text("Design"), Text("Launch"), Text("design")}),
		"few":  List([]Value{Text("a"), Blank(), Text(" "), Text("b")}),
	}
	cases := map[string]interface{}{
		`{Tags}`:                                "Design, Launch, design",
		`LEN({Tags})`:                           float64(22),
		`ARRAYJOIN({Tags}, " | ")`:              "Design | Launch | design",
		`ARRAYJOIN({Tags})`:                     "Design, Launch, design",
		`ARRAYUNIQUE({Tags})`:                   "Design, Launch",
		`ARRAYCOMPACT({Few})`:                   "a, b",
		`COUNTA(ARRAYCOMPACT({Few}))`:           float64(2),
		`ARRAYFLATTEN({Tags}, "x")`:             "Design, Launch, design, x",
		`COUNTALL(ARRAYFLATTEN({Tags}, {Few}))`: float64(7),
		`ARRAYSLICE({Tags}, 2)`:                 "Launch, design",
		`ARRAYSLICE({Tags}, -1)`:                "design",
		`ARRAYSLICE({Tags}, 1, 2)`:              "Design, Launch",
		`ARRAYSLICE({Tags}, 3, 1)`:              nil,
		`COUNTA({Tags})`:                        float64(3),
		`COUNTALL({Few})`:                       float64(4),
		`CONTAINS({Tags}, "launch")`:            true,
		`CONTAINS({Tags}, "Laun")`:              false, // an item, not part of one
		`CONTAINS("Launch", "Laun")`:            true,
		`ARRAYJOIN("one")`:                      "one", // a value is a list of one
		`COUNTA(BLANK())`:                       float64(0),
	}
	for src, want := range cases {
		p := Compile(append(append([]Field{}, fields...), Field{ID: "f", Name: "F", IsFormula: true, Formula: src}))
		if got := runOn(p, cells)["f"].JSON(); !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %#v, want %#v", src, got, want)
		}
	}
	if got := List(nil); got != Blank() {
		t.Errorf("an empty list is %#v, want a blank", got)
	}
}

// A formula reads its own row's id, and when it was made and changed, on the
// reader's clock; a draft's preview has none.
func TestWhatARowSaysOfItself(t *testing.T) {
	p := Compile([]Field{
		{ID: "id", Name: "Id", IsFormula: true, Formula: `RECORD_ID()`},
		{ID: "made", Name: "Made", IsFormula: true, Formula: `DATETIME_FORMAT(CREATED_TIME(), "YYYY-MM-DD HH:mm")`},
		{ID: "age", Name: "Age", IsFormula: true, Formula: `DATETIME_DIFF(LAST_MODIFIED_TIME(), CREATED_TIME(), "hours")`},
	})
	row := Row{ID: "3f0c", Created: time.Date(2026, 10, 1, 18, 45, 0, 0, time.UTC), Modified: time.Date(2026, 10, 2, 0, 45, 0, 0, time.UTC)}
	got := p.RunRow(row, func(string) (Value, int) { return Blank(), 0 }, now, ist)
	if got["id"].JSON() != "3f0c" || got["made"].JSON() != "2026-10-02 00:15" || got["age"].JSON() != float64(6) {
		t.Errorf("a row's own values are %v %v %v", got["id"].JSON(), got["made"].JSON(), got["age"].JSON())
	}
	draft := p.Run(func(string) (Value, int) { return Blank(), 0 }, now, ist)
	if draft["id"].JSON() != nil || draft["made"].JSON() != nil {
		t.Errorf("a draft's own values are %v %v, want blanks", draft["id"].JSON(), draft["made"].JSON())
	}
	if p.Kind("made") != KindText || Compile([]Field{{ID: "c", IsFormula: true, Formula: `CREATED_TIME()`}}).Kind("c") != KindDate {
		t.Error("CREATED_TIME gives a date, and DATETIME_FORMAT text")
	}
}

// A backslash keeps its place in text unless it escapes a quote, a backslash
// or a line break, so a pattern reads as it's written.
func TestBackslashesInText(t *testing.T) {
	cases := map[string]interface{}{
		`"a\nb"`:        "a\nb",
		`"say \"hi\""`:  `say "hi"`,
		`"back\\slash"`: `back\slash`,
		`"\d{3}"`:       `\d{3}`,
		`LEN("\w")`:     float64(2),
	}
	for src, want := range cases {
		if got := eval(t, src).JSON(); got != want {
			t.Errorf("%s = %#v, want %#v", src, got, want)
		}
	}
}

// IFS and ERROR give the kind their branches give, as IF does.
func TestKindsOfTheNewFunctions(t *testing.T) {
	cases := map[string]Kind{
		`IFS({Price} > 10, 1, 2)`:      KindNumber,
		`IF({Done}, ERROR(), {Price})`: KindNumber,
		`WORKDAY({Due}, 3)`:            KindDate,
		`DATETIME_PARSE("2026-10-12")`: KindDate,
		`ARRAYJOIN({Name})`:            KindText,
		`REGEX_MATCH({Name}, "a")`:     KindBool,
		`COUNTA({Name})`:               KindNumber,
	}
	for src, want := range cases {
		p := Compile(append(append([]Field{}, tracker...), Field{ID: "f", IsFormula: true, Formula: src}))
		if got := p.Kind("f"); got != want {
			t.Errorf("%s gives %v, want %v", src, got, want)
		}
	}
}

// Every pattern is held to its limits, and a replacement that would grow
// past the text limit stops there rather than building it.
func TestRegexLimits(t *testing.T) {
	v, steps := evalSpent(t, `REGEX_REPLACE(REPT("a", 5000), "", REPT("b", 5000))`)
	if v.Kind != KindError || !strings.Contains(v.Str, "longer than") {
		t.Errorf("an empty pattern between 5,000 characters gives %#v", v)
	}
	if steps > maxSteps {
		t.Errorf("stopping took %d steps", steps)
	}
}

// The new functions are charged for the work they do, as measured at about
// 50 ns a step (a throwaway page probe, as the engine's budget was set):
// steps, not time, so a loaded machine can't make it flake. Each floor is the
// least a call over the input may be charged.
func TestNewFunctionsAreCharged(t *testing.T) {
	long := strings.Repeat("The quick brown fox jumps over the lazy dog 12345. ", 80) // 4 KB
	var items []Value
	for i := 0; i < 2000; i++ {
		items = append(items, Text(fmt.Sprintf("Item %d", i%700)))
	}
	var hol []string
	for i := 0; i < 1000; i++ {
		hol = append(hol, time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i*3).Format("2006-01-02"))
	}
	cells := map[string]Value{"notes": Text(long), "tags": List(items), "hol": Text(strings.Join(hol, ", "))}
	fields := []Field{{ID: "notes", Name: "Notes", Kind: KindText}, {ID: "tags", Name: "Tags", Kind: KindText}, {ID: "hol", Name: "Hol", Kind: KindText}}
	floors := map[string]int{
		// 170 ns a byte for a short pattern with groups: 4 steps a byte.
		`REGEX_MATCH({Notes}, "(fox|dog)+ (\w+) z{2,}")`: 4 * len(long),
		`REGEX_REPLACE({Notes}, "\w+", "[$0]")`:          4 * len(long),
		// A long pattern keeps a thread an instruction.
		`REGEX_MATCH({Notes}, "[a-z]{200}x")`:             10 * len(long),
		`ARRAYUNIQUE({Tags})`:                             40 * len(items),
		`ARRAYSLICE({Tags}, 1, 1500)`:                     20 * 1500,
		`WORKDAY_DIFF("2026-10-12", "2030-10-12", {Hol})`: 60 * len(hol),
		`WORKDAY("2026-10-12", 10000, {Hol})`:             6 * 14000,
		`ENCODE_URL_COMPONENT({Notes})`:                   len(long),
		`YEAR(NOW()) + HOUR(NOW()) + WEEKNUM(NOW())`:      3 * dateCost,
		`DATETIME_FORMAT(NOW(), "dddd Do MMMM YYYY")`:     20 * len("dddd Do MMMM YYYY"),
	}
	for src, floor := range floors {
		p := Compile(append(append([]Field{}, fields...), Field{ID: "f", Name: "F", IsFormula: true, Formula: src}))
		runOn(p, cells)
		if p.budget.spent < floor {
			t.Errorf("%s is charged %d steps, under the %d its work takes", src, p.budget.spent, floor)
		}
	}
}

// A list of only blanks is a blank, as its text "" always was.
func TestAListOfBlanksIsBlank(t *testing.T) {
	if got := List([]Value{Text(""), Blank()}); got != Blank() {
		t.Errorf("a list of blanks is %#v", got)
	}
	if got := List([]Value{Text(""), Text("a")}); got.Str != ", a" || len(got.Items()) != 2 {
		t.Errorf("a list with a blank and an item is %#v", got)
	}
}

// FILTER and MAP go through a list with CURRENT() as the item, as Notion's
// filter() and map(); CURRENT() outside them says where it goes.
func TestFilterAndMap(t *testing.T) {
	fields := []Field{{ID: "amounts", Name: "Amounts", Kind: KindText}, {ID: "names", Name: "Names", Kind: KindText}}
	cells := map[string]Value{
		"amounts": List([]Value{Number(40), Number(250), Number(120), Blank()}),
		"names":   List([]Value{Text("venue"), Text("print")}),
	}
	cases := map[string]interface{}{
		`FILTER({Amounts}, CURRENT() > 100)`:                      "250, 120",
		`SUM(FILTER({Amounts}, CURRENT() > 100))`:                 float64(370),
		`AVERAGE({Amounts})`:                                      136.6666666667, // JSON tidies float noise
		`MAX({Amounts}, 300)`:                                     float64(300),
		`COUNTA(FILTER({Amounts}, CURRENT() > 100))`:              float64(2),
		`MAP({Names}, UPPER(CURRENT()))`:                          "VENUE, PRINT",
		`ARRAYJOIN(MAP({Amounts}, CURRENT() * 2), " + ")`:         "80 + 500 + 240 + 0",
		`MAP(FILTER({Amounts}, CURRENT() > 100), CURRENT() / 10)`: "25, 12",
		`FILTER({Names}, CONTAINS(CURRENT(), "VEN"))`:             "venue",
		`FILTER({Names}, FALSE())`:                                nil,
	}
	for src, want := range cases {
		p := Compile(append(append([]Field{}, fields...), Field{ID: "f", Name: "F", IsFormula: true, Formula: src}))
		got := runOn(p, cells)["f"]
		if !reflect.DeepEqual(got.JSON(), want) {
			t.Errorf("%s = %#v, want %#v", src, got.JSON(), want)
		}
	}
	if got := eval(t, `CURRENT()`); got.Kind != KindError || !strings.Contains(got.Str, "inside one") {
		t.Errorf("CURRENT() outside FILTER or MAP is %#v", got)
	}
}

// {Link}.{Field} reads a field of the rows a link links to, as a list: by
// name as it's written, by id as it's stored, and back to names to edit.
func TestLinkedFields(t *testing.T) {
	vendor := []Field{{ID: "city", Name: "City", Kind: KindText}, {ID: "spend", Name: "Spend", Kind: KindNumber}}
	fields := []Field{
		{ID: "name", Name: "Name", Kind: KindText},
		{ID: "vendor", Name: "Vendor", Kind: KindText, Linked: vendor},
	}
	src := `ARRAYJOIN(ARRAYUNIQUE({Vendor}.{city}), " / ") & " " & COUNTA({ Vendor }.{Spend})`
	stored, err := Canonical(src, fields)
	if err != nil || stored != `ARRAYJOIN(ARRAYUNIQUE({#vendor}.{#city}), " / ") & " " & COUNTA({#vendor}.{#spend})` {
		t.Fatalf("stored as %q, %v", stored, err)
	}
	if shown := Display(stored, fields); shown != `ARRAYJOIN(ARRAYUNIQUE({Vendor}.{City}), " / ") & " " & COUNTA({Vendor}.{Spend})` {
		t.Errorf("shown as %q", shown)
	}
	// A linked field deleted since shows as one.
	if shown := Display(`{#vendor}.{#gone}`, fields); shown != `{Vendor}.{Deleted field}` {
		t.Errorf("a deleted linked field shows as %q", shown)
	}
	p := Compile(append(append([]Field{}, fields...), Field{ID: "f", Name: "F", IsFormula: true, Formula: stored}))
	var asked []string
	got := p.Run(func(id string) (Value, int) {
		asked = append(asked, id)
		switch id {
		case LinkedRef("vendor", "city"):
			return List([]Value{Text("Pune"), Text("Delhi"), Text("Pune")}), 0
		case LinkedRef("vendor", "spend"):
			return List([]Value{Number(10), Number(20)}), 0
		}
		return Blank(), 0
	}, now, ist)["f"]
	if got.JSON() != "Pune / Delhi 2" {
		t.Errorf("{Vendor}.{City} gave %#v, reading %v", got.JSON(), asked)
	}
	if k := p.Kind("f"); k != KindText {
		t.Errorf("the formula gives %v", k)
	}
	if k := Compile(append(append([]Field{}, fields...), Field{ID: "g", IsFormula: true, Formula: `{Vendor}.{Spend}`})).Kind("g"); k != KindText {
		t.Errorf("{Vendor}.{Spend} gives %v, want a list (text)", k)
	}
	if refs := Refs(src); !reflect.DeepEqual(refs, []string{"Vendor", "Vendor"}) {
		t.Errorf("the fields it reads here are %v", refs)
	}
	errs := map[string]string{
		`{Name}.{City}`:    `{Name} doesn't link to a table's rows, so it has no fields to read`,
		`{Vendor}.{Owner}`: `There's no field called "Owner" in the rows {Vendor} links to`,
		`{Vendor}.City`:    `"." can't be used here`,
		`{Vendor}.{#gone}`: `A field this formula reads has been deleted in the rows {Vendor} links to`,
	}
	for src, want := range errs {
		_, err := Check(src, fields, "f")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", src, err, want)
		}
	}
}

// A comment says what a formula is for, and is read as nothing: kept as
// written when stored and shown, braces in it naming no field.
func TestComments(t *testing.T) {
	src := "/* price with tax */ {Price} * 1.18 /* {Quantity} isn't read */"
	if got := eval(t, src).JSON(); got != 14.75 {
		t.Errorf("%s = %#v", src, got)
	}
	if refs := Refs(src); !reflect.DeepEqual(refs, []string{"Price"}) {
		t.Errorf("it reads %v", refs)
	}
	stored, err := Canonical(src, tracker)
	if err != nil || stored != "/* price with tax */ {#price} * 1.18 /* {Quantity} isn't read */" {
		t.Errorf("stored as %q, %v", stored, err)
	}
	if _, err := Check("{Price} /* never closed", tracker, "f"); err == nil || !strings.Contains(err.Error(), "A comment has no closing */") {
		t.Errorf("an open comment: %v", err)
	}
	if got := eval(t, "{Price} / 2").JSON(); got != 6.25 {
		t.Errorf("a division is still one: %#v", got)
	}
}
