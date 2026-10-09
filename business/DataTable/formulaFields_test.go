package business

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// A template's formulas are added in one pass, each after those it reads,
// whatever order the template lists them in.
func TestFormulasInOrder(t *testing.T) {
	f := func(name, src string) FieldInput {
		return FieldInput{Name: name, Type: model.FieldFormula, Config: map[string]interface{}{"formula": src}}
	}
	got := formulasInOrder([]FieldInput{
		f("Grand total", "{total} + {Tax}"),
		f("Tax", "{ Total } * 0.18"),
		f("Loop", "{Loop}"),
		f("Total", "{Cost} * {Quantity}"),
		f("Label", `"Total: " & {Grand total}`),
	})
	var names []string
	for _, fi := range got {
		names = append(names, fi.Name)
	}
	if want := "Total, Tax, Grand total, Label, Loop"; strings.Join(names, ", ") != want {
		t.Errorf("order: %s, want %s", strings.Join(names, ", "), want)
	}
}

// table is fields for withFormulas: a number field "c", a text field
// "notes", and formula fields f0, f1… each src.
func table(n int, src string) []*model.Field {
	fields := []*model.Field{
		{Id: uuid.MustParse("00000000-0000-0000-0000-00000000000c"), Name: "C", Type: model.FieldNumber, Config: "{}"},
		{Id: uuid.MustParse("00000000-0000-0000-0000-0000000000ad"), Name: "Notes", Type: model.FieldText, Config: "{}"},
	}
	for i := 0; i < n; i++ {
		cfg, _ := json.Marshal(map[string]string{"formula": src})
		fields = append(fields, &model.Field{Id: uuid.New(), Name: fmt.Sprint("F", i), Type: model.FieldFormula, Config: string(cfg)})
	}
	return fields
}

func rowsOf(n int, values map[string]interface{}) []*model.Row {
	b, _ := json.Marshal(values)
	rows := make([]*model.Row, n)
	for i := range rows {
		rows[i] = &model.Row{Id: uuid.New(), Values: string(b)}
	}
	return rows
}

// A cell named many times in a formula is read once a row: each name read
// it again, so a number cell holding 100 KB of "1,1,1…" named 500 times took
// 0.5 s a row, and a list of 34,000 empty items joined 250 times 0.4 s.
func TestLongCellsAreReadOnce(t *testing.T) {
	empty := make([]interface{}, 34000)
	for i := range empty {
		empty[i] = map[string]interface{}{}
	}
	cases := []struct {
		name, cell, ref, join string
		value                 interface{}
	}{
		{"a long number cell", "00000000-0000-0000-0000-00000000000c", "{C}", "+", strings.Repeat("1,", 50<<10)},
		{"a long list", "00000000-0000-0000-0000-0000000000ad", "{Notes}", "&", empty},
	}
	for _, tc := range cases {
		took := func(names int) time.Duration {
			src := strings.TrimSuffix(strings.Repeat(tc.ref+tc.join, names), tc.join)
			rows := rowsOf(5, map[string]interface{}{tc.cell: tc.value})
			start := time.Now()
			withComputed(context.Background(), table(1, src), rows)
			return time.Since(start)
		}
		// As many names as a formula has room for.
		most := (2000 - 1) / len(tc.ref+tc.join)
		once, many := took(1), took(most)
		if many > 3*once+100*time.Millisecond {
			t.Errorf("%s: named once %v, %d times %v", tc.name, once, most, many)
		}
	}
}

// A row's formulas can't send many times what a row holds, whatever JSON
// makes of their text.
func TestRowsStayTheirSize(t *testing.T) {
	rows := rowsOf(1, map[string]interface{}{})
	withComputed(context.Background(), table(12, `REPT("<", 10000)`), rows)
	if n := len(rows[0].Values); n > 110<<10 {
		t.Errorf("the row's values are %d bytes", n)
	}
}

