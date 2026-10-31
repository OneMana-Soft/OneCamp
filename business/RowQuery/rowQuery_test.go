package business

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	query "github.com/akashc777/OneCamp/business/QuerySpec"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// A table of deals: a name, an amount, a due day, a won tick, a stage, tags
// and owners; and a formula, which the SQL can't read yet.
var (
	fName   = &model.Field{Id: uuid.MustParse("00000000-0000-0000-0000-0000000000a1"), Name: "Name", Type: model.FieldText, Config: "{}"}
	fAmount = &model.Field{Id: uuid.MustParse("00000000-0000-0000-0000-0000000000a2"), Name: "Amount", Type: model.FieldNumber, Config: "{}"}
	fDue    = &model.Field{Id: uuid.MustParse("00000000-0000-0000-0000-0000000000a3"), Name: "Due", Type: model.FieldDate, Config: "{}"}
	fWon    = &model.Field{Id: uuid.MustParse("00000000-0000-0000-0000-0000000000a4"), Name: "Won", Type: model.FieldCheckbox, Config: "{}"}
	fStage  = &model.Field{Id: uuid.MustParse("00000000-0000-0000-0000-0000000000a5"), Name: "Stage", Type: model.FieldStatus,
		Config: `{"options":[{"id":"o_new","label":"New","group":"todo"},{"id":"o_talk","label":"Talking","group":"doing"},{"id":"o_won","label":"Won","group":"done"}]}`}
	fTags   = &model.Field{Id: uuid.MustParse("00000000-0000-0000-0000-0000000000a6"), Name: "Tags", Type: model.FieldMultiSelect, Config: `{"options":[{"label":"hot"},{"label":"big"},{"label":"new"}]}`}
	fOwners = &model.Field{Id: uuid.MustParse("00000000-0000-0000-0000-0000000000a7"), Name: "Owners", Type: model.FieldPeople, Config: "{}"}
	fTax    = &model.Field{Id: uuid.MustParse("00000000-0000-0000-0000-0000000000a8"), Name: "Tax", Type: model.FieldFormula, Config: `{"formula":"{Amount} * 0.18"}`}
	deals   = []*model.Field{fName, fAmount, fDue, fWon, fStage, fTags, fOwners, fTax}
	sam     = uuid.MustParse("00000000-0000-0000-0000-00000000005a")
	priya   = uuid.MustParse("00000000-0000-0000-0000-00000000009a")
)

func row(n int, cells map[*model.Field]interface{}) *evalRow {
	vals := map[string]interface{}{}
	for f, v := range cells {
		b, _ := json.Marshal(v)
		var x interface{}
		_ = json.Unmarshal(b, &x)
		vals[f.Id.String()] = x
	}
	return &evalRow{id: uuid.MustParse("00000000-0000-0000-0000-0000000000" + []string{"01", "02", "03", "04", "05"}[n]), positionKey: "d000" + string(rune('0'+n)), values: vals, rowNo: int64(n + 1)}
}

var people = func(ids ...uuid.UUID) []map[string]string {
	var out []map[string]string
	for _, id := range ids {
		out = append(out, map[string]string{"id": id.String(), "type": "user"})
	}
	return out
}

var rows5 = []*evalRow{
	row(0, map[*model.Field]interface{}{fName: "Booth", fAmount: 1000, fDue: "2026-10-12", fWon: true, fStage: "Won", fTags: []string{"hot", "big"}, fOwners: people(sam)}),
	row(1, map[*model.Field]interface{}{fName: "Banners", fAmount: 250, fDue: "2026-10-20", fStage: "Talking", fTags: []string{"new"}, fOwners: people(priya, sam)}),
	row(2, map[*model.Field]interface{}{fName: "power cables", fAmount: 90, fDue: "2026-09-30T10:00:00Z", fStage: "New"}),
	row(3, map[*model.Field]interface{}{fName: "  ", fAmount: "lots", fDue: "2026-02-30", fStage: "Lost"}),
	row(4, map[*model.Field]interface{}{}),
}

var now = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

func cond(f *model.Field, op string, vals ...query.Value) *query.Filter {
	return &query.Filter{Field: "field:" + f.Id.String(), Operator: op, Values: vals}
}

func text(s string) query.Value { return query.Value{Text: s} }
func num(f float64) query.Value { return query.Value{Number: &f} }

