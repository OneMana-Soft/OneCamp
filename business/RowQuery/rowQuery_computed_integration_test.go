//go:build integration

package business_test

// airtable-03 (main, 29 Oct: airtable-02 stays PAR until a view sorts and
// filters by a rollup or a formula at 100,000 rows, served from the Computed
// Store). A 100,000-row table sorts by a rollup of another table's numbers
// and by a formula of its own cells, and filters by them, a window at a time,
// along the cell index as a stored field's sort does: the right rows, in
// order, with no read of a table through and no more index entries per row
// shown than a stored field's window may read (entriesPerRow). A filter that
// keeps few rows, with a sort by another field, reads what the same window
// over stored fields reads (main, 29 Oct): the CONTROL leg measures that
// window over stored copies of the same numbers, in the same run, and its
// computed twin may read no more. Until the store holds them the server
// works computed values out on each read and refuses past 10,000 rows
// (ErrTooManyToWorkOut).
//
// Per reader at every level (main's security bar): a stored value answers a
// reader only when they open every table it reaches, so it is what their own
// read works out. A member who can't open Ledger is never answered by Paid
// (a rollup of a rollup of Ledger) or Owed (a rollup of Ledger): no order,
// row, count, group or sum by them, through the query or the API's window;
// past what one read works out as them, they are refused. What reaches only
// tables they open answers them as it answers the owner, whatever else their
// table links to: Projects links straight to Ledger, and Double (twice the
// row's own Budget) and Total (a rollup of Vendors) still serve the member,
// alone and together, at the gate, in a sort and in a sum over HTTP. With
// Ledger in the trash nobody opens it, so nothing stored from it answers
// anyone.
//
// Run: integ_rel.sh <worktree> <log> '^(TestComputedFieldsSortAndFilterAt100k|TestAFormulaOverLinkedItemsIsWorkedOutForItsReader)$' with PKGS='./business/RowQuery/'

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	computedStore "github.com/akashc777/OneCamp/business/ComputedStore"
	dataTable "github.com/akashc777/OneCamp/business/DataTable"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	fieldTypes "github.com/akashc777/OneCamp/business/FieldTypes"
	query "github.com/akashc777/OneCamp/business/QuerySpec"
	rowQuery "github.com/akashc777/OneCamp/business/RowQuery"
	tableAccess "github.com/akashc777/OneCamp/business/TableAccess"
	rowQueryController "github.com/akashc777/OneCamp/controllers/RowQuery"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

const (
	computedProjects = 100000
	computedVendors  = 1000
)

// fill writes n rows into a table at once, as an import does: each row's
// cells from cells(i), its id from ids[i].
func fill(ctx context.Context, t *testing.T, table uuid.UUID, me uuid.UUID, ids []uuid.UUID, cells func(i int) map[string]interface{}) {
	t.Helper()
	for i := 0; i < len(ids); i += 500 {
		var sb strings.Builder
		args := []interface{}{table}
		sb.WriteString(`INSERT INTO data_table_rows (id, table_id, "values", position, position_key, row_no, created_by, created_at, updated_at) VALUES `)
		for j := i; j < i+500 && j < len(ids); j++ {
			b, _ := json.Marshal(cells(j))
			if j > i {
				sb.WriteString(", ")
			}
			n := len(args)
			fmt.Fprintf(&sb, "($%d, $1, $%d::jsonb, $%d, $%d, $%d, $%d, NOW(), NOW())", n+1, n+2, n+3, n+4, n+5, n+6)
			args = append(args, ids[j], string(b), float64(j), fmt.Sprintf("d%06d", j), j+1, me)
		}
		if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, sb.String(), args...); err != nil {
			t.Fatalf("rows: %v", err)
		}
	}
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `UPDATE data_tables SET next_row_no = $2 WHERE id = $1`, table, len(ids)+1); err != nil {
		t.Fatal(err)
	}
}

// queryReply is what POST /tables/{id}/query answers: its words when it
// refuses, and the window's rows, its count and its summaries.
type queryReply struct {
	Msg  string `json:"msg"`
	Data struct {
		Rows []struct {
			ID uuid.UUID `json:"id"`
		} `json:"rows"`
		Total     int                           `json:"total"`
		Summaries map[string]map[string]float64 `json:"summaries"`
	} `json:"data"`
}

// overHTTP calls handler as the web does: a POST of body, as JSON, to the
// table's path, with the route's id set to table, as who.
func overHTTP(t *testing.T, handler http.HandlerFunc, path string, table, who uuid.UUID, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	route := chi.NewRouteContext()
	route.URLParams.Add("id", table.String())
	rctx := context.WithValue(req.Context(), chi.RouteCtxKey, route)
	rctx = context.WithValue(rctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: userModels.User{Id: who}})
	rec := httptest.NewRecorder()
	handler(rec, req.WithContext(rctx))
	return rec
}

