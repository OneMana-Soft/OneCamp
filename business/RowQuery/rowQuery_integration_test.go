//go:build integration

package business_test

// Row Query's SQL means what its evaluator means (tables A3's property
// test): on Postgres 12 with every migration, random specs over a table of
// random rows give, window after window, the rows the evaluator gives over
// the same rows, in the same order, and the same total. The rows hold what a
// table holds after years of imports: numbers written as text, 30 February,
// times in Z and with offsets, blank and padded text, options no field has,
// people as bare ids, cells missing.
//
// Run: integ_rel.sh <worktree> <log> 'TestRowQueryMeansWhatItsEvaluatorMeans' with PKGS='./business/RowQuery/'

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"reflect"
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

const randomRows = 10000

// statementsGetAMinute gives each statement a minute until the test ends,
// where production gives it 3 s (ConnectPostgres's dbTimeout). The tests here
// check what Row Query's SQL means and how much it reads, never how fast it
// runs, and on a loaded machine a window ran past the 3 s in runs that pass
// alone: the evaluator test in batches 12 and 13, the window budget in 14.
func statementsGetAMinute(t *testing.T) {
	t.Helper()
	saved := postgresInit.DBConn.DBTimeout
	postgresInit.DBConn.DBTimeout = time.Minute
	t.Cleanup(func() { postgresInit.DBConn.DBTimeout = saved })
}