// Each family's conditions mean what eval.go says: the rows (by index) that
// meet each.
func TestConditionsMean(t *testing.T) {
	cols := columns(deals)
	who := asker{user: sam, now: now, zone: time.UTC}
	cases := []struct {
		name string
		f    *query.Filter
		want string
	}{
		{"text contains, ignoring case", cond(fName, query.OperatorContains, text("B")), "0,1,2"},
		{"text is, trimmed", cond(fName, query.OperatorIs, text("booth")), "0"},
		{"text empty: blank and none", cond(fName, query.OperatorEmpty), "3,4"},
		{"text doesn't contain takes in the empty", cond(fName, query.OperatorNotContains, text("b")), "3,4"},
		{"starts with", cond(fName, query.OperatorStartsWith, text("Po")), "2"},
		{"number gt: text that isn't one is empty", cond(fAmount, query.OperatorGt, num(100)), "0,1"},
		{"number between, inclusive", cond(fAmount, query.OperatorBetween, num(90), num(250)), "1,2"},
		{"number is not takes in the empty", cond(fAmount, query.OperatorIsNot, num(250)), "0,2,3,4"},
		{"number empty", cond(fAmount, query.OperatorEmpty), "3,4"},
		{"date before a day; 30 February is no date", cond(fDue, query.OperatorBefore, text("2026-10-12")), "2"},
		{"date this week (Sunday 18 Oct: from Monday the 12th)", cond(fDue, query.OperatorIs, query.Value{Relative: query.RelativeThisWeek}), "0"},
		{"date on or after", cond(fDue, query.OperatorOnOrAfter, text("2026-10-12")), "0,1"},
		{"date empty", cond(fDue, query.OperatorEmpty), "3,4"},
		{"checked", cond(fWon, query.OperatorChecked), "0"},
		{"unchecked", cond(fWon, query.OperatorUnchecked), "1,2,3,4"},
		{"status by option id", cond(fStage, query.OperatorIs, query.Value{ID: "o_talk"}), "1"},
		{"status group", cond(fStage, query.OperatorStatusGroupIs, text("doing"), text("done")), "0,1"},
		{"status not in takes in the empty and the unknown", cond(fStage, query.OperatorNotIn, text("Won"), text("New")), "1,3,4"},
		{"tags any", cond(fTags, query.OperatorIn, text("big"), text("new")), "0,1"},
		{"tags all of", cond(fTags, query.OperatorAllOf, text("hot"), text("big")), "0"},
		{"tags is exactly", cond(fTags, query.OperatorIsExactly, text("new")), "1"},
		{"status contains, ignoring case", cond(fStage, query.OperatorContains, text("O")), "0,3"},
		{"status doesn't contain takes in the empty", cond(fStage, query.OperatorNotContains, text("O")), "1,2,4"},
		{"tags don't contain takes in the empty", cond(fTags, query.OperatorNotContains, text("e")), "0,2,3,4"},
		{"owners include me", cond(fOwners, query.OperatorIn, text(query.Me)), "0,1"},
		{"owners all of", cond(fOwners, query.OperatorAllOf, query.Value{ID: sam.String()}, query.Value{ID: priya.String()}), "1"},
		{"owners empty", cond(fOwners, query.OperatorEmpty), "2,3,4"},
		{"and, or, not", &query.Filter{Op: query.OpAnd, Filters: []query.Filter{
			*cond(fAmount, query.OperatorGte, num(90)),
			{Op: query.OpNot, Filters: []query.Filter{*cond(fWon, query.OperatorChecked)}},
		}}, "1,2"},
		{"or", &query.Filter{Op: query.OpOr, Filters: []query.Filter{*cond(fName, query.OperatorIs, text("booth")), *cond(fStage, query.OperatorIs, text("New"))}}, "0,2"},
	}
	for _, c := range cases {
		var got []string
		for i, r := range rows5 {
			ok, err := matches(c.f, r, cols, nil, who)
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			if ok {
				got = append(got, string(rune('0'+i)))
			}
		}
		if strings.Join(got, ",") != c.want {
			t.Errorf("%s: rows %s, want %s", c.name, strings.Join(got, ","), c.want)
		}
	}
}