// queryOverHTTP asks for a table's rows as the web does (POST
// /tables/{id}/query, with body) as who, and answers the status and reply.
func queryOverHTTP(t *testing.T, table, who uuid.UUID, body map[string]any) (int, queryReply) {
	t.Helper()
	rec := overHTTP(t, rowQueryController.QueryRows, "/tables/"+table.String()+"/query", table, who, body)
	var out queryReply
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Errorf("over HTTP: %d %s", rec.Code, rec.Body.String())
	}
	return rec.Code, out
}

func TestComputedFieldsSortAndFilterAt100k(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("connect: %v", err)
	}
	statementsGetAMinute(t)
	integration.SetupDgraph(t)
	me := uuid.New()
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, 'owner@example.test', 'owner', 'Owner', NOW(), NOW())`, me); err != nil {
		t.Fatal(err)
	}
	owner := dataTable.Actor{UserID: me}
	newTable := func(name string) *model.DataTable {
		tb, err := dataTable.CreateTable(ctx, dataTable.TableInput{Name: name, Visibility: "workspace"}, owner)
		if err != nil {
			t.Fatal(err)
		}
		return tb
	}
	add := func(tb *model.DataTable, name, typ string, cfg map[string]interface{}) *model.Field {
		f, err := dataTable.CreateField(ctx, tb.Id, dataTable.FieldInput{Name: name, Type: typ, Config: cfg}, owner)
		if err != nil {
			t.Fatalf("field %s: %v", name, err)
		}
		return f
	}

	// link writes a relation field's links at once, as an import does.
	link := func(field uuid.UUID, pairs [][2]uuid.UUID) {
		for i := 0; i < len(pairs); i += 1000 {
			var sb strings.Builder
			args := []interface{}{field}
			sb.WriteString(`INSERT INTO data_table_links (field_id, from_row, to_row) VALUES `)
			for j := i; j < i+1000 && j < len(pairs); j++ {
				if j > i {
					sb.WriteString(", ")
				}
				n := len(args)
				fmt.Fprintf(&sb, "($1, $%d, $%d)", n+1, n+2)
				args = append(args, pairs[j][0], pairs[j][1])
			}
			if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, sb.String(), args...); err != nil {
				t.Fatalf("links: %v", err)
			}
		}
	}

	// Ledger, private to its owner: entries a member of the workspace never opens.
	ledger, err := dataTable.CreateTable(ctx, dataTable.TableInput{Name: "Ledger", Visibility: model.VisibilityPrivate}, owner)
	if err != nil {
		t.Fatal(err)
	}
	amount := add(ledger, "Amount", model.FieldNumber, nil)
	ledgerIDs := make([]uuid.UUID, 3*computedVendors)
	amountOf := make([]float64, len(ledgerIDs))
	for l := range ledgerIDs {
		ledgerIDs[l] = uuid.New()
		amountOf[l] = float64((l * 613) % 5003)
	}
	fill(ctx, t, ledger.Id, me, ledgerIDs, func(l int) map[string]interface{} {
		return map[string]interface{}{amount.Id.String(): amountOf[l]}
	})

	// Vendors, each with its spend, and what it paid: its three Ledger entries
	// added up (a rollup of a table the member can't open).
	vendors := newTable("Vendors")
	vName := add(vendors, "Name", model.FieldText, nil)
	spend := add(vendors, "Spend", model.FieldNumber, nil)
	vLedger := add(vendors, "Ledger", model.FieldRelation, map[string]interface{}{"relation_target": "table", "table_id": ledger.Id.String(), "two_way": false})
	vPaid := add(vendors, "Paid", model.FieldRollup, map[string]interface{}{"relation": vLedger.Id.String(), "field": amount.Id.String(), "aggregate": "sum"})
	vendorIDs := make([]uuid.UUID, computedVendors)
	spendOf := make([]float64, computedVendors)
	paidOfVendor := make([]float64, computedVendors)
	var entries [][2]uuid.UUID
	for v := range vendorIDs {
		vendorIDs[v] = uuid.New()
		spendOf[v] = float64((v * 7919) % 10007)
		for k := 0; k < 3; k++ {
			entries = append(entries, [2]uuid.UUID{vendorIDs[v], ledgerIDs[3*v+k]})
			paidOfVendor[v] += amountOf[3*v+k]
		}
	}
	fill(ctx, t, vendors.Id, me, vendorIDs, func(v int) map[string]interface{} {
		return map[string]interface{}{vName.Id.String(): fmt.Sprintf("Vendor %04d", v), spend.Id.String(): spendOf[v]}
	})
	link(vLedger.Id, entries)

	// Projects, each linked to one vendor or, every third, two: Total adds up
	// their spend (a rollup), Double is twice the project's own budget (a formula).
	projects := newTable("Projects")
	pName := add(projects, "Name", model.FieldText, nil)
	budget := add(projects, "Budget", model.FieldNumber, nil)
	rel := add(projects, "Vendors", model.FieldRelation, map[string]interface{}{"relation_target": "table", "table_id": vendors.Id.String(), "two_way": false})
	total := add(projects, "Total", model.FieldRollup, map[string]interface{}{"relation": rel.Id.String(), "field": spend.Id.String(), "aggregate": "sum"})
	double := add(projects, "Double", model.FieldFormula, map[string]interface{}{"formula": "{Budget} * 2"})
	// Total and Double again as stored numbers, for the CONTROL leg.
	totalCopy := add(projects, "Total stored", model.FieldNumber, nil)
	doubleCopy := add(projects, "Double stored", model.FieldNumber, nil)
	// Paid adds up Vendors' Paid, so it reaches Ledger a level down; Owed adds
	// up the one Ledger entry each project links to, so it reaches it straight.
	paid := add(projects, "Paid", model.FieldRollup, map[string]interface{}{"relation": rel.Id.String(), "field": vPaid.Id.String(), "aggregate": "sum"})
	pLedger := add(projects, "Ledger", model.FieldRelation, map[string]interface{}{"relation_target": "table", "table_id": ledger.Id.String(), "two_way": false})
	owed := add(projects, "Owed", model.FieldRollup, map[string]interface{}{"relation": pLedger.Id.String(), "field": amount.Id.String(), "aggregate": "sum"})
	projectIDs := make([]uuid.UUID, computedProjects)
	budgetOf := make(map[uuid.UUID]float64, computedProjects)
	totalOf := make(map[uuid.UUID]float64, computedProjects)
	paidOf := make(map[uuid.UUID]float64, computedProjects)
	owedOf := make(map[uuid.UUID]float64, computedProjects)
	links := make([][2]uuid.UUID, 0, computedProjects*4/3+1)
	owes := make([][2]uuid.UUID, 0, computedProjects)
	for p := range projectIDs {
		id := uuid.New()
		projectIDs[p] = id
		budgetOf[id] = float64((p * 104729) % 100000)
		first := (p * 31) % computedVendors
		links = append(links, [2]uuid.UUID{id, vendorIDs[first]})
		totalOf[id], paidOf[id] = spendOf[first], paidOfVendor[first]
		if p%3 == 0 {
			second := (p*17 + 5) % computedVendors
			if second != first {
				links = append(links, [2]uuid.UUID{id, vendorIDs[second]})
				totalOf[id] += spendOf[second]
				paidOf[id] += paidOfVendor[second]
			}
		}
		entry := (p * 7) % len(ledgerIDs)
		owes = append(owes, [2]uuid.UUID{id, ledgerIDs[entry]})
		owedOf[id] = amountOf[entry]
	}
	fill(ctx, t, projects.Id, me, projectIDs, func(p int) map[string]interface{} {
		return map[string]interface{}{pName.Id.String(): fmt.Sprintf("Project %06d", p), budget.Id.String(): budgetOf[projectIDs[p]], totalCopy.Id.String(): totalOf[projectIDs[p]], doubleCopy.Id.String(): budgetOf[projectIDs[p]] * 2}
	})
	link(rel.Id, links)
	link(pLedger.Id, owes)
	// Made whole as the repair sweep makes tables filled at once.
	for _, tb := range []*model.DataTable{ledger, vendors, projects} {
		if err := rowQuery.IndexTable(ctx, tb.Id); err != nil {
			t.Fatalf("index %s: %v", tb.Name, err)
		}
	}

	f := func(m *model.Field) string { return "field:" + m.Id.String() }
	num := func(x float64) []query.Value { return []query.Value{{Number: &x}} }
	// The top of a ranking, as a multiset, whatever order ties take.
	top := func(of map[uuid.UUID]float64, keep func(uuid.UUID) bool, n int) []float64 {
		all := []float64{}
		for id, x := range of {
			if keep(id) {
				all = append(all, x)
			}
		}
		sort.Sort(sort.Reverse(sort.Float64Slice(all)))
		if len(all) > n {
			all = all[:n]
		}
		return all
	}
	everyRow := func(uuid.UUID) bool { return true }
	rich := func(id uuid.UUID) bool { return budgetOf[id]*2 > 150000 }
	doubleOf := make(map[uuid.UUID]float64, computedProjects)
	for id, b := range budgetOf {
		doubleOf[id] = b * 2
	}
	scope := query.Scope{Kind: query.ScopeTable, ID: projects.Id.String()}
	// perRow is a leg's budget of index entries: what a stored field's
	// window may read, entriesPerRow a row shown. measured is the CONTROL
	// leg's: none, as it is what its computed twin is held to.
	perRow := func(windows int) int64 { return int64(windows * (windowSize + 1) * entriesPerRow) }
	measured := func(int) int64 { return math.MaxInt64 }
	// walk reads a spec's first 4 windows as who: the right rows in order,
	// along the index, within budget, and says how many index entries they
	// read. Each leg says what failed and the legs after it still run.
	walk := func(who accessBusiness.Subject, name string, spec query.Spec, value map[uuid.UUID]float64, keep func(uuid.UUID) bool, budget func(windows int) int64) int64 {
		spec.Scope = scope
		before := readsNow(ctx, t)
		var cursor query.Cursor
		var got []float64
		windows := 0
		for ; windows < 4; windows++ {
			res, err := query.Queries.Run(ctx, who, spec, query.Page{Size: windowSize, After: cursor})
			if err != nil {
				t.Errorf("%s: %v", name, err)
				return 0
			}
			if len(res.IDs) != windowSize || res.Next == "" {
				t.Errorf("%s: window %d has %d rows (next %q)", name, windows, len(res.IDs), res.Next)
				return 0
			}
			for _, raw := range res.IDs {
				id, err := uuid.Parse(raw)
				if err != nil || !keep(id) {
					t.Errorf("%s: window %d holds %s, which its filter leaves out", name, windows, raw)
					return 0
				}
				got = append(got, value[id])
			}
			cursor = res.Next
		}
		after := readsNow(ctx, t)
		if want := top(value, keep, len(got)); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: the first %d rows read %v..., where the top of the table is %v...", name, len(got), got[:5], want[:5])
		}
		if n := after.seqScans - before.seqScans; n != 0 {
			t.Errorf("%s: %d windows read a table through, %d times, %d tuples", name, windows, n, after.seqTuples-before.seqTuples)
		}
		n := after.indexTuples - before.indexTuples
		if most := budget(windows); n > most {
			t.Errorf("%s: %d windows read %d index entries, past the budget of %d", name, windows, n, most)
		}
		return n
	}
	by := func(m *model.Field) []query.Sort { return []query.Sort{{Field: f(m), Desc: true}} }

	// The owner opens every table: each computed field, its rollups' every
	// level included, sorts and filters along the index.
	asOwner := accessBusiness.Subject{UserUUID: me.String()}
	walk(asOwner, "by a rollup, most first", query.Spec{Sorts: by(total)}, totalOf, everyRow, perRow)
	walk(asOwner, "by a formula, most first", query.Spec{Sorts: by(double)}, doubleOf, everyRow, perRow)
	walk(asOwner, "a formula over 150,000, by a rollup", query.Spec{
		Filter: &query.Filter{Field: f(double), Operator: query.OperatorGt, Values: num(150000)},
		Sorts:  by(total)}, totalOf, rich, perRow)
	// Total over 15,000 keeps about 4% of the rows, and Double orders them:
	// any plan reads the matching rows' entries, a stored field's too, so the
	// computed window is held to what the same window over stored copies reads.
	over15k := func(id uuid.UUID) bool { return totalOf[id] > 15000 }
	stored := walk(asOwner, "CONTROL: a stored number over 15,000, by a stored number", query.Spec{
		Filter: &query.Filter{Field: f(totalCopy), Operator: query.OperatorGt, Values: num(15000)},
		Sorts:  by(doubleCopy)}, doubleOf, over15k, measured)
	walk(asOwner, "a rollup over 15,000, by a formula", query.Spec{
		Filter: &query.Filter{Field: f(total), Operator: query.OperatorGt, Values: num(15000)},
		Sorts:  by(double)}, doubleOf, over15k, func(int) int64 { return stored })
	walk(asOwner, "by a rollup of a rollup, most first", query.Spec{Sorts: by(paid)}, paidOf, everyRow, perRow)
	walk(asOwner, "by a rollup of the private Ledger, most first", query.Spec{Sorts: by(owed)}, owedOf, everyRow, perRow)

	// A member of the workspace opens Projects and Vendors but not Ledger.
	// Total reaches only tables they open, so what is stored of it is what
	// their own read works out: it answers them as it does the owner.
	member := uuid.New()
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, 'member@example.test', 'member', 'Member', NOW(), NOW())`, member); err != nil {
		t.Fatal(err)
	}
	asMember := accessBusiness.Subject{UserUUID: member.String()}
	walk(asMember, "as a member, by a rollup of tables they open, most first", query.Spec{Sorts: by(total)}, totalOf, everyRow, perRow)

	// Over HTTP, as the web asks (POST /tables/{id}/query): the owner's first
	// window by Paid is the top of the table, and so are the member's by
	// Total and by Double (which reads only the row's own cells, though
	// Projects links to Ledger); the member asking by Paid is refused, in the
	// server's words.
	overHTTP := func(who uuid.UUID, sortBy *model.Field, value map[uuid.UUID]float64) (int, []float64, string) {
		code, out := queryOverHTTP(t, projects.Id, who, map[string]any{"spec": query.Spec{Sorts: by(sortBy)}, "n": windowSize})
		var got []float64
		for _, r := range out.Data.Rows {
			got = append(got, value[r.ID])
		}
		return code, got, out.Msg
	}
	first := func(xs []float64) []float64 { return xs[:min(5, len(xs))] }
	for _, leg := range []struct {
		name  string
		who   uuid.UUID
		by    *model.Field
		value map[uuid.UUID]float64
	}{{"the owner by Paid", me, paid, paidOf}, {"the member by Total", member, total, totalOf}, {"the member by Double", member, double, doubleOf}} {
		code, got, msg := overHTTP(leg.who, leg.by, leg.value)
		if want := top(leg.value, everyRow, windowSize); code != http.StatusOK || fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("over HTTP, %s: %d %q, the first rows %v..., where the top of the table is %v...", leg.name, code, msg, first(got), first(want))
		}
	}
	if code, got, msg := overHTTP(member, paid, paidOf); code != http.StatusUnprocessableEntity || msg == "" || len(got) != 0 {
		t.Errorf("over HTTP, the member by Paid: %d %q with %d rows, where it is refused in the server's words", code, msg, len(got))
	}

	// airtable-03 (main's call, 29 Oct): a formula's and a rollup's summaries
	// are the server's, over every row the reader may see, as a stored field's
	// are, and never a window's. One count over HTTP adds up Total (a rollup)
	// and Double (a formula) over all 100,000 rows, as the owner and as the
	// member (Total reaches only tables they open); the member adding up Paid
	// is refused, in the server's words.
	sumOverHTTP := func(who uuid.UUID, of ...*model.Field) (int, int, map[string]map[string]float64, string) {
		var asks []query.SummaryRequest
		for _, m := range of {
			asks = append(asks, query.SummaryRequest{Field: f(m), Fns: []string{"sum", "count"}})
		}
		code, out := queryOverHTTP(t, projects.Id, who, map[string]any{"spec": query.Spec{Summaries: asks}, "n": 1, "count": true})
		return code, out.Data.Total, out.Data.Summaries, out.Msg
	}
	sumOf := func(value map[uuid.UUID]float64) float64 {
		s := 0.0
		for _, v := range value {
			s += v
		}
		return s
	}
	for _, leg := range []struct {
		name string
		who  uuid.UUID
	}{{"the owner", me}, {"the member", member}} {
		code, n, got, msg := sumOverHTTP(leg.who, total, double)
		for _, c := range []struct {
			m     *model.Field
			value map[uuid.UUID]float64
		}{{total, totalOf}, {double, doubleOf}} {
			s := got[f(c.m)]
			if want := sumOf(c.value); code != http.StatusOK || n != computedProjects || math.Abs(s["sum"]-want) > 1e-6*math.Max(1, math.Abs(want)) || int(s["count"]) != computedProjects {
				t.Errorf("summed over HTTP as %s, %s: %d %q, %d rows, sum %v count %v, where all %d rows add up to %v", leg.name, c.m.Name, code, msg, n, s["sum"], s["count"], computedProjects, want)
			}
		}
	}
	if code, _, got, msg := sumOverHTTP(member, paid); code != http.StatusUnprocessableEntity || msg == "" || got != nil {
		t.Errorf("summed over HTTP, the member adding up Paid: %d %q %v, where it is refused in the server's words", code, msg, got)
	}

	// SECURITY (main's bar): Paid reaches Ledger a level down and Owed reaches
	// it straight, so what is stored of either adds up entries the member
	// can't open. Nothing they ask is ordered, kept, counted, grouped or added
	// up by it: past what one read works out as them, they are refused.
	refusals := []struct {
		name  string
		spec  query.Spec
		count bool
	}{
		{"sorted by Paid (a rollup of a rollup of Ledger)", query.Spec{Sorts: by(paid)}, false},
		{"sorted by Owed (a rollup of Ledger)", query.Spec{Sorts: by(owed)}, false},
		{"kept by Paid over 15,000, and counted", query.Spec{Filter: &query.Filter{Field: f(paid), Operator: query.OperatorGt, Values: num(15000)}}, true},
		{"kept by Owed over 2,500, and counted", query.Spec{Filter: &query.Filter{Field: f(owed), Operator: query.OperatorGt, Values: num(2500)}}, true},
		{"grouped by Owed", query.Spec{Groups: []query.Group{{Field: f(owed)}}}, true},
		{"with Paid added up", query.Spec{Summaries: []query.SummaryRequest{{Field: f(paid), Fns: []string{"sum", "max"}}}}, true},
		{"a formula over 150,000, sorted by Paid", query.Spec{
			Filter: &query.Filter{Field: f(double), Operator: query.OperatorGt, Values: num(150000)},
			Sorts:  by(paid)}, true},
	}
	for _, r := range refusals {
		spec, qctx := r.spec, ctx
		spec.Scope = scope
		if r.count {
			qctx = rowQuery.WithCount(ctx)
		}
		res, err := query.Queries.Run(qctx, asMember, spec, query.Page{Size: windowSize})
		if !errors.Is(err, rowQuery.ErrTooManyToWorkOut) {
			t.Errorf("as a member who can't open Ledger, %s: %d rows, total %d, %d groups, summaries %v (error %v), where only a refusal is theirs", r.name, len(res.IDs), res.Total, len(res.Groups), res.Summaries, err)
		}
	}
	// The API's window and the export read through QueryRows, as the member.
	if w, err := dataTable.QueryRows(ctx, projects.Id, dataTable.Actor{UserID: member}, query.Spec{Sorts: by(owed)}, query.Page{Size: windowSize}, true); !errors.Is(err, rowQuery.ErrTooManyToWorkOut) {
		n := 0
		if w != nil {
			n = len(w.Rows)
		}
		t.Errorf("as a member, the API's window sorted by Owed: %d rows (error %v), where only a refusal is theirs", n, err)
	}

	// The gate (computedStore.Values.Serves) holds a kept value to the tables
	// that value reaches, each level of its rollups and linked reads, never
	// to every table its table links to. Projects links straight to Ledger,
	// which neither Double nor Total reads, so both serve the member, alone
	// and together, as they serve the owner. Owed, Paid and Owed twice (a
	// formula reading Owed) reach Ledger, so none serves the member, and
	// asking for any of them with one that does serves nothing. The fields are
	// made here, after every leg that reads kept values: a new field makes
	// the table's kept values wait for the sweep, and the gate reads none.
	owedTwice := add(projects, "Owed twice", model.FieldFormula, map[string]interface{}{"formula": "{Owed} * 2"})
	// SECURITY (main's bar for this gate): a value reaching Ledger by every
	// kind of reference stays refused to the member, though the owner is
	// served each. Ledger len reads the link straight and Ledger len plus one
	// reads that formula: only their reach refuses them (no plan problem),
	// so they hold the gate's reach to the link. Ledger sum reads
	// {Ledger}.{Amount}, Ledger amounts is a lookup (a rollup listing them),
	// Owed a rollup, Paid a rollup of a rollup, and Owed four times reads
	// Owed twice, which reads Owed: formulas two levels deep.
	ledgerLen := add(projects, "Ledger len", model.FieldFormula, map[string]interface{}{"formula": "LEN({Ledger})"})
	lenPlus := add(projects, "Ledger len plus one", model.FieldFormula, map[string]interface{}{"formula": "{Ledger len} + 1"})
	ledgerSum := add(projects, "Ledger sum", model.FieldFormula, map[string]interface{}{"formula": "SUM({Ledger}.{Amount})"})
	lookup := add(projects, "Ledger amounts", model.FieldRollup, map[string]interface{}{"relation": pLedger.Id.String(), "field": amount.Id.String(), "aggregate": "list"})
	owedFour := add(projects, "Owed four times", model.FieldFormula, map[string]interface{}{"formula": "{Owed twice} * 2"})
	for _, g := range []struct {
		name   string
		who    uuid.UUID
		of     []*model.Field
		serves bool
	}{
		{"the owner", me, []*model.Field{total, double, paid, owed, owedTwice, ledgerLen, lenPlus, ledgerSum, lookup, owedFour}, true},
		{"the member", member, []*model.Field{double}, true},
		{"the member", member, []*model.Field{total}, true},
		{"the member", member, []*model.Field{total, double}, true},
		{"the member, a link read straight", member, []*model.Field{ledgerLen}, false},
		{"the member, a formula reading that one", member, []*model.Field{lenPlus}, false},
		{"the member, {Link}.{Field}", member, []*model.Field{ledgerSum}, false},
		{"the member, a lookup", member, []*model.Field{lookup}, false},
		{"the member, a rollup", member, []*model.Field{owed}, false},
		{"the member, a rollup of a rollup", member, []*model.Field{paid}, false},
		{"the member, a formula reading a rollup", member, []*model.Field{owedTwice}, false},
		{"the member, a formula two levels over a rollup", member, []*model.Field{owedFour}, false},
		{"the member", member, []*model.Field{double, owed}, false},
		{"the member", member, []*model.Field{total, paid}, false},
		{"the member", member, []*model.Field{double, ledgerLen}, false},
	} {
		ids, names := make([]string, len(g.of)), make([]string, len(g.of))
		for i, m := range g.of {
			ids[i], names[i] = m.Id.String(), m.Name
		}
		kinds, err := computedStore.Values.Serves(ctx, tableAccess.Who{User: g.who}, projects.Id, ids)
		served := err == nil && len(kinds) == len(ids)
		for _, id := range ids {
			served = served && kinds[id] != ""
		}
		if served != g.serves || err != nil {
			want := "nothing"
			if g.serves {
				want = "each by what it gives"
			}
			t.Errorf("the gate, %s asking for %s: %v (error %v), where it serves %s", g.name, strings.Join(names, " and "), kinds, err, want)
		}
	}

	// Fail closed: once Ledger is in the trash nobody opens it, the owner
	// included, so nothing stored from it before answers anyone.
	if err := dataTable.DeleteTable(ctx, ledger.Id, owner); err != nil {
		t.Fatal(err)
	}
	for _, m := range []*model.Field{owed, paid} {
		res, err := query.Queries.Run(ctx, asOwner, query.Spec{Scope: scope, Sorts: by(m)}, query.Page{Size: windowSize})
		if !errors.Is(err, rowQuery.ErrTooManyToWorkOut) {
			t.Errorf("with Ledger in the trash, the owner sorted by %s: %d rows (error %v), where only a refusal belongs", m.Name, len(res.IDs), err)
		}
	}
}

