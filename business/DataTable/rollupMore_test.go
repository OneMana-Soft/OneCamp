package business

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	formula "github.com/akashc777/OneCamp/business/Formula"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// A rollup adds up by a formula over values, as Airtable's: SUM(values) with
// tax, or the values joined another way; and only the linked rows that meet
// its conditions count, a count among them (tables B2).
func TestRollupsByFormulaAndConditions(t *testing.T) {
	r := newLinkRead(3, 3, 3)
	r.c.now, r.c.loc = time.Date(2026, 10, 8, 20, 0, 0, 0, time.UTC), time.UTC
	// Costs 10, 250 and 120; the second and third rows are open, as the
	// target's one condition found them.
	costs := []float64{10, 250, 120}
	open := []bool{false, true, true}
	r.t.conds = []*rollupCond{{}}
	for i, id := range r.of {
		r.t.rows[id].values = []formula.Value{formula.Number(costs[i])}
		r.t.rows[id].passes = []bool{open[i]}
	}
	cost := &model.Field{Id: uuid.New(), Name: "Cost"}
	r.t.reads, r.t.readAt = []*model.Field{cost}, map[string]int{cost.Id.String(): 0}
	roll := func(cfg rollupConfig, cond int) formula.Value {
		f := &rollupField{field: &model.Field{Id: uuid.New()}, cfg: cfg, link: r.l, of: cost, cond: cond}
		if cfg.Aggregate == rollupByFormula {
			f.program = rollupProgram(cfg.Formula).Within(r.c.budget)
		}
		return r.c.rollUp(f, r.row)
	}
	cases := []struct {
		cfg  rollupConfig
		cond int
		want interface{}
	}{
		{rollupConfig{Aggregate: rollupByFormula, Formula: "SUM(values) * 1.18"}, 0, 448.4},
		{rollupConfig{Aggregate: rollupByFormula, Formula: `ARRAYJOIN(values, " + ")`}, 0, "10 + 250 + 120"},
		{rollupConfig{Aggregate: rollupByFormula, Formula: "COUNTA(FILTER(values, CURRENT() > 100))"}, 0, float64(2)},
		{rollupConfig{Aggregate: "sum"}, 1, float64(370)},
		{rollupConfig{Aggregate: "count"}, 1, float64(2)},
		{rollupConfig{Aggregate: rollupByFormula, Formula: "MAX(values) - MIN(values)"}, 1, float64(130)},
		{rollupConfig{Aggregate: "sum"}, 0, float64(380)},
	}
	for _, c := range cases {
		if got := roll(c.cfg, c.cond).JSON(); got != c.want {
			t.Errorf("%s %q with conditions %v: %#v, want %#v", c.cfg.Aggregate, c.cfg.Formula, c.cond > 0, got, c.want)
		}
	}
	if k := kindOfField(&model.Field{Type: model.FieldRollup, Config: `{"aggregate":"formula","formula":"SUM(values)"}`}); k != formula.KindNumber {
		t.Errorf("SUM(values) gives %v", k)
	}
}

// A rollup sorts the linked rows it adds up by one of their fields, either
// way, and adds up only its first N, as Airtable's sorted rollups do: empty
// values last either way, rows that tie in link order (airtable-02).
func TestRollupsSortAndTakeTheirFirst(t *testing.T) {
	r := newLinkRead(4, 4, 4)
	r.c.loc = time.UTC
	name := &model.Field{Id: uuid.New(), Name: "Name", Type: model.FieldText}
	amount := &model.Field{Id: uuid.New(), Name: "Amount", Type: model.FieldNumber}
	due := &model.Field{Id: uuid.New(), Name: "Due", Type: model.FieldDate}
	files := &model.Field{Id: uuid.New(), Name: "Files", Type: model.FieldAttachment}
	day := func(d int) formula.Value { return formula.Date(time.Date(2026, 10, d, 0, 0, 0, 0, time.UTC), true) }
	// In link order: Bravo, alpha (no date), Charlie, and one with no name.
	roll := rollupOver(t, r, []*model.Field{name, amount, due, files}, []map[string]formula.Value{
		{"Name": formula.Text("Bravo"), "Amount": formula.Number(10), "Due": day(3)},
		{"Name": formula.Text("alpha"), "Amount": formula.Number(250)},
		{"Name": formula.Text("Charlie"), "Amount": formula.Number(120), "Due": day(1)},
		{"Amount": formula.Number(40), "Due": day(2)},
	})
	cases := []struct {
		cfg  string
		want interface{}
	}{
		{fmt.Sprintf(`{"aggregate":"list","field":%q,"sort":%q}`, name.Id, name.Id), "alpha, Bravo, Charlie"},
		{fmt.Sprintf(`{"aggregate":"list","field":%q,"sort":%q,"desc":true,"limit":2}`, name.Id, amount.Id), "alpha, Charlie"},
		{fmt.Sprintf(`{"aggregate":"sum","field":%q,"sort":%q,"limit":2}`, amount.Id, due.Id), float64(160)},
		{fmt.Sprintf(`{"aggregate":"sum","field":%q,"sort":%q,"desc":true,"limit":2}`, amount.Id, due.Id), float64(50)},
		{fmt.Sprintf(`{"aggregate":"list","field":%q,"limit":2}`, name.Id), "Bravo, alpha"},
		{`{"aggregate":"count","limit":3}`, float64(3)},
		{`{"aggregate":"count","limit":10}`, float64(4)},
	}
	for _, c := range cases {
		if got, why := roll(c.cfg); got != c.want {
			t.Errorf("%s: %#v (%s), want %#v", c.cfg, got, why, c.want)
		}
	}
	for cfg, want := range map[string]string{
		fmt.Sprintf(`{"aggregate":"sum","field":%q,"sort":%q}`, amount.Id, uuid.New()): "The field this rollup sorts by has been deleted",
		fmt.Sprintf(`{"aggregate":"sum","field":%q,"sort":%q}`, amount.Id, files.Id):   `A rollup can't sort by "Files": sort by a number, a date, text, a choice or a tick`,
	} {
		if _, why := roll(cfg); why != want {
			t.Errorf("%s: %q, want %q", cfg, why, want)
		}
	}
}

