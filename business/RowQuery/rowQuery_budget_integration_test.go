//go:build integration

package business_test

// A window's work budget (tables T5, "a 100k-row window reads in 20 ms"): a
// window of a sort or a filter the cell index walks reads only along its
// indexes, a bounded number of entries for the rows it shows, and never a
// table through. Measured as work, which a loaded machine doesn't change, and
// not as time: the unknown-label check once read all 1.12 million entries of
// a 100,000-row table on every window (120 to 190 ms) while each query of
// the window took under 15.
//
// Run: integ_rel.sh <worktree> <log> '^(TestRowQueryWindowsReadOnlyAlongTheirIndexes)$' with PKGS='./business/RowQuery/'

import (
	"context"
	"encoding/json"
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

// budgetRows is the table's size: big enough that reading it through costs
// far more than any window's own rows.
const budgetRows = 20000

// windowSize is the grid's window.
const windowSize = 100

// entriesPerRow is the most index entries a window may read for each row it
// shows: its walk's entries (a quarter of the rows in a group, so about
// four), each row's probes and its row. Three times what it reads now.
const entriesPerRow = 40

// reads is what Postgres counted for the cell index and the rows: scans
// through the table, the tuples they read, and the entries read by index.
type reads struct{ seqScans, seqTuples, indexTuples int64 }

// readsNow is the counts once the collector has heard of every transaction
// so far: they have held still for a second. A backend reports a
// transaction's counts up to half a second after it ends, and later on a
// loaded machine, so a fixed wait could read them early.
func readsNow(ctx context.Context, t *testing.T) reads {
	t.Helper()
	const settled, every, most = time.Second, 100 * time.Millisecond, 20 * time.Second
	last, since, start := readsOnce(ctx, t), time.Now(), time.Now()
	for time.Since(since) < settled {
		if time.Since(start) > most {
			t.Fatalf("the counts never held still for %v in %v", settled, most)
		}
		time.Sleep(every)
		if r := readsOnce(ctx, t); r != last {
			last, since = r, time.Now()
		}
	}
	return last
}

// readsOnce is the counts as the collector has them now.
func readsOnce(ctx context.Context, t *testing.T) reads {
	t.Helper()
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `SELECT pg_stat_clear_snapshot()`); err != nil {
		t.Fatal(err)
	}
	var r reads
	err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `SELECT COALESCE(SUM(seq_scan), 0), COALESCE(SUM(seq_tup_read), 0), COALESCE(SUM(idx_tup_read), 0)
		FROM pg_stat_user_tables t JOIN LATERAL (SELECT SUM(idx_tup_read) AS idx_tup_read FROM pg_stat_user_indexes i WHERE i.relid = t.relid) i ON TRUE
		WHERE t.relname IN ('data_table_cell_index', 'data_table_rows')`).Scan(&r.seqScans, &r.seqTuples, &r.indexTuples)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// budgetTable is a table of budgetRows deals, filled at once and indexed
// whole as the repair sweep makes such a table, with its owner and fields.
type budgetTable struct {
	ctx                             context.Context
	me                              uuid.UUID
	tb                              *model.DataTable
	name, amount, due, done, status *model.Field
}