// CR-92 M1 (SECURITY): a link to OneCamp items reads as the items ITS reader
// may open (itemLinks.go nameItems), so no stored value of a formula over
// one is every reader's: it is worked out per read, for the reader, and the
// store keeps none. Rows link a member's private doc, the owner's private
// doc and a doc open to the workspace. Once the store has filled the table
// (it serves both of them Double, twice a number of the row's own), each of
// them filters, sorts and adds up by LEN({Docs}) and UPPER({Docs}) over
// HTTP and gets exactly their own read: the member counts their own doc and
// never the owner's, the owner theirs and never the member's, and the store
// serves neither formula to anyone.
func TestAFormulaOverLinkedItemsIsWorkedOutForItsReader(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("connect: %v", err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	people := map[string]uuid.UUID{"owner": uuid.New(), "member": uuid.New()}
	var nodes []map[string]any
	for name, id := range people {
		if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, id.String()+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name})
	}
	uids := dg.Mutate(t, nodes)
	doc := func(by, title string, private bool) string {
		d, err := docBusiness.CreateDocInTree(ctx, &dgraphStruct.DgraphUser{Uid: uids[by], Uuid: people[by].String(), UserName: by}, docBusiness.NewDoc{Title: title, Private: &private}, nil)
		if err != nil {
			t.Fatalf("the doc %q: %v", title, err)
		}
		return d.Uuid
	}
	mine, theirs, open := doc("member", "Member plan", true), doc("owner", "Owner secret ledger", true), doc("owner", "Open notes", false)
	// The docs each of them may open, by title.
	opens := map[string]map[string]string{
		"member": {mine: "Member plan", open: "Open notes"},
		"owner":  {theirs: "Owner secret ledger", open: "Open notes"},
	}

	owner := dataTable.Actor{UserID: people["owner"]}
	tb, err := dataTable.CreateTable(ctx, dataTable.TableInput{Name: "Reading list", Visibility: "workspace"}, owner)
	if err != nil {
		t.Fatal(err)
	}
	add := func(name, typ string, cfg map[string]interface{}) *model.Field {
		fl, err := dataTable.CreateField(ctx, tb.Id, dataTable.FieldInput{Name: name, Type: typ, Config: cfg}, owner)
		if err != nil {
			t.Fatalf("field %s: %v", name, err)
		}
		return fl
	}
	name := add("Name", model.FieldText, nil)
	docs := add("Docs", model.FieldRelation, map[string]interface{}{"relation_target": fieldTypes.ItemDoc})
	amount := add("Amount", model.FieldNumber, nil)
	docsLen := add("Docs len", model.FieldFormula, map[string]interface{}{"formula": "LEN({Docs})"})
	docsUpper := add("Docs upper", model.FieldFormula, map[string]interface{}{"formula": "UPPER({Docs})"})
	double := add("Double", model.FieldFormula, map[string]interface{}{"formula": "{Amount} * 2"})
	rows := []struct {
		name string
		docs []string
	}{{"Mine", []string{mine}}, {"Theirs", []string{theirs}}, {"Open", []string{open}}, {"Both", []string{mine, theirs}}, {"None", nil}}
	ids := make([]uuid.UUID, len(rows))
	nameOf := make(map[uuid.UUID]string, len(rows))
	for i, r := range rows {
		ids[i] = uuid.New()
		nameOf[ids[i]] = r.name
	}
	fill(ctx, t, tb.Id, people["owner"], ids, func(i int) map[string]interface{} {
		refs := []fieldTypes.ItemRef{}
		for _, d := range rows[i].docs {
			refs = append(refs, fieldTypes.ItemRef{ID: d, Type: fieldTypes.ItemDoc})
		}
		return map[string]interface{}{name.Id.String(): rows[i].name, docs.Id.String(): refs, amount.Id.String(): float64(i + 1)}
	})
	// Made whole as the repair sweep makes a table filled at once, the
	// store's kept values with it.
	if err := rowQuery.IndexTable(ctx, tb.Id); err != nil {
		t.Fatalf("index: %v", err)
	}
	serves := func(who string, m *model.Field) bool {
		kinds, err := computedStore.Values.Serves(ctx, tableAccess.Who{User: people[who]}, tb.Id, []string{m.Id.String()})
		return err == nil && kinds[m.Id.String()] != ""
	}
	readers := []string{"member", "owner"}
	for _, who := range readers {
		if !serves(who, double) {
			t.Fatalf("the store serves the %s no Double, so it never filled the table and no leg here reads from it", who)
		}
		for _, m := range []*model.Field{docsLen, docsUpper} {
			if serves(who, m) {
				t.Errorf("the store serves the %s %s, where no stored value is every reader's: their own read works it out", who, m.Name)
			}
		}
	}

	// read is row i's Docs as who reads them: the titles of those they may open.
	read := func(who string, i int) string {
		var titles []string
		for _, d := range rows[i].docs {
			if title, ok := opens[who][d]; ok {
				titles = append(titles, title)
			}
		}
		return strings.Join(titles, ", ")
	}
	f := func(m *model.Field) string { return "field:" + m.Id.String() }
	ask := func(who string, spec query.Spec) (int, queryReply) {
		return queryOverHTTP(t, tb.Id, people[who], map[string]any{"spec": spec, "n": len(rows), "count": true})
	}
	names := func(r queryReply) []string {
		out := []string{}
		for _, row := range r.Data.Rows {
			out = append(out, nameOf[row.ID])
		}
		sort.Strings(out)
		return out
	}
	zero := 0.0
	for _, who := range readers {
		// The rows whose Docs read as something to who, and their lengths
		// most first, as their own read gives them.
		some, lengths, sum := []string{}, []float64{}, 0.0
		for i, r := range rows {
			n := float64(len(read(who, i)))
			if n > 0 {
				some = append(some, r.name)
			}
			lengths, sum = append(lengths, n), sum+n
		}
		sort.Strings(some)
		sort.Sort(sort.Reverse(sort.Float64Slice(lengths)))

		code, out := ask(who, query.Spec{Filter: &query.Filter{Field: f(docsLen), Operator: query.OperatorGt, Values: []query.Value{{Number: &zero}}}})
		if got := names(out); code != http.StatusOK || fmt.Sprint(got) != fmt.Sprint(some) {
			t.Errorf("over HTTP, the %s filtering by Docs len over 0: %d %q, rows %v, where their own read keeps %v", who, code, out.Msg, got, some)
		}
		code, out = ask(who, query.Spec{Sorts: []query.Sort{{Field: f(docsLen), Desc: true}}})
		got := []float64{}
		for _, row := range out.Data.Rows {
			got = append(got, float64(len(read(who, slices.Index(ids, row.ID)))))
		}
		if code != http.StatusOK || fmt.Sprint(got) != fmt.Sprint(lengths) {
			t.Errorf("over HTTP, the %s sorting by Docs len, most first: %d %q, lengths %v, where their own read gives %v", who, code, out.Msg, got, lengths)
		}
		code, out = ask(who, query.Spec{Summaries: []query.SummaryRequest{{Field: f(docsLen), Fns: []string{"sum"}}}})
		if s := out.Data.Summaries[f(docsLen)]["sum"]; code != http.StatusOK || s != sum {
			t.Errorf("over HTTP, the %s adding up Docs len: %d %q, sum %v, where their own read adds up to %v", who, code, out.Msg, s, sum)
		}
		// A private doc's title finds its rows only for who may open it.
		for d, title := range map[string]string{mine: "Member plan", theirs: "Owner secret ledger"} {
			want := []string{}
			for _, r := range rows {
				if _, may := opens[who][d]; may && slices.Contains(r.docs, d) {
					want = append(want, r.name)
				}
			}
			sort.Strings(want)
			text := strings.ToUpper(title)
			code, out := ask(who, query.Spec{Filter: &query.Filter{Field: f(docsUpper), Operator: query.OperatorContains, Values: []query.Value{{Text: text}}}})
			if got := names(out); code != http.StatusOK || fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("over HTTP, the %s filtering Docs upper for %q: %d %q, rows %v, where their own read finds %v", who, text, code, out.Msg, got, want)
			}
		}
	}
}
