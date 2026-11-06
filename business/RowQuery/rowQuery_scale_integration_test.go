//go:build integration

package business_test

// Scale budgets (notion-db-06, airtable-16; V-114). A table filled at once
// with 100,000 rows, as an import fills one, answers the FIRST window of a
// sort by a stored number, a sort by a formula and a filter along the cell
// index: no read of the rows or the cell index through, and a bounded number
// of index entries for the rows it shows. A filter that keeps few rows,
// sorted by another field, is held to the same budget. A search reads every
// row's cells (no index answers it yet): its first window is measured and
// said on its own line. That is what the server's
// count after a fill (CountAfterFill: VACUUM, then ANALYZE) buys: Postgres 12
// never vacuums a table for rows added, so until a vacuum marks the fill's
// pages whole, every entry an index walk reads is fetched from the table,
// and until an ANALYZE counts them, the planner plans for a handful.
//
// The test measures three states of the same table, each with its plan for
// a sort's first window (EXPLAIN ANALYZE, BUFFERS) in the log: uncounted (the
// server's count failed), counted only (ANALYZE, as before V-150) and counted
// as the server counts (model.VacuumRows). Only the last is the server's
// state after a fill, and it is held to the budget.
//
// Then, over HTTP as the web asks: what a reader waits for at the top, the
// middle and the end of a sort, two filters and a search (p50 and p95, with
// the machine's facts, written to $BUDGET_OUT when set: 30 windows a place
// then, 3 otherwise), held only to work, never to time; and Table Access
// View on every read, the count that picks a filter's read included.
//
// Run: integ_rel.sh <worktree> <log> '^TestTheFirstWindowsAfterAHundredThousandRowFill$' with PKGS='./business/RowQuery/'

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"runtime"
	"slices"
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

// scaleRows is the table's size: the size the scorecard's rows name.
const scaleRows = 100000

// heapBlocks is how many of the rows' and the cell index's table pages
// Postgres has read, from its cache or the disk.
func heapBlocks(ctx context.Context, t *testing.T) int64 {
	t.Helper()
	readsNow(ctx, t) // the collector has every transaction so far
	var n int64
	err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `SELECT COALESCE(SUM(heap_blks_read + heap_blks_hit), 0)
		FROM pg_statio_user_tables WHERE relname IN ('data_table_cell_index', 'data_table_rows')`).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// sortPlan is the plan of a sort's walk along the cell index, run: its top
// node, the rows the planner expected and the entries it fetched from the
// table one by one. It is the walk a window by one number takes, written
// out here, not the window's own statement: the windows below are measured
// on the server's own path (query.Queries.Run).
func sortPlan(ctx context.Context, t *testing.T, field uuid.UUID) (node string, planned float64, heapFetches float64, text string) {
	t.Helper()
	q := fmt.Sprintf(`EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) SELECT row_id FROM data_table_cell_index
		WHERE field_id = '%s' AND n IS NOT NULL ORDER BY n DESC, t DESC, p, row_id LIMIT 101`, field)
	var raw string
	if err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, q).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var plan []struct {
		Plan map[string]any `json:"Plan"`
	}
	if err := json.Unmarshal([]byte(raw), &plan); err != nil || len(plan) == 0 {
		t.Fatalf("plan %q: %v", raw, err)
	}
	// The walk is the deepest node: Limit -> Index Only Scan, or a Sort over
	// a scan when the planner thought the field had a handful of entries.
	var walk func(p map[string]any)
	walk = func(p map[string]any) {
		typ, _ := p["Node Type"].(string)
		if node != "" {
			node += " > "
		}
		node += typ
		if typ == "Index Only Scan" || typ == "Index Scan" || typ == "Seq Scan" || typ == "Bitmap Heap Scan" {
			planned, _ = p["Plan Rows"].(float64)
			heapFetches, _ = p["Heap Fetches"].(float64)
		}
		if kids, ok := p["Plans"].([]any); ok && len(kids) > 0 {
			if k, ok := kids[0].(map[string]any); ok {
				walk(k)
			}
		}
	}
	walk(plan[0].Plan)
	return node, planned, heapFetches, raw
}