// A link cell, as a read gives it, may count links past those it names
// ({"type": "more"}), or name none of them: it has links all the same, as
// the browser's filters (viewRules isBlank) and the stored links say.
func TestALinkCellCountingLinksItDoesntNameHasLinks(t *testing.T) {
	vendor := &model.Field{Id: uuid.MustParse("00000000-0000-0000-0000-0000000000b1"), Name: "Vendor", Type: model.FieldRelation,
		Config: `{"relation_target":"table","table_id":"00000000-0000-0000-0000-0000000000c1"}`}
	acme := map[string]string{"id": "00000000-0000-0000-0000-0000000000d1", "label": "Acme", "type": "row"}
	more := map[string]string{"id": "", "label": "12 links", "type": "more"}
	rows := []*evalRow{
		row(0, map[*model.Field]interface{}{vendor: []interface{}{more}}),
		row(1, map[*model.Field]interface{}{vendor: []interface{}{acme, more}}),
		row(2, map[*model.Field]interface{}{vendor: []interface{}{}}),
		row(3, map[*model.Field]interface{}{}),
	}
	cols := columns([]*model.Field{vendor})
	who := asker{user: sam, now: now, zone: time.UTC}
	for op, want := range map[string]string{query.OperatorEmpty: "2,3", query.OperatorNotEmpty: "0,1"} {
		var got []string
		for i, r := range rows {
			ok, err := matches(cond(vendor, op), r, cols, nil, who)
			if err != nil {
				t.Fatalf("%s: %v", op, err)
			}
			if ok {
				got = append(got, fmt.Sprint(i))
			}
		}
		if strings.Join(got, ",") != want {
			t.Errorf("%s: rows %s, want %s", op, strings.Join(got, ","), want)
		}
	}
}

// Who last changed a row is whoever made it until someone changes it, as
// the grid shows it (FieldTypes' rowModifiedBy): a row from before the
// ledger kept who changed it has no one there.
func TestWhoLastChangedARowIsItsMakerUntilSomeoneDoes(t *testing.T) {
	changedBy := &model.Field{Id: uuid.MustParse("00000000-0000-0000-0000-0000000000b2"), Name: "Changed by", Type: model.FieldModifiedBy, Config: "{}"}
	rows := []*evalRow{
		{id: uuid.MustParse("00000000-0000-0000-0000-000000000001"), createdBy: sam},
		{id: uuid.MustParse("00000000-0000-0000-0000-000000000002"), createdBy: sam, updatedBy: priya},
		{id: uuid.MustParse("00000000-0000-0000-0000-000000000003")},
	}
	cols := columns([]*model.Field{changedBy})
	who := asker{user: sam, now: now, zone: time.UTC}
	for _, c := range []struct {
		f    *query.Filter
		want string
	}{
		{cond(changedBy, query.OperatorIs, text(query.Me)), "0"},
		{cond(changedBy, query.OperatorIs, query.Value{ID: priya.String()}), "1"},
		{cond(changedBy, query.OperatorEmpty), "2"},
	} {
		var got []string
		for i, r := range rows {
			ok, err := matches(c.f, r, cols, nil, who)
			if err != nil {
				t.Fatal(err)
			}
			if ok {
				got = append(got, fmt.Sprint(i))
			}
		}
		if strings.Join(got, ",") != c.want {
			t.Errorf("%s %v: rows %s, want %s", c.f.Operator, c.f.Values, strings.Join(got, ","), c.want)
		}
	}
}

// The order: by stage's options (an unknown label after them), empties last
// either way; ties in the table's order.
func TestOrder(t *testing.T) {
	cols := columns(deals)
	who := asker{now: now, zone: time.UTC}
	order := func(sorts ...query.Sort) string {
		rs := append([]*evalRow{}, rows5...)
		if err := sortRows(rs, bySorts(sorts), cols, nil, who); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range rs {
			out = append(out, string(r.positionKey[4]))
		}
		return strings.Join(out, ",")
	}
	if got := order(query.Sort{Field: "field:" + fStage.Id.String()}); got != "2,1,0,3,4" {
		t.Errorf("by stage: %s", got)
	}
	if got := order(query.Sort{Field: "field:" + fStage.Id.String(), Desc: true}); got != "3,0,1,2,4" {
		t.Errorf("by stage, last first: %s", got)
	}
	if got := order(query.Sort{Field: "field:" + fAmount.Id.String(), Desc: true}); got != "0,1,2,3,4" {
		t.Errorf("by amount, most first: %s", got)
	}
	if got := order(query.Sort{Field: "field:" + fDue.Id.String()}); got != "2,0,1,3,4" {
		t.Errorf("by due: %s", got)
	}
	if got := order(query.Sort{Field: "field:" + fName.Id.String()}); got != "1,0,2,3,4" {
		t.Errorf("by name: %s", got)
	}
	if _, _, err := sortValuesOf(bySorts([]query.Sort{{Field: "field:" + fOwners.Id.String()}}), cols, nil, rows5[0], who); err == nil {
		t.Error("sorting by people isn't built and should say so")
	}
}

