//go:build integration

package business_test

// A spec's groups (QUERY_STACK_CONTRACT.md item 7): its rows in the order of
// its group levels, ahead of its sorts, with every group's count and
// summaries at every level, over every row the spec keeps; the reader's
// collapsed groups are left out of the windows, never out of the counts. On
// Postgres 12, by the evaluator (a table whose index isn't whole yet) and by
// the one query over the cell index alike.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	dataTable "github.com/akashc777/OneCamp/business/DataTable"
	query "github.com/akashc777/OneCamp/business/QuerySpec"
	rowQuery "github.com/akashc777/OneCamp/business/RowQuery"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestRowQueryGroupsEveryRowItKeeps(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("connect: %v", err)
	}
	statementsGetAMinute(t)
	integration.SetupDgraph(t)
	user := func(name string) uuid.UUID {
		id := uuid.New()
		if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, name+"@example.test", name, name); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
		return id
	}
	me, other := user("owner"), user("member")
	owner := dataTable.Actor{UserID: me}
	newTable := func(name string) *model.DataTable {
		tb, err := dataTable.CreateTable(ctx, dataTable.TableInput{Name: name, Visibility: model.VisibilityPrivate}, owner)
		if err != nil {
			t.Fatal(err)
		}
		return tb
	}
	tb := newTable("Launch")
	add := func(tb *model.DataTable, name, typ string, cfg map[string]interface{}) *model.Field {
		f, err := dataTable.CreateField(ctx, tb.Id, dataTable.FieldInput{Name: name, Type: typ, Config: cfg}, owner)
		if err != nil {
			t.Fatalf("field %s: %v", name, err)
		}
		return f
	}
	item := add(tb, "Item", model.FieldText, nil)
	amount := add(tb, "Amount", model.FieldNumber, nil)
	due := add(tb, "Due", model.FieldDate, nil)
	paid := add(tb, "Paid", model.FieldCheckbox, nil)
	stage := add(tb, "Stage", model.FieldStatus, map[string]interface{}{"options": []interface{}{
		map[string]interface{}{"label": "New", "group": "todo"}, map[string]interface{}{"label": "Doing", "group": "doing"}, map[string]interface{}{"label": "Done", "group": "done"}}})
	tags := add(tb, "Tags", model.FieldMultiSelect, map[string]interface{}{"options": []interface{}{map[string]interface{}{"label": "hot"}}})
	made := add(tb, "Made", model.FieldCreatedTime, nil)
	f := func(fl *model.Field) string { return "field:" + fl.Id.String() }

	// Eight rows, written straight in. "done" is the option Done; "STRASSE"
	// and "Straße" are stages no option has, which sort alike and are two
	// groups; "true" isn't a tick, "lots" isn't a number and "soon" isn't a
	// date. In Asia/Kolkata, Banners is due on Sunday 1 November and Stand
	// on Monday 19 October.
	type cells map[*model.Field]interface{}
	rows := []cells{
		{item: "Booth", amount: 120.0, due: "2026-10-31", paid: true, stage: "New"},
		{item: "Banners", amount: 30.5, due: "2026-10-31T20:00:00Z", paid: false, stage: "Doing"},
		{item: "Cables", due: "2026-10-18", paid: true, stage: "done"},
		{item: "Stand", amount: 0.0, due: "2026-10-18T20:00:00Z", paid: "true", stage: "STRASSE"},
		{item: "Lights", amount: -50.0, due: "soon", stage: "Straße"},
		{item: "Chairs", amount: 120.0, paid: false, stage: "New"},
		{item: "Tables", amount: "lots", due: "2026-11-02", paid: true},
		{item: "Signs", amount: 30.5, due: "2026-10-19", paid: false, stage: "Doing"},
	}
	names := map[string]string{}
	insert := func(tb *model.DataTable, i int, vals map[string]interface{}) uuid.UUID {
		b, _ := json.Marshal(vals)
		id := uuid.New()
		at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC).Add(time.Duration(i) * 24 * time.Hour)
		if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `INSERT INTO data_table_rows (id, table_id, "values", position, position_key, row_no, created_by, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)`, id, tb.Id, string(b), float64(i), fmt.Sprintf("r%05d", i), i+1, me, at); err != nil {
			t.Fatalf("row %d: %v", i+1, err)
		}
		return id
	}
	for i, r := range rows {
		vals := map[string]interface{}{}
		for fl, v := range r {
			vals[fl.Id.String()] = v
		}
		names[insert(tb, i, vals).String()] = r[item].(string)
	}
	// A table of 1,001 different amounts: one group too many.
	many := newTable("Many")
	price := add(many, "Price", model.FieldNumber, nil)
	for i := 0; i <= 1000; i++ {
		insert(many, i, map[string]interface{}{price.Id.String(): float64(i)})
	}

	zone, _ := time.LoadLocation("Asia/Kolkata")
	qctx := rowQuery.WithZone(ctx, zone)
	cctx := rowQuery.WithCount(qctx)
	mine := accessBusiness.Subject{UserUUID: me.String()}
	spec := func(tb *model.DataTable, groups ...query.Group) query.Spec {
		return query.Spec{Scope: query.Scope{Kind: query.ScopeTable, ID: tb.Id.String()}, Groups: groups}
	}
	by := func(fl *model.Field, desc bool, granularity string, collapsed ...string) query.Group {
		return query.Group{Field: f(fl), Desc: desc, Granularity: granularity, Collapsed: collapsed}
	}
	// groupsOf writes groups as "key/key"=count, in order.
	groupsOf := func(gs []query.GroupCount) string {
		parts := make([]string, len(gs))
		for i, g := range gs {
			parts[i] = fmt.Sprintf("%q=%d", strings.Join(g.Key, "/"), g.Count)
		}
		return strings.Join(parts, ", ")
	}
	// read pages through a spec in windows of three: the rows the reader's
	// windows show, by item, and the first window's answer.
	read := func(s query.Spec) (string, query.Result, error) {
		var shown []string
		var first query.Result
		var cursor query.Cursor
		for page := 0; page < 10; page++ {
			at := qctx
			if cursor == "" {
				at = cctx
			}
			res, err := query.Queries.Run(at, mine, s, query.Page{Size: 3, After: cursor})
			if err != nil {
				return "", res, err
			}
			if cursor == "" {
				first = res
			} else if res.Groups != nil {
				return "", res, fmt.Errorf("window %d answered groups %v: only the first does", page+1, res.Groups)
			}
			for _, id := range res.IDs {
				shown = append(shown, names[id])
			}
			if cursor = res.Next; cursor == "" {
				break
			}
		}
		return strings.Join(shown, " "), first, nil
	}

	twoLevels := `"New"=2, "New/false"=1, "New/true"=1, "Doing"=2, "Doing/false"=2, "Done"=1, "Done/true"=1, "strasse"=1, "strasse/false"=1, "straße"=1, "straße/false"=1, ""=1, "/true"=1`
	legs := []struct {
		name   string
		spec   query.Spec
		shown  string
		groups string
	}{
		{"by stage", spec(tb, by(stage, false, "")), "Booth Chairs Banners Signs Cables Stand Lights Tables",
			`"New"=2, "Doing"=2, "Done"=1, "strasse"=1, "straße"=1, ""=1`},
		{"by stage, backwards", spec(tb, by(stage, true, "")), "Lights Stand Cables Banners Signs Booth Chairs Tables",
			`"straße"=1, "strasse"=1, "Done"=1, "Doing"=2, "New"=2, ""=1`},
		{"by stage, then paid", spec(tb, by(stage, false, ""), by(paid, false, "")), "Chairs Booth Banners Signs Cables Stand Lights Tables", twoLevels},
		{"by paid", spec(tb, by(paid, false, "")), "Banners Stand Lights Chairs Signs Booth Cables Tables", `"false"=5, "true"=3`},
		{"by paid, backwards", spec(tb, by(paid, true, "")), "Booth Cables Tables Banners Stand Lights Chairs Signs", `"true"=3, "false"=5`},
		{"by amount", spec(tb, by(amount, false, "")), "Lights Stand Banners Signs Booth Chairs Cables Tables",
			`"-50"=1, "0"=1, "30.5"=2, "120"=2, ""=2`},
		{"by the month due", spec(tb, by(due, false, query.GranularityMonth)), "Booth Cables Stand Signs Banners Tables Lights Chairs",
			`"2026-10-01"=4, "2026-11-01"=2, ""=2`},
		{"by the week due", spec(tb, by(due, false, query.GranularityWeek)), "Cables Stand Signs Booth Banners Tables Lights Chairs",
			`"2026-10-12"=1, "2026-10-19"=2, "2026-10-26"=2, "2026-11-02"=1, ""=2`},
		{"by the day due", spec(tb, by(due, false, "")), "Cables Stand Signs Booth Banners Tables Lights Chairs",
			`"2026-10-18"=1, "2026-10-19"=2, "2026-10-31"=1, "2026-11-01"=1, "2026-11-02"=1, ""=2`},
		{"by the quarter due, backwards", spec(tb, by(due, true, query.GranularityQuarter)), "Booth Banners Cables Stand Tables Signs Lights Chairs",
			`"2026-10-01"=6, ""=2`},
		{"by the month made", spec(tb, by(made, false, query.GranularityMonth)), "Booth Banners Cables Stand Lights Chairs Tables Signs", `"2026-10-01"=8`},
		// Collapsed groups leave the windows, never the counts.
		{"Doing collapsed", spec(tb, by(stage, false, "", `["Doing"]`)), "Booth Chairs Cables Stand Lights Tables",
			`"New"=2, "Doing"=2, "Done"=1, "strasse"=1, "straße"=1, ""=1`},
		{"New's ticks collapsed", spec(tb, by(stage, false, ""), by(paid, false, "", `["New","true"]`)), "Chairs Banners Signs Cables Stand Lights Tables", twoLevels},
		{"collapsed at both levels", spec(tb, by(stage, false, "", `["Doing"]`, `[""]`), by(paid, false, "", `["New","true"]`, `["straße","false"]`)), "Chairs Cables Stand", twoLevels},
		{"a group no row is in, collapsed", spec(tb, by(stage, false, "", `["Lost"]`)), "Booth Chairs Banners Signs Cables Stand Lights Tables",
			`"New"=2, "Doing"=2, "Done"=1, "strasse"=1, "straße"=1, ""=1`},
	}
	// Each group's summaries beside its count, and the whole set's.
	summed := spec(tb, by(stage, false, ""))
	summed.Summaries = []query.SummaryRequest{{Field: f(amount), Fns: []string{"sum", "filled"}}}
	wantSums := `{"Doing":{"filled":2,"sum":61},"Done":{"filled":0,"sum":null},"New":{"filled":2,"sum":240},"strasse":{"filled":1,"sum":0},"straße":{"filled":1,"sum":-50},"":{"filled":0,"sum":null},"*":{"filled":6,"sum":251}}`

	check := func(how string) {
		for _, leg := range legs {
			shown, first, err := read(leg.spec)
			if err != nil {
				t.Errorf("%s, %s: %v", how, leg.name, err)
				continue
			}
			if shown != leg.shown {
				t.Errorf("%s, %s: the windows show\n %s\nwant\n %s", how, leg.name, shown, leg.shown)
			}
			if got := groupsOf(first.Groups); got != leg.groups || first.Total != 8 {
				t.Errorf("%s, %s: %d rows in groups\n %s\nwant 8 in\n %s", how, leg.name, first.Total, got, leg.groups)
			}
		}
		_, first, err := read(summed)
		if err != nil {
			t.Fatalf("%s, summed: %v", how, err)
		}
		sums := map[string]interface{}{"*": first.Summaries[f(amount)]}
		for _, g := range first.Groups {
			sums[strings.Join(g.Key, "/")] = g.Summaries[f(amount)]
		}
		if b, _ := json.Marshal(sums); !sameJSON(string(b), wantSums) {
			t.Errorf("%s: each stage's amounts\n %s\nwant\n %s", how, b, wantSums)
		}
		// A first window that doesn't count answers its rows alone.
		if res, err := query.Queries.Run(qctx, mine, summed, query.Page{Size: 3}); err != nil || len(res.IDs) != 3 || res.Groups != nil {
			t.Errorf("%s, a first window that doesn't count: %v, %d rows, groups %v; want 3 rows and none", how, err, len(res.IDs), res.Groups)
		}
		// Refused: text and many-valued fields (by combination, later), a
		// number by a period, a field the table hasn't got, 1,001 groups.
		for name, s := range map[string]query.Spec{
			"text":            spec(tb, by(item, false, "")),
			"tags":            spec(tb, by(tags, false, "")),
			"amount by month": spec(tb, by(amount, false, query.GranularityMonth)),
			"1,001 prices":    spec(many, by(price, false, "")),
		} {
			if res, err := query.Queries.Run(cctx, mine, s, query.Page{Size: 3}); !errors.Is(err, rowQuery.ErrCantQuery) {
				t.Errorf("%s, grouped by %s: %v, %d groups; want ErrCantQuery", how, name, err, len(res.Groups))
			}
		}
		if _, err := query.Queries.Run(cctx, mine, spec(tb, query.Group{Field: "field:" + uuid.NewString()}), query.Page{Size: 3}); err == nil {
			t.Errorf("%s, grouped by a field the table hasn't got: answered", how)
		}
		// A thousand groups are answered.
		upTo := spec(many, by(price, true, ""))
		upTo.Filter = &query.Filter{Field: f(price), Operator: query.OperatorLt, Values: []query.Value{{Number: func() *float64 { n := 1000.0; return &n }()}}}
		if res, err := query.Queries.Run(cctx, mine, upTo, query.Page{Size: 3}); err != nil || len(res.Groups) != 1000 || groupsOf(res.Groups[:2]) != `"999"=1, "998"=1` {
			t.Errorf("%s, 1,000 prices: %v, %d groups; want 1,000 from 999 down", how, err, len(res.Groups))
		}
		// A collapsed group names its path from the top.
		if _, err := query.Queries.Run(cctx, mine, spec(tb, by(stage, false, "", "Doing")), query.Page{Size: 3}); !errors.Is(err, query.ErrBadSpec) {
			t.Errorf("%s, a collapsed group that isn't a path: %v, want ErrBadSpec", how, err)
		}
		// A member who can't see the private table learns nothing of it.
		res, err := query.Queries.Run(cctx, accessBusiness.Subject{UserUUID: other.String()}, summed, query.Page{Size: 3})
		if !errors.Is(err, rowQuery.ErrNotFound) || res.Groups != nil || len(res.IDs) != 0 {
			t.Errorf("%s, a member without the table: %v, groups %v, %d rows; want ErrNotFound and nothing", how, err, res.Groups, len(res.IDs))
		}
	}

	// The evaluator, while the tables' indexes aren't whole.
	for _, tb := range []*model.DataTable{tb, many} {
		if err := model.MarkCellIndexStale(ctx, tb.Id); err != nil {
			t.Fatal(err)
		}
	}
	check("by the evaluator")

	// The one query, once the sweep has made them whole.
	for _, tb := range []*model.DataTable{tb, many} {
		if err := rowQuery.IndexTable(ctx, tb.Id); err != nil {
			t.Fatalf("index: %v", err)
		}
	}
	check("by the index")

	// A groups read that fails leaves the groups and the summaries out, never
	// the window, its order or its count.
	func() {
		defer rowQuery.FailSummaries()()
		s := spec(tb, by(stage, false, ""), by(paid, false, ""))
		res, err := query.Queries.Run(cctx, mine, s, query.Page{Size: 3})
		var shown []string
		for _, id := range res.IDs {
			shown = append(shown, names[id])
		}
		if err != nil || strings.Join(shown, " ") != "Chairs Booth Banners" || res.Total != 8 || res.Groups != nil || res.Summaries != nil {
			t.Errorf("a groups read that fails: %v, %v of %d, groups %v; want Chairs Booth Banners of 8 and no groups", err, shown, res.Total, res.Groups)
		}
	}()
}

// sameJSON is whether two JSON texts hold the same values.
func sameJSON(a, b string) bool {
	var x, y interface{}
	if json.Unmarshal([]byte(a), &x) != nil || json.Unmarshal([]byte(b), &y) != nil {
		return false
	}
	xb, _ := json.Marshal(x)
	yb, _ := json.Marshal(y)
	return string(xb) == string(yb)
}