func TestTheFirstWindowsAfterAHundredThousandRowFill(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("connect: %v", err)
	}
	statementsGetAMinute(t)
	integration.SetupDgraph(t)
	// Only the server's count, or the test's, counts the tables here.
	noAutovacuumOn(ctx, t, "data_table_rows", "data_table_cell_index")
	me := uuid.New()
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, 'scale@example.test', 'scale', 'Scale', NOW(), NOW())`, me); err != nil {
		t.Fatal(err)
	}
	owner := dataTable.Actor{UserID: me}
	tb, err := dataTable.CreateTable(ctx, dataTable.TableInput{Name: "Lines", Visibility: "workspace"}, owner)
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
	stages := []string{"Lead", "Won", "Lost"}
	opts := []interface{}{}
	for _, s := range stages {
		opts = append(opts, map[string]interface{}{"label": s})
	}
	stage := add("Stage", model.FieldSelect, map[string]interface{}{"options": opts})
	double := add("Double", model.FieldFormula, map[string]interface{}{"formula": "{Amount} * 2"})

	ids := make([]uuid.UUID, scaleRows)
	for i := range ids {
		ids[i] = uuid.New()
	}
	start := time.Now()
	fill(ctx, t, tb.Id, me, ids, func(i int) map[string]interface{} {
		return map[string]interface{}{
			item.Id.String():   fmt.Sprintf("Line %06d", i),
			amount.Id.String(): float64((i * 7) % 1000),
			stage.Id.String():  stages[i%len(stages)],
		}
	})
	// Indexed whole, its formula kept, as the repair sweep makes a table
	// filled at once; the server's count after it fails, so the tables stand
	// as the fill left them.
	restore := rowQuery.FailCounts()
	err = rowQuery.IndexTable(ctx, tb.Id)
	restore()
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	t.Logf("%d rows written and indexed in %v", scaleRows, time.Since(start).Round(time.Millisecond))

	f := func(m *model.Field) string { return "field:" + m.Id.String() }
	lo, hi := 500.0, 509.0
	specs := []struct {
		name string
		spec query.Spec
	}{
		{"by amount, most first", query.Spec{Sorts: []query.Sort{{Field: f(amount), Desc: true}}}},
		{"by double (a formula), most first", query.Spec{Sorts: []query.Sort{{Field: f(double), Desc: true}}}},
		{"amount in a range, by item", query.Spec{
			Filter: &query.Filter{Field: f(amount), Operator: query.OperatorBetween, Values: []query.Value{{Number: &lo}, {Number: &hi}}},
			Sorts:  []query.Sort{{Field: f(item)}}}},
		{"a stage, by amount", query.Spec{
			Filter: &query.Filter{Field: f(stage), Operator: query.OperatorIs, Values: []query.Value{{Text: "Won"}}},
			Sorts:  []query.Sort{{Field: f(amount), Desc: true}}}},
		{"a search", query.Spec{Search: "Line 0999"}},
	}
	who := accessBusiness.Subject{UserUUID: me.String()}

	// first reads each spec's first window and says what it cost.
	type cost struct {
		seqScans, entries, blocks int64
		took                      time.Duration
	}
	first := func(state string) map[string]cost {
		out := map[string]cost{}
		node, planned, fetched, raw := sortPlan(ctx, t, amount.Id)
		t.Logf("%s: a sort's first window is planned %s for %.0f entries, with %.0f fetched from the table", state, node, planned, fetched)
		if testing.Verbose() {
			t.Logf("%s: plan %s", state, strings.Join(strings.Fields(raw), " "))
		}
		for _, s := range specs {
			s.spec.Scope = query.Scope{Kind: query.ScopeTable, ID: tb.Id.String()}
			before, blocks := readsNow(ctx, t), heapBlocks(ctx, t)
			began := time.Now()
			res, err := query.Queries.Run(ctx, who, s.spec, query.Page{Size: windowSize})
			took := time.Since(began)
			if err != nil {
				t.Errorf("%s, %s: %v", state, s.name, err)
				continue
			}
			if len(res.IDs) == 0 {
				t.Errorf("%s, %s: the first window is empty", state, s.name)
			}
			after, blocksAfter := readsNow(ctx, t), heapBlocks(ctx, t)
			c := cost{after.seqScans - before.seqScans, after.indexTuples - before.indexTuples, blocksAfter - blocks, took}
			out[s.name] = c
			t.Logf("%s, %s: %d rows, %d scans through, %d index entries, %d table pages, %v", state, s.name, len(res.IDs), c.seqScans, c.entries, c.blocks, took.Round(time.Microsecond))
		}
		return out
	}

	first("uncounted (the count failed)")
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `ANALYZE data_table_rows, data_table_cell_index`); err != nil {
		t.Fatal(err)
	}
	first("counted by ANALYZE only (before V-150)")
	if err := rowQuery.CountAfterFill(ctx, scaleRows); err != nil {
		t.Fatalf("the server's count: %v", err)
	}
	counted := first("counted as the server counts after a fill")

	// The crossover that sets rowQuery.NarrowSet: a filter keeping 100 to
	// 10,000 rows, sorted by another field, its first window walked along the
	// sort's index and read as its set, side by side.
	saved := rowQuery.NarrowSet
	t.Cleanup(func() { rowQuery.NarrowSet = saved })
	for _, width := range []float64{1, 5, 10, 20, 30, 50, 100} {
		lo, hi := 500.0, 500+width-1
		spec := query.Spec{
			Scope:  query.Scope{Kind: query.ScopeTable, ID: tb.Id.String()},
			Filter: &query.Filter{Field: f(amount), Operator: query.OperatorBetween, Values: []query.Value{{Number: &lo}, {Number: &hi}}},
			Sorts:  []query.Sort{{Field: f(item)}},
		}
		line := fmt.Sprintf("a filter keeping %d rows, by item:", int(width)*scaleRows/1000)
		for _, way := range []struct {
			name string
			set  int
		}{{"walked", 0}, {"as its set", scaleRows}} {
			rowQuery.NarrowSet = way.set
			before := readsNow(ctx, t)
			began := time.Now()
			res, err := query.Queries.Run(ctx, who, spec, query.Page{Size: windowSize})
			took := time.Since(began)
			after := readsNow(ctx, t)
			if err != nil || len(res.IDs) == 0 {
				t.Errorf("crossover, %s %s: %d rows, %v", line, way.name, len(res.IDs), err)
			}
			line += fmt.Sprintf(" %s %d entries in %v;", way.name, after.indexTuples-before.indexTuples, took.Round(time.Microsecond))
		}
		rowQuery.NarrowSet = saved
		t.Logf("crossover, %s", line)
	}

	// The server's state after a fill is held to the budget.
	_, _, fetched, _ := sortPlan(ctx, t, amount.Id)
	if fetched > windowSize+1 {
		t.Errorf("after the server's count, a sort's first window fetched %.0f entries from the table one by one", fetched)
	}
	for name, c := range counted {
		// A search reads every row's cells (runner.go searchSQL): no index
		// answers it yet, so its number is measured and said on its own
		// line, never held to a sort's budget or left out.
		if name == "a search" {
			t.Logf("after the server's count, a search's first window read %d index entries in %v (its own line)", c.entries, c.took.Round(time.Microsecond))
			continue
		}
		if c.seqScans != 0 {
			t.Errorf("after the server's count, %s: the first window read the rows or the cell index through %d times", name, c.seqScans)
		}
		if most := int64((windowSize + 1) * entriesPerRow); c.entries > most {
			t.Errorf("after the server's count, %s: the first window read %d index entries, past the budget of %d", name, c.entries, most)
		}
	}

	measured := []struct {
		name string
		spec query.Spec
	}{
		{"sort: by amount, most first", specs[0].spec},
		{"filter: amount in a range (1%), by item", specs[2].spec},
		{"filter: a stage (a third), by amount", specs[3].spec},
		{"search: a row's item", specs[4].spec},
	}
	waits(ctx, t, tb.Id, me, measured)

	// Table Access View gates every read, the count that picks a filter's
	// read included: with the table made private, a member who isn't its
	// owner is answered "not found", with no row and no total, and none of
	// the asks reads a row or an entry, so neither the count nor the read
	// ran. Its owner still reads every one.
	other := uuid.New()
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, 'other@example.test', 'other', 'Other', NOW(), NOW())`, other); err != nil {
		t.Fatal(err)
	}
	private := model.VisibilityPrivate
	if _, _, err := dataTable.UpdateTable(ctx, tb.Id, dataTable.TablePatch{Visibility: &private}, owner); err != nil {
		t.Fatalf("make the table private: %v", err)
	}
	for _, m := range measured {
		before := readsNow(ctx, t)
		code, out := queryOverHTTP(t, tb.Id, other, map[string]any{"spec": m.spec, "n": windowSize, "count": true})
		after := readsNow(ctx, t)
		if code != http.StatusNotFound || len(out.Data.Rows) != 0 || out.Data.Total != 0 {
			t.Errorf("a member without view, %s: %d with %d rows of %d, want not found and nothing", m.name, code, len(out.Data.Rows), out.Data.Total)
		}
		if entries, through := after.indexTuples-before.indexTuples, after.seqScans-before.seqScans; entries != 0 || through != 0 {
			t.Errorf("a member without view, %s: read %d index entries and the rows or entries through %d times: a count or a read ran before access", m.name, entries, through)
		}
		if code, out := queryOverHTTP(t, tb.Id, me, map[string]any{"spec": m.spec, "n": windowSize}); code != http.StatusOK || len(out.Data.Rows) == 0 {
			t.Errorf("its owner, %s, on the private table: %d with %d rows", m.name, code, len(out.Data.Rows))
		}
	}
}