func TestRowQueryMeansWhatItsEvaluatorMeans(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("connect: %v", err)
	}
	statementsGetAMinute(t)
	// A write reads its row back with the people cells named, from Dgraph.
	integration.SetupDgraph(t)
	user := func(name string) uuid.UUID {
		id := uuid.New()
		if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, name+"@example.test", name, name); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
		return id
	}
	me, other := user("owner"), user("other")
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
	name := add("Deal name", model.FieldText, nil)
	amount := add("Amount", model.FieldNumber, nil)
	due := add("Due", model.FieldDate, nil)
	won := add("Won", model.FieldCheckbox, nil)
	stage := add("Stage", model.FieldStatus, map[string]interface{}{"options": []interface{}{
		map[string]interface{}{"label": "New", "group": "todo"}, map[string]interface{}{"label": "Talking", "group": "doing"}, map[string]interface{}{"label": "Won", "group": "done"}}})
	tags := add("Tags", model.FieldMultiSelect, map[string]interface{}{"options": []interface{}{
		map[string]interface{}{"label": "hot"}, map[string]interface{}{"label": "big"}, map[string]interface{}{"label": "new"}}})
	owners := add("Owners", model.FieldPeople, nil)
	// Who last changed a row: rows written straight in have no one there, so
	// each reads as whoever made it (idsOf, idsExpr).
	changedBy := add("Changed by", model.FieldModifiedBy, nil)

	// The rows, random but the same every run, written straight in.
	rnd := rand.New(rand.NewSource(20261019))
	// Text that sorts by its natural key (T5c): numbers in it, accents written
	// either way, ß; and two long texts past the t index's bound whose keys,
	// cut to 256 code points, tie, so the table's order breaks it.
	long := incompressible(rand.New(rand.NewSource(7)), 3200)
	words := []string{"Booth", "booth", "Banners", "power cables", "  padded  ", "", "Café", "BIG deal", "quote's", "100%_off",
		"Row 2", "row 10", "Row 007", "Cafe" + string(rune(0x301)), "STRASSE", "Straße", "x²", "v1.10", "v1.9", long, long + " b"}
	pick := func(xs ...interface{}) interface{} { return xs[rnd.Intn(len(xs))] }
	day := func() string {
		return time.Date(2026, time.Month(1+rnd.Intn(12)), 1+rnd.Intn(28), 0, 0, 0, 0, time.UTC).Format("2006-01-02")
	}
	for i := 0; i < randomRows; i += 500 {
		var sb strings.Builder
		args := []interface{}{tb.Id}
		sb.WriteString(`INSERT INTO data_table_rows (id, table_id, "values", position, position_key, row_no, created_by, created_at, updated_at) VALUES `)
		for j := i; j < i+500; j++ {
			cells := map[string]interface{}{}
			set := func(f *model.Field, v interface{}) {
				if v != nil {
					cells[f.Id.String()] = v
				}
			}
			set(name, pick(nil, words[rnd.Intn(len(words))], words[rnd.Intn(len(words))]))
			set(amount, pick(nil, float64(rnd.Intn(2000))-200, float64(rnd.Intn(100))/4, "1200", "lots", 0.0))
			set(due, pick(nil, day(), day(), "2026-02-30", "2026-10-18T23:30:00Z", "2026-10-18T20:00:00+05:30", "2026-10-19T00:15:00.250Z", "soon", "2026-10-18T20:00"))
			set(won, pick(nil, true, false, "true"))
			set(stage, pick(nil, "New", "Talking", "Won", "won", "Lost", " New "))
			set(tags, pick(nil, []interface{}{"hot"}, []interface{}{"big", "hot"}, []interface{}{"new", "gone"}, []interface{}{}, "hot"))
			set(owners, pick(nil, []interface{}{map[string]interface{}{"id": me.String(), "type": "user"}},
				[]interface{}{map[string]interface{}{"id": other.String()}, map[string]interface{}{"id": me.String()}}, []interface{}{other.String()}, []interface{}{}))
			b, _ := json.Marshal(cells)
			if j > i {
				sb.WriteString(", ")
			}
			n := len(args)
			fmt.Fprintf(&sb, "($%d, $1, $%d::jsonb, $%d, $%d, $%d, $%d, $%d, $%d)", n+1, n+2, n+3, n+4, n+5, n+6, n+7, n+8)
			made := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(rnd.Intn(300*24)) * time.Hour)
			var by interface{} = me
			if rnd.Intn(3) == 0 {
				by = other
			}
			// Some rows share a place: ties fall to their ids.
			args = append(args, uuid.New(), string(b), float64(j), fmt.Sprintf("d%05d", j-j%7*rnd.Intn(2)), j+1, by, made, made)
		}
		if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, sb.String(), args...); err != nil {
			t.Fatalf("rows: %v", err)
		}
	}
	// The table's next row number goes past those written, as a write would
	// have moved it (the numbers are unique within a table).
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `UPDATE data_tables SET next_row_no =
		(SELECT COALESCE(MAX(row_no), 0) + 1 FROM data_table_rows WHERE table_id = $1) WHERE id = $1`, tb.Id); err != nil {
		t.Fatalf("row numbers: %v", err)
	}
	// Rows written straight in, as a table had before the index: made whole
	// the way the repair sweep makes it.
	if err := rowQuery.IndexTable(ctx, tb.Id); err != nil {
		t.Fatalf("index: %v", err)
	}
	all, err := model.ListFields(ctx, tb.Id)
	if err != nil {
		t.Fatal(err)
	}
	load := func() []*model.Row {
		var stored []*model.Row
		key, after := "", uuid.Nil
		for {
			page, err := model.ListRowsAfter(ctx, tb.Id, key, after, 1000)
			if err != nil {
				t.Fatal(err)
			}
			stored = append(stored, page...)
			if len(page) < 1000 {
				return stored
			}
			key, after = page[len(page)-1].PositionKey, page[len(page)-1].Id
		}
	}
	if n := len(load()); n != randomRows {
		t.Fatalf("%d rows stored", n)
	}

	// Random specs.
	f := func(fl *model.Field) string { return "field:" + fl.Id.String() }
	textV := func() query.Value {
		return query.Value{Text: pick("b", "Booth", "PADDED", "café", "%_", "'", "power", "zzz", "row 2", long, long[3000:]).(string)}
	}
	numV := func() query.Value { n := float64(rnd.Intn(2200) - 300); return query.Value{Number: &n} }
	dayV := func() query.Value {
		return pick(query.Value{Text: day()}, query.Value{Relative: query.RelativeThisMonth}, query.Value{Relative: query.RelativeLastWeek},
			query.Value{Relative: query.RelativeToday}, query.Value{Text: "2026-10"}).(query.Value)
	}
	condition := func() query.Filter {
		switch rnd.Intn(7) {
		case 0:
			op := pick(query.OperatorIs, query.OperatorIsNot, query.OperatorContains, query.OperatorNotContains, query.OperatorStartsWith, query.OperatorEndsWith, query.OperatorEmpty, query.OperatorNotEmpty).(string)
			c := query.Filter{Field: f(name), Operator: op}
			if op != query.OperatorEmpty && op != query.OperatorNotEmpty {
				c.Values = []query.Value{textV()}
			}
			return c
		case 1:
			op := pick(query.OperatorIs, query.OperatorIsNot, query.OperatorGt, query.OperatorGte, query.OperatorLt, query.OperatorLte, query.OperatorBetween, query.OperatorEmpty, query.OperatorNotEmpty).(string)
			c := query.Filter{Field: f(amount), Operator: op}
			switch op {
			case query.OperatorBetween:
				c.Values = []query.Value{numV(), numV()}
			case query.OperatorEmpty, query.OperatorNotEmpty:
			default:
				c.Values = []query.Value{numV()}
			}
			return c
		case 2:
			op := pick(query.OperatorIs, query.OperatorBefore, query.OperatorAfter, query.OperatorOnOrBefore, query.OperatorOnOrAfter, query.OperatorBetween, query.OperatorEmpty, query.OperatorNotEmpty).(string)
			c := query.Filter{Field: f(due), Operator: op}
			switch op {
			case query.OperatorBetween:
				c.Values = []query.Value{dayV(), dayV()}
			case query.OperatorEmpty, query.OperatorNotEmpty:
			default:
				c.Values = []query.Value{dayV()}
			}
			return c
		case 3:
			return query.Filter{Field: f(won), Operator: pick(query.OperatorChecked, query.OperatorUnchecked).(string)}
		case 4:
			op := pick(query.OperatorIs, query.OperatorNotIn, query.OperatorStatusGroupIs, query.OperatorEmpty).(string)
			c := query.Filter{Field: f(stage), Operator: op}
			switch op {
			case query.OperatorStatusGroupIs:
				c.Values = []query.Value{{Text: pick("todo", "doing", "done").(string)}}
			case query.OperatorEmpty:
			default:
				c.Values = []query.Value{{Text: pick("New", "won", "Lost").(string)}, {Text: "Talking"}}
			}
			return c
		case 5:
			op := pick(query.OperatorIn, query.OperatorNotIn, query.OperatorAllOf, query.OperatorIsExactly, query.OperatorContains, query.OperatorEmpty).(string)
			c := query.Filter{Field: f(tags), Operator: op}
			switch op {
			case query.OperatorEmpty:
			case query.OperatorContains:
				c.Values = []query.Value{{Text: pick("o", "ig", "NEW").(string)}}
			default:
				c.Values = []query.Value{{Text: pick("hot", "big", "new", "gone").(string)}, {Text: pick("hot", "big").(string)}}
			}
			return c
		default:
			op := pick(query.OperatorIn, query.OperatorNotIn, query.OperatorAllOf, query.OperatorEmpty, query.OperatorNotEmpty).(string)
			c := query.Filter{Field: f(pick(owners, changedBy).(*model.Field)), Operator: op}
			if op != query.OperatorEmpty && op != query.OperatorNotEmpty {
				c.Values = []query.Value{{Text: query.Me}, {ID: other.String()}}[:1+rnd.Intn(2)]
			}
			return c
		}
	}
	var tree func(depth int) query.Filter
	tree = func(depth int) query.Filter {
		if depth >= 3 || rnd.Intn(3) == 0 {
			return condition()
		}
		g := query.Filter{Op: pick(query.OpAnd, query.OpOr, query.OpNot).(string)}
		n := 1
		if g.Op != query.OpNot {
			n = 1 + rnd.Intn(3)
		}
		for i := 0; i < n; i++ {
			g.Filters = append(g.Filters, tree(depth+1))
		}
		return g
	}
	sortable := []*model.Field{name, amount, due, won, stage}
	zone, _ := time.LoadLocation("Asia/Kolkata")
	qctx := rowQuery.WithZone(ctx, zone)
	who := accessBusiness.Subject{UserUUID: me.String()}
	mixed, budget := false, rowQuery.WalkBudget
	// Summaries without a budget: a read past it on a busy machine would leave
	// them out, which this test would read as the SQL and the evaluator parting.
	sumBudget := rowQuery.SummaryBudget
	rowQuery.SummaryBudget = 0
	defer func() { rowQuery.SummaryBudget = sumBudget }()
	// Summaries a reader might ask of each kind of column, picked by a source
	// of their own, so the specs stay the ones this test always drew.
	anyFns := []string{"count", "empty", "filled", "pct_empty", "pct_filled"}
	with := func(more ...string) []string { return append(append([]string{}, anyFns...), more...) }
	fnsOf := map[*model.Field][]string{
		name: with("unique"), amount: with("unique", "sum", "average", "median", "min", "max", "range"),
		due: with("unique", "earliest", "latest", "range"), won: with("unique", "checked", "unchecked"),
		stage: with("unique"), tags: with(), owners: with(), changedBy: with("unique"),
	}
	summed := []*model.Field{name, amount, due, won, stage, tags, owners, changedBy}
	srnd := rand.New(rand.NewSource(20261027))
	summarise := func() []query.SummaryRequest {
		var reqs []query.SummaryRequest
		for _, k := range srnd.Perm(len(summed))[:1+srnd.Intn(4)] {
			fns := fnsOf[summed[k]]
			var picked []string
			for _, j := range srnd.Perm(len(fns))[:1+srnd.Intn(len(fns))] {
				picked = append(picked, fns[j])
			}
			reqs = append(reqs, query.SummaryRequest{Field: f(summed[k]), Fns: picked})
		}
		return reqs
	}
	// The same summaries, a sum or a mean as near as two ways of adding
	// floats come.
	sameSums := func(a, b query.Summaries) bool {
		var x, y map[string]map[string]interface{}
		ab, _ := json.Marshal(a)
		bb, _ := json.Marshal(b)
		if json.Unmarshal(ab, &x) != nil || json.Unmarshal(bb, &y) != nil || len(x) != len(y) {
			return false
		}
		for field, fns := range x {
			if len(fns) != len(y[field]) {
				return false
			}
			for fn, v := range fns {
				p, isNum := v.(float64)
				q, alsoNum := y[field][fn].(float64)
				if isNum && alsoNum {
					if math.Abs(p-q) > 1e-9*math.Max(1, math.Max(math.Abs(p), math.Abs(q))) {
						return false
					}
				} else if !reflect.DeepEqual(v, y[field][fn]) {
					return false
				}
			}
		}
		return true
	}
	// Groups a reader might ask, 1 to 3 levels deep, by a source of their own
	// too: a date by any period, mostly a longer one than a day.
	grnd := rand.New(rand.NewSource(20261028))
	groupable := []*model.Field{amount, due, won, stage}
	periods := []string{"", query.GranularityWeek, query.GranularityMonth, query.GranularityMonth, query.GranularityQuarter, query.GranularityYear}
	groupings := func() []query.Group {
		var gs []query.Group
		for _, k := range grnd.Perm(len(groupable))[:1+grnd.Intn(3)] {
			g := query.Group{Field: f(groupable[k]), Desc: grnd.Intn(2) == 0}
			if groupable[k] == due {
				g.Granularity = periods[grnd.Intn(len(periods))]
			}
			gs = append(gs, g)
		}
		return gs
	}
	// The same groups: keys, counts and summaries, in order.
	sameGroups := func(a, b []query.GroupCount) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if strings.Join(a[i].Key, "\x00") != strings.Join(b[i].Key, "\x00") || len(a[i].Key) != len(b[i].Key) || a[i].Count != b[i].Count || !sameSums(a[i].Summaries, b[i].Summaries) {
				return false
			}
		}
		return true
	}
	check := func(when string, specs int) {
		stored := load()
		for i := 0; i < specs; i++ {
			started := time.Now()
			spec := query.Spec{Scope: query.Scope{Kind: query.ScopeTable, ID: tb.Id.String()}}
			if rnd.Intn(5) > 0 {
				fl := tree(1)
				spec.Filter = &fl
			}
			for s := rnd.Intn(3); s > 0; s-- {
				spec.Sorts = append(spec.Sorts, query.Sort{Field: f(sortable[rnd.Intn(len(sortable))]), Desc: rnd.Intn(2) == 0})
			}
			if rnd.Intn(6) == 0 {
				spec.Search = pick("booth", "hot", "caf", "power b").(string)
			}
			if srnd.Intn(2) == 0 {
				spec.Summaries = summarise()
			}
			if grnd.Intn(3) == 0 {
				spec.Groups = groupings()
			}
			want, wantSums, wantGroups, err := rowQuery.Oracle(spec, all, stored, me, time.Now(), zone)
			if errors.Is(err, rowQuery.ErrCantQuery) && len(spec.Groups) > 0 {
				// More groups than the counts answer: refused by both.
				if _, err := query.Queries.Run(rowQuery.WithCount(qctx), who, spec, query.Page{Size: 40}); !errors.Is(err, rowQuery.ErrCantQuery) {
					b, _ := json.Marshal(spec)
					t.Fatalf("spec %d: the evaluator refused its groups, the SQL answered (%v)\n%s", i, err, b)
				}
				continue
			}
			if err == nil && len(wantGroups) > 0 && grnd.Intn(2) == 0 {
				// The reader collapses a few of the groups.
				for _, k := range grnd.Perm(len(wantGroups))[:1+grnd.Intn(min(3, len(wantGroups)))] {
					key := wantGroups[k].Key
					b, _ := json.Marshal(key)
					spec.Groups[len(key)-1].Collapsed = append(spec.Groups[len(key)-1].Collapsed, string(b))
				}
				want, wantSums, wantGroups, err = rowQuery.Oracle(spec, all, stored, me, time.Now(), zone)
			}
			if err != nil {
				t.Fatalf("spec %d, the evaluator: %v", i, err)
			}
			// Every row the spec keeps is counted, a collapsed group's too.
			kept := len(want)
			if len(wantGroups) > 0 {
				kept = 0
				for _, g := range wantGroups {
					if len(g.Key) == 1 {
						kept += g.Count
					}
				}
			}
			var got []string
			size := 40 + rnd.Intn(360)
			total := -1
			var gotSums query.Summaries
			var gotGroups []query.GroupCount
			var cursor query.Cursor
			for page := 0; ; page++ {
				at := qctx
				if cursor == "" {
					at = rowQuery.WithCount(qctx) // a first window asks for its count
				}
				if mixed {
					// Every other window's walk runs past its budget, so a
					// walked listing goes on as the one query from its cursor.
					rowQuery.WalkBudget = budget
					if page%2 == 1 {
						rowQuery.WalkBudget = time.Nanosecond
					}
				}
				res, err := query.Queries.Run(at, who, spec, query.Page{Size: size, After: cursor})
				if err != nil {
					b, _ := json.Marshal(spec)
					t.Fatalf("spec %d: %v\n%s", i, err, b)
				}
				if total < 0 {
					total = res.Total
					gotSums, gotGroups = res.Summaries, res.Groups
				}
				got = append(got, res.IDs...)
				if res.Next == "" {
					break
				}
				cursor = res.Next
				if page > randomRows {
					t.Fatalf("spec %d never ends", i)
				}
			}
			if took := time.Since(started); took > 5*time.Second {
				b, _ := json.Marshal(spec)
				t.Logf("spec %d took %v over %d windows of %d: %s", i, took.Round(time.Millisecond), (len(got)+size-1)/max(size, 1), size, b)
			}
			if strings.Join(got, ",") != strings.Join(want, ",") || total != kept {
				b, _ := json.Marshal(spec)
				first := 0
				for first < len(got) && first < len(want) && got[first] == want[first] {
					first++
				}
				t.Fatalf("%s, spec %d (window %d): SQL gave %d rows (total %d), the evaluator %d; they part at %d\n%s", when, i, size, len(got), total, len(want), first, b)
			}
			if !sameSums(gotSums, wantSums) {
				b, _ := json.Marshal(spec)
				g, _ := json.Marshal(gotSums)
				w, _ := json.Marshal(wantSums)
				t.Fatalf("%s, spec %d: SQL summed up\n%s\nthe evaluator\n%s\n%s", when, i, g, w, b)
			}
			if !sameGroups(gotGroups, wantGroups) {
				b, _ := json.Marshal(spec)
				g, _ := json.Marshal(gotGroups)
				w, _ := json.Marshal(wantGroups)
				t.Fatalf("%s, spec %d: SQL grouped\n%.2000s\nthe evaluator\n%.2000s\n%s", when, i, g, w, b)
			}
		}
	}
	check("indexed by the sweep", 150)

	// Writes through the ledger keep the index: edits, pastes, deletions, new
	// rows.
	edit := func(vals map[string]interface{}) {
		switch rnd.Intn(5) {
		case 0:
			vals[amount.Id.String()] = float64(rnd.Intn(2000) - 200)
		case 1:
			vals[stage.Id.String()] = pick("New", "Talking", "Won").(string)
		case 2:
			vals[tags.Id.String()] = pick([]interface{}{"hot"}, []interface{}{"big", "new"}, []interface{}{}).([]interface{})
		case 3:
			vals[won.Id.String()] = rnd.Intn(2) == 0
		default:
			vals[name.Id.String()] = words[rnd.Intn(len(words))]
		}
	}
	ids := load()
	for i := 0; i < 300; i++ {
		r := ids[rnd.Intn(len(ids))]
		vals := map[string]interface{}{}
		edit(vals)
		if _, err := dataTable.PatchRow(ctx, tb.Id, r.Id, vals, owner); err != nil {
			t.Fatalf("patch: %v", err)
		}
	}
	// A paste changes different cells of different rows in one change set:
	// the index replaces each changed cell's entry, and no other.
	for i := 0; i < 20; i++ {
		var patch []dataTable.PatchInput
		for _, k := range rnd.Perm(len(ids))[:8] {
			vals := map[string]interface{}{}
			edit(vals)
			edit(vals)
			patch = append(patch, dataTable.PatchInput{Row: ids[k].Id, Values: vals})
		}
		if _, err := dataTable.Batch(ctx, tb.Id, dataTable.BatchInput{Patch: patch, Kind: "paste"}, owner); err != nil {
			t.Fatalf("paste: %v", err)
		}
	}
	for i := 0; i < 20; i++ {
		if err := dataTable.DeleteRow(ctx, tb.Id, ids[rnd.Intn(len(ids))].Id, owner); err != nil && !dataTable.IsRowNotFound(err) {
			t.Fatalf("delete: %v", err)
		}
		if _, err := dataTable.CreateRow(ctx, tb.Id, dataTable.RowInput{Values: map[string]interface{}{
			name.Id.String(): words[rnd.Intn(len(words))], amount.Id.String(): float64(rnd.Intn(500)), stage.Id.String(): "Talking", tags.Id.String(): []interface{}{"hot"},
		}}, owner); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	check("after writes", 50)

	// A walk past its budget, a later window's too, gives what the evaluator does.
	mixed = true
	check("walks past their budget", 50)
	mixed, rowQuery.WalkBudget = false, budget
}
