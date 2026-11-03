//go:build integration

package business_test

// A reader that jumps (a scrollbar dragged far, airtable-04) reads the window
// at its place at once, never the windows before it. It holds the rows the
// windows read in order from the first hold there, its cursor goes on from
// them as any window's does, and it never counts. So on each path a window
// takes: an index walk, a list, the one query and the evaluator's read.
//
// Run: integ_rel.sh <worktree> <log> '^TestAWindowAtAPlaceIsTheRowsThere$' with PKGS='./business/RowQuery/'

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	dataTable "github.com/akashc777/OneCamp/business/DataTable"
	query "github.com/akashc777/OneCamp/business/QuerySpec"
	rowQuery "github.com/akashc777/OneCamp/business/RowQuery"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// cursorKindOf is the kind a window's cursor names ("walk", "sql", "list",
// "worked"): the path that answered it.
func cursorKindOf(t *testing.T, c query.Cursor) string {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(string(c))
	var m struct {
		K string `json:"k"`
	}
	if err == nil {
		err = json.Unmarshal(b, &m)
	}
	if err != nil {
		t.Fatalf("a cursor that isn't one: %q (%v)", c, err)
	}
	return m.K
}

func TestAWindowAtAPlaceIsTheRowsThere(t *testing.T) {
	b := newBudgetTable(t)
	integration.SetupRedis(t)
	ctx := b.ctx
	f := func(m *model.Field) string { return "field:" + m.Id.String() }
	who := accessBusiness.Subject{UserUUID: b.me.String()}
	l := listReader{t: t, ctx: ctx, who: who}

	// jump reads the window at a place, asked to count as a first window is.
	jump := func(name string, spec query.Spec, at int) query.Result {
		t.Helper()
		res, err := query.Queries.Run(rowQuery.WithCount(ctx), who, spec, query.Page{Size: windowSize, At: at})
		if err != nil {
			t.Fatalf("%s: the window at %d: %v", name, at, err)
		}
		if res.Total != rowQuery.NotCounted || res.Summaries != nil || res.Groups != nil {
			t.Fatalf("%s: the window at %d counted (total %d, summaries %v, groups %v)", name, at, res.Total, res.Summaries, res.Groups)
		}
		return res
	}
	// on is the rows of the n windows after res, read by their cursors.
	on := func(spec query.Spec, res query.Result, n int) []string {
		t.Helper()
		var got []string
		for i := 0; i < n && res.Next != ""; i++ {
			res = l.window(spec, res.Next)
			got = append(got, res.IDs...)
		}
		return got
	}
	// check jumps along every row of the view, all (the windows in order),
	// and past its last; the cursor after a jump is kind's.
	check := func(name string, spec query.Spec, all []string, kind string) {
		t.Helper()
		for _, at := range []int{1, 3*windowSize + 7, len(all) / 2, len(all) - windowSize/2, len(all) - 1} {
			res := jump(name, spec, at)
			l.same(fmt.Sprintf("%s: the window at %d", name, at), res.IDs, all[at:min(at+windowSize, len(all))])
			read := at + len(res.IDs)
			l.same(fmt.Sprintf("%s: on from the window at %d", name, at), on(spec, res, 2), all[read:min(read+2*windowSize, len(all))])
			if res.Next != "" && cursorKindOf(t, res.Next) != kind {
				t.Fatalf("%s: the window at %d went on as %q, not the %s path", name, at, cursorKindOf(t, res.Next), kind)
			}
		}
		if res := jump(name, spec, len(all)+3); len(res.IDs) != 0 || res.Next != "" {
			t.Fatalf("%s: past its last row, %d rows and a cursor %q", name, len(res.IDs), res.Next)
		}
	}

	scope := query.Scope{Kind: query.ScopeTable, ID: b.tb.Id.String()}
	text := func(s string) []query.Value { return []query.Value{{Text: s}} }
	for _, s := range []struct {
		name string
		spec query.Spec
	}{
		{"the table's own order", query.Spec{Scope: scope}},
		{"by amount, most first", query.Spec{Scope: scope, Sorts: []query.Sort{{Field: f(b.amount), Desc: true}}}},
		{"blocked deals by due", query.Spec{Scope: scope, Filter: &query.Filter{Field: f(b.status), Operator: query.OperatorIs, Values: text("Blocked")}, Sorts: []query.Sort{{Field: f(b.due)}}}},
		{"by status, then done, then amount", query.Spec{Scope: scope, Sorts: []query.Sort{{Field: f(b.status)}, {Field: f(b.done), Desc: true}, {Field: f(b.amount)}}}},
		{"deals with a 7, by name", query.Spec{Scope: scope, Search: "7", Sorts: []query.Sort{{Field: f(b.name)}}}},
	} {
		all := l.oneQuery(s.spec, "")
		if len(all) < 10*windowSize {
			t.Fatalf("%s: %d rows, too few to jump in", s.name, len(all))
		}
		// A jump is the one query's from its place: a walk or a list would
		// read every row before it to get there.
		check(s.name, s.spec, all, "sql")
	}

	// Past what a count reaches, no place is known: a bad page.
	if _, err := query.Queries.Run(ctx, who, query.Spec{Scope: scope}, query.Page{Size: windowSize, At: rowQuery.CountCap + 1}); !errors.Is(err, query.ErrBadSpec) {
		t.Fatalf("a window past the count's cap: %v", err)
	}

	// A table whose index isn't whole is the evaluator's: its windows are
	// places already, and a jump starts there.
	owner := dataTable.Actor{UserID: b.me}
	small, err := dataTable.CreateTable(ctx, dataTable.TableInput{Name: "Small", Visibility: "workspace"}, owner)
	if err != nil {
		t.Fatal(err)
	}
	amount, err := dataTable.CreateField(ctx, small.Id, dataTable.FieldInput{Name: "Amount", Type: model.FieldNumber}, owner)
	if err != nil {
		t.Fatal(err)
	}
	const smallRows = 1234
	var sb strings.Builder
	args := []interface{}{small.Id}
	sb.WriteString(`INSERT INTO data_table_rows (id, table_id, "values", position, position_key, row_no, created_by, created_at, updated_at) VALUES `)
	for j := 0; j < smallRows; j++ {
		cells, _ := json.Marshal(map[string]interface{}{amount.Id.String(): float64((j * 104729) % 100000)})
		if j > 0 {
			sb.WriteString(", ")
		}
		n := len(args)
		fmt.Fprintf(&sb, "($%d, $1, $%d::jsonb, $%d, $%d, $%d, $%d, NOW(), NOW())", n+1, n+2, n+3, n+4, n+5, n+6)
		args = append(args, uuid.New(), string(cells), float64(j), fmt.Sprintf("s%06d", j), j+1, b.me)
	}
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, sb.String(), args...); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `UPDATE data_tables SET next_row_no = $2, cell_index_at = NULL WHERE id = $1`, small.Id, smallRows+1); err != nil {
		t.Fatal(err)
	}
	spec := query.Spec{Scope: query.Scope{Kind: query.ScopeTable, ID: small.Id.String()}, Sorts: []query.Sort{{Field: "field:" + amount.Id.String(), Desc: true}}}
	all := l.oneQuery(spec, "")
	if len(all) != smallRows {
		t.Fatalf("the evaluator's read: %d rows, want %d", len(all), smallRows)
	}
	check("the evaluator's read", spec, all, "worked")
}
