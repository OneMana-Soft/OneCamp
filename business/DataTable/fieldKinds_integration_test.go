//go:build integration

package business

// Tables B1's kinds end to end, against Postgres 12 with every migration:
// people, a status, a phone number, a duration and a rating stored as their
// kinds store them; a row's number, when it was made and last changed and by
// whom read from the row on every read, never written; a table linking to it
// adding up when its rows were made and their numbers; and a query (the AI's
// table tools) filtering and grouping by them.
//
// Run: go test -tags=integration ./business/DataTable/ -run 'TestPeopleAndWhatARowSaysOfItself' -v

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	fieldTypes "github.com/akashc777/OneCamp/business/FieldTypes"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
)

func TestPeopleAndWhatARowSaysOfItself(t *testing.T) {
	w := newRelationWorld(t)
	ctx := w.ctx
	// People are named from the graph, which this test doesn't start: its
	// two people are named here, as the graph names them (TestTableItemTitles,
	// tests/integration, reads them from it).
	names := map[string]string{strings.ToLower(w.me.UserID.String()): "owner", strings.ToLower(w.member.UserID.String()): "member"}
	was := itemNamers[fieldTypes.ItemUser]
	itemNamers[fieldTypes.ItemUser] = func(_ context.Context, ids []string, _ *itemReader) (map[string]string, error) {
		out := map[string]string{}
		for _, id := range ids {
			if n, ok := names[strings.ToLower(id)]; ok {
				out[strings.ToLower(id)] = n
			}
		}
		return out, nil
	}
	t.Cleanup(func() { itemNamers[fieldTypes.ItemUser] = was })
	deals, dName := w.table("Deals")
	owners := w.field(deals, "Owners", model.FieldPeople, nil)
	stage := w.field(deals, "Stage", model.FieldStatus, nil)
	phone := w.field(deals, "Phone", model.FieldPhone, nil)
	took := w.field(deals, "Took", model.FieldDuration, nil)
	stars := w.field(deals, "Stars", model.FieldRating, nil)
	id := w.field(deals, "ID", model.FieldAutonumber, map[string]interface{}{"prefix": "DEAL"})
	made := w.field(deals, "Made", model.FieldCreatedTime, nil)
	madeBy := w.field(deals, "Made by", model.FieldCreatedBy, nil)
	changed := w.field(deals, "Changed", model.FieldModifiedTime, nil)
	changedBy := w.field(deals, "Changed by", model.FieldModifiedBy, nil)
	year := w.field(deals, "Year made", model.FieldFormula, map[string]interface{}{"formula": fmt.Sprintf("YEAR({#%s})", made.Id)})

	// A new status starts with three options in their groups.
	if opts := fmt.Sprint(stage.Config); !strings.Contains(opts, "Not started") || !strings.Contains(opts, `"group":"done"`) {
		t.Errorf("a new status's settings: %s", stage.Config)
	}

	before := time.Now().Add(-time.Minute)
	acme := w.row(deals, map[string]interface{}{
		dName.Id.String():  "Acme",
		owners.Id.String(): []interface{}{w.member.UserID.String()},
		stage.Id.String():  "In progress",
		phone.Id.String():  " +91 98765 43210 ",
		took.Id.String():   "1:30",
		stars.Id.String():  4,
		// A row's own fields, written: none of them is kept.
		id.Id.String():     99,
		madeBy.Id.String(): "Nobody",
		made.Id.String():   "1999-01-01",
	})
	globex := w.row(deals, map[string]interface{}{dName.Id.String(): "Globex"})

	stored, err := model.GetRowByID(ctx, deals.Id, acme)
	if err != nil {
		t.Fatal(err)
	}
	cells := parseRowValues(stored.Values)
	for _, f := range []*model.Field{id, made, madeBy, changed, changedBy} {
		if v, in := cells[f.Id.String()]; in {
			t.Errorf("%s stored as %v", f.Name, v)
		}
	}
	if cells[phone.Id.String()] != "+91 98765 43210" || cells[took.Id.String()] != float64(5400) || cells[stars.Id.String()] != float64(4) || cells[stage.Id.String()] != "In progress" {
		t.Errorf("stored as %s", stored.Values)
	}
	if got := fmt.Sprint(cells[owners.Id.String()]); !strings.Contains(got, w.member.UserID.String()) || strings.Contains(got, "label") {
		t.Errorf("people stored as %v", got)
	}

	// The member changes Globex: it says so, and still says who made it.
	if _, err := PatchRow(ctx, deals.Id, globex, map[string]interface{}{stage.Id.String(): "Done"}, w.member); err != nil {
		t.Fatalf("the member's change: %v", err)
	}
	// A patch naming only what a row says of itself names nothing it stores.
	if _, err := PatchRow(ctx, deals.Id, globex, map[string]interface{}{madeBy.Id.String(): "Nobody"}, w.me); err == nil {
		t.Error("a patch of who made a row was taken")
	}

	read := w.cells(deals, &w.me)
	a, g := read[acme], read[globex]
	if namesIn(a[owners.Id.String()]) != "member" || namesIn(a[madeBy.Id.String()]) != "owner" || namesIn(a[changedBy.Id.String()]) != "owner" {
		t.Errorf("Acme's people %v, made by %v, changed by %v", a[owners.Id.String()], a[madeBy.Id.String()], a[changedBy.Id.String()])
	}
	if namesIn(g[madeBy.Id.String()]) != "owner" || namesIn(g[changedBy.Id.String()]) != "member" {
		t.Errorf("Globex made by %v, changed by %v", g[madeBy.Id.String()], g[changedBy.Id.String()])
	}
	if a[id.Id.String()] != float64(1) || g[id.Id.String()] != float64(2) {
		t.Errorf("the rows' numbers %v and %v", a[id.Id.String()], g[id.Id.String()])
	}
	at, err := time.Parse(time.RFC3339, fmt.Sprint(a[made.Id.String()]))
	if err != nil || at.Before(before) || at.After(time.Now().Add(time.Minute)) {
		t.Errorf("Acme made at %v (%v)", a[made.Id.String()], err)
	}
	if a[year.Id.String()] != float64(at.Year()) {
		t.Errorf("the year Acme was made, by formula: %v", a[year.Id.String()])
	}
	if _, err := time.Parse(time.RFC3339, fmt.Sprint(g[changed.Id.String()])); err != nil {
		t.Errorf("Globex changed at %v", g[changed.Id.String()])
	}

	// A guest's read names people too, as a link to people's does.
	if guest := w.cells(deals, nil)[acme]; namesIn(guest[madeBy.Id.String()]) != "owner" || namesIn(guest[owners.Id.String()]) != "member" {
		t.Errorf("a guest reads made by %v and people %v", guest[madeBy.Id.String()], guest[owners.Id.String()])
	}

	// A table linking to Deals adds up when its rows were made, their
	// numbers, and a formula over when they were made.
	accounts, aName := w.table("Accounts")
	link := w.field(accounts, "Deals", model.FieldRelation, map[string]interface{}{"relation_target": "table", "table_id": deals.Id.String()})
	latest := w.field(accounts, "Latest deal", model.FieldRollup, map[string]interface{}{"relation": link.Id.String(), "field": made.Id.String(), "aggregate": "latest"})
	highest := w.field(accounts, "Highest ID", model.FieldRollup, map[string]interface{}{"relation": link.Id.String(), "field": id.Id.String(), "aggregate": "max"})
	years := w.field(accounts, "Years", model.FieldRollup, map[string]interface{}{"relation": link.Id.String(), "field": year.Id.String(), "aggregate": "max"})
	if _, err := CreateField(ctx, accounts.Id, FieldInput{Name: "Who", Type: model.FieldRollup, Config: map[string]interface{}{"relation": link.Id.String(), "field": madeBy.Id.String(), "aggregate": "unique"}}, w.me); err == nil {
		t.Error("a rollup of who made the linked rows was made")
	}
	north := w.row(accounts, map[string]interface{}{aName.Id.String(): "North", link.Id.String(): []interface{}{acme.String(), globex.String()}})
	n := w.cells(accounts, &w.me)[north]
	if n[highest.Id.String()] != float64(2) || n[years.Id.String()] != float64(at.Year()) {
		t.Errorf("North's highest ID %v, years %v", n[highest.Id.String()], n[years.Id.String()])
	}
	if s := fmt.Sprint(n[latest.Id.String()]); !strings.HasPrefix(s, at.UTC().Format("2006-01-02")) && !strings.HasPrefix(s, at.Format("2006-01-02")) {
		t.Errorf("North's latest deal %v", n[latest.Id.String()])
	}

	// A query filters and groups by them: nothing stored to find, so the rows
	// it reads aren't narrowed by them.
	today := at.UTC().Format(time.DateOnly)
	res, _, err := AggregateTable(ctx, deals.Id, w.me, QuerySpec{GroupBy: "Changed by", Op: OpCount, Filters: []Filter{{Field: "Made", Op: "eq", Value: today}, {Field: "Made by", Op: "contains", Value: "own"}}})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, b := range res.Buckets {
		got = append(got, fmt.Sprintf("%s:%d", b.Label, b.Count))
	}
	if strings.Join(got, ",") != "member:1,owner:1" && strings.Join(got, ",") != "owner:1,member:1" {
		t.Errorf("deals made today by the owner, by who changed them last: %v (matched %d)", got, res.MatchedRows)
	}
	res, _, err = AggregateTable(ctx, deals.Id, w.me, QuerySpec{GroupBy: "Made", Op: OpCount})
	if err != nil || len(res.Buckets) != 1 || res.Buckets[0].Label != today || res.GroupByType != model.FieldDate {
		t.Errorf("deals by the day made: %+v (%v)", res, err)
	}
}

// namesIn is a people or link cell's names, in order, "" for each it doesn't
// name (labels, beside, fails the test's run on one).
func namesIn(cell interface{}) string {
	refs, _ := cell.([]interface{})
	var out []string
	for _, r := range refs {
		m, _ := r.(map[string]interface{})
		s, _ := m["label"].(string)
		out = append(out, s)
	}
	return strings.Join(out, ", ")
}
