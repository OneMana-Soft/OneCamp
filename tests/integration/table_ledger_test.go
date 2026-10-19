//go:build integration
// +build integration

package integration_test

// The row ledger (tables A2, design/tables-vs-airtable/PROGRAM.md §4.2 #62),
// against Postgres 12 with every migration: every write to a table's rows is
// a change set, stored with it, from which a person undoes and redoes what
// they did, a row has a history, and a deleted row, field or table comes back
// from the trash.
//
// Run: go test -tags=integration ./tests/integration/ -run TestTableLedger -v

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	rowLedgerController "github.com/akashc777/OneCamp/controllers/RowLedger"
	dataTableModel "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/akashc777/OneCamp/tests/integration"
)

// tableEvent is a webhook event about a table's rows, as an in-process
// listener hears it.
type tableEvent struct {
	kind string
	data map[string]interface{}
}

// heardTables is every table row event this test binary has dispatched, by
// table. Listeners can't be taken away, so one is registered, once.
var heardTables = struct {
	sync.Mutex
	once    sync.Once
	byTable map[string][]tableEvent
}{byTable: map[string][]tableEvent{}}

func listenForTableEvents() {
	heardTables.once.Do(func() {
		webhookBusiness.RegisterEventListener(func(_ context.Context, kind string, data map[string]interface{}) {
			switch kind {
			case "table.rows.changed", "table.row.created", "table.row.updated":
			default:
				return
			}
			table, _ := data["table_id"].(string)
			heardTables.Lock()
			heardTables.byTable[table] = append(heardTables.byTable[table], tableEvent{kind, data})
			heardTables.Unlock()
		})
	})
}

// eventsOf is the events heard about a table, of kind.
func eventsOf(table uuid.UUID, kind string) []tableEvent {
	heardTables.Lock()
	defer heardTables.Unlock()
	var out []tableEvent
	for _, e := range heardTables.byTable[table.String()] {
		if e.kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// untilHeard waits for a listener to have heard n events of kind about a
// table: listeners run on goroutines of their own, which nothing settles.
func untilHeard(t *testing.T, table uuid.UUID, kind string, n int) []tableEvent {
	t.Helper()
	integration.Settle(t)
	deadline := time.Now().Add(30 * time.Second)
	for len(eventsOf(table, kind)) < n && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	// Anything more would have been dispatched by now: give it a moment.
	time.Sleep(200 * time.Millisecond)
	return eventsOf(table, kind)
}

// idsIn is an event's list of ids under key, sorted.
func idsIn(e tableEvent, key string) []string {
	var out []string
	switch v := e.data[key].(type) {
	case []string:
		out = append(out, v...)
	case []interface{}:
		for _, x := range v {
			out = append(out, fmt.Sprint(x))
		}
	}
	sort.Strings(out)
	return out
}

func sortedIDs(rows []*dataTableModel.Row) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Id.String()
	}
	sort.Strings(out)
	return out
}

// order is a table's rows' ids in the order they're read.
func (w *tableWorld) order(tb *dataTableModel.DataTable) []uuid.UUID {
	w.t.Helper()
	rows, err := dataTableModel.ListRows(w.ctx, tb.Id, 1000, 0)
	if err != nil {
		w.t.Fatal(err)
	}
	out := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		out[i] = r.Id
	}
	return out
}

