//go:build integration

package business_test

// A view no index walks lists its order once (tables T5d): its first window
// is the one query, its second lists the order from there, and the windows
// after it read the list, not the table. They give the one query's rows in
// its order, and after a write they go on from the last row shown as the one
// query goes on, with the write in.
//
// Run: integ_rel.sh <worktree> <log> '^(TestRowQueryListsAViewsOrderOnce|TestAListedViewOfLinksHearsTheOtherTable)$' with PKGS='./business/RowQuery/'

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"slices"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	dataTable "github.com/akashc777/OneCamp/business/DataTable"
	query "github.com/akashc777/OneCamp/business/QuerySpec"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	redisInit "github.com/akashc777/OneCamp/initializers/redis"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// listReader reads a view's windows as one reader.
type listReader struct {
	t   *testing.T
	ctx context.Context
	who accessBusiness.Subject
}

// window reads one window after cursor.
func (l listReader) window(spec query.Spec, cursor query.Cursor) query.Result {
	l.t.Helper()
	res, err := query.Queries.Run(l.ctx, l.who, spec, query.Page{Size: windowSize, After: cursor})
	if err != nil {
		l.t.Fatalf("window after %q: %v", cursor, err)
	}
	return res
}

// rest reads every window after res.
func (l listReader) rest(spec query.Spec, res query.Result) []string {
	l.t.Helper()
	var got []string
	for res.Next != "" {
		res = l.window(spec, res.Next)
		got = append(got, res.IDs...)
	}
	return got
}

// oneQuery is the view's rows after the row shown last ("" from the first),
// as the one query lists them: every window without Redis.
func (l listReader) oneQuery(spec query.Spec, last string) []string {
	l.t.Helper()
	was := redisInit.RedisClient
	redisInit.RedisClient = nil
	defer func() { redisInit.RedisClient = was }()
	res := l.window(spec, "")
	all := append(res.IDs, l.rest(spec, res)...)
	for i, id := range all {
		if id == last {
			return all[i+1:]
		}
	}
	if last != "" {
		l.t.Fatalf("the row shown last, %s, isn't in the view", last)
	}
	return all
}

// same fails the test when got isn't the one query's rows, want, in order.
func (l listReader) same(name string, got, want []string) {
	l.t.Helper()
	if len(got) != len(want) {
		l.t.Fatalf("%s: %d rows, the one query %d", name, len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			l.t.Fatalf("%s: row %d is %s, the one query's %s", name, i, got[i], want[i])
		}
	}
}

func TestRowQueryListsAViewsOrderOnce(t *testing.T) {
	b := newBudgetTable(t)
	integration.SetupRedis(t)
	ctx := b.ctx
	f := func(m *model.Field) string { return "field:" + m.Id.String() }
	l := listReader{t: t, ctx: ctx, who: accessBusiness.Subject{UserUUID: b.me.String()}}
	scope := query.Scope{Kind: query.ScopeTable, ID: b.tb.Id.String()}
	specs := []struct {
		name string
		spec query.Spec
	}{
		{"by status, then done, then amount", query.Spec{Scope: scope, Sorts: []query.Sort{{Field: f(b.status)}, {Field: f(b.done), Desc: true}, {Field: f(b.amount)}}}},
		{"deals with a 7, by amount, then name", query.Spec{Scope: scope, Search: "7", Sorts: []query.Sort{{Field: f(b.amount)}, {Field: f(b.name)}}}},
	}

	for _, s := range specs {
		want := l.oneQuery(s.spec, "")
		if len(want) < 20*windowSize {
			t.Fatalf("%s: %d rows, too few to list", s.name, len(want))
		}
		var got []string
		res := l.window(s.spec, "")
		got = append(got, res.IDs...)
		res = l.window(s.spec, res.Next) // lists the rest
		got = append(got, res.IDs...)
		before := readsNow(ctx, t)
		const listed = 10
		for i := 0; i < listed; i++ {
			res = l.window(s.spec, res.Next)
			got = append(got, res.IDs...)
		}
		after := readsNow(ctx, t)
		if n := after.seqScans - before.seqScans; n != 0 {
			t.Errorf("%s: %d listed windows read the rows or the cell index through, %d times, %d tuples", s.name, listed, n, after.seqTuples-before.seqTuples)
		}
		// Each window reads its last row's keys, and nothing else of the table.
		if n, most := after.indexTuples-before.indexTuples, int64(listed*windowSize); n > most {
			t.Errorf("%s: %d listed windows read %d index entries, past %d", s.name, listed, n, most)
		}
		l.same(s.name, append(got, l.rest(s.spec, res)...), want)
	}

	// A write while someone scrolls: the windows after it go on from the last
	// row shown, as the one query does with the write in. The row moved is
	// one not shown yet, which the write takes to the view's start.
	s := specs[0]
	res := l.window(s.spec, "")
	shown := append([]string{}, res.IDs...)
	for i := 0; i < 3; i++ {
		res = l.window(s.spec, res.Next)
		shown = append(shown, res.IDs...)
	}
	moved := uuid.MustParse(l.oneQuery(s.spec, shown[len(shown)-1])[windowSize])
	if _, err := dataTable.PatchRow(ctx, b.tb.Id, moved, map[string]interface{}{b.status.Id.String(): "Todo", b.done.Id.String(): true, b.amount.Id.String(): -1}, dataTable.Actor{UserID: b.me}); err != nil {
		t.Fatalf("move a row: %v", err)
	}
	want := l.oneQuery(s.spec, shown[len(shown)-1])
	got := l.rest(s.spec, res)
	l.same(s.name+", after a write", got, want)
	for _, id := range got {
		if id == moved.String() {
			t.Errorf("the row moved before the rows shown was shown again after them")
		}
	}

	// A cursor is its view's alone.
	other := query.Spec{Scope: scope, Sorts: []query.Sort{{Field: f(b.amount)}, {Field: f(b.done)}, {Field: f(b.status)}}}
	first := l.window(s.spec, "")
	second := l.window(s.spec, first.Next)
	if _, err := query.Queries.Run(ctx, l.who, other, query.Page{Size: windowSize, After: second.Next}); err == nil {
		t.Error("a list's cursor read another view's rows")
	}
	// Nor can a cursor name another view's list: the other view's cursor,
	// with this view's list written into it, reads the other view's rows.
	theirs := l.window(other, l.window(other, "").Next)
	forged := withListKey(t, theirs.Next, second.Next)
	if got, want := l.window(other, forged), l.window(other, theirs.Next); !slices.Equal(got.IDs, want.IDs) {
		t.Errorf("a cursor naming another view's list read %v, not the view's next rows", got.IDs[:min(3, len(got.IDs))])
	}
}

