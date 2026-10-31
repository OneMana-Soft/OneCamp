//go:build integration

package business_test

// A set's summaries (QUERY_STACK_CONTRACT.md item 8): what a grid's summary
// bar shows under each column, worked out on the server over every row a spec
// keeps, never over the page a reader has. On Postgres 12, by the one query
// over the cell index and by the evaluator (a table whose index isn't whole
// yet) alike.

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
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

func TestRowQuerySummarisesEveryRowItKeeps(t *testing.T) {
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
	tb, err := dataTable.CreateTable(ctx, dataTable.TableInput{Name: "Budget", Visibility: model.VisibilityPrivate}, owner)
	if err != nil {
		t.Fatal(err)
	}
	add := func(name, typ string, cfg map[string]interface{}) *model.Field {
		f, err := dataTable.CreateField(ctx, tb.Id, dataTable.FieldInput{Name: name, Type: typ, Config: cfg}, owner)
		if err != nil {
			t.Fatalf("field %s: %v", name, err)
		}
		return f
	}
	item := add("Item", model.FieldText, nil)
	amount := add("Amount", model.FieldNumber, nil)
	due := add("Due", model.FieldDate, nil)
	paid := add("Paid", model.FieldCheckbox, nil)
	stage := add("Stage", model.FieldStatus, map[string]interface{}{"options": []interface{}{
		map[string]interface{}{"label": "New", "group": "todo"}, map[string]interface{}{"label": "Doing", "group": "doing"}, map[string]interface{}{"label": "Done", "group": "done"}}})
	tags := add("Tags", model.FieldMultiSelect, map[string]interface{}{"options": []interface{}{
		map[string]interface{}{"label": "hot"}, map[string]interface{}{"label": "big"}}})
	made := add("Made", model.FieldCreatedTime, nil)
	maker := add("Maker", model.FieldCreatedBy, nil)
	f := func(fl *model.Field) string { return "field:" + fl.Id.String() }

	// Six rows, written straight in. Row 4 holds what isn't a value of its
	// field: blank text, a word for a number and a date, "true" for a tick, a
	// stage no option has and no tags.
	type cells map[*model.Field]interface{}
	rows := []cells{
		{item: "Booth", amount: 120.0, due: "2026-10-01", paid: true, stage: "New", tags: []interface{}{"hot"}},
		{item: "Banners", amount: 30.5, due: "2026-10-18T23:30:00Z", paid: false, stage: "Doing", tags: []interface{}{"hot", "big"}},
		{item: "Cables", paid: true, stage: "Done"},
		{item: "", amount: "lots", due: "soon", paid: "true", stage: "Lost", tags: []interface{}{}},
		{item: "Stand", amount: 0.0, due: "2026-09-30", tags: []interface{}{"big", "hot"}},
		{item: "booth ", amount: -50.0, due: "2026-11-02T08:00:00+05:30", paid: true, stage: "Done", tags: []interface{}{"hot"}},
	}
	for i, r := range rows {
		vals := map[string]interface{}{}
		for fl, v := range r {
			vals[fl.Id.String()] = v
		}
		b, _ := json.Marshal(vals)
		by := me
		if i >= 4 {
			by = other
		}
		at := time.Date(2026, 10, 1+i, 9, 0, 0, 0, time.UTC)
		if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `INSERT INTO data_table_rows (id, table_id, "values", position, position_key, row_no, created_by, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)`, uuid.New(), tb.Id, string(b), float64(i), "r"+string(rune('a'+i)), i+1, by, at); err != nil {
			t.Fatalf("row %d: %v", i+1, err)
		}
	}

	all := []query.SummaryRequest{
		{Field: f(item), Fns: []string{"count", "empty", "filled", "pct_empty", "pct_filled", "unique"}},
		{Field: f(amount), Fns: []string{"sum", "average", "median", "min", "max", "range", "empty"}},
		{Field: f(due), Fns: []string{"earliest", "latest", "range", "filled"}},
		{Field: f(paid), Fns: []string{"checked", "unchecked"}},
		{Field: f(stage), Fns: []string{"filled", "unique"}},
		{Field: f(tags), Fns: []string{"filled", "empty"}},
		{Field: f(made), Fns: []string{"earliest", "latest"}},
		{Field: f(maker), Fns: []string{"unique", "filled"}},
	}
	// "booth " is Booth again, as a filter reads text; "Lost" is a stage
	// of its own; a moment is written at UTC, a day as its date.
	want := map[string]string{
		f(item):   `{"count":6,"empty":1,"filled":5,"pct_empty":16.666666666666668,"pct_filled":83.33333333333333,"unique":4}`,
		f(amount): `{"sum":100.5,"average":25.125,"median":15.25,"min":-50,"max":120,"range":170,"empty":2}`,
		f(due):    `{"earliest":"2026-09-30","latest":"2026-11-02T02:30:00Z","range":["2026-09-30","2026-11-02T02:30:00Z"],"filled":4}`,
		f(paid):   `{"checked":3,"unchecked":3}`,
		f(stage):  `{"filled":5,"unique":4}`,
		f(tags):   `{"filled":4,"empty":2}`,
		f(made):   `{"earliest":"2026-10-01T09:00:00Z","latest":"2026-10-06T09:00:00Z"}`,
		f(maker):  `{"unique":2,"filled":6}`,
	}
	zone, _ := time.LoadLocation("Asia/Kolkata")
	qctx := rowQuery.WithZone(ctx, zone)
	// Summaries come beside a first window that counts: the request a reader
	// sends beside its first window, so the rows never wait on them.
	cctx := rowQuery.WithCount(qctx)
	mine := accessBusiness.Subject{UserUUID: me.String()}
	spec := func(filter *query.Filter, sums []query.SummaryRequest) query.Spec {
		return query.Spec{Scope: query.Scope{Kind: query.ScopeTable, ID: tb.Id.String()}, Filter: filter, Summaries: sums}
	}
	same := func(leg string, got query.Summaries, want map[string]string) {
		t.Helper()
		if len(got) != len(want) {
			t.Errorf("%s: summaries of %d fields, want %d: %v", leg, len(got), len(want), got)
		}
		for field, w := range want {
			var wantFns, gotFns map[string]interface{}
			if err := json.Unmarshal([]byte(w), &wantFns); err != nil {
				t.Fatal(err)
			}
			b, _ := json.Marshal(got[field])
			_ = json.Unmarshal(b, &gotFns)
			if !reflect.DeepEqual(gotFns, wantFns) {
				t.Errorf("%s, %s:\n got %s\nwant %s", leg, field, b, w)
			}
		}
	}

	// The evaluator, while the table's index isn't whole.
	if err := model.MarkCellIndexStale(ctx, tb.Id); err != nil {
		t.Fatal(err)
	}
	if whole, _, err := model.CellIndexState(ctx, tb.Id); err != nil || whole {
		t.Fatalf("the index should be waiting for the sweep: whole=%v, %v", whole, err)
	}
	res, err := query.Queries.Run(cctx, mine, spec(nil, all), query.Page{Size: 2})
	if err != nil {
		t.Fatalf("by the evaluator: %v", err)
	}
	if len(res.IDs) != 2 || res.Next == "" {
		t.Fatalf("by the evaluator: a first window of %d rows, next %q", len(res.IDs), res.Next)
	}
	same("by the evaluator", res.Summaries, want)

	// The one query, once the sweep has made the index whole.
	if err := rowQuery.IndexTable(ctx, tb.Id); err != nil {
		t.Fatalf("index: %v", err)
	}
	res, err = query.Queries.Run(cctx, mine, spec(nil, all), query.Page{Size: 2})
	if err != nil {
		t.Fatalf("by the index: %v", err)
	}
	if len(res.IDs) != 2 || res.Next == "" {
		t.Fatalf("by the index: a first window of %d rows, next %q", len(res.IDs), res.Next)
	}
	same("by the index", res.Summaries, want)

	// A first window that doesn't count answers its rows alone: they never
	// wait on the summaries.
	if res, err := query.Queries.Run(qctx, mine, spec(nil, all), query.Page{Size: 2}); err != nil || len(res.IDs) != 2 || res.Summaries != nil {
		t.Errorf("a first window that doesn't count: %v, %d rows, summaries %v; want 2 rows and none", err, len(res.IDs), res.Summaries)
	}

	// A summaries read that fails leaves them out, never the window or its
	// count: the bar keeps what it shows.
	func() {
		defer rowQuery.FailSummaries()()
		res, err := query.Queries.Run(cctx, mine, spec(nil, all), query.Page{Size: 2})
		if err != nil || len(res.IDs) != 2 || res.Total != 6 || res.Summaries != nil {
			t.Errorf("summaries that fail: %v, %d rows of %d, summaries %v; want 2 rows of 6 and none", err, len(res.IDs), res.Total, res.Summaries)
		}
	}()

	// The windows after the first carry none: a reader keeps the first's.
	next, err := query.Queries.Run(qctx, mine, spec(nil, all), query.Page{Size: 2, After: res.Next})
	if err != nil {
		t.Fatal(err)
	}
	if len(next.IDs) != 2 || next.Summaries != nil {
		t.Errorf("a second window: %d rows, summaries %v; want 2 rows and none", len(next.IDs), next.Summaries)
	}

	// Over the rows a filter keeps, not the table: the three paid rows.
	ticked := &query.Filter{Field: f(paid), Operator: query.OperatorChecked}
	res, err = query.Queries.Run(cctx, mine, spec(ticked, all[:2]), query.Page{Size: 50})
	if err != nil {
		t.Fatal(err)
	}
	same("the paid rows", res.Summaries, map[string]string{
		f(item):   `{"count":3,"empty":0,"filled":3,"pct_empty":0,"pct_filled":100,"unique":2}`,
		f(amount): `{"sum":70,"average":35,"median":35,"min":-50,"max":120,"range":170,"empty":1}`,
	})

	// Refused: a function a field's kind hasn't got, the different sets of
	// a many-valued field (combinations come with its groups), a field the
	// table hasn't got.
	for _, bad := range []query.SummaryRequest{
		{Field: f(amount), Fns: []string{"checked"}},
		{Field: f(item), Fns: []string{"sum"}},
		{Field: f(paid), Fns: []string{"earliest"}},
		{Field: f(tags), Fns: []string{"unique"}},
		{Field: "field:" + uuid.NewString(), Fns: []string{"count"}},
	} {
		if res, err := query.Queries.Run(cctx, mine, spec(nil, []query.SummaryRequest{bad}), query.Page{Size: 2}); err == nil {
			t.Errorf("%s %v: answered %v, want it refused", bad.Field, bad.Fns, res.Summaries)
		}
	}
	if _, err := query.Queries.Run(cctx, mine, spec(nil, []query.SummaryRequest{{Field: f(amount), Fns: []string{"checked"}}}), query.Page{Size: 2}); !errors.Is(err, rowQuery.ErrCantQuery) {
		t.Errorf("a number's ticks: %v, want ErrCantQuery", err)
	}
	// A field asked twice is refused, not answered by whichever ask came last.
	twice := []query.SummaryRequest{{Field: f(amount), Fns: []string{"sum"}}, {Field: f(amount), Fns: []string{"max"}}}
	if _, err := query.Queries.Run(cctx, mine, spec(nil, twice), query.Page{Size: 2}); !errors.Is(err, rowQuery.ErrCantQuery) {
		t.Errorf("a field asked twice: %v, want ErrCantQuery", err)
	}

	// A member who can't see the private table learns nothing of it: not
	// its rows, and not one of its sums.
	res, err = query.Queries.Run(cctx, accessBusiness.Subject{UserUUID: other.String()}, spec(nil, all), query.Page{Size: 2})
	if !errors.Is(err, rowQuery.ErrNotFound) || res.Summaries != nil || len(res.IDs) != 0 {
		t.Errorf("a member without the table: %v, summaries %v, %d rows; want ErrNotFound and nothing", err, res.Summaries, len(res.IDs))
	}
}