func TestTableLedger(t *testing.T) {
	listenForTableEvents()
	w := newTableWorld(t, "amara", "bo")
	amara, bo := w.people["amara"], w.people["bo"]
	select2 := map[string]interface{}{"options": []map[string]interface{}{{"label": "Open"}, {"label": "Won"}}}

	// D10 (16 Oct): a bulk change sends one table.rows.changed event that
	// names every row and what happened to it, in place of one event a row.
	t.Run("a bulk change of 1,000 rows is one change set and exactly one event, with every row in it", func(t *testing.T) {
		deals := w.table("amara", "Deals")
		status := w.field("amara", deals, "Status", dataTableModel.FieldSelect, select2)
		notes := w.field("amara", deals, "Notes", dataTableModel.FieldText, nil)
		var in dataTableBusiness.BatchInput
		for i := 0; i < 1000; i++ {
			in.Create = append(in.Create, dataTableBusiness.RowInput{Values: map[string]interface{}{notes: fmt.Sprintf("row %d", i), status: "Open"}})
		}
		made, err := dataTableBusiness.Batch(w.ctx, deals.Id, in, amara)
		if err != nil || len(made.Created) != 1000 {
			t.Fatalf("a batch of 1,000 new rows: %v", err)
		}
		for i := 1; i < len(made.Created); i++ {
			if made.Created[i].PositionKey <= made.Created[i-1].PositionKey {
				t.Fatalf("row %d's place %q isn't after row %d's %q", i, made.Created[i].PositionKey, i-1, made.Created[i-1].PositionKey)
			}
		}
		var sets, changes int
		if err := w.env.PG.QueryRow(`SELECT COUNT(*), COALESCE(SUM(rows), 0) FROM data_table_change_sets WHERE table_id = $1`, deals.Id).Scan(&sets, &changes); err != nil {
			t.Fatal(err)
		}
		if sets != 1 || changes != 1000 {
			t.Errorf("the batch is %d change sets of %d rows, want one of 1,000", sets, changes)
		}
		created := untilHeard(t, deals.Id, "table.rows.changed", 1)
		if len(created) != 1 {
			t.Fatalf("1,000 new rows sent %d table.rows.changed events, want exactly one", len(created))
		}
		if got := idsIn(created[0], "created_row_ids"); fmt.Sprint(got) != fmt.Sprint(sortedIDs(made.Created)) {
			t.Errorf("the event names %d new rows, want all 1,000", len(got))
		}
		if created[0].data["change_set_id"] != made.ChangeSet.String() || created[0].data["change"] != "create" {
			t.Errorf("the event is %v %v, want change set %s, create", created[0].data["change_set_id"], created[0].data["change"], made.ChangeSet)
		}
		if n := len(eventsOf(deals.Id, "table.row.created")); n != 0 {
			t.Errorf("a batch sent %d per-row events", n)
		}

		// Every row's Status, in one paste.
		paste := dataTableBusiness.BatchInput{Kind: "paste"}
		for _, r := range made.Created {
			paste.Patch = append(paste.Patch, dataTableBusiness.PatchInput{Row: r.Id, Values: map[string]interface{}{status: "Won"}})
		}
		pasted, err := dataTableBusiness.Batch(w.ctx, deals.Id, paste, bo)
		if err != nil || len(pasted.Updated) != 1000 {
			t.Fatalf("pasting into 1,000 rows: %v", err)
		}
		events := untilHeard(t, deals.Id, "table.rows.changed", 2)
		if len(events) != 2 {
			t.Fatalf("the paste sent %d events, want one more", len(events)-1)
		}
		e := events[1]
		if got := idsIn(e, "updated_row_ids"); fmt.Sprint(got) != fmt.Sprint(sortedIDs(made.Created)) || e.data["change"] != "paste" {
			t.Errorf("the paste's event names %d changed rows (%v), want all 1,000", len(got), e.data["change"])
		}
		byRow, _ := e.data["row_field_ids"].(map[string][]string)
		if len(byRow) != 1000 || fmt.Sprint(byRow[made.Created[7].Id.String()]) != fmt.Sprint([]string{status}) {
			t.Errorf("the paste's event names fields for %d rows: %v", len(byRow), byRow[made.Created[7].Id.String()])
		}
		if n := len(eventsOf(deals.Id, "table.row.updated")); n != 0 {
			t.Errorf("a paste sent %d per-row events", n)
		}

		// Bo's paste, undone: every row says Open again, in one change set.
		undone, err := dataTableBusiness.Undo(w.ctx, deals.Id, nil, bo)
		if err != nil || undone.Reversed != pasted.ChangeSet || undone.Changes != 1000 || undone.Skipped != 0 {
			t.Fatalf("undoing the paste: %+v %v", undone, err)
		}
		var open int
		if err := w.env.PG.QueryRow(`SELECT COUNT(*) FROM data_table_rows WHERE table_id = $1 AND values ->> $2 = 'Open'`, deals.Id, status).Scan(&open); err != nil || open != 1000 {
			t.Errorf("after the undo %d rows say Open (%v)", open, err)
		}
		if events := untilHeard(t, deals.Id, "table.rows.changed", 3); len(events) != 3 || events[2].data["change"] != "undo" {
			t.Fatalf("the undo sent %d events", len(events)-2)
		}

		// And all of them deleted at once: one event, every row in it.
		var del dataTableBusiness.BatchInput
		for _, r := range made.Created {
			del.Delete = append(del.Delete, r.Id)
		}
		if _, err := dataTableBusiness.Batch(w.ctx, deals.Id, del, amara); err != nil {
			t.Fatalf("deleting 1,000 rows: %v", err)
		}
		events = untilHeard(t, deals.Id, "table.rows.changed", 4)
		if len(events) != 4 {
			t.Fatalf("the undo and the delete sent %d events, want two", len(events)-2)
		}
		if got := idsIn(events[3], "deleted_row_ids"); len(got) != 1000 {
			t.Errorf("the delete's event names %d rows", len(got))
		}
	})

	t.Run("a bad cell refuses the whole batch, naming every row it's in", func(t *testing.T) {
		tb := w.table("amara", "Refused")
		status := w.field("amara", tb, "Status", dataTableModel.FieldSelect, select2)
		_, err := dataTableBusiness.Batch(w.ctx, tb.Id, dataTableBusiness.BatchInput{Create: []dataTableBusiness.RowInput{
			{Values: map[string]interface{}{status: "Open"}},
			{Values: map[string]interface{}{status: "Lost"}},
			{Values: map[string]interface{}{status: "Gone"}},
		}}, amara)
		var bad dataTableBusiness.BatchErrors
		if !errors.As(err, &bad) || len(bad) != 2 || *bad[0].Index != 1 || *bad[1].Index != 2 || bad[0].Field != status {
			t.Fatalf("a batch with two bad rows answered %v", err)
		}
		if rows := w.order(tb); len(rows) != 0 {
			t.Errorf("a refused batch wrote %d rows", len(rows))
		}
		// Over HTTP: a 400 that lists them.
		h := w.asMember("amara", http.MethodPost, "/tables/{id}/rows/batch", rowLedgerController.Batch)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/tables/"+tb.Id.String()+"/rows/batch",
			bytes.NewBufferString(fmt.Sprintf(`{"create": [{"values": {%q: "Lost"}}]}`, status))))
		var out struct {
			Errors []dataTableBusiness.BatchError `json:"errors"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if rec.Code != http.StatusBadRequest || len(out.Errors) != 1 || out.Errors[0].Field != status {
			t.Errorf("the batch route answered %d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("an undo puts back only what still holds what it changed, and says what it left", func(t *testing.T) {
		tb := w.table("amara", "Undo")
		status := w.field("amara", tb, "Status", dataTableModel.FieldSelect, select2)
		notes := w.field("amara", tb, "Notes", dataTableModel.FieldText, nil)
		r, err := dataTableBusiness.CreateRow(w.ctx, tb.Id, dataTableBusiness.RowInput{Values: map[string]interface{}{status: "Open", notes: "first"}}, bo)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := dataTableBusiness.PatchRow(w.ctx, tb.Id, r.Id, map[string]interface{}{status: "Won", notes: "Amara's"}, amara); err != nil {
			t.Fatal(err)
		}
		if _, err := dataTableBusiness.PatchRow(w.ctx, tb.Id, r.Id, map[string]interface{}{notes: "Bo's"}, bo); err != nil {
			t.Fatal(err)
		}
		done, err := dataTableBusiness.Undo(w.ctx, tb.Id, nil, amara)
		if err != nil || done.Changes != 1 || done.Skipped != 1 {
			t.Fatalf("Amara's undo: %+v %v, want one cell back and Bo's note left", done, err)
		}
		if v := w.stored(tb, r.Id); v[status] != "Open" || v[notes] != "Bo's" {
			t.Errorf("after Amara's undo the row holds %v, want Status Open and Bo's note", v)
		}
		// Bo can't undo Amara's change, named or not.
		if _, err := dataTableBusiness.Undo(w.ctx, tb.Id, &done.ChangeSet, bo); err == nil {
			t.Error("Bo undid Amara's undo")
		}
		redone, err := dataTableBusiness.Redo(w.ctx, tb.Id, amara)
		if err != nil || redone.Changes != 1 {
			t.Fatalf("Amara's redo: %+v %v", redone, err)
		}
		if v := w.stored(tb, r.Id); v[status] != "Won" || v[notes] != "Bo's" {
			t.Errorf("after the redo the row holds %v", v)
		}
		// Undo again undoes the redo; with nothing left of Amara's, there's nothing to redo.
		if _, err := dataTableBusiness.Undo(w.ctx, tb.Id, nil, amara); err != nil {
			t.Fatal(err)
		}
		if v := w.stored(tb, r.Id); v[status] != "Open" {
			t.Errorf("undoing the redo left Status %v", v[status])
		}
		if _, err := dataTableBusiness.Undo(w.ctx, tb.Id, nil, amara); !dataTableBusiness.IsConflict(err) {
			t.Errorf("an undo with nothing left of Amara's answered %v", err)
		}
		// Bo's own changes are his to undo: his note, then the row he made.
		if _, err := dataTableBusiness.Undo(w.ctx, tb.Id, nil, bo); err != nil {
			t.Fatal(err)
		}
		if v := w.stored(tb, r.Id); v[notes] != "Amara's" {
			t.Errorf("Bo's undo left the note %v, want Amara's again", v[notes])
		}
		if done, err := dataTableBusiness.Undo(w.ctx, tb.Id, nil, bo); err != nil || done.Skipped != 1 {
			t.Errorf("undoing the row Bo made, changed since: %+v %v, want it left", done, err)
		}
		if row, _ := dataTableModel.GetRowByID(w.ctx, tb.Id, r.Id); row == nil {
			t.Error("a row changed since it was made was deleted by its maker's undo")
		}
	})

	t.Run("each kind of change undoes: rows made, deleted with their links, moved, and links", func(t *testing.T) {
		vendors := w.table("amara", "Vendors")
		vName := w.field("amara", vendors, "Vendor", dataTableModel.FieldText, nil)
		budget := w.table("amara", "Budget")
		item := w.field("amara", budget, "Item", dataTableModel.FieldText, nil)
		vendor := w.field("amara", budget, "Vendor", dataTableModel.FieldRelation,
			map[string]interface{}{"relation_target": "table", "table_id": vendors.Id.String(), "two_way": true})
		acme, err := dataTableBusiness.CreateRow(w.ctx, vendors.Id, dataTableBusiness.RowInput{Values: map[string]interface{}{vName: "Acme"}}, amara)
		if err != nil {
			t.Fatal(err)
		}
		made, err := dataTableBusiness.Batch(w.ctx, budget.Id, dataTableBusiness.BatchInput{Create: []dataTableBusiness.RowInput{
			{Values: map[string]interface{}{item: "Booth", vendor: []interface{}{acme.Id.String()}}},
			{Values: map[string]interface{}{item: "Banner"}},
			{Values: map[string]interface{}{item: "Badges"}},
		}}, amara)
		if err != nil {
			t.Fatal(err)
		}
		booth, banner, badges := made.Created[0].Id, made.Created[1].Id, made.Created[2].Id
		if has, _ := dataTableModel.RowHasLinks(w.ctx, booth); !has {
			t.Fatal("the booth isn't linked to Acme")
		}

		// Moved: the badges first, then back.
		if _, err := dataTableBusiness.MoveRows(w.ctx, budget.Id, []uuid.UUID{badges}, dataTableBusiness.PlaceInput{Before: &booth}, amara); err != nil {
			t.Fatal(err)
		}
		if got := w.order(budget); fmt.Sprint(got) != fmt.Sprint([]uuid.UUID{badges, booth, banner}) {
			t.Errorf("after the move the rows are %v", got)
		}
		if _, err := dataTableBusiness.Undo(w.ctx, budget.Id, nil, amara); err != nil {
			t.Fatal(err)
		}
		if got := w.order(budget); fmt.Sprint(got) != fmt.Sprint([]uuid.UUID{booth, banner, badges}) {
			t.Errorf("after the move's undo the rows are %v", got)
		}

		// Deleted with its link, and back with it.
		if err := dataTableBusiness.DeleteRow(w.ctx, budget.Id, booth, amara); err != nil {
			t.Fatal(err)
		}
		if has, _ := dataTableModel.RowHasLinks(w.ctx, acme.Id); has {
			t.Error("a deleted row's link outlived it")
		}
		if _, err := dataTableBusiness.Undo(w.ctx, budget.Id, nil, amara); err != nil {
			t.Fatal(err)
		}
		if row, _ := dataTableModel.GetRowByID(w.ctx, budget.Id, booth); row == nil {
			t.Fatal("the undo didn't bring the booth back")
		}
		if has, _ := dataTableModel.RowHasLinks(w.ctx, booth); !has {
			t.Error("the booth came back without its link to Acme")
		}
		if got := w.order(budget); got[0] != booth {
			t.Errorf("the booth came back at %v, not first", got)
		}

		// A link made, and undone.
		if _, _, err := dataTableBusiness.ChangeLinks(w.ctx, budget.Id, banner, uuid.MustParse(vendor), []uuid.UUID{acme.Id}, nil, amara); err != nil {
			t.Fatal(err)
		}
		if has, _ := dataTableModel.RowHasLinks(w.ctx, banner); !has {
			t.Fatal("the banner wasn't linked")
		}
		if _, err := dataTableBusiness.Undo(w.ctx, budget.Id, nil, amara); err != nil {
			t.Fatal(err)
		}
		if has, _ := dataTableModel.RowHasLinks(w.ctx, banner); has {
			t.Error("the link's undo left the banner linked")
		}

		// The rows made, undone: they go; redone, they're back.
		gone := w.table("amara", "Made")
		g := w.field("amara", gone, "Item", dataTableModel.FieldText, nil)
		three, err := dataTableBusiness.Batch(w.ctx, gone.Id, dataTableBusiness.BatchInput{Create: []dataTableBusiness.RowInput{
			{Values: map[string]interface{}{g: "a"}}, {Values: map[string]interface{}{g: "b"}}, {Values: map[string]interface{}{g: "c"}},
		}}, amara)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := dataTableBusiness.Undo(w.ctx, gone.Id, &three.ChangeSet, amara); err != nil {
			t.Fatal(err)
		}
		if rows := w.order(gone); len(rows) != 0 {
			t.Errorf("after the undo %d of the rows made are left", len(rows))
		}
		if _, err := dataTableBusiness.Redo(w.ctx, gone.Id, amara); err != nil {
			t.Fatal(err)
		}
		if rows := w.order(gone); fmt.Sprint(rows) != fmt.Sprint([]uuid.UUID{three.Created[0].Id, three.Created[1].Id, three.Created[2].Id}) {
			t.Errorf("after the redo the rows are %v", rows)
		}
	})

	t.Run("the trash brings rows back with their links, but not to rows deleted since", func(t *testing.T) {
		vendors := w.table("amara", "Vendors 2")
		vName := w.field("amara", vendors, "Vendor", dataTableModel.FieldText, nil)
		budget := w.table("amara", "Budget 2")
		item := w.field("amara", budget, "Item", dataTableModel.FieldText, nil)
		w.field("amara", budget, "Vendor", dataTableModel.FieldRelation,
			map[string]interface{}{"relation_target": "table", "table_id": vendors.Id.String(), "two_way": true})
		vs, err := dataTableBusiness.Batch(w.ctx, vendors.Id, dataTableBusiness.BatchInput{Create: []dataTableBusiness.RowInput{
			{Values: map[string]interface{}{vName: "Acme"}}, {Values: map[string]interface{}{vName: "Hooli"}},
		}}, amara)
		if err != nil {
			t.Fatal(err)
		}
		acme, hooli := vs.Created[0].Id, vs.Created[1].Id
		fields, _ := dataTableModel.ListFields(w.ctx, budget.Id)
		var vendor string
		for _, f := range fields {
			if f.Name == "Vendor" {
				vendor = f.Id.String()
			}
		}
		b, err := dataTableBusiness.CreateRow(w.ctx, budget.Id, dataTableBusiness.RowInput{Values: map[string]interface{}{item: "Booth", vendor: []interface{}{acme.String(), hooli.String()}}}, amara)
		if err != nil {
			t.Fatal(err)
		}
		if err := dataTableBusiness.DeleteRow(w.ctx, budget.Id, b.Id, bo); err != nil {
			t.Fatal(err)
		}
		trash, err := dataTableBusiness.TableTrash(w.ctx, budget.Id, nil, amara)
		if err != nil || len(trash.Rows) != 1 || trash.Rows[0].ID != b.Id || trash.Rows[0].DeletedByName != "bo" {
			t.Fatalf("the trash holds %+v (%v), want the booth, deleted by bo", trash, err)
		}
		if err := dataTableBusiness.DeleteRow(w.ctx, vendors.Id, hooli, amara); err != nil {
			t.Fatal(err)
		}
		if _, err := dataTableBusiness.RestoreFromTrash(w.ctx, budget.Id, dataTableBusiness.RestoreInput{Rows: []uuid.UUID{b.Id}}, bo); err != nil {
			t.Fatal(err)
		}
		if v := w.stored(budget, b.Id); v[item] != "Booth" {
			t.Fatalf("the booth came back holding %v", v)
		}
		var toAcme, toHooli int
		_ = w.env.PG.QueryRow(`SELECT COUNT(*) FILTER (WHERE to_row = $2), COUNT(*) FILTER (WHERE to_row = $3) FROM data_table_links WHERE from_row = $1`,
			b.Id, acme, hooli).Scan(&toAcme, &toHooli)
		if toAcme != 1 || toHooli != 0 {
			t.Errorf("the booth came back linked to Acme %d times and Hooli, deleted since, %d", toAcme, toHooli)
		}
		if trash, _ := dataTableBusiness.TableTrash(w.ctx, budget.Id, nil, amara); len(trash.Rows) != 0 {
			t.Errorf("the trash still holds %d rows", len(trash.Rows))
		}
	})

	t.Run("a deleted field comes back with its cells and links", func(t *testing.T) {
		vendors := w.table("amara", "Vendors 3")
		vName := w.field("amara", vendors, "Vendor", dataTableModel.FieldText, nil)
		budget := w.table("amara", "Budget 3")
		item := w.field("amara", budget, "Item", dataTableModel.FieldText, nil)
		notes := w.field("amara", budget, "Notes", dataTableModel.FieldText, nil)
		vendor := w.field("amara", budget, "Vendor", dataTableModel.FieldRelation,
			map[string]interface{}{"relation_target": "table", "table_id": vendors.Id.String(), "two_way": true})
		acme, _ := dataTableBusiness.CreateRow(w.ctx, vendors.Id, dataTableBusiness.RowInput{Values: map[string]interface{}{vName: "Acme"}}, amara)
		b, err := dataTableBusiness.CreateRow(w.ctx, budget.Id, dataTableBusiness.RowInput{Values: map[string]interface{}{item: "Booth", notes: "Hall 2", vendor: []interface{}{acme.Id.String()}}}, amara)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range []string{vendor, notes} {
			if err := dataTableBusiness.DeleteField(w.ctx, budget.Id, uuid.MustParse(f), amara); err != nil {
				t.Fatal(err)
			}
		}
		trash, err := dataTableBusiness.TableTrash(w.ctx, budget.Id, nil, amara)
		if err != nil || len(trash.Fields) != 2 {
			t.Fatalf("the trash holds %d fields (%v)", len(trash.Fields), err)
		}
		if member, _ := dataTableBusiness.TableTrash(w.ctx, budget.Id, nil, bo); len(member.Fields) != 0 {
			t.Error("someone who can't change the table sees its deleted fields")
		}
		if _, err := dataTableBusiness.RestoreFromTrash(w.ctx, budget.Id, dataTableBusiness.RestoreInput{Fields: []uuid.UUID{uuid.MustParse(notes)}}, bo); !dataTableBusiness.IsForbidden(err) {
			t.Errorf("a member brought a field back: %v", err)
		}
		if _, err := dataTableBusiness.RestoreFromTrash(w.ctx, budget.Id, dataTableBusiness.RestoreInput{Fields: []uuid.UUID{uuid.MustParse(vendor), uuid.MustParse(notes)}}, amara); err != nil {
			t.Fatal(err)
		}
		if v := w.stored(budget, b.Id); v[notes] != "Hall 2" {
			t.Errorf("the notes came back holding %v", v[notes])
		}
		if has, _ := dataTableModel.RowHasLinks(w.ctx, b.Id); !has {
			t.Error("the vendor field came back without its links")
		}
		live, _ := dataTableModel.ListFields(w.ctx, vendors.Id)
		back := false
		for _, f := range live {
			back = back || f.Name == "Budget 3"
		}
		if !back {
			t.Error("the field showing the links in Vendors didn't come back with it")
		}
	})

	t.Run("a deleted table comes back for its creator, not for a member", func(t *testing.T) {
		tb := w.table("amara", "Gone for now")
		if err := dataTableBusiness.DeleteTable(w.ctx, tb.Id, amara); err != nil {
			t.Fatal(err)
		}
		mine, err := dataTableBusiness.TablesTrash(w.ctx, amara)
		if err != nil || len(mine) == 0 || mine[0].Table.Id != tb.Id || mine[0].DeletedByName != "amara" {
			t.Fatalf("Amara's tables' trash is %+v (%v)", mine, err)
		}
		if theirs, _ := dataTableBusiness.TablesTrash(w.ctx, bo); len(theirs) != 0 {
			t.Errorf("Bo's trash holds %d of Amara's tables", len(theirs))
		}
		if _, err := dataTableBusiness.RestoreTable(w.ctx, tb.Id, bo); !dataTableBusiness.IsForbidden(err) {
			t.Errorf("Bo brought Amara's table back: %v", err)
		}
		if _, err := dataTableBusiness.RestoreTable(w.ctx, tb.Id, amara); err != nil {
			t.Fatal(err)
		}
		if _, err := dataTableBusiness.GetBundle(w.ctx, tb.Id, amara); err != nil {
			t.Errorf("the table came back unreadable: %v", err)
		}
	})

	t.Run("a row's history says who did what, newest first, to those who can read the table", func(t *testing.T) {
		tb, err := dataTableBusiness.CreateTable(w.ctx, dataTableBusiness.TableInput{Name: "Private", Visibility: "private"}, amara)
		if err != nil {
			t.Fatal(err)
		}
		status := w.field("amara", tb, "Status", dataTableModel.FieldSelect, select2)
		r, err := dataTableBusiness.CreateRow(w.ctx, tb.Id, dataTableBusiness.RowInput{Values: map[string]interface{}{status: "Open"}}, amara)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := dataTableBusiness.PatchRow(w.ctx, tb.Id, r.Id, map[string]interface{}{status: "Won"}, amara); err != nil {
			t.Fatal(err)
		}
		h, err := dataTableBusiness.RowHistory(w.ctx, tb.Id, r.Id, nil, amara)
		if err != nil || len(h.Items) != 2 {
			t.Fatalf("the history is %+v (%v)", h, err)
		}
		edit, made := h.Items[0], h.Items[1]
		if edit.Kind != "edit" || edit.ActorName != "amara" || len(edit.Changes) != 1 ||
			string(edit.Changes[0].Before) != `"Open"` || string(edit.Changes[0].After) != `"Won"` || edit.Changes[0].Field != status {
			t.Errorf("the edit reads %+v", edit)
		}
		if made.Kind != "create" || len(made.Changes) != 1 || made.Changes[0].Event != dataTableModel.RowCreated {
			t.Errorf("the row's making reads %+v", made)
		}
		if _, err := dataTableBusiness.RowHistory(w.ctx, tb.Id, r.Id, nil, bo); !dataTableBusiness.IsForbidden(err) {
			t.Errorf("Bo, who can't open the table, read its row's history: %v", err)
		}
	})

	// G-TABLE-PRIV: a row's history carries what the row stores, a link to an
	// item by its id and kind, never the item's title, which a reader of the
	// table may not be allowed to see.
	t.Run("a row's history names an item by its id and kind, never its title", func(t *testing.T) {
		// The write's answer names the doc for its writer, through the graph:
		// without one of its own the test panicked when run alone, and passed
		// only after a test that had set one up.
		integration.SetupDgraph(t)
		tb := w.table("amara", "Items")
		docs := w.field("amara", tb, "Docs", dataTableModel.FieldRelation, map[string]interface{}{"relation_target": "doc"})
		r, err := dataTableBusiness.CreateRow(w.ctx, tb.Id, dataTableBusiness.RowInput{}, amara)
		if err != nil {
			t.Fatal(err)
		}
		doc := uuid.New().String()
		if _, err := dataTableBusiness.PatchRow(w.ctx, tb.Id, r.Id, map[string]interface{}{docs: []interface{}{map[string]interface{}{"id": doc, "type": "doc", "label": "Layoffs plan"}}}, amara); err != nil {
			t.Fatal(err)
		}
		h, err := dataTableBusiness.RowHistory(w.ctx, tb.Id, r.Id, nil, bo)
		if err != nil || len(h.Items) == 0 || len(h.Items[0].Changes) != 1 {
			t.Fatalf("the history is %+v (%v)", h, err)
		}
		raw, _ := json.Marshal(h)
		if bytes.Contains(raw, []byte("Layoffs")) || !bytes.Contains([]byte(h.Items[0].Changes[0].After), []byte(doc)) {
			t.Errorf("the history reads %s", raw)
		}
	})

	t.Run("a write naming the version it read is refused once the row has changed, with the row as it is", func(t *testing.T) {
		tb := w.table("amara", "Versions")
		notes := w.field("amara", tb, "Notes", dataTableModel.FieldText, nil)
		r, err := dataTableBusiness.CreateRow(w.ctx, tb.Id, dataTableBusiness.RowInput{Values: map[string]interface{}{notes: "one"}}, amara)
		if err != nil {
			t.Fatal(err)
		}
		patch := func(v int64, text string) error {
			_, err := dataTableBusiness.Batch(w.ctx, tb.Id, dataTableBusiness.BatchInput{Patch: []dataTableBusiness.PatchInput{
				{Row: r.Id, Values: map[string]interface{}{notes: text}, Version: v}}}, amara)
			return err
		}
		if err := patch(r.Version, "two"); err != nil {
			t.Fatalf("a write at the row's version: %v", err)
		}
		err = patch(r.Version, "three")
		var conflict *dataTableBusiness.RowConflict
		if !errors.As(err, &conflict) || conflict.Row.Version != r.Version+1 || !bytes.Contains([]byte(conflict.Row.Values), []byte(`"two"`)) {
			t.Fatalf("a stale write answered %v", err)
		}
		if v := w.stored(tb, r.Id); v[notes] != "two" {
			t.Errorf("the stale write left %v", v[notes])
		}
	})

	t.Run("rows go between rows by their keys, and rows made at once never share one", func(t *testing.T) {
		tb := w.table("amara", "Places")
		name := w.field("amara", tb, "Name", dataTableModel.FieldText, nil)
		three, err := dataTableBusiness.Batch(w.ctx, tb.Id, dataTableBusiness.BatchInput{Create: []dataTableBusiness.RowInput{
			{Values: map[string]interface{}{name: "a"}}, {Values: map[string]interface{}{name: "c"}}, {Values: map[string]interface{}{name: "d"}},
		}}, amara)
		if err != nil {
			t.Fatal(err)
		}
		a := three.Created[0].Id
		between, err := dataTableBusiness.Batch(w.ctx, tb.Id, dataTableBusiness.BatchInput{At: &dataTableBusiness.PlaceInput{After: &a},
			Create: []dataTableBusiness.RowInput{{Values: map[string]interface{}{name: "b"}}}}, amara)
		if err != nil {
			t.Fatal(err)
		}
		if got := w.order(tb); fmt.Sprint(got) != fmt.Sprint([]uuid.UUID{a, between.Created[0].Id, three.Created[1].Id, three.Created[2].Id}) {
			t.Errorf("a row after the first went to %v", got)
		}
		var wg sync.WaitGroup
		errs := make(chan error, 40)
		for i := 0; i < 40; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				if _, err := dataTableBusiness.CreateRow(w.ctx, tb.Id, dataTableBusiness.RowInput{Values: map[string]interface{}{name: fmt.Sprint("x", i)}}, bo); err != nil {
					errs <- err
				}
			}(i)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("a row made at once: %v", err)
		}
		var rows, keys int
		if err := w.env.PG.QueryRow(`SELECT COUNT(*), COUNT(DISTINCT position_key) FROM data_table_rows WHERE table_id = $1`, tb.Id).Scan(&rows, &keys); err != nil || rows != 44 || keys != 44 {
			t.Errorf("%d rows have %d places (%v)", rows, keys, err)
		}
	})

	t.Run("an AI fill is the AI's: in the row's history, on no one's undo", func(t *testing.T) {
		tb := w.table("amara", "AI")
		notes := w.field("amara", tb, "Notes", dataTableModel.FieldText, nil)
		summary := w.field("amara", tb, "Summary", dataTableModel.FieldText, nil)
		r, err := dataTableBusiness.CreateRow(w.ctx, tb.Id, dataTableBusiness.RowInput{Values: map[string]interface{}{notes: "Booth"}}, amara)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := dataTableBusiness.PatchRow(w.ctx, tb.Id, r.Id, map[string]interface{}{notes: "Booth, hall 2"}, amara); err != nil {
			t.Fatal(err)
		}
		cs := &dataTableModel.ChangeSet{TableID: tb.Id, ActorKind: "automation", Kind: "ai"}
		answer, _ := json.Marshal(map[string]interface{}{summary: "A booth in hall 2"})
		if _, err := dataTableModel.WriteAICell(w.ctx, tb.Id, r.Id, summary, string(answer), nil, "", false, 100*1024, cs, nil); err != nil {
			t.Fatal(err)
		}
		h, err := dataTableBusiness.RowHistory(w.ctx, tb.Id, r.Id, nil, amara)
		if err != nil || len(h.Items) != 3 || h.Items[0].Kind != "ai" || h.Items[0].ActorKind != "automation" || h.Items[0].ActorID != nil {
			t.Fatalf("the history after the fill is %+v (%v)", h, err)
		}
		// Amara's undo undoes her edit, not the AI's fill.
		if _, err := dataTableBusiness.Undo(w.ctx, tb.Id, nil, amara); err != nil {
			t.Fatal(err)
		}
		if v := w.stored(tb, r.Id); v[notes] != "Booth" || v[summary] != "A booth in hall 2" {
			t.Errorf("Amara's undo left %v", v)
		}
	})
}
