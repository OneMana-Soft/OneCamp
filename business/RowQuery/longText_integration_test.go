//go:build integration

package business_test

// What a table holds, whatever it holds (tables T5c), on Postgres 12 with
// every migration:
//
//   - A long text saves. The cell index kept each text whole in a btree,
//     which refuses an entry over 2,704 bytes: 3,200 bytes that don't
//     compress, or about 12,000 characters of prose, failed the save that
//     wrote them, and the repair sweep's every pass over their table.
//   - A choice whose options move, with no cell changed, sorts its rows by
//     the options as they are: the index's option places followed the
//     options only when a save changed cells.
//
// Both sorts and filters answered from the index mean what the evaluator
// means, row for row.
//
// Run: integ_rel.sh <worktree> <log> '^(TestALongTextSavesAndSorts|TestAChoicesOptionsMovedSortItsRowsAnew)$' with PKGS='./business/RowQuery/'

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
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

// tableOwner is a database with every migration, Dgraph (a write reads its
// row back with its people named) and a member to own tables.
func tableOwner(t *testing.T) (context.Context, dataTable.Actor) {
	t.Helper()
	return tableOwnerAs(t, uuid.New())
}

// tableOwnerAs is tableOwner whose member has the given id: a fixture's
// "me", so a query asked as them reads its @me as the person its rows name.
func tableOwnerAs(t *testing.T, me uuid.UUID) (context.Context, dataTable.Actor) {
	t.Helper()
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("connect: %v", err)
	}
	integration.SetupDgraph(t)
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, 'owner@example.test', 'owner', 'Owner', NOW(), NOW())`, me); err != nil {
		t.Fatal(err)
	}
	return ctx, dataTable.Actor{UserID: me}
}

// storedRows is a table's live rows as stored, in its order.
func storedRows(ctx context.Context, t *testing.T, table uuid.UUID) []*model.Row {
	t.Helper()
	var out []*model.Row
	key, after := "", uuid.Nil
	for {
		page, err := model.ListRowsAfter(ctx, table, key, after, 1000)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, page...)
		if len(page) < 1000 {
			return out
		}
		key, after = page[len(page)-1].PositionKey, page[len(page)-1].Id
	}
}

// sameAsTheEvaluator runs spec window after window (size rows each) and
// fails unless it gives the rows the evaluator gives over what's stored, in
// its order.
func sameAsTheEvaluator(ctx context.Context, t *testing.T, owner dataTable.Actor, table uuid.UUID, spec query.Spec, size int) []string {
	t.Helper()
	spec.Scope = query.Scope{Kind: query.ScopeTable, ID: table.String()}
	fields, err := model.ListFields(ctx, table)
	if err != nil {
		t.Fatal(err)
	}
	var live []*model.Field
	for _, f := range fields {
		if f.DeletedAt == nil {
			live = append(live, f)
		}
	}
	want, _, _, err := rowQuery.Oracle(spec, live, storedRows(ctx, t, table), owner.UserID, time.Now(), time.UTC)
	if err != nil {
		t.Fatalf("the evaluator: %v", err)
	}
	who := accessBusiness.Subject{UserUUID: owner.UserID.String()}
	var got []string
	var cursor query.Cursor
	for page := 0; ; page++ {
		res, err := query.Queries.Run(ctx, who, spec, query.Page{Size: size, After: cursor})
		if err != nil {
			b, _ := json.Marshal(spec)
			t.Fatalf("%s: %v", b, err)
		}
		got = append(got, res.IDs...)
		if res.Next == "" {
			break
		}
		cursor = res.Next
		if page > 1000 {
			t.Fatal("never ends")
		}
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		b, _ := json.Marshal(spec)
		t.Errorf("%s: the index gave %v, the evaluator %v", b, got, want)
	}
	return got
}

// incompressible is n bytes of hex digits, which Postgres's compression
// leaves as they are.
func incompressible(rnd *rand.Rand, n int) string {
	const digits = "0123456789abcdef"
	b := make([]byte, n)
	for i := range b {
		b[i] = digits[rnd.Intn(len(digits))]
	}
	return string(b)
}

// prose is about n characters of words, as a long note holds them.
func prose(rnd *rand.Rand, n int) string {
	words := strings.Fields(`the client asked about pricing for next quarter and we agreed to follow up with a proposal
		including delivery dates budget risks owners milestones review meeting notes action items decisions open questions
		launch plan design research customer feedback support tickets roadmap hiring onboarding travel venue booth banners
		power cables shipping invoices contracts legal approval security audit performance metrics retention churn growth`)
	var sb strings.Builder
	for sb.Len() < n {
		if sb.Len() > 0 {
			sb.WriteByte(' ')
		}
		sb.WriteString(words[rnd.Intn(len(words))])
	}
	return sb.String()
}

func TestALongTextSavesAndSorts(t *testing.T) {
	ctx, owner := tableOwner(t)
	tb, err := dataTable.CreateTable(ctx, dataTable.TableInput{Name: "Notes", Visibility: "workspace"}, owner)
	if err != nil {
		t.Fatal(err)
	}
	add := func(name, typ string) *model.Field {
		f, err := dataTable.CreateField(ctx, tb.Id, dataTable.FieldInput{Name: name, Type: typ}, owner)
		if err != nil {
			t.Fatalf("field %s: %v", name, err)
		}
		return f
	}
	name, notes := add("Title", model.FieldText), add("Notes", model.FieldLongText)
	rnd := rand.New(rand.NewSource(20261021))
	hex := incompressible(rnd, 3200)
	long := prose(rnd, 12000) + " zebracorn"
	if len(long) < 12000 {
		t.Fatalf("the prose is %d characters", len(long))
	}
	n, l := name.Id.String(), notes.Id.String()

	// Each save keeps the cell index in its own transaction.
	made := func(vals map[string]interface{}) *model.Row {
		r, err := dataTable.CreateRow(ctx, tb.Id, dataTable.RowInput{Values: vals}, owner)
		if err != nil {
			t.Fatalf("a row holding %d and %d bytes didn't save: %v", len(fmt.Sprint(vals[n])), len(fmt.Sprint(vals[l])), err)
		}
		return r
	}
	made(map[string]interface{}{n: "Row 10", l: hex})
	short := made(map[string]interface{}{n: "Row 2", l: "short"})
	made(map[string]interface{}{n: hex, l: long})
	made(map[string]interface{}{n: "row 1", l: long + " and again"})
	if _, err := dataTable.PatchRow(ctx, tb.Id, short.Id, map[string]interface{}{l: "Row 2's " + long}, owner); err != nil {
		t.Fatalf("an edit to a long text didn't save: %v", err)
	}

	// The repair sweep indexes the table again, each row's entries whole.
	if err := rowQuery.IndexTable(ctx, tb.Id); err != nil {
		t.Fatalf("the sweep couldn't index a table holding long texts: %v", err)
	}

	field := func(f *model.Field) string { return "field:" + f.Id.String() }
	is := func(f *model.Field, op, v string) *query.Filter {
		return &query.Filter{Field: field(f), Operator: op, Values: []query.Value{{Text: v}}}
	}
	for _, spec := range []query.Spec{
		{Sorts: []query.Sort{{Field: field(notes)}}},
		{Sorts: []query.Sort{{Field: field(notes), Desc: true}}},
		{Sorts: []query.Sort{{Field: field(name)}}},
		// Past the first 2,000 bytes of its text, and past its sort key's 256
		// characters: a filter reads the whole text.
		{Filter: is(notes, query.OperatorContains, "zebracorn")},
		{Filter: is(notes, query.OperatorIs, long)},
		{Filter: is(name, query.OperatorIs, hex), Sorts: []query.Sort{{Field: field(notes)}}},
		{Filter: is(notes, query.OperatorStartsWith, hex[:2500])},
	} {
		sameAsTheEvaluator(ctx, t, owner, tb.Id, spec, 2)
	}
	if got := sameAsTheEvaluator(ctx, t, owner, tb.Id, query.Spec{Filter: is(notes, query.OperatorContains, "zebracorn")}, 10); len(got) != 3 {
		t.Errorf("%d rows have the word at their long note's end, not 3", len(got))
	}
}

func TestAChoicesOptionsMovedSortItsRowsAnew(t *testing.T) {
	ctx, owner := tableOwner(t)
	tb, err := dataTable.CreateTable(ctx, dataTable.TableInput{Name: "Deals", Visibility: "workspace"}, owner)
	if err != nil {
		t.Fatal(err)
	}
	opts := []interface{}{}
	for _, l := range []string{"New", "Talking", "Won"} {
		opts = append(opts, map[string]interface{}{"label": l})
	}
	stage, err := dataTable.CreateField(ctx, tb.Id, dataTable.FieldInput{Name: "Stage", Type: model.FieldSelect, Config: map[string]interface{}{"options": opts}}, owner)
	if err != nil {
		t.Fatal(err)
	}
	// Rows written straight in, as a table holds them after years of
	// imports: labels no option has, others differing in case and spaces.
	labels := []string{"Won", "New", "Talking", "Lost", " New ", "talking", "Won", "Lost", "New"}
	for i, label := range labels {
		vals, _ := json.Marshal(map[string]interface{}{stage.Id.String(): label})
		if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `INSERT INTO data_table_rows (id, table_id, "values", position, position_key, row_no, created_by, created_at, updated_at)
			VALUES ($1, $2, $3::jsonb, $4, $5, $6, $7, NOW(), NOW())`, uuid.New(), tb.Id, string(vals), float64(i), fmt.Sprintf("d%04d", i), i+1, owner.UserID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `UPDATE data_tables SET next_row_no = $2 WHERE id = $1`, tb.Id, len(labels)+1); err != nil {
		t.Fatal(err)
	}
	if err := rowQuery.IndexTable(ctx, tb.Id); err != nil {
		t.Fatal(err)
	}
	by := func(desc bool) query.Spec {
		return query.Spec{Sorts: []query.Sort{{Field: "field:" + stage.Id.String(), Desc: desc}}}
	}
	sameAsTheEvaluator(ctx, t, owner, tb.Id, by(false), 4)

	// The options as stored, moved by a save that changes no cell: their
	// order turned round, then a label the rows hold made the second option.
	move := func(order ...string) {
		t.Helper()
		fields, err := model.ListFields(ctx, tb.Id)
		if err != nil {
			t.Fatal(err)
		}
		var cfg struct {
			Options []map[string]interface{} `json:"options"`
		}
		for _, f := range fields {
			if f.Id == stage.Id {
				_ = json.Unmarshal([]byte(f.Config), &cfg)
			}
		}
		byLabel := map[string]map[string]interface{}{}
		for _, o := range cfg.Options {
			byLabel[o["label"].(string)] = o
		}
		var moved []interface{}
		for _, l := range order {
			if o, ok := byLabel[l]; ok {
				moved = append(moved, o)
			} else {
				moved = append(moved, map[string]interface{}{"label": l})
			}
		}
		changed, err := dataTable.UpdateField(ctx, tb.Id, stage.Id, dataTable.FieldPatch{Config: map[string]interface{}{"options": moved}}, owner)
		if err != nil {
			t.Fatalf("options %v: %v", order, err)
		}
		if changed != 0 {
			t.Fatalf("options %v changed %d rows' cells; this test is of a save that changes none", order, changed)
		}
	}
	move("Won", "Talking", "New")
	sameAsTheEvaluator(ctx, t, owner, tb.Id, by(false), 4)
	sameAsTheEvaluator(ctx, t, owner, tb.Id, by(true), 4)
	move("Won", "Lost", "Talking", "New")
	sameAsTheEvaluator(ctx, t, owner, tb.Id, by(false), 4)
	sameAsTheEvaluator(ctx, t, owner, tb.Id, by(true), 3)
}