// A rollup sorts its linked rows in the tables' one order, as a view sorted
// by the field shows them (RowQuery): a choice by its options' order, then
// its label; a numbered text as people count, "PO 9" before "PO 10"; empty
// values last either way. A field a view can't sort by, a list of choices,
// is refused (airtable-02, CR-77 MF1).
func TestRollupsSortAsTheTablesSort(t *testing.T) {
	r := newLinkRead(4, 4, 4)
	r.c.loc = time.UTC
	name := &model.Field{Id: uuid.New(), Name: "Name", Type: model.FieldText}
	priority := &model.Field{Id: uuid.New(), Name: "Priority", Type: model.FieldSelect, Config: `{"options":[{"id":"h","label":"High"},{"id":"m","label":"Medium"},{"id":"l","label":"Low"}]}`}
	ref := &model.Field{Id: uuid.New(), Name: "Ref", Type: model.FieldText}
	tags := &model.Field{Id: uuid.New(), Name: "Tags", Type: model.FieldMultiSelect, Config: `{"options":[{"id":"r","label":"Rush"}]}`}
	// In link order: A (Low, PO 10), B (High, PO 9), C (Medium, PO 100), D (no priority, PO 1).
	roll := rollupOver(t, r, []*model.Field{name, priority, ref, tags}, []map[string]formula.Value{
		{"Name": formula.Text("A"), "Priority": formula.Text("Low"), "Ref": formula.Text("PO 10")},
		{"Name": formula.Text("B"), "Priority": formula.Text("High"), "Ref": formula.Text("PO 9")},
		{"Name": formula.Text("C"), "Priority": formula.Text("Medium"), "Ref": formula.Text("PO 100")},
		{"Name": formula.Text("D"), "Ref": formula.Text("PO 1")},
	})
	names := func(by *model.Field, more string) string {
		return fmt.Sprintf(`{"aggregate":"list","field":%q,"sort":%q%s}`, name.Id, by.Id, more)
	}
	for cfg, want := range map[string]interface{}{
		names(priority, ""):             "B, C, A, D",
		names(priority, `,"desc":true`): "A, C, B, D",
		names(priority, `,"limit":2`):   "B, C",
		names(ref, ""):                  "D, B, A, C",
		names(ref, `,"desc":true`):      "C, A, B, D",
	} {
		if got, why := roll(cfg); got != want {
			t.Errorf("%s: %#v (%s), want %#v", cfg, got, why, want)
		}
	}
	if _, why := roll(names(tags, "")); why != `A rollup can't sort by "Tags": sort by a number, a date, text, a choice or a tick` {
		t.Errorf("a sort by a list of choices: %q", why)
	}
}

// A count of its first N is N for a row with more links than that, however
// many of them the read fetched: a cell fetches only the links it shows
// (maxShownLinks) when nothing adds up their rows, and a count needs no more.
func TestRollupCountsItsFirstNPastTheLinksFetched(t *testing.T) {
	r := newLinkRead(150, maxShownLinks, maxShownLinks)
	roll := rollupOver(t, r, []*model.Field{{Id: uuid.New(), Name: "Name", Type: model.FieldText}}, nil)
	for cfg, want := range map[string]interface{}{
		`{"aggregate":"count","limit":5}`:   float64(5),
		`{"aggregate":"count","limit":120}`: float64(120),
		`{"aggregate":"count","limit":200}`: float64(150),
		`{"aggregate":"count"}`:             float64(150),
	} {
		if got, why := roll(cfg); got != want {
			t.Errorf("%s: %#v (%s), want %#v", cfg, got, why, want)
		}
	}
}