func newBudgetTable(t *testing.T) budgetTable {
	t.Helper()
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
	tb, err := dataTable.CreateTable(ctx, dataTable.TableInput{Name: "Deals", Visibility: "workspace"}, owner)
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
	name := add("Name", model.FieldText, nil)
	amount := add("Amount", model.FieldNumber, nil)
	due := add("Due", model.FieldDate, nil)
	done := add("Done", model.FieldCheckbox, nil)
	statuses := []string{"Todo", "Doing", "Blocked", "Done"}
	opts := []interface{}{}
	for _, s := range statuses {
		opts = append(opts, map[string]interface{}{"label": s})
	}
	status := add("Status", model.FieldSelect, map[string]interface{}{"options": opts})

	for i := 0; i < budgetRows; i += 500 {
		var sb strings.Builder
		args := []interface{}{tb.Id}
		sb.WriteString(`INSERT INTO data_table_rows (id, table_id, "values", position, position_key, row_no, created_by, created_at, updated_at) VALUES `)
		for j := i; j < i+500; j++ {
			cells := map[string]interface{}{
				name.Id.String():   fmt.Sprintf("Deal %05d", (j*7919)%budgetRows),
				amount.Id.String(): float64((j * 104729) % 100000),
				due.Id.String():    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, (j*31)%365).Format("2006-01-02"),
				done.Id.String():   j%3 == 0,
				status.Id.String(): statuses[j%len(statuses)],
			}
			b, _ := json.Marshal(cells)
			if j > i {
				sb.WriteString(", ")
			}
			n := len(args)
			fmt.Fprintf(&sb, "($%d, $1, $%d::jsonb, $%d, $%d, $%d, $%d, NOW(), NOW())", n+1, n+2, n+3, n+4, n+5, n+6)
			args = append(args, uuid.New(), string(b), float64(j), fmt.Sprintf("d%06d", j), j+1, me)
		}
		if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, sb.String(), args...); err != nil {
			t.Fatalf("rows: %v", err)
		}
	}
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `UPDATE data_tables SET next_row_no = $2 WHERE id = $1`, tb.Id, budgetRows+1); err != nil {
		t.Fatal(err)
	}
	// Made whole as the repair sweep makes a table filled at once, which has
	// the planner count it too.
	if err := rowQuery.IndexTable(ctx, tb.Id); err != nil {
		t.Fatalf("index: %v", err)
	}
	return budgetTable{ctx: ctx, me: me, tb: tb, name: name, amount: amount, due: due, done: done, status: status}
}

func TestRowQueryWindowsReadOnlyAlongTheirIndexes(t *testing.T) {
	b := newBudgetTable(t)
	ctx, me, tb, name, amount, due, done, status := b.ctx, b.me, b.tb, b.name, b.amount, b.due, b.done, b.status

	f := func(m *model.Field) string { return "field:" + m.Id.String() }
	text := func(s string) []query.Value { return []query.Value{{Text: s}} }
	blocked := &query.Filter{Field: f(status), Operator: query.OperatorIs, Values: text("Blocked")}
	specs := []struct {
		name string
		spec query.Spec
	}{
		{"every row in the table's order", query.Spec{}},
		{"a status", query.Spec{Filter: blocked}},
		{"by amount, most first", query.Spec{Sorts: []query.Sort{{Field: f(amount), Desc: true}}}},
		{"by name", query.Spec{Sorts: []query.Sort{{Field: f(name)}}}},
		{"due in a range, by due", query.Spec{
			Filter: &query.Filter{Field: f(due), Operator: query.OperatorBetween, Values: []query.Value{{Text: "2026-03-01"}, {Text: "2026-06-30"}}},
			Sorts:  []query.Sort{{Field: f(due)}}}},
		{"a status, by due", query.Spec{Filter: blocked, Sorts: []query.Sort{{Field: f(due)}}}},
		{"by status, then amount", query.Spec{Sorts: []query.Sort{{Field: f(status)}, {Field: f(amount), Desc: true}}}},
		{"by done, then amount", query.Spec{Sorts: []query.Sort{{Field: f(done), Desc: true}, {Field: f(amount)}}}},
	}
	who := accessBusiness.Subject{UserUUID: me.String()}
	for _, s := range specs {
		s.spec.Scope = query.Scope{Kind: query.ScopeTable, ID: tb.Id.String()}
		before := readsNow(ctx, t)
		// A first window, and three after it.
		var cursor query.Cursor
		windows := 0
		for ; windows < 4; windows++ {
			res, err := query.Queries.Run(ctx, who, s.spec, query.Page{Size: windowSize, After: cursor})
			if err != nil {
				t.Fatalf("%s: %v", s.name, err)
			}
			if len(res.IDs) != windowSize || res.Next == "" {
				t.Fatalf("%s: window %d has %d rows (next %q)", s.name, windows, len(res.IDs), res.Next)
			}
			cursor = res.Next
		}
		after := readsNow(ctx, t)
		if n := after.seqScans - before.seqScans; n != 0 {
			t.Errorf("%s: %d windows read the rows or the cell index through, %d times, %d tuples", s.name, windows, n, after.seqTuples-before.seqTuples)
		}
		if n, most := after.indexTuples-before.indexTuples, int64(windows*(windowSize+1)*entriesPerRow); n > most {
			t.Errorf("%s: %d windows read %d index entries, past the budget of %d", s.name, windows, n, most)
		}
	}
}