// The SQL takes every value and every cell's key as a parameter; a formula
// isn't SQL's to filter yet.
func TestCompiledSQL(t *testing.T) {
	cols := columns(deals)
	who := asker{user: sam, now: now, zone: time.UTC}
	a := &sqlArgs{base: 1}
	where, err := whereOf(&query.Filter{Op: query.OpAnd, Filters: []query.Filter{
		*cond(fName, query.OperatorContains, text("x'); DROP TABLE users; --")),
		*cond(fOwners, query.OperatorIn, text(query.Me)),
		*cond(fDue, query.OperatorBefore, text("2026-10-12")),
	}}, cols, who, a)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(where, "DROP") || strings.Contains(where, fName.Id.String()) {
		t.Errorf("a value or a key was written into the SQL: %s", where)
	}
	if !strings.Contains(where, "ci.t < ") || !strings.Contains(where, "ci.d < ") {
		t.Errorf("a date isn't read from the index's day and moment: %s", where)
	}
	found := false
	for _, v := range a.vals {
		if arr, ok := v.([]string); ok && len(arr) == 1 && arr[0] == sam.String() {
			found = true
		}
	}
	if !found {
		t.Errorf("@me isn't the asker in %v", a.vals)
	}
	if _, err := whereOf(cond(fTax, query.OperatorGt, num(1)), cols, who, &sqlArgs{}); err == nil {
		t.Error("a formula compiled to SQL")
	}
	keys, err := sortKeysOf(bySorts([]query.Sort{{Field: "field:" + fStage.Id.String(), Desc: true}}), cols, who, &sqlArgs{base: 1})
	if err != nil || len(keys) != 4 || !strings.Contains(orderBy(keys), "DESC NULLS LAST") || !strings.HasSuffix(orderBy(keys), `(r.position_key) COLLATE "C" ASC NULLS LAST, (r.id) ASC NULLS LAST`) {
		t.Errorf("order: %v %s", err, orderBy(keys))
	}
}

// A choice's or a list's "doesn't contain" is its own condition, the NOT of
// "contains" that keeps the blank cells, so a view's filter needs no "not"
// group for it, which would take one of a filter's three levels.
func TestAChoiceOrAListCanBeSaidNotToContain(t *testing.T) {
	cols := columns(deals)
	who := asker{user: sam, now: now, zone: time.UTC}
	for _, f := range []*model.Field{fStage, fTags} {
		where, err := whereOf(cond(f, query.OperatorNotContains, text("o")), cols, who, &sqlArgs{base: 1})
		if err != nil {
			t.Fatalf("%s: %v", f.Name, err)
		}
		if !strings.HasPrefix(where, "NOT ") || !strings.Contains(where, "strpos(") {
			t.Errorf("%s: %s isn't the NOT of contains", f.Name, where)
		}
	}
}

