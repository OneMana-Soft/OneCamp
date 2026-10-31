//go:build integration
// +build integration

package integration_test

// Tables A1 (design/tables-vs-airtable/PROGRAM.md §6): one registry decides
// what every cell may hold, for every writer; fields say what they are for,
// what a new row starts with and whether a row must give them; one field names
// a table's rows; every row has a number. Against Postgres 12 with every
// migration.
//
// Run: go test -tags=integration ./tests/integration/ -run TestTableFieldTypes -v

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	notionAdapter "github.com/akashc777/OneCamp/business/ContentIO/adapters/notion"
	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	rowLedger "github.com/akashc777/OneCamp/business/RowLedger"
	dataTableController "github.com/akashc777/OneCamp/controllers/DataTable"
	v1Controller "github.com/akashc777/OneCamp/controllers/V1"
	dataTableModel "github.com/akashc777/OneCamp/models/postgres/DataTable"
)

func TestTableFieldTypes(t *testing.T) {
	w := newTableWorld(t, "amara", "bo")
	deals := w.table("amara", "Deals")
	amount := w.field("amara", deals, "Amount", dataTableModel.FieldNumber, nil)
	stage := w.field("amara", deals, "Stage", dataTableModel.FieldSelect,
		map[string]interface{}{"options": []map[string]interface{}{{"label": "Lead"}, {"label": "Won"}}})
	when := w.field("amara", deals, "Close", dataTableModel.FieldDate, nil)

	// post sends a body to a route as a member, and answers its code and body.
	post := func(t *testing.T, who, pattern, path string, h http.HandlerFunc, body string) (int, map[string]interface{}) {
		t.Helper()
		rec := httptest.NewRecorder()
		w.asMember(who, http.MethodPost, pattern, h).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body)))
		var out map[string]interface{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	newRow := func(t *testing.T, values map[string]interface{}) uuid.UUID {
		t.Helper()
		r, err := dataTableBusiness.CreateRow(w.ctx, deals.Id, dataTableBusiness.RowInput{Values: values}, w.people["amara"])
		if err != nil {
			t.Fatalf("create row: %v", err)
		}
		return r.Id
	}

	t.Run("the web's cell edit is stored as its kind stores it, and refused when it can't be", func(t *testing.T) {
		row := newRow(t, nil)
		const pattern = "/tables/{id}/rows/{rowId}/patch"
		path := "/tables/" + deals.Id.String() + "/rows/" + row.String() + "/patch"
		if code, out := post(t, "bo", pattern, path, dataTableController.PatchRow, fmt.Sprintf(`{"values": {%q: "1,250.50"}}`, amount)); code != http.StatusOK {
			t.Fatalf("a number as text answered %d %v", code, out)
		}
		if v := w.stored(deals, row); v[amount] != 1250.5 {
			t.Errorf("the amount is stored as %#v, want the number 1250.5", v[amount])
		}
		for body, says := range map[string]string{
			fmt.Sprintf(`{"values": {%q: "12 kg"}}`, amount):  `"Amount" takes a number`,
			fmt.Sprintf(`{"values": {%q: "Lost"}}`, stage):    `"Stage" has no option "Lost": its options are Lead, Won`,
			fmt.Sprintf(`{"values": {%q: "Friday"}}`, when):   `"Close" takes a date`,
			fmt.Sprintf(`{"values": {%q: ["Won"]}}`, stage):   `"Stage" has no option`,
			fmt.Sprintf(`{"values": {%q: {"x": 1}}}`, amount): `"Amount" takes a number`,
		} {
			code, out := post(t, "bo", pattern, path, dataTableController.PatchRow, body)
			if code != http.StatusBadRequest || !strings.Contains(fmt.Sprint(out["msg"]), says) {
				t.Errorf("%s answered %d %v, want 400 saying %s", body, code, out, says)
			}
		}
		if v := w.stored(deals, row); v[amount] != 1250.5 || v[stage] != nil {
			t.Errorf("a refused edit changed the row: %v", v)
		}
	})

	t.Run("a new row from the web, Turn into database among them, is checked too", func(t *testing.T) {
		const pattern = "/tables/{id}/rows"
		path := "/tables/" + deals.Id.String() + "/rows"
		code, out := post(t, "bo", pattern, path, dataTableController.CreateRow, fmt.Sprintf(`{"values": {%q: "TBD"}, "position": 1000}`, amount))
		if code != http.StatusBadRequest || !strings.Contains(fmt.Sprint(out["msg"]), "takes a number") {
			t.Errorf("a row with a word for a number answered %d %v", code, out)
		}
		if code, out := post(t, "bo", pattern, path, dataTableController.CreateRow, fmt.Sprintf(`{"values": {%q: "Won"}}`, stage)); code != http.StatusCreated {
			t.Errorf("a row with an option answered %d %v", code, out)
		}
	})

	t.Run("the API and AI tools give every required field; the grid's new row needn't", func(t *testing.T) {
		tb := w.table("amara", "Contacts")
		email := w.field("amara", tb, "Email", dataTableModel.FieldEmail, nil)
		required := true
		if _, err := dataTableBusiness.UpdateField(w.ctx, tb.Id, uuid.MustParse(email), dataTableBusiness.FieldPatch{Required: &required}, w.people["amara"]); err != nil {
			t.Fatal(err)
		}
		const pattern = "/v1/tables/{id}/rows"
		code, out := post(t, "bo", pattern, "/v1/tables/"+tb.Id.String()+"/rows", v1Controller.CreateRow, `{"values": {}}`)
		if code != http.StatusBadRequest || !strings.Contains(fmt.Sprint(out["msg"]), `"Email" is required`) {
			t.Errorf("an API row without its required field answered %d %v", code, out)
		}
		agent := w.people["bo"]
		agent.Kind = rowLedger.ActorAgent
		if _, err := dataTableBusiness.CreateRow(w.ctx, tb.Id, dataTableBusiness.RowInput{Values: map[string]interface{}{}}, agent); err == nil {
			t.Error("an AI tool made a row without its required field")
		}
		if _, err := dataTableBusiness.CreateRow(w.ctx, tb.Id, dataTableBusiness.RowInput{}, w.people["bo"]); err != nil {
			t.Errorf("the grid's blank new row was refused: %v", err)
		}
		if code, out := post(t, "bo", pattern, "/v1/tables/"+tb.Id.String()+"/rows", v1Controller.CreateRow, fmt.Sprintf(`{"values": {%q: "maya@example.com"}}`, email)); code != http.StatusCreated {
			t.Errorf("a complete API row answered %d %v", code, out)
		}
	})

	t.Run("a field's description, default and required setting are kept, and guests see none of them", func(t *testing.T) {
		tb := w.table("amara", "Leads")
		src, err := dataTableBusiness.CreateField(w.ctx, tb.Id, dataTableBusiness.FieldInput{
			Name: "Source", Type: dataTableModel.FieldSelect, Description: "Where the lead came from",
			Config:  map[string]interface{}{"options": []map[string]interface{}{{"label": "Web"}, {"label": "Event"}}},
			Default: json.RawMessage(`"Web"`),
		}, w.people["amara"])
		if err != nil {
			t.Fatal(err)
		}
		r, err := dataTableBusiness.CreateRow(w.ctx, tb.Id, dataTableBusiness.RowInput{}, w.people["bo"])
		if err != nil {
			t.Fatal(err)
		}
		if v := w.stored(tb, r.Id); v[src.Id.String()] != "Web" {
			t.Errorf("a new row's source is %#v, want its default", v[src.Id.String()])
		}
		if _, err := dataTableBusiness.CreateField(w.ctx, tb.Id, dataTableBusiness.FieldInput{
			Name: "Score", Type: dataTableModel.FieldNumber, Default: json.RawMessage(`"high"`)}, w.people["amara"]); err == nil {
			t.Error("a number field took a word as its default")
		}
		bundle, err := dataTableBusiness.GetBundle(w.ctx, tb.Id, w.people["bo"])
		if err != nil {
			t.Fatal(err)
		}
		var got *dataTableModel.Field
		for _, f := range bundle.Fields {
			if f.Id == src.Id {
				got = f
			}
		}
		if got == nil || got.Description == nil || *got.Description != "Where the lead came from" || string(got.Default) != `"Web"` {
			t.Fatalf("the bundle's field is %+v", got)
		}
		guest, err := dataTableBusiness.GetGuestBundle(w.ctx, tb.Id)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range guest.Fields {
			if f.Description != nil || f.Default != nil || f.Required {
				t.Errorf("a guest is told how %q is set up: %+v", f.Name, f)
			}
		}
	})

	t.Run("one field names the rows: the first, until another is made the primary", func(t *testing.T) {
		tb := w.table("amara", "Vendors")
		fields, err := dataTableModel.ListFields(w.ctx, tb.Id)
		if err != nil || len(fields) != 1 || !fields[0].Primary {
			t.Fatalf("a new table's fields: %+v %v", fields, err)
		}
		name := fields[0]
		code := w.field("amara", tb, "Code", dataTableModel.FieldText, nil)
		yes, no := true, false
		if _, err := dataTableBusiness.UpdateField(w.ctx, tb.Id, name.Id, dataTableBusiness.FieldPatch{Primary: &no}, w.people["amara"]); err == nil {
			t.Error("the primary field stopped being it with no other made it")
		}
		if err := dataTableBusiness.DeleteField(w.ctx, tb.Id, name.Id, w.people["amara"]); err == nil || !strings.Contains(err.Error(), "primary") {
			t.Errorf("deleting the primary field: %v", err)
		}
		if _, err := dataTableBusiness.UpdateField(w.ctx, tb.Id, uuid.MustParse(code), dataTableBusiness.FieldPatch{Primary: &yes}, w.people["amara"]); err != nil {
			t.Fatal(err)
		}
		fields, _ = dataTableModel.ListFields(w.ctx, tb.Id)
		if len(fields) != 2 || fields[0].Id.String() != code || !fields[0].Primary || fields[1].Primary {
			t.Errorf("after making Code the primary: %+v", fields)
		}
		// Links name a row by the primary.
		r, err := dataTableBusiness.CreateRow(w.ctx, tb.Id, dataTableBusiness.RowInput{Values: map[string]interface{}{name.Id.String(): "Acme", code: "V-7"}}, w.people["amara"])
		if err != nil {
			t.Fatal(err)
		}
		link := w.field("amara", deals, "Vendor", dataTableModel.FieldRelation, map[string]interface{}{"relation_target": "table", "table_id": tb.Id.String()})
		row := newRow(t, map[string]interface{}{link: []interface{}{r.Id.String()}})
		page, err := dataTableBusiness.ListRows(w.ctx, deals.Id, w.people["bo"], 1000, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range page {
			if p.Id != row {
				continue
			}
			var v map[string]interface{}
			_ = json.Unmarshal([]byte(p.Values), &v)
			if refs, _ := v[link].([]interface{}); len(refs) != 1 || refs[0].(map[string]interface{})["label"] != "V-7" {
				t.Errorf("the link reads %v, want the vendor named by its code", v[link])
			}
		}
	})

	t.Run("every row has a number, in the order rows were made, never given twice", func(t *testing.T) {
		tb := w.table("amara", "Numbered")
		const n = 40
		var mu sync.Mutex
		var nos []int64
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r, err := dataTableBusiness.CreateRow(w.ctx, tb.Id, dataTableBusiness.RowInput{}, w.people["bo"])
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				nos = append(nos, r.RowNo)
				mu.Unlock()
			}()
		}
		wg.Wait()
		// A create that failed is a failure here, not a missing row to index.
		if len(nos) != n {
			t.Fatalf("%d of the %d rows were made", len(nos), n)
		}
		sort.Slice(nos, func(i, j int) bool { return nos[i] < nos[j] })
		for i, no := range nos {
			if no != int64(i+1) {
				t.Fatalf("rows made at once are numbered %v", nos)
			}
		}
		page, err := dataTableModel.ListRows(w.ctx, tb.Id, 1, 0)
		if err != nil || len(page) == 0 {
			t.Fatalf("the table reads %d rows (%v)", len(page), err)
		}
		if err := dataTableBusiness.DeleteRow(w.ctx, tb.Id, page[0].Id, w.people["bo"]); err != nil {
			t.Fatal(err)
		}
		r, err := dataTableBusiness.CreateRow(w.ctx, tb.Id, dataTableBusiness.RowInput{}, w.people["bo"])
		if err != nil {
			t.Fatalf("the row after a delete wasn't made: %v", err)
		}
		if r.RowNo != n+1 {
			t.Errorf("the row after a delete is number %d, want %d", r.RowNo, n+1)
		}
	})

	t.Run("an option's id is stored for good, and its cells keep it through 1,200 rows of renames", func(t *testing.T) {
		tb := w.table("amara", "Tasks")
		status := w.field("amara", tb, "Status", dataTableModel.FieldSelect, nil)
		// Saved before options had ids: labels only.
		if _, err := w.env.PG.Exec(`UPDATE data_table_fields SET config = '{"options": ["Todo", {"label": "Done", "color": "moss"}]}' WHERE id = $1`, status); err != nil {
			t.Fatal(err)
		}
		if _, err := w.env.PG.Exec(`INSERT INTO data_table_rows (id, table_id, values, position)
			SELECT md5(random()::text || g::text)::uuid, $1, jsonb_build_object($2::text, CASE WHEN g % 2 = 0 THEN 'Todo' ELSE 'Done' END), g
			FROM generate_series(1, 1200) g`, tb.Id, status); err != nil {
			t.Fatal(err)
		}
		if _, err := w.env.PG.Exec(`DELETE FROM system_configs WHERE key = 'tables_option_ids_permanent'`); err != nil {
			t.Fatal(err)
		}
		if saved, err := dataTableBusiness.KeepOptionIDs(w.ctx); err != nil || saved < 1 {
			t.Fatalf("the job saved %d fields: %v", saved, err)
		}
		fields, _ := dataTableModel.ListFields(w.ctx, tb.Id)
		var cfg struct {
			Options []map[string]interface{} `json:"options"`
		}
		for _, f := range fields {
			if f.Id.String() == status {
				_ = json.Unmarshal([]byte(f.Config), &cfg)
			}
		}
		if len(cfg.Options) != 2 || cfg.Options[0]["id"] != "label:Todo" || cfg.Options[1]["id"] != "label:Done" || cfg.Options[1]["color"] != "moss" {
			t.Fatalf("the options are %v", cfg.Options)
		}
		if again, err := dataTableBusiness.KeepOptionIDs(w.ctx); err != nil || again != 0 {
			t.Errorf("the job ran again once done: %d %v", again, err)
		}
		// A rename by the stored id moves every cell with it.
		changed, err := dataTableBusiness.UpdateField(w.ctx, tb.Id, uuid.MustParse(status), dataTableBusiness.FieldPatch{Config: map[string]interface{}{
			"options": []map[string]interface{}{{"id": "label:Todo", "label": "To do"}, {"id": "label:Done", "label": "Done"}}}}, w.people["amara"])
		if err != nil || changed != 600 {
			t.Fatalf("the rename changed %d rows (%v), want 600", changed, err)
		}
		var todo int
		if err := w.env.PG.QueryRow(`SELECT COUNT(*) FROM data_table_rows WHERE table_id = $1 AND values ->> $2 = 'To do'`, tb.Id, status).Scan(&todo); err != nil || todo != 600 {
			t.Errorf("%d cells say To do (%v), want 600", todo, err)
		}
	})

	// An import never loses a row for one bad cell, nor the value in it: a
	// value outside a select's or a multi-select's options becomes one of
	// them in the table the import makes, as Notion's own import makes it,
	// once however many rows carry it, and the import's note says so. A Notion
	// database goes through the importer every source's tables share
	// (ImportBaseAs), whose notes the import's report keeps.
	t.Run("a Notion database's value outside its options becomes an option, and its row keeps it, with a note", func(t *testing.T) {
		db := notionAdapter.Database{
			Title: "Reading list",
			Columns: []notionAdapter.Column{{Name: "Title", Type: "title"}, {Name: "State", Type: "select", Options: []string{"Reading", "Read"}},
				{Name: "Tags", Type: "multi_select", Options: []string{"Classic"}}},
			Rows: []map[string]any{{"Title": "Dune", "State": "Read", "Tags": []string{"Classic"}},
				{"Title": "Emma", "State": "Abandoned", "Tags": []string{"Classic", "Romance"}}, {"Title": "Ulysses", "State": "Abandoned"}},
		}
		tb, notes, err := notionAdapter.TableFromDatabase(w.ctx, w.people["amara"], db)
		if err != nil {
			t.Fatal(err)
		}
		var said []string
		for _, n := range notes {
			said = append(said, n.Code+" "+n.Field+": "+n.Msg)
		}
		sort.Strings(said)
		if got, want := strings.Join(said, "\n"), `OPTION_ADDED State: "State" gained the option "Abandoned"`+"\n"+
			`OPTION_ADDED Tags: "Tags" gained the option "Romance"`; got != want {
			t.Errorf("the notes are\n%s\nwant\n%s", got, want)
		}
		if n, _ := dataTableModel.CountRows(w.ctx, tb.Id); n != 3 {
			t.Errorf("the table holds %d rows, want all three", n)
		}
		fields, err := dataTableModel.ListFields(w.ctx, tb.Id)
		if err != nil {
			t.Fatal(err)
		}
		id := map[string]string{}
		var state struct {
			Options []map[string]any `json:"options"`
		}
		for _, f := range fields {
			id[f.Name] = f.Id.String()
			if f.Name == "State" {
				_ = json.Unmarshal([]byte(f.Config), &state)
			}
		}
		var labels []string
		for _, o := range state.Options {
			labels = append(labels, fmt.Sprint(o["label"]))
		}
		if got := strings.Join(labels, ", "); got != "Reading, Read, Abandoned" {
			t.Errorf("State's options are %s, want Abandoned after Notion's two", got)
		}
		// cellOf is a row's cell as stored, as JSON.
		cellOf := func(title, field string) string {
			var v *string
			if err := w.env.PG.QueryRow(`SELECT values -> $2 FROM data_table_rows WHERE table_id = $1 AND deleted_at IS NULL AND values ->> $3 = $4`,
				tb.Id, id[field], id["Title"], title).Scan(&v); err != nil {
				t.Fatalf("%s's row: %v", title, err)
			}
			if v == nil {
				return ""
			}
			return *v
		}
		for _, c := range []struct{ title, field, want string }{
			{"Emma", "State", `"Abandoned"`}, {"Ulysses", "State", `"Abandoned"`}, {"Dune", "State", `"Read"`},
			{"Emma", "Tags", `["Classic", "Romance"]`}, {"Dune", "Tags", `["Classic"]`},
		} {
			if got := cellOf(c.title, c.field); got != c.want {
				t.Errorf("%s's %s is %s, want %s", c.title, c.field, got, c.want)
			}
		}
	})
}
