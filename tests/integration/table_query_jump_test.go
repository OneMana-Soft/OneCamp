//go:build integration

package integration_test

// A page that jumps (a scrollbar dragged far, airtable-04) asks POST
// /tables/{id}/query for the window at a place, not the windows before it.
// The answer holds the rows there, says where they start (so a page never
// draws a server's first window at the place it jumped to), and its cursor
// reads on from them. A place with a cursor, before the first row or past
// what a count reaches is refused in the API's words.
//
// Run: integ_rel.sh <worktree> <log> '^TestATableQueryJumpsToAPlace$' with PKGS=./tests/integration/

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	rowQuery "github.com/akashc777/OneCamp/business/RowQuery"
	rowQueryController "github.com/akashc777/OneCamp/controllers/RowQuery"
	dataTableModel "github.com/akashc777/OneCamp/models/postgres/DataTable"
)

func TestATableQueryJumpsToAPlace(t *testing.T) {
	w := newTableWorld(t, "ana")
	tb := w.table("ana", "Ledger")
	amount := w.field("ana", tb, "Amount", dataTableModel.FieldNumber, nil)

	// 1,200 rows whose amount is their place by amount, made whole as the
	// repair sweep makes a table filled at once.
	const rows = 1200
	var sb strings.Builder
	args := []interface{}{tb.Id}
	sb.WriteString(`INSERT INTO data_table_rows (id, table_id, "values", position, position_key, row_no, created_by, created_at, updated_at) VALUES `)
	for j := 0; j < rows; j++ {
		// Laid in the table's own order backwards: a window's rows come from its sort.
		cells, _ := json.Marshal(map[string]interface{}{amount: float64(j)})
		if j > 0 {
			sb.WriteString(", ")
		}
		n := len(args)
		fmt.Fprintf(&sb, "($%d, $1, $%d::jsonb, $%d, $%d, $%d, $%d, NOW(), NOW())", n+1, n+2, n+3, n+4, n+5, n+6)
		args = append(args, uuid.New(), string(cells), float64(rows-j), fmt.Sprintf("j%06d", rows-j), j+1, w.people["ana"].UserID)
	}
	if _, err := w.env.PG.Exec(sb.String(), args...); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if _, err := w.env.PG.Exec(`UPDATE data_tables SET next_row_no = $2 WHERE id = $1`, tb.Id, rows+1); err != nil {
		t.Fatal(err)
	}
	if err := rowQuery.IndexTable(w.ctx, tb.Id); err != nil {
		t.Fatalf("index: %v", err)
	}

	h := w.asMember("ana", http.MethodPost, "/tables/{id}/query", rowQueryController.QueryRows)
	ask := func(body map[string]any) (int, map[string]any) {
		t.Helper()
		b, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/tables/"+tb.Id.String()+"/query", bytes.NewReader(b)))
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	// amounts is a window's rows, by their amounts, its place and its cursor.
	amounts := func(out map[string]any) (got []int, at int, next string) {
		t.Helper()
		data, _ := out["data"].(map[string]any)
		list, _ := data["rows"].([]any)
		for _, r := range list {
			raw, _ := r.(map[string]any)["values"].(string)
			var v map[string]float64
			if err := json.Unmarshal([]byte(raw), &v); err != nil {
				t.Fatalf("a row's values: %q (%v)", raw, err)
			}
			got = append(got, int(v[amount]))
		}
		f, _ := data["at"].(float64)
		next, _ = data["next"].(string)
		return got, int(f), next
	}
	run := func(from, n int) []int {
		out := make([]int, n)
		for i := range out {
			out[i] = from + i
		}
		return out
	}
	same := func(name string, got, want []int) {
		t.Helper()
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("%s: %d rows %v..., want %d from %v", name, len(got), got[:min(3, len(got))], len(want), want[:min(3, len(want))])
		}
	}
	spec := map[string]any{"sorts": []map[string]any{{"field": "field:" + amount}}}

	code, out := ask(map[string]any{"spec": spec, "at": 700, "n": 100})
	got, at, next := amounts(out)
	if code != http.StatusOK || at != 700 || next == "" {
		t.Fatalf("the window at 700 answered %d at %d, cursor %q: %v", code, at, next, out)
	}
	same("the window at 700", got, run(700, 100))
	if d, _ := out["data"].(map[string]any); d["total"] != nil {
		t.Fatalf("a jump counted: %v", d["total"])
	}
	code, out = ask(map[string]any{"spec": spec, "cursor": next, "n": 100})
	got, at, _ = amounts(out)
	if code != http.StatusOK || at != 0 {
		t.Fatalf("on from the window at 700 answered %d at %d: %v", code, at, out)
	}
	same("on from the window at 700", got, run(800, 100))

	// The last rows at once, as a scrollbar dragged to the end asks.
	_, out = ask(map[string]any{"spec": spec, "at": rows - 30, "n": 100})
	got, at, next = amounts(out)
	if at != rows-30 || next != "" {
		t.Fatalf("the window at the end answered at %d, cursor %q", at, next)
	}
	same("the window at the end", got, run(rows-30, 30))

	// A first window says no place: a page reads its rows from the first.
	_, out = ask(map[string]any{"spec": spec, "n": 100})
	got, at, _ = amounts(out)
	if at != 0 {
		t.Fatalf("a first window said a place: %d", at)
	}
	same("the first window", got, run(0, 100))

	for name, body := range map[string]map[string]any{
		"a place and a cursor":         {"spec": spec, "at": 700, "cursor": next + "x", "n": 100},
		"a place before the first":     {"spec": spec, "at": -1, "n": 100},
		"a place past a count's reach": {"spec": spec, "at": rowQuery.CountCap + 1, "n": 100},
	} {
		if code, out := ask(body); code != http.StatusUnprocessableEntity || out["msg"] == nil {
			t.Errorf("%s answered %d: %v", name, code, out)
		}
	}
}