// A cursor is this query's own: another kind or count of keys is refused.
func TestCursors(t *testing.T) {
	c := cursorOf("sql", []interface{}{"booth", "12.5", time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC), nil, "d0001", "id"})
	vals, err := readCursor(c, "sql", 6)
	if err != nil || vals[0] != "booth" || vals[2] != "2026-10-12T00:00:00Z" || vals[3] != nil {
		t.Errorf("read back %v %v", vals, err)
	}
	if _, err := readCursor(c, "worked", 6); err != ErrBadCursor {
		t.Errorf("another kind: %v", err)
	}
	if _, err := readCursor(c, "sql", 4); err != ErrBadCursor {
		t.Errorf("another count of keys: %v", err)
	}
	if _, err := readCursor("not-base64!", "sql", 6); err != ErrBadCursor {
		t.Errorf("by hand: %v", err)
	}

	// A list's cursor (lists.go) reads back whole, and carries the one
	// query's cursor to go on from, or it's refused.
	lc := listCursor{key: "k", at: 200, cut: true, fresh: "f", view: "v", after: c}
	if back, err := readListCursor(lc.cursor()); err != nil || back != lc {
		t.Errorf("a list's cursor read back as %+v, %v", back, err)
	}
	for name, bad := range map[string]listCursor{
		"no cursor to go on from":   {key: "k", fresh: "f", view: "v"},
		"a walk's to go on from":    {key: "k", fresh: "f", view: "v", after: cursorOf("walk", []interface{}{"x"})},
		"a place before the list's": {key: "k", at: -1, fresh: "f", view: "v", after: c},
	} {
		if _, err := readListCursor(bad.cursor()); err != ErrBadCursor {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := readListCursor(c); err != ErrBadCursor {
		t.Errorf("the one query's cursor as a list's: %v", err)
	}
}

// A condition is never NULL in SQL: a missing cell doesn't meet "checked",
// and so does meet NOT "checked" (the property test's first find).
func TestConditionsAreNeverNull(t *testing.T) {
	cols := columns(deals)
	who := asker{user: sam, now: now, zone: time.UTC}
	for _, f := range []*query.Filter{cond(fWon, query.OperatorChecked), cond(fStage, query.OperatorIs, text("Won")), cond(fAmount, query.OperatorGt, num(1))} {
		sql, err := whereOf(f, cols, who, &sqlArgs{base: 1})
		wrapped := strings.HasPrefix(sql, "COALESCE((") && strings.HasSuffix(sql, "), FALSE)")
		if err != nil || !(wrapped || loneExists(sql)) {
			t.Errorf("%s: %s %v", f.Operator, sql, err)
		}
	}
	// Only a lone EXISTS goes unwrapped: one that's part of more could be NULL.
	for sql, lone := range map[string]bool{
		"EXISTS (SELECT 1 FROM x WHERE (a) AND (b))": true,
		"NOT EXISTS (SELECT 1 FROM x)":               true,
		"EXISTS (SELECT 1 FROM x) OR r.row_no > $2":  false,
		"EXISTS (SELECT 1) AND EXISTS (SELECT 2)":    false,
		"(EXISTS (SELECT 1))":                        false,
		"NULLIF(r.row_no, 0) > $2":                   false,
	} {
		if loneExists(sql) != lone {
			t.Errorf("loneExists(%q) = %v", sql, !lone)
		}
	}
}

// A window after a cursor on the table's own order is one row comparison the
// index answers, taking in rows with no place only when the table has some.
func TestAfterTheTablesOrder(t *testing.T) {
	keys, _ := sortKeysOf(nil, nil, asker{}, &sqlArgs{base: 1})
	keyed := afterSQL(keys, []interface{}{"d0001", "0a6c1f0e-0000-4000-8000-000000000001"}, &sqlArgs{base: 1}, false)
	if keyed != `((r.position_key, r.id) > ($2::text COLLATE "C", $3::uuid))` {
		t.Errorf("keyed: %s", keyed)
	}
	if s := afterSQL(keys, []interface{}{"d0001", "id"}, &sqlArgs{base: 1}, true); !strings.Contains(s, "OR r.position_key IS NULL") {
		t.Errorf("unkeyed: %s", s)
	}
	if s := afterSQL(keys, []interface{}{nil, "id"}, &sqlArgs{base: 1}, false); s != `((r.position_key) IS NULL AND (r.id) > $2::uuid)` {
		t.Errorf("after a row with no place: %s", s)
	}
}

// What the index holds for each family: what the evaluator compares, so the
// SQL over it means the same.
func TestIndexEntries(t *testing.T) {
	cols := columns(deals)
	col := func(f *model.Field) *column { return cols[f.Id.String()] }
	type want struct {
		ok bool
		n  *float64
		t  string
		s  string
		d  string
		b  bool
		a  []string
	}
	f := func(x float64) *float64 { return &x }
	long := strings.Repeat("Row 7, ", 600) // 4,200 bytes: its t whole, its s cut
	cases := []struct {
		f   *model.Field
		raw interface{}
		w   want
	}{
		{fName, "  Booth ", want{ok: true, t: "booth", s: "booth"}},
		{fName, "   ", want{}},
		{fName, 12.0, want{ok: true, t: "12", s: "00000000000000000012"}},
		{fName, "Row 2", want{ok: true, t: "row 2", s: "row 00000000000000000002"}},
		{fName, long, want{ok: true, t: strings.ToLower(strings.TrimSpace(long)), s: naturalKey(long)}},
		{fAmount, 1200.0, want{ok: true, n: f(1200)}},
		{fAmount, "1200", want{}},
		{fDue, "2026-10-12", want{ok: true, t: "2026-10-12", d: "2026-10-12T00:00:00Z"}}, // sorts as its midnight at UTC
		{fDue, "2026-02-30", want{}},
		{fDue, "2026-10-18T20:00:00+05:30", want{ok: true, d: "2026-10-18T14:30:00Z"}},
		{fDue, "2026-10-18T20:00", want{}},
		{fWon, true, want{ok: true, b: true}},
		{fWon, false, want{}},
		{fWon, "true", want{}},
		{fStage, " Won ", want{ok: true, t: "won", s: "won", n: f(3)}},
		{fStage, "Lost", want{ok: true, t: "lost", s: "lost", n: f(4)}}, // after every option
		{fStage, []interface{}{"Won"}, want{}},
		{fTags, []interface{}{"Hot", " ", "big"}, want{ok: true, a: []string{"hot", "big"}}},
		{fTags, []interface{}{}, want{}},
		{fOwners, []interface{}{map[string]interface{}{"id": strings.ToUpper(sam.String())}, priya.String()}, want{ok: true, a: []string{sam.String(), priya.String()}}},
		{fTax, 5.0, want{}},
	}
	for _, c := range cases {
		e, ok := entryOf(col(c.f), c.raw)
		got := want{ok: ok, n: e.N, b: e.B, a: e.A}
		if e.T != nil {
			got.t = *e.T
		}
		if e.S != nil {
			got.s = *e.S
		}
		if e.D != nil {
			got.d = e.D.Format(time.RFC3339)
		}
		if got.ok != c.w.ok || got.t != c.w.t || got.s != c.w.s || got.d != c.w.d || got.b != c.w.b || strings.Join(got.a, ",") != strings.Join(c.w.a, ",") ||
			(got.n == nil) != (c.w.n == nil) || (got.n != nil && *got.n != *c.w.n) {
			t.Errorf("%s %.80q: %+v, want %+v", c.f.Name, fmt.Sprint(c.raw), got, c.w)
		}
	}
	// A long text's key fits any index entry: 256 code points at most.
	if s := naturalKey(long); utf8.RuneCountInString(s) != keyRunes || len(s) > 4*keyRunes {
		t.Errorf("a long text's key: %d code points, %d bytes", utf8.RuneCountInString(s), len(s))
	}
}

// One sort on a number, a text, a choice or a tick is walked along the
// index: its values first, then the rows with none (a tick: none first going
// up); several sorts, a date or a row's own column take the one query.
func TestWalks(t *testing.T) {
	cols := columns(deals)
	by := func(f *model.Field, desc bool) []query.Sort {
		return []query.Sort{{Field: "field:" + f.Id.String(), Desc: desc}}
	}
	if w, _, err := walksOf(nil, nil, cols); err != nil || len(w) != 1 || w[0].field != nil || w[0].empty {
		t.Errorf("no sort: %+v %v", w, err)
	}
	for _, c := range []struct {
		f     *model.Field
		desc  bool
		first string // "values" or "rest"
	}{{fAmount, true, "values"}, {fName, false, "values"}, {fStage, false, "values"}, {fDue, true, "values"}, {fWon, false, "rest"}, {fWon, true, "values"}} {
		w, _, err := walksOf(bySorts(by(c.f, c.desc)), nil, cols)
		if err != nil || len(w) != 2 || w[0].empty == (c.first == "values") || !w[boolIndex(c.first == "values")].empty {
			t.Errorf("%s desc %v: %+v %v", c.f.Name, c.desc, w, err)
		}
	}
	for _, sorts := range [][]query.Sort{append(by(fAmount, false), by(fName, false)...), {{Field: "row_no"}}} {
		if w, _, err := walksOf(bySorts(sorts), nil, cols); w != nil {
			t.Errorf("%v walked: %+v %v", sorts, w, err)
		}
	}
	// After a cursor: the rest of its tie, then past its value, both ranges,
	// each with only the parameters it reads.
	w, _, _ := walksOf(bySorts(by(fStage, true)), nil, cols)
	at := []interface{}{"3", "won", "d0001", "0a6c1f0e-0000-4000-8000-000000000001"}
	if tie := w[0].afterIn(at, 0, &sqlArgs{base: 1}); tie != `s.n = $2::numeric AND s.s = $3::text COLLATE "C" AND (s.p, s.row_id) > ($4::text COLLATE "C", $5::uuid)` {
		t.Errorf("tie: %s", tie)
	}
	past := &sqlArgs{base: 1}
	if s := w[0].afterIn(at, 1, past); s != `(s.n, s.s) < ($2::numeric, $3::text COLLATE "C")` || len(past.vals) != 2 {
		t.Errorf("past: %s %v", s, past.vals)
	}
	// A number's s is always NULL: its tie says so, and its past is n alone.
	w, _, _ = walksOf(bySorts(by(fAmount, false)), nil, cols)
	at = []interface{}{"12.5", nil, "d0001", "id"}
	if tie := w[0].afterIn(at, 0, &sqlArgs{base: 1}); tie != `s.n = $2::numeric AND s.s IS NULL AND (s.p, s.row_id) > ($3::text COLLATE "C", $4::uuid)` {
		t.Errorf("a number's tie: %s", tie)
	}
	if s := w[0].afterIn(at, 1, &sqlArgs{base: 1}); s != `(s.n) > ($2::numeric)` {
		t.Errorf("a number's past: %s", s)
	}
	// A text walks its sort key's index, not t's: a long text's t isn't there.
	w, _, _ = walksOf(bySorts(by(fName, true)), nil, cols)
	if _, cond, order := w[0].sql(&sqlArgs{base: 1}); cond != "s.s IS NOT NULL" || order != "ORDER BY s.s DESC, s.p, s.row_id" {
		t.Errorf("a text's walk: %s %s", cond, order)
	}
}

// With no sort, a filter that a choice or a text is one value walks that
// value's entries in the table's order; one under an OR, or of two values,
// doesn't.
func TestEqualityWalks(t *testing.T) {
	cols := columns(deals)
	and := &query.Filter{Op: query.OpAnd, Filters: []query.Filter{*cond(fAmount, query.OperatorGt, num(1)), *cond(fStage, query.OperatorIs, text(" Won "))}}
	w, _, err := walksOf(nil, and, cols)
	if err != nil || len(w) != 1 || w[0].eq == nil || *w[0].eq != "won" || w[0].field.key != fStage.Id.String() {
		t.Fatalf("an AND's is: %+v %v", w, err)
	}
	a := &sqlArgs{base: 1}
	// In the words of the t index's predicate, which only short texts meet.
	if _, cond, order := w[0].sql(a); cond != `s.t = $3::text COLLATE "C" AND octet_length(s.t) <= 2000` || order != "ORDER BY s.p, s.row_id" {
		t.Errorf("its walk: %s %s", cond, order)
	}
	long := cond(fName, query.OperatorIs, text(strings.Repeat("x", model.IndexedTextBytes+1)))
	if w, _, _ := walksOf(nil, long, cols); len(w) != 1 || w[0].eq != nil {
		t.Errorf("a text longer than the t index holds walked as an equality: %+v", w)
	}
	for _, f := range []*query.Filter{
		{Op: query.OpOr, Filters: []query.Filter{*cond(fStage, query.OperatorIs, text("Won")), *cond(fName, query.OperatorIs, text("booth"))}},
		cond(fStage, query.OperatorIs, text("Won"), text("New")),
		cond(fName, query.OperatorContains, text("boo")),
		{Op: query.OpNot, Filters: []query.Filter{*cond(fStage, query.OperatorIs, text("Won"))}},
	} {
		if w, _, _ := walksOf(nil, f, cols); len(w) != 1 || w[0].eq != nil {
			t.Errorf("%+v walked as an equality: %+v", f, w)
		}
	}
}

// A walk reads the filter's conditions on its own field from its entry, so
// a range on the field it sorts by bounds the index it walks; another
// field's, or one under an OR, it leaves to the filter.
func TestAWalksOwnConditions(t *testing.T) {
	cols := columns(deals)
	who := asker{user: sam, now: now, zone: time.UTC}
	due := cols["field:"+fDue.Id.String()]
	f := &query.Filter{Op: query.OpAnd, Filters: []query.Filter{
		*cond(fDue, query.OperatorOnOrAfter, text("2026-10-01")),
		*cond(fName, query.OperatorIs, text("booth")),
		{Op: query.OpOr, Filters: []query.Filter{*cond(fDue, query.OperatorEmpty), *cond(fName, query.OperatorEmpty)}},
	}}
	a := &sqlArgs{base: 1}
	got := onWalk(f, due, who, a)
	if strings.Contains(got, "EXISTS") || !strings.Contains(got, "s.t >= $2::text") || !strings.Contains(got, "s.d >= $3::timestamptz") {
		t.Errorf("a walk's own range: %s", got)
	}
	if strings.Contains(got, "COALESCE") || !strings.Contains(got, "AND s.d >= $4::timestamptz)") {
		t.Errorf("a walk's own range, bare and bounding d alone: %s", got)
	}
	if len(a.vals) != 3 || !a.vals[2].(time.Time).Equal(time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("its parameters: %v", a.vals)
	}
	if got := onWalk(cond(fName, query.OperatorIs, text("booth")), due, who, &sqlArgs{base: 1}); got != "" {
		t.Errorf("another field's condition read on the walk: %s", got)
	}
	// NOT of an entry's condition is NOT of the entry's own: one entry a row.
	if got := onWalk(cond(fDue, query.OperatorNotEmpty), due, who, &sqlArgs{base: 1}); got != "((s.n IS NOT NULL OR s.t IS NOT NULL OR s.d IS NOT NULL OR s.b IS NOT NULL OR s.a IS NOT NULL))" {
		t.Errorf("not empty on the walk: %s", got)
	}
	// A negation keeps the empty cells, which on the walk's entry are NULL:
	// it is left to the filter.
	name := cols["field:"+fName.Id.String()]
	if got := onWalk(cond(fName, query.OperatorIsNot, text("booth")), name, who, &sqlArgs{base: 1}); got != "" {
		t.Errorf("a negation read on the walk: %s", got)
	}
	if got := onWalk(cond(fDue, query.OperatorEmpty), due, who, &sqlArgs{base: 1}); got != "" {
		t.Errorf("empty read on the walk: %s", got)
	}
}

// Two sorts, the first a choice, walk the second's index within each of the
// choice's options in their order (backwards going down), then the rows with
// no choice, and ask that no label is one no option has; a first sort of
// another kind, or more than two, take the one query.
func TestTwoSortsWalk(t *testing.T) {
	cols := columns(deals)
	stage, amount := "field:"+fStage.Id.String(), "field:"+fAmount.Id.String()
	w, unknown, err := walksOf(bySorts([]query.Sort{{Field: stage, Desc: true}, {Field: amount}}), nil, cols)
	if err != nil || unknown == nil || unknown.key != fStage.Id.String() {
		t.Fatalf("walks %d, unknown %v, %v", len(w), unknown, err)
	}
	var order []string
	for _, x := range w {
		label := "none"
		if x.in.label != nil {
			label = *x.in.label
		}
		part := "values"
		if x.empty {
			part = "rest"
		}
		order = append(order, label+"/"+part)
	}
	if strings.Join(order, ",") != "won/values,won/rest,talking/values,talking/rest,new/values,new/rest,none/values,none/rest" {
		t.Errorf("order: %v", order)
	}
	a := &sqlArgs{base: 1}
	if _, cond, _ := w[0].sql(a); !strings.Contains(cond, "s.n IS NOT NULL AND COALESCE((EXISTS") || !strings.Contains(cond, "ci.t = $3::text") {
		t.Errorf("a group's walk: %s", cond)
	}
	if _, cond, _ := w[7].sql(&sqlArgs{base: 1}); !strings.HasPrefix(cond, "s.n IS NULL AND s.t IS NULL") || !strings.Contains(cond, " AND COALESCE((NOT EXISTS") {
		t.Errorf("no choice, no amount: %s", cond)
	}
	for _, sorts := range [][]query.Sort{{{Field: amount}, {Field: stage}}, {{Field: stage}, {Field: amount}, {Field: amount}}} {
		if w, _, _ := walksOf(bySorts(sorts), nil, cols); w != nil {
			t.Errorf("%v walked", sorts)
		}
	}
}

func boolIndex(b bool) int {
	if b {
		return 1
	}
	return 0
}

// A walked column (a text, a number, a choice, a date, a tick) keeps an entry
// for every row, an empty one for a cell with no value, so its empty cells
// are walked too; a multi-select, people or links keep none for an empty cell.
func TestEveryRowHasAWalkedColumnsEntry(t *testing.T) {
	cols := columns(deals)
	for _, f := range []*model.Field{fName, fAmount, fStage, fDue, fWon} {
		e, ok := cellEntry(cols[f.Id.String()], nil)
		if !ok || e.N != nil || e.T != nil || e.D != nil || e.B || e.A != nil || e.Field != f.Id {
			t.Errorf("%s's empty cell: %+v %v", f.Name, e, ok)
		}
	}
	if _, ok := cellEntry(cols[fWon.Id.String()], false); !ok {
		t.Error("an unticked box has no entry")
	}
	for _, f := range []*model.Field{fTags, fOwners} {
		if _, ok := cellEntry(cols[f.Id.String()], nil); ok {
			t.Errorf("%s's empty cell has an entry", f.Name)
		}
	}
}

// A listing goes on as it began: a cursor names the plan that made it, so a
// first window the one query answered (its walk past WalkBudget) is followed
// by the one query, never by a walk reading the one query's keys.
func TestACursorNamesItsPlan(t *testing.T) {
	if got := cursorKind(cursorOf("sql", []interface{}{"a", 1})); got != "sql" {
		t.Errorf("the one query's cursor: %q", got)
	}
	if got := cursorKind(cursorOf("walk", []interface{}{0, "a"})); got != "walk" {
		t.Errorf("a walk's cursor: %q", got)
	}
	if got := cursorKind("not a cursor!"); got != "" {
		t.Errorf("no cursor: %q", got)
	}
}