// withListKey is the list cursor c naming the list another list cursor
// names, as a reader could write it: a cursor is the reader's to send.
func withListKey(t *testing.T, c, other query.Cursor) query.Cursor {
	t.Helper()
	read := func(c query.Cursor) map[string]interface{} {
		b, err := base64.RawURLEncoding.DecodeString(string(c))
		var m map[string]interface{}
		if err == nil {
			err = json.Unmarshal(b, &m)
		}
		if err != nil || m["k"] != "list" {
			t.Fatalf("not a list's cursor: %s (%v)", c, err)
		}
		return m
	}
	m := read(c)
	m["v"].([]interface{})[0] = read(other)["v"].([]interface{})[0]
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return query.Cursor(base64.RawURLEncoding.EncodeToString(b))
}

// A filter on links reads them, and a link made from the other table is a
// write to that table alone, which this table's ledger never sees: the
// listed windows of a view filtered by links go on with the link in, as the
// one query does.
func TestAListedViewOfLinksHearsTheOtherTable(t *testing.T) {
	b := newBudgetTable(t)
	integration.SetupRedis(t)
	ctx := b.ctx
	owner := dataTable.Actor{UserID: b.me}
	l := listReader{t: t, ctx: ctx, who: accessBusiness.Subject{UserUUID: b.me.String()}}
	tags, err := dataTable.CreateTable(ctx, dataTable.TableInput{Name: "Tags", Visibility: "workspace"}, owner)
	if err != nil {
		t.Fatal(err)
	}
	rel, err := dataTable.CreateField(ctx, b.tb.Id, dataTable.FieldInput{Name: "Tags", Type: model.FieldRelation,
		Config: map[string]interface{}{"relation_target": "table", "table_id": tags.Id.String(), "two_way": true}}, owner)
	if err != nil {
		t.Fatal(err)
	}
	var shows *model.Field
	fields, err := model.ListFields(ctx, tags.Id)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fields {
		var cfg struct {
			InverseOf string `json:"inverse_of"`
		}
		if json.Unmarshal([]byte(f.Config), &cfg) == nil && cfg.InverseOf == rel.Id.String() {
			shows = f
		}
	}
	if shows == nil {
		t.Fatal("Tags has no field showing the deals' tags")
	}
	tag, err := dataTable.CreateRow(ctx, tags.Id, dataTable.RowInput{Values: map[string]interface{}{}}, owner)
	if err != nil {
		t.Fatal(err)
	}
	// A third of the deals are tagged, straight in the database.
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `INSERT INTO data_table_links (field_id, from_row, to_row)
		SELECT $1, id, $2 FROM data_table_rows WHERE table_id = $3 AND row_no % 3 = 0`, rel.Id, tag.Id, b.tb.Id); err != nil {
		t.Fatal(err)
	}
	f := func(m *model.Field) string { return "field:" + m.Id.String() }
	scope := query.Scope{Kind: query.ScopeTable, ID: b.tb.Id.String()}
	sorts := []query.Sort{{Field: f(b.status)}, {Field: f(b.done), Desc: true}, {Field: f(b.amount)}}
	tagged := query.Spec{Scope: scope, Sorts: sorts, Filter: &query.Filter{Field: f(rel), Operator: query.OperatorIs, Values: []query.Value{{ID: tag.Id.String()}}}}

	res := l.window(tagged, "")
	shown := append([]string{}, res.IDs...)
	res = l.window(tagged, res.Next) // lists the rest
	shown = append(shown, res.IDs...)

	// The last untagged deal in the order: tagged now, from Tags' side, it
	// comes after every deal shown (tagged deals may follow it).
	all := l.oneQuery(query.Spec{Scope: scope, Sorts: sorts}, "")
	var deal uuid.UUID
	for i := len(all) - 1; i >= 0 && deal == uuid.Nil; i-- {
		var linked bool
		if err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM data_table_links WHERE field_id = $1 AND from_row = $2)`,
			rel.Id, all[i]).Scan(&linked); err != nil {
			t.Fatal(err)
		}
		if !linked {
			deal = uuid.MustParse(all[i])
		}
	}
	if _, _, err := dataTable.ChangeLinks(ctx, tags.Id, tag.Id, shows.Id, []uuid.UUID{deal}, nil, owner); err != nil {
		t.Fatalf("tag a deal from Tags: %v", err)
	}
	want := l.oneQuery(tagged, shown[len(shown)-1])
	if !slices.Contains(want, deal.String()) {
		t.Fatalf("the one query doesn't list the deal just tagged after the deals shown")
	}
	l.same("after a deal was tagged from Tags", l.rest(tagged, res), want)
}