// rollupOver is a rollup over r's linked rows, of a table with fields, whose
// cells by field name are cells, in link order: what rollUp gives for a
// config, or why checkRollup refuses it.
func rollupOver(t *testing.T, r *linkRead, fields []*model.Field, cells []map[string]formula.Value) func(raw string) (interface{}, string) {
	t.Helper()
	r.t.fields = fields
	vendor := &model.Field{Id: r.l.field.Id, Name: "Vendor", Type: model.FieldRelation, Config: `{"relation_target":"table","table_id":"` + r.t.table.Id.String() + `"}`}
	return func(raw string) (interface{}, string) {
		var cfg rollupConfig
		if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
			t.Fatal(err)
		}
		cfg.Relation = vendor.Id.String()
		f := &rollupField{field: &model.Field{Id: uuid.New()}, cfg: cfg}
		if why := r.c.checkRollup(context.Background(), f, map[string]*model.Field{vendor.Id.String(): vendor}); why != "" {
			return nil, why
		}
		// Each linked row's values, as the read works them out for the fields
		// it reads, and its place in each sort, as load reads it (raw).
		r.t.readySorts(nil)
		for i, id := range r.of {
			lr := r.t.rows[id]
			if lr == nil {
				continue
			}
			lr.values = make([]formula.Value, len(r.t.reads))
			raw := map[string]interface{}{}
			for j, read := range r.t.reads {
				if i < len(cells) {
					lr.values[j] = cells[i][read.Name]
				}
				raw[read.Id.String()] = lr.values[j].JSON()
			}
			r.t.key(lr, &model.Row{Id: id}, raw)
		}
		return r.c.rollUp(f, r.row).JSON(), ""
	}
}

func TestRollupSettingsAreChecked(t *testing.T) {
	theirs := []*model.Field{
		{Id: uuid.New(), Name: "Cost", Type: model.FieldNumber},
		{Id: uuid.New(), Name: "Status", Type: model.FieldSelect},
		{Id: uuid.New(), Name: "Vendor", Type: model.FieldRelation, Config: `{"relation_target":"table","table_id":"` + uuid.NewString() + `"}`},
	}
	where, err := rollupWhere([]interface{}{map[string]interface{}{"field": "status", "op": "EQ", "value": "Open"}}, theirs)
	if err != nil || len(where) != 1 || where[0].Field != theirs[1].Id.String() || where[0].Op != "eq" {
		t.Errorf("a condition by name is stored by id: %#v %v", where, err)
	}
	bad := map[string]interface{}{
		"a field that isn't there": []interface{}{map[string]interface{}{"field": "Owner", "op": "eq"}},
		"a link":                   []interface{}{map[string]interface{}{"field": "Vendor", "op": "not_empty"}},
		"an odd comparison":        []interface{}{map[string]interface{}{"field": "Cost", "op": "near"}},
		"not a list":               "Cost > 5",
	}
	for what, raw := range bad {
		if _, err := rollupWhere(raw, theirs); err == nil {
			t.Errorf("%s was taken as a condition", what)
		}
	}
	if err := checkRollupFormula("SUM(values"); err == nil {
		t.Error("an unclosed formula was taken")
	}
	if err := checkRollupFormula("{Cost} * 2"); err == nil || !strings.Contains(err.Error(), "Cost") {
		t.Errorf("a formula naming a field it can't read: %v", err)
	}
	formats := map[string]bool{
		`{"currency":"INR","decimals":2}`: true,
		`{"percent":true,"bar":true}`:     true,
		`{"duration":"h:mm"}`:             true,
		`{"date":"relative"}`:             true,
		`{"currency":"EUR"}`:              false,
		`{"decimals":9}`:                  false,
		`{"colour":"red"}`:                false,
	}
	for raw, ok := range formats {
		var m map[string]interface{}
		_ = json.Unmarshal([]byte(raw), &m)
		if _, err := resultFormat(m); (err == nil) != ok {
			t.Errorf("format %s: %v", raw, err)
		}
	}
}

// A format from a Go caller (an AI tool, a template) reads as from JSON.
func TestAResultFormatFromGo(t *testing.T) {
	got, err := resultFormat(map[string]interface{}{"decimals": 2, "currency": "INR"})
	if err != nil || got["decimals"] != float64(2) {
		t.Errorf("%v %v", got, err)
	}
}