// Totals read a long table page by page, and each page has a budget of its
// own: sharing one made 2,747 of these 5,000 rows' totals errors, and the
// sum came out at 45,060, without saying so.
func TestTotalsOfALongTableAreRight(t *testing.T) {
	name, price, qty, notes := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	fields := []*model.Field{
		{Id: name, Name: "Name", Type: model.FieldText, Config: "{}"},
		{Id: price, Name: "Price", Type: model.FieldNumber, Config: "{}"},
		{Id: qty, Name: "Qty", Type: model.FieldNumber, Config: "{}"},
		{Id: notes, Name: "Notes", Type: model.FieldText, Config: "{}"},
	}
	for _, f := range [][2]string{
		{"Total", "{Price}*{Qty}"},
		{"Summary", `{Name} & ": " & {Notes}`},
		{"Short", `LEFT({Notes},1000) & "…"`},
		{"Words", `LEN({Notes}) - LEN(SUBSTITUTE({Notes}," ","")) + 1`},
	} {
		cfg, _ := json.Marshal(map[string]string{"formula": f[1]})
		fields = append(fields, &model.Field{Id: uuid.New(), Name: f[0], Type: model.FieldFormula, Config: string(cfg)})
	}
	values := map[string]interface{}{name.String(): "Row", price.String(): 4, qty.String(): 5, notes.String(): strings.Repeat("word ", 400)}
	var all []*model.Row
	for page := 0; page < 10; page++ {
		rows := rowsOf(500, values)
		withComputed(context.Background(), fields, rows)
		all = append(all, rows...)
	}
	res, err := Aggregate(fields, all, QuerySpec{Op: OpSum, ValueField: "Total"})
	if err != nil {
		t.Fatal(err)
	}
	sum := 0.0
	for _, b := range res.Buckets {
		sum += b.Value
	}
	if sum != 5000*20 {
		t.Errorf("the total is %v, want %v", sum, 5000*20)
	}
	for _, r := range all {
		if strings.Contains(r.Values, `"error"`) {
			t.Fatalf("a row with an error: %.200s", r.Values)
		}
	}
}

