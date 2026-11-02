//go:build integration
// +build integration

package integration_test

// A step over a view's rows (G2a: POST /tables/{id}/rows/transform), against
// Postgres 12 with every migration, over HTTP as a member reaches it: a
// replace over a filtered view of 2,500 rows writes three change sets of one
// operation, and one undo takes all of them back; a find counts without
// writing; a dry run writes nothing; trim and split; a dedupe keeps the first
// row in view order, and its undo brings the rest back; and a table the
// reader can't open is not found.
//
// Run: go test -tags=integration ./tests/integration/ -run '^(TestTableTransform|TestATransformOfATableTheReaderCantOpenIsNotFound)$' -v

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	query "github.com/akashc777/OneCamp/business/QuerySpec"
	rowLedgerController "github.com/akashc777/OneCamp/controllers/RowLedger"
	dataTableModel "github.com/akashc777/OneCamp/models/postgres/DataTable"
)

// transform posts in to table's transform route as who, and answers the
// status, the result and the body.
func (w *tableWorld) transform(t *testing.T, who string, table uuid.UUID, in any) (int, dataTableBusiness.TransformResult, string) {
	t.Helper()
	body, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	h := w.asMember(who, http.MethodPost, "/tables/{id}/rows/transform", rowLedgerController.Transform)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/tables/"+table.String()+"/rows/transform", bytes.NewReader(body)))
	var out struct {
		Data dataTableBusiness.TransformResult `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out.Data, rec.Body.String()
}

// cell is a stored row's cell as text ("" when empty).
func (w *tableWorld) cell(t *testing.T, tb *dataTableModel.DataTable, row uuid.UUID, field string) string {
	t.Helper()
	s, _ := w.stored(tb, row)[field].(string)
	return s
}

// changeSets is how many change sets of kind (a LIKE pattern) tb has, and how many operations
// join them.
func (w *tableWorld) changeSets(t *testing.T, tb *dataTableModel.DataTable, kind string) (int, int) {
	t.Helper()
	var sets, ops int
	if err := w.env.PG.QueryRow(`SELECT COUNT(*), COUNT(DISTINCT operation) FROM data_table_change_sets WHERE table_id = $1 AND kind LIKE $2`, tb.Id, kind).Scan(&sets, &ops); err != nil {
		t.Fatal(err)
	}
	return sets, ops
}

func TestTableTransform(t *testing.T) {
	w := newTableWorld(t, "amara", "bo")
	amara, bo := w.people["amara"], w.people["bo"]
	tb := w.table("amara", "Vendors")
	name := w.field("amara", tb, "Name", dataTableModel.FieldText, nil)
	region := w.field("amara", tb, "Region", dataTableModel.FieldText, nil)
	city := w.field("amara", tb, "City", dataTableModel.FieldText, nil)
	code := w.field("amara", tb, "Code", dataTableModel.FieldText, nil)
	prefix := w.field("amara", tb, "Prefix", dataTableModel.FieldText, nil)
	rest := w.field("amara", tb, "Rest", dataTableModel.FieldText, nil)
	// 3,000 rows: every sixth East, the other 2,500 West; the first ten with
	// untidy cities.
	var rows []uuid.UUID
	west := map[uuid.UUID]bool{}
	for b := 0; b < 3; b++ {
		var in dataTableBusiness.BatchInput
		for i := 0; i < 1000; i++ {
			n := b*1000 + i
			r := "West"
			if n%6 == 5 {
				r = "East"
			}
			c := "Pune"
			if n < 10 {
				c = "  Pune \t  West  "
			}
			in.Create = append(in.Create, dataTableBusiness.RowInput{Values: map[string]interface{}{
				name: fmt.Sprintf("Acme %d", n), region: r, city: c, code: fmt.Sprintf("A-%d-x", n)}})
		}
		made, err := dataTableBusiness.Batch(w.ctx, tb.Id, in, amara)
		if err != nil {
			t.Fatalf("seed batch %d: %v", b, err)
		}
		for _, r := range made.Created {
			rows = append(rows, r.Id)
		}
	}
	for n, id := range rows {
		west[id] = n%6 != 5
	}
	westView := &query.Spec{Filter: &query.Filter{Field: "field:" + region, Operator: query.OperatorIs, Values: []query.Value{{Text: "West"}}}}

	t.Run("a replace over a filtered view is three change sets of one operation, and one undo takes them all back", func(t *testing.T) {
		status, res, body := w.transform(t, "bo", tb.Id, dataTableBusiness.TransformInput{
			Step: "replace", Spec: westView, Fields: []uuid.UUID{uuid.MustParse(name)}, Find: "acme", Replace: "ACME"})
		if status != http.StatusOK || res.Walked != 2500 || res.Rows != 2500 || res.Cells != 2500 || len(res.ChangeSets) != 3 || res.Operation == nil {
			t.Fatalf("the replace answered %d: %s", status, body)
		}
		if sets, ops := w.changeSets(t, tb, "replace"); sets != 3 || ops != 1 {
			t.Errorf("the replace stored %d change sets of %d operations, want 3 of one", sets, ops)
		}
		for _, n := range []int{0, 1500, 2999, 5, 2999 - 4} {
			want := fmt.Sprintf("Acme %d", n)
			if west[rows[n]] {
				want = fmt.Sprintf("ACME %d", n)
			}
			if got := w.cell(t, tb, rows[n], name); got != want {
				t.Errorf("row %d's name is %q, want %q", n, got, want)
			}
		}
		undone, err := dataTableBusiness.Undo(w.ctx, tb.Id, nil, bo)
		if err != nil || undone.Changes != 2500 {
			t.Fatalf("one undo: %+v, %v; want all 2,500 rows back", undone, err)
		}
		for _, n := range []int{0, 1500, 2998} {
			if got := w.cell(t, tb, rows[n], name); got != fmt.Sprintf("Acme %d", n) {
				t.Errorf("after the undo, row %d's name is %q", n, got)
			}
		}
	})

	t.Run("a find counts every match and lists where they are, writing nothing", func(t *testing.T) {
		before, _ := w.changeSets(t, tb, "%")
		status, res, body := w.transform(t, "bo", tb.Id, dataTableBusiness.TransformInput{
			Step: "find", Fields: []uuid.UUID{uuid.MustParse(name), uuid.MustParse(region)}, Find: "acme 12"})
		// Acme 12, Acme 120 to 129 and Acme 1200 to 1299.
		if status != http.StatusOK || res.Rows != 111 || res.Cells != 111 || len(res.Matches) != 111 || res.Matches[0].Row != rows[12] || res.Matches[0].At != 12 {
			t.Fatalf("the find answered %d: %.300s", status, body)
		}
		if after, _ := w.changeSets(t, tb, "%"); after != before || len(res.ChangeSets) != 0 {
			t.Errorf("a find wrote %d change sets", after-before+len(res.ChangeSets))
		}
	})

	t.Run("a dry run counts what a trim would do and writes nothing, and the trim does it", func(t *testing.T) {
		status, res, body := w.transform(t, "bo", tb.Id, dataTableBusiness.TransformInput{Step: "trim", Fields: []uuid.UUID{uuid.MustParse(city)}, DryRun: true})
		if status != http.StatusOK || res.Rows != 10 || res.Cells != 10 || len(res.Matches) != 10 || res.Operation != nil {
			t.Fatalf("the dry run answered %d: %.300s", status, body)
		}
		if got := w.cell(t, tb, rows[3], city); got != "  Pune \t  West  " {
			t.Errorf("a dry run wrote %q", got)
		}
		status, res, body = w.transform(t, "bo", tb.Id, dataTableBusiness.TransformInput{Step: "trim", Fields: []uuid.UUID{uuid.MustParse(city)}})
		if status != http.StatusOK || res.Rows != 10 || len(res.ChangeSets) != 1 {
			t.Fatalf("the trim answered %d: %.300s", status, body)
		}
		if got := w.cell(t, tb, rows[3], city); got != "Pune West" {
			t.Errorf("row 3's city is %q, want Pune West", got)
		}
	})

	t.Run("a split puts each part in its field, the rest in the last", func(t *testing.T) {
		split := dataTableBusiness.TransformInput{Step: "split", Fields: []uuid.UUID{uuid.MustParse(code)}, Separator: "-",
			Into: []uuid.UUID{uuid.MustParse(prefix), uuid.MustParse(rest)}, DryRun: true}
		status, res, body := w.transform(t, "amara", tb.Id, split)
		if status != http.StatusOK || res.MaxParts != 3 || res.Rows != 3000 || res.Overwrites != 0 {
			t.Fatalf("the split's dry run answered %d: %.300s", status, body)
		}
		split.DryRun = false
		if status, res, body = w.transform(t, "amara", tb.Id, split); status != http.StatusOK || res.Rows != 3000 || len(res.ChangeSets) != 3 {
			t.Fatalf("the split answered %d: %.300s", status, body)
		}
		if p, r, c := w.cell(t, tb, rows[42], prefix), w.cell(t, tb, rows[42], rest), w.cell(t, tb, rows[42], code); p != "A" || r != "42-x" || c != "A-42-x" {
			t.Errorf("row 42 split into %q and %q from %q", p, r, c)
		}
	})

	t.Run("a dedupe keeps the first row in view order, and its undo brings the rest back", func(t *testing.T) {
		people := w.table("amara", "Contacts")
		who := w.field("amara", people, "Who", dataTableModel.FieldText, nil)
		where := w.field("amara", people, "Where", dataTableModel.FieldText, nil)
		var in dataTableBusiness.BatchInput
		for _, v := range [][2]string{{"Ann", "Pune"}, {"Bo", "Delhi"}, {"Ann", "Pune"}, {"ann", "Pune"}, {"Ann", "Pune"}} {
			in.Create = append(in.Create, dataTableBusiness.RowInput{Values: map[string]interface{}{who: v[0], where: v[1]}})
		}
		made, err := dataTableBusiness.Batch(w.ctx, people.Id, in, amara)
		if err != nil {
			t.Fatal(err)
		}
		status, res, body := w.transform(t, "bo", people.Id, dataTableBusiness.TransformInput{Step: "dedupe", Fields: []uuid.UUID{uuid.MustParse(who), uuid.MustParse(where)}})
		if status != http.StatusOK || res.Deleted != 2 || len(res.ChangeSets) != 1 {
			t.Fatalf("the dedupe answered %d: %.300s", status, body)
		}
		left := w.order(people)
		if want := []uuid.UUID{made.Created[0].Id, made.Created[1].Id, made.Created[3].Id}; fmt.Sprint(left) != fmt.Sprint(want) {
			t.Errorf("left %v, want rows 0, 1 and 3", left)
		}
		if _, err := dataTableBusiness.Undo(w.ctx, people.Id, nil, bo); err != nil || len(w.order(people)) != 5 {
			t.Errorf("the undo left %d rows (%v), want all 5", len(w.order(people)), err)
		}
	})
}

func TestATransformOfATableTheReaderCantOpenIsNotFound(t *testing.T) {
	w := newTableWorld(t, "amara", "bo")
	tb, err := dataTableBusiness.CreateTable(w.ctx, dataTableBusiness.TableInput{Name: "Private", Visibility: "private"}, w.people["amara"])
	if err != nil {
		t.Fatal(err)
	}
	name := w.field("amara", tb, "Name", dataTableModel.FieldText, nil)
	row, err := dataTableBusiness.CreateRow(w.ctx, tb.Id, dataTableBusiness.RowInput{Values: map[string]interface{}{name: "Acme"}}, w.people["amara"])
	if err != nil {
		t.Fatal(err)
	}
	fields := []uuid.UUID{uuid.MustParse(name)}
	for _, in := range []dataTableBusiness.TransformInput{
		{Step: "find", Fields: fields, Find: "acme"},
		{Step: "replace", Fields: fields, Find: "acme", Replace: "Bolt"},
		{Step: "replace", Fields: fields, Find: "acme", Replace: "Bolt", Rows: []uuid.UUID{row.Id}},
	} {
		if status, _, body := w.transform(t, "bo", tb.Id, in); status != http.StatusNotFound {
			t.Errorf("bo's %s of amara's private table answered %d: %s", in.Step, status, body)
		}
	}
	if got := w.cell(t, tb, row.Id, name); got != "Acme" {
		t.Errorf("the private row's name is %q", got)
	}
	// Its owner may; a view named both ways is refused in words.
	if status, res, _ := w.transform(t, "amara", tb.Id, dataTableBusiness.TransformInput{Step: "find", Fields: fields, Find: "acme"}); status != http.StatusOK || res.Rows != 1 {
		t.Errorf("amara's find answered %d, %d rows", status, res.Rows)
	}
	both := dataTableBusiness.TransformInput{Step: "find", Fields: fields, Find: "acme", Rows: []uuid.UUID{row.Id}, Spec: &query.Spec{}}
	if status, _, body := w.transform(t, "amara", tb.Id, both); status != http.StatusBadRequest {
		t.Errorf("a view named both ways answered %d: %s", status, body)
	}
}