// waits asks, over HTTP as the web does, for the windows of each of specs at
// its top, its middle and its end, as table's owner me, and says what a
// reader waits for there: p50 and p95, with the index entries one window
// read. Held only to work: each window holds the rows the spec has from its
// place on. With $BUDGET_OUT set it asks 30 windows a place and writes the
// numbers there as JSON beside the machine's facts (CPU, cores, Postgres,
// load); else 3, to keep the gate short.
func waits(ctx context.Context, t *testing.T, table, me uuid.UUID, specs []struct {
	name string
	spec query.Spec
}) {
	t.Helper()
	out, windows := os.Getenv("BUDGET_OUT"), 3
	if out != "" {
		windows = 30
	}
	type place struct {
		Spec    string  `json:"spec"`
		Place   string  `json:"place"`
		At      int     `json:"at"`
		Rows    int     `json:"rows"`
		Windows int     `json:"windows"`
		P50     float64 `json:"p50_ms"`
		P95     float64 `json:"p95_ms"`
		Entries int64   `json:"index_entries_one_window"`
	}
	report := struct {
		Host struct {
			CPU       string `json:"cpu"`
			Cores     int    `json:"cores"`
			Postgres  string `json:"postgres"`
			LoadStart string `json:"load_start"`
			LoadEnd   string `json:"load_end"`
		} `json:"host"`
		TableRows int     `json:"table_rows"`
		Window    int     `json:"window"`
		Places    []place `json:"places"`
	}{TableRows: scaleRows, Window: windowSize}
	report.Host.CPU, report.Host.Cores, report.Host.LoadStart = cpuModel(), runtime.NumCPU(), loadNow()
	if err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `SELECT version()`).Scan(&report.Host.Postgres); err != nil {
		t.Fatal(err)
	}
	ms := func(d time.Duration) float64 { return math.Round(float64(d)/1e4) / 100 }
	for _, s := range specs {
		code, first := queryOverHTTP(t, table, me, map[string]any{"spec": s.spec, "n": windowSize, "count": true})
		total := first.Data.Total
		if code != http.StatusOK || total == 0 {
			t.Errorf("%s over HTTP: %d, %d rows in all", s.name, code, total)
			continue
		}
		for _, p := range []struct {
			name string
			at   int
		}{{"top", 0}, {"middle", total / 2}, {"end", max(0, total-windowSize)}} {
			want := min(windowSize, total-p.at)
			var took []time.Duration
			var entries int64
			for i := 0; i < windows; i++ {
				var before reads
				if i == 0 {
					before = readsNow(ctx, t)
				}
				began := time.Now()
				code, got := queryOverHTTP(t, table, me, map[string]any{"spec": s.spec, "n": windowSize, "at": p.at})
				took = append(took, time.Since(began))
				if i == 0 {
					entries = readsNow(ctx, t).indexTuples - before.indexTuples
				}
				if code != http.StatusOK || len(got.Data.Rows) != want {
					t.Errorf("%s at its %s (%d): %d with %d rows, want %d", s.name, p.name, p.at, code, len(got.Data.Rows), want)
					break
				}
			}
			slices.Sort(took)
			q := func(f float64) time.Duration { return took[max(0, int(math.Ceil(f*float64(len(took))))-1)] }
			r := place{Spec: s.name, Place: p.name, At: p.at, Rows: total, Windows: len(took), P50: ms(q(0.5)), P95: ms(q(0.95)), Entries: entries}
			report.Places = append(report.Places, r)
			t.Logf("waits, %s, at its %s (%d of %d): p50 %.2f ms, p95 %.2f ms over %d windows; one window read %d index entries", s.name, p.name, p.at, total, r.P50, r.P95, r.Windows, r.Entries)
		}
	}
	report.Host.LoadEnd = loadNow()
	t.Logf("waits measured on %s (%d cores), %s, load %s to %s", report.Host.CPU, report.Host.Cores, report.Host.Postgres, report.Host.LoadStart, report.Host.LoadEnd)
	if out == "" {
		return
	}
	b, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// cpuModel is the machine's CPU as Linux names it, or "unknown".
func cpuModel() string {
	b, _ := os.ReadFile("/proc/cpuinfo")
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == "model name" {
			return strings.TrimSpace(v)
		}
	}
	return "unknown"
}

// loadNow is the machine's load over 1, 5 and 15 minutes, or "unknown".
func loadNow() string {
	b, err := os.ReadFile("/proc/loadavg")
	if f := strings.Fields(string(b)); err == nil && len(f) >= 3 {
		return strings.Join(f[:3], " ")
	}
	return "unknown"
}