// Cells no formula reads are passed on as they're stored, not decoded and
// written again: a page of rows holding long lists took 3.4 s that way with
// one formula, against 50 µs with none.
func TestRowsPassThroughAsStored(t *testing.T) {
	notes := "00000000-0000-0000-0000-0000000000ad"
	fields := table(1, "1")
	f0 := fields[2].Id.String()
	// As Postgres hands a row back: spaced, with its own forms of numbers,
	// and a value left behind for the formula.
	stored := `{"` + notes + `": "<b>Café</b>  ", "x": 1.50, "y": [1e2, {"label": "A"}], "` + f0 + `": "stale"}`
	rows := []*model.Row{{Id: uuid.New(), Values: stored}, {Id: uuid.New(), Values: "not json"}}
	withComputed(context.Background(), fields, rows)
	var got map[string]interface{}
	if err := json.Unmarshal([]byte(rows[0].Values), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]interface{}{
		notes: "<b>Café</b>  ", "x": 1.5, f0: float64(1),
		"y": []interface{}{float64(100), map[string]interface{}{"label": "A"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the row: %s", rows[0].Values)
	}
	if rows[1].Values != `{"`+f0+`":1}` {
		t.Errorf("a row that can't be read: %s", rows[1].Values)
	}

	empty := make([]interface{}, 34000)
	for i := range empty {
		empty[i] = map[string]interface{}{}
	}
	long := rowsOf(30, map[string]interface{}{notes: empty})
	start := time.Now()
	for _, r := range long {
		_, _ = json.Marshal(parseRowValues(r.Values))
	}
	rewrite := time.Since(start)
	start = time.Now()
	withComputed(context.Background(), table(1, "1"), long)
	if took := time.Since(start); took > rewrite {
		t.Errorf("adding a formula to 30 rows took %v; decoding and writing them again takes %v", took, rewrite)
	}
}

// A total reading formula values that ran out of working out says it falls
// short; one reading a formula's ordinary error (dividing by zero) doesn't,
// as a total leaves errors out like blanks.
func TestTotalsSayWhenFormulasRanOut(t *testing.T) {
	notes := uuid.New()
	heavy := strings.TrimSuffix(strings.Repeat("LEN(UPPER({Notes}))+", 80), "+")
	fields := []*model.Field{{Id: notes, Name: "Notes", Type: model.FieldText, Config: "{}"}}
	// Heavy last: once a row's budget runs out, the formulas after it in the
	// row run out too.
	for _, f := range [][2]string{{"Ratio", "1/0"}, {"Two", "2"}, {"Heavy", heavy}} {
		cfg, _ := json.Marshal(map[string]string{"formula": f[1]})
		fields = append(fields, &model.Field{Id: uuid.New(), Name: f[0], Type: model.FieldFormula, Config: string(cfg)})
	}
	rows := rowsOf(3, map[string]interface{}{notes.String(): strings.Repeat("x", 64<<10)})
	withComputed(context.Background(), fields, rows)
	for _, c := range []struct {
		value string
		short bool
	}{{"Heavy", true}, {"Ratio", false}, {"Two", false}} {
		res, err := Aggregate(fields, rows, QuerySpec{Op: OpSum, ValueField: c.value})
		plan, perr := RunPlan(fields, rows, QueryPlan{Metrics: []PlanMetric{{Op: OpSum, ValueField: c.value}}})
		if err != nil || perr != nil {
			t.Fatalf("%s: %v %v", c.value, err, perr)
		}
		if res.Truncated != c.short || plan.Truncated != c.short {
			t.Errorf("summing %s: says it's short %v and %v, want %v", c.value, res.Truncated, plan.Truncated, c.short)
		}
	}
	// A filter reading it falls short too: whether those rows match is unknown.
	res, _ := Aggregate(fields, rows, QuerySpec{Op: OpCount, Filters: []Filter{{Field: "Heavy", Op: "not_empty"}}})
	if !res.Truncated {
		t.Error("a count filtered on a formula that ran out doesn't say it's short")
	}
}

// Totals over link cells, as reads give them, count the links shown and say
// they fall short of those past them; so do totals over a rollup a read
// left out.
func TestTotalsSayWhenLinksAreCut(t *testing.T) {
	vendor, spend := uuid.New(), uuid.New()
	other := uuid.New().String()
	fields := []*model.Field{
		{Id: vendor, Name: "Vendor", Type: model.FieldRelation, Config: fmt.Sprintf(`{"relation_target":"table","table_id":%q}`, other)},
		{Id: spend, Name: "Spend", Type: model.FieldRollup, Config: `{"aggregate":"sum"}`},
	}
	ref := func(label string) map[string]interface{} {
		return map[string]interface{}{"id": uuid.New().String(), "label": label, "type": "row", "table_id": other}
	}
	more := map[string]interface{}{"id": "", "label": "250 more", "type": "more", "table_id": other}
	row := func(cell []interface{}, sum interface{}) *model.Row {
		b, _ := json.Marshal(map[string]interface{}{vendor.String(): cell, spend.String(): sum})
		return &model.Row{Id: uuid.New(), Values: string(b)}
	}
	whole := []*model.Row{row([]interface{}{ref("Acme")}, 10), row([]interface{}{ref("Acme"), ref("Globex")}, 20)}
	cut := append(whole, row([]interface{}{ref("Acme"), more}, errTooManyLinked.JSON()))
	for _, c := range []struct {
		rows  []*model.Row
		spec  QuerySpec
		short bool
	}{
		{whole, QuerySpec{Op: OpCount, GroupBy: "Vendor"}, false},
		{whole, QuerySpec{Op: OpSum, ValueField: "Spend"}, false},
		{cut, QuerySpec{Op: OpCount, GroupBy: "Vendor"}, true},
		{cut, QuerySpec{Op: OpSum, ValueField: "Spend"}, true},
		{cut, QuerySpec{Op: OpCount, Filters: []Filter{{Field: "Vendor", Op: "contains", Value: "Initech"}}}, true},
	} {
		res, err := Aggregate(fields, c.rows, c.spec)
		if err != nil {
			t.Fatal(err)
		}
		if res.Truncated != c.short {
			t.Errorf("%+v over %d rows: short %v, want %v", c.spec, len(c.rows), res.Truncated, c.short)
		}
		for _, b := range res.Buckets {
			if strings.Contains(b.Label, "more") {
				t.Errorf("%+v: a group for the links not shown, %q", c.spec, b.Label)
			}
		}
	}
	res, _ := Aggregate(fields, cut, QuerySpec{Op: OpCount, GroupBy: "Vendor"})
	if len(res.Buckets) != 2 || res.Buckets[0].Label != "Acme" || res.Buckets[0].Count != 3 {
		t.Errorf("grouped by the links shown: %+v", res.Buckets)
	}
	// A cell counting links it names none of still has links.
	unnamed := append(whole, row([]interface{}{map[string]interface{}{"id": "", "label": "12 links", "type": "more", "table_id": other}}, nil))
	for op, want := range map[string]int{"empty": 0, "not_empty": 3} {
		if res, _ := Aggregate(fields, unnamed, QuerySpec{Op: OpCount, Filters: []Filter{{Field: "Vendor", Op: op}}}); res.MatchedRows != want {
			t.Errorf("%s: %d rows, want %d", op, res.MatchedRows, want)
		}
	}
}

// A row's stored text is read as encoding/json reads it, whatever its
// spacing, escapes or nesting, and keeps every cell.
func TestRowMembers(t *testing.T) {
	rows := []string{
		`{}`, ` { } `, `{"a":1}`, `{"a": 1.50, "b": -0, "c": 1e2, "d": true, "e": null}`,
		"{\n\t\"a\" :\r\n [1, [2, {\"x\": \"}]\\\"\"}], {}],\n \"b\": {\"c\": {\"d\": []}} \n}",
		`{"q\"uote": "a\\b\"c{[", "é": "é", "<b>": "</b>&"}`,
		`{"dup": 1, "dup": 2}`, `{"a": "` + strings.Repeat(`\\\"`, 50) + `"}`,
	}
	for _, s := range rows {
		ms, ok := rowMembers(s)
		if !ok {
			t.Errorf("can't read %q", s)
			continue
		}
		var want map[string]interface{}
		if err := json.Unmarshal([]byte(s), &want); err != nil {
			t.Fatalf("%q isn't JSON: %v", s, err)
		}
		got := map[string]interface{}{}
		for _, m := range ms {
			var v interface{}
			if err := json.Unmarshal([]byte(s[m.value:m.end]), &v); err != nil {
				t.Errorf("%q: the value of %q, %q, isn't JSON", s, m.key, s[m.value:m.end])
			}
			got[m.key] = v
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%q read as %v", s, got)
		}
		// Written again with nothing to add, it's the same object.
		var again map[string]interface{}
		if err := json.Unmarshal([]byte(withValues(s, ms, nil)), &again); err != nil || !reflect.DeepEqual(again, want) {
			t.Errorf("%q written again as %q", s, withValues(s, ms, nil))
		}
	}
	for _, s := range []string{``, `[]`, `"a"`, `{"a"}`, `{"a":}`, `{"a":1,}`, `{"a":1} x`, `{"a":"1}`} {
		if _, ok := rowMembers(s); ok {
			t.Errorf("read %q as an object", s)
		}
	}
}

// Any JSON object rowMembers reads, it reads as encoding/json does.
func FuzzRowMembers(f *testing.F) {
	for _, s := range []string{`{}`, `{"a": [1, {"b": "}"}]}`, `{"k\"": "v\\"}`, `{"a":1e-7,"b":" "}`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		var want map[string]interface{}
		valid := json.Unmarshal([]byte(s), &want) == nil && want != nil
		ms, ok := rowMembers(s)
		if valid && !ok {
			t.Fatalf("can't read the object %q", s)
		}
		if !valid || !ok {
			return
		}
		var got map[string]interface{}
		if err := json.Unmarshal([]byte(withValues(s, ms, nil)), &got); err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("%q written again as %q", s, withValues(s, ms, nil))
		}
	})
}

// Only formula fields can run out of working out: a plain field holding text
// that looks like one's error doesn't make a total say it falls short.
func TestOnlyFormulasFallShort(t *testing.T) {
	note := uuid.New()
	fields := []*model.Field{{Id: note, Name: "Note", Type: model.FieldText, Config: "{}"}}
	rows := rowsOf(2, map[string]interface{}{note.String(): map[string]interface{}{"error": "This formula takes too much working out"}})
	res, err := Aggregate(fields, rows, QuerySpec{Op: OpCount, GroupBy: "Note"})
	if err != nil || res.Truncated {
		t.Errorf("a plain field's look-alike: %+v, %v", res, err)
	}
}

// Reading a stored list costs what decoding it does: a page of rows each
// holding 25,000 one-letter items, read by one formula, took 2 s while
// charged for a tenth of its budget. Now the page's budget runs out partway.
func TestListCellsAreChargedAsRead(t *testing.T) {
	tags := uuid.New()
	fields := []*model.Field{{Id: tags, Name: "Tags", Type: model.FieldMultiSelect, Config: "{}"}}
	fields = append(table(1, "{Tags}")[2:], fields...)
	items := make([]interface{}, 25000)
	for i := range items {
		items[i] = "a"
	}
	rows := rowsOf(500, map[string]interface{}{tags.String(): items})
	withComputed(context.Background(), fields, rows)
	if strings.Contains(rows[0].Values, `"error"`) || !strings.Contains(rows[499].Values, "This table's formulas take too much working out") {
		t.Errorf("the first row %.80s…, the last %.200s…", rows[0].Values, rows[499].Values)
	}
}

// A number reads the same in a formula as a number cell or in a list: from
// 1e21 up as JavaScript writes it, not as 22 digits or more.
func TestNumbersInListsReadAsNumbers(t *testing.T) {
	tags := uuid.New()
	fields := append(table(1, `{Tags} & ""`)[2:], &model.Field{Id: tags, Name: "Tags", Type: model.FieldMultiSelect, Config: "{}"})
	rows := rowsOf(1, map[string]interface{}{tags.String(): []interface{}{1e308, 1500000, 0.25, "Live"}})
	withComputed(context.Background(), fields, rows)
	var got map[string]interface{}
	_ = json.Unmarshal([]byte(rows[0].Values), &got)
	if v := got[fields[0].Id.String()]; v != "1e+308, 1500000, 0.25, Live" {
		t.Errorf("the list reads as %#v", v)
	}
}
