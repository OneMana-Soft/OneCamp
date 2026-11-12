package business

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	rowLedger "github.com/akashc777/OneCamp/business/RowLedger"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// A change set's changes, as the store records them.
func cellChange(row uuid.UUID, field, before, after string) model.Change {
	c := model.Change{Row: row, Field: field}
	if before != "" {
		c.Before = json.RawMessage(before)
	}
	if after != "" {
		c.After = json.RawMessage(after)
	}
	return c
}

func made(row uuid.UUID) model.Change {
	return cellChange(row, model.WholeRow, "", `{"values":{"f":"x"},"key":"a0","version":1}`)
}

func gone(row uuid.UUID) model.Change {
	return cellChange(row, model.WholeRow, `{"values":{"f":"x"},"version":3}`, `{"deleted":true,"version":4}`)
}

func back(row uuid.UUID) model.Change {
	return cellChange(row, model.WholeRow, `{"deleted":true,"version":4}`, `{"values":{"f":"x"},"version":5}`)
}

// D10 (16 Oct): a bulk change is one table.rows.changed webhook that names
// every row it touched and what happened to it, with the fields it changed
// row by row, so a consumer needs no read per row; ids alone, in keys a
// webhook sent only ids keeps (…_id, …_ids).
func TestABulkChangeIsOneEventNamingEveryRow(t *testing.T) {
	table := &model.DataTable{Id: uuid.New(), Name: "Deals"}
	cs := &model.ChangeSet{ID: uuid.New(), TableID: table.Id, Kind: string(rowLedger.ChangePaste), Fields: []string{"status", "notes"}}
	var created, updated, deleted, restored []string
	rowFields := map[string][]string{}
	for i := 0; i < 1000; i++ {
		row := uuid.New()
		switch i % 4 {
		case 0:
			cs.Changes = append(cs.Changes, made(row))
			created = append(created, row.String())
		case 1:
			cs.Changes = append(cs.Changes, cellChange(row, "status", `"Open"`, `"Won"`), cellChange(row, "notes", "", `"Call"`))
			updated = append(updated, row.String())
			rowFields[row.String()] = []string{"status", "notes"}
		case 2:
			cs.Changes = append(cs.Changes, gone(row))
			deleted = append(deleted, row.String())
		case 3:
			cs.Changes = append(cs.Changes, back(row))
			restored = append(restored, row.String())
		}
	}
	got := rowsChanged(table, cs)
	want := map[string]interface{}{
		"table_id":         table.Id.String(),
		"table_name":       "Deals",
		"change_set_id":    cs.ID.String(),
		"change":           "paste",
		"rows_changed":     1000,
		"created_row_ids":  created,
		"updated_row_ids":  updated,
		"deleted_row_ids":  deleted,
		"restored_row_ids": restored,
		"field_ids":        []string{"status", "notes"},
		"row_field_ids":    rowFields,
	}
	if !reflect.DeepEqual(got, want) {
		for k := range want {
			if !reflect.DeepEqual(got[k], want[k]) {
				t.Errorf("%s: got %v", k, got[k])
			}
		}
	}
	// What a webhook sent only ids keeps: every row, by what happened to it.
	for _, k := range []string{"table_id", "change_set_id", "created_row_ids", "updated_row_ids", "deleted_row_ids", "restored_row_ids", "field_ids", "row_field_ids"} {
		if k[len(k)-3:] != "_id" && k[len(k)-4:] != "_ids" {
			t.Errorf("%s doesn't name ids, so a webhook sent ids alone loses it", k)
		}
	}
}

// V-457 / H3: a table's event says where its change came from (the change
// set's actor kind: person, api, agent, automation, import, form, system, as
// Airtable's fromSources) and who made it, the agent too when one acted for
// them, so a sync tells its own writes from everyone else's with no read; in
// keys a webhook sent only ids keeps. A change by no one says no one.
func TestATablesEventSaysWhereItsChangeCameFromAndWho(t *testing.T) {
	table := &model.DataTable{Id: uuid.New(), Name: "Deals"}
	person, agent, row := uuid.New(), uuid.New(), uuid.New()
	cs := &model.ChangeSet{ID: uuid.New(), TableID: table.Id, Kind: string(rowLedger.ChangeEdit), Actor: person, Agent: agent, ActorKind: "api",
		Changes: []model.Change{cellChange(row, "status", `"Open"`, `"Won"`)}}
	got := rowsChanged(table, cs)
	for k, want := range map[string]interface{}{"source": "api", "actor_id": person.String(), "agent_id": agent.String()} {
		if got[k] != want {
			t.Errorf("%s: got %v, want %v", k, got[k], want)
		}
	}
	// What a webhook sent only ids keeps (business/Webhook namesSomething):
	// "source", and keys ending _id.
	for _, k := range []string{"actor_id", "agent_id"} {
		if k[len(k)-3:] != "_id" {
			t.Errorf("%s doesn't name an id, so a webhook sent ids alone loses it", k)
		}
	}

	// The server's own change, made by no one and for no one, names neither.
	own := &model.ChangeSet{ID: uuid.New(), TableID: table.Id, Kind: string(rowLedger.ChangeEdit), Changes: []model.Change{cellChange(row, "status", `"Won"`, `"Lost"`)}}
	got = rowsChanged(table, own)
	for _, k := range []string{"source", "actor_id", "agent_id"} {
		if v, ok := got[k]; ok {
			t.Errorf("a change by no one says %s %v", k, v)
		}
	}
	// A person acting themselves names no agent.
	own.Actor, own.ActorKind = person, "person"
	got = rowsChanged(table, own)
	if got["source"] != "person" || got["actor_id"] != person.String() {
		t.Errorf("a person's change says %v / %v", got["source"], got["actor_id"])
	}
	if v, ok := got["agent_id"]; ok {
		t.Errorf("a person acting themselves names agent %v", v)
	}
}

// A field's change may rewrite every row: its event names at most a batch's
// worth, and says how many it changed.
func TestAFieldsChangeNamesAtMostABatchOfRows(t *testing.T) {
	cs := &model.ChangeSet{ID: uuid.New(), Kind: string(rowLedger.ChangeField), Fields: []string{"status"}}
	for i := 0; i < 2500; i++ {
		cs.Changes = append(cs.Changes, cellChange(uuid.New(), "status", `"Todo"`, `"To do"`))
	}
	got := rowsChanged(&model.DataTable{Id: uuid.New()}, cs)
	if n := len(got["updated_row_ids"].([]string)); n != maxBatch || got["rows_changed"] != 2500 {
		t.Errorf("a field's change named %d rows of %v", n, got["rows_changed"])
	}
	if n := len(got["row_field_ids"].(map[string][]string)); n != maxBatch {
		t.Errorf("its fields are named for %d rows", n)
	}
}

// The one live message a bulk change sends names at most 500 rows; past that,
// open views read the table again.
func TestABulkChangesLiveMessageNamesItsRowsUpToAPoint(t *testing.T) {
	cs := &model.ChangeSet{ID: uuid.New()}
	first := uuid.New()
	cs.Changes = append(cs.Changes, made(first))
	for i := 0; i < 10; i++ {
		cs.Changes = append(cs.Changes, gone(uuid.New()))
	}
	msg := liveChanged(cs)
	if msg["action"] != "changed" || msg["change_set"] != cs.ID.String() || !reflect.DeepEqual(msg["created"], []string{first.String()}) {
		t.Errorf("the message is %v", msg)
	}
	if len(msg["deleted"].([]string)) != 10 || msg["more"] != nil {
		t.Errorf("a small change's message is %v", msg)
	}
	moved := &model.ChangeSet{ID: uuid.New(), Changes: []model.Change{cellChange(first, model.FieldPosition, `"a0"`, `"a1"`)}}
	if m := liveChanged(moved); m["moved"] != true || !reflect.DeepEqual(m["updated"], []string{first.String()}) {
		t.Errorf("a move's message is %v", m)
	}
	for i := 0; i < 600; i++ {
		cs.Changes = append(cs.Changes, cellChange(uuid.New(), "f", `1`, `2`))
	}
	msg = liveChanged(cs)
	named := len(msg["created"].([]string)) + len(msg["updated"].([]string)) + len(msg["deleted"].([]string)) + len(msg["restored"].([]string))
	if named != rowsTold || msg["more"] != true {
		t.Errorf("a big change's message names %d rows (more %v)", named, msg["more"])
	}
}

// A row's own live message (a cell edit, an AI fill) names the cells its
// change set changed, by id alone, so an open view reads those cells and no
// more (tables L1); a row created, or moved, is read whole.
func TestARowsLiveMessageNamesTheCellsItChanged(t *testing.T) {
	row := uuid.New()
	r := &model.Row{Id: row}
	cs := &model.ChangeSet{ID: uuid.New(), Changes: []model.Change{
		cellChange(row, "amount", `1`, `2`), cellChange(row, "note", `"a"`, ""), cellChange(row, "amount", `2`, `3`),
		cellChange(uuid.New(), "other", `1`, `2`),
	}}
	want := map[string]interface{}{"action": "updated", "row_id": row.String(), "fields": []string{"amount", "note"}}
	if msg := rowMessage("updated", r, cs); !reflect.DeepEqual(msg, want) {
		t.Errorf("a cell edit's message is %v, want %v", msg, want)
	}
	moved := &model.ChangeSet{ID: uuid.New(), Changes: append(append([]model.Change{}, cs.Changes...), cellChange(row, model.FieldPosition, `"a0"`, `"a1"`))}
	if msg := rowMessage("updated", r, moved); msg["fields"] != nil {
		t.Errorf("a row moved is read whole, but its message is %v", msg)
	}
	if msg := rowMessage("created", r, nil); len(msg) != 2 || msg["fields"] != nil {
		t.Errorf("a row made is read whole, but its message is %v", msg)
	}
}

// A row's history says each change as its reader can show it: a cell's value
// before and after, or what happened to the row.
func TestAHistorysChanges(t *testing.T) {
	row := uuid.New()
	cases := []struct {
		c    model.Change
		want HistoryChange
	}{
		{cellChange(row, "status", `"Open"`, `"Won"`), HistoryChange{Field: "status", Before: json.RawMessage(`"Open"`), After: json.RawMessage(`"Won"`)}},
		{made(row), HistoryChange{Event: model.RowCreated}},
		{gone(row), HistoryChange{Event: model.RowDeleted}},
		{back(row), HistoryChange{Event: model.RowRestored}},
		{cellChange(row, model.FieldPosition, `"a0"`, `"a1"`), HistoryChange{Event: "moved"}},
		{cellChange(row, model.LinksPrefix+"vendor", "", fmt.Sprintf(`{"link_field":%q,"add":[{"from":%q,"to":%q}]}`, uuid.New(), row, uuid.New())),
			HistoryChange{Field: "vendor", Event: "links", After: json.RawMessage(`{"linked":1,"unlinked":0}`)}},
	}
	for _, c := range cases {
		if got := historyChange(c.c); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s %s: %+v, want %+v", c.c.Field, c.c.After, got, c.want)
		}
	}
}

// A batch's kind is one a writer may name, or what it does.
func TestABatchsKind(t *testing.T) {
	cases := []struct {
		in   BatchInput
		want rowLedger.ChangeKind
	}{
		{BatchInput{Kind: "paste", Patch: []PatchInput{{}}}, rowLedger.ChangePaste},
		{BatchInput{Kind: "undo", Patch: []PatchInput{{}}}, rowLedger.ChangeEdit},
		{BatchInput{Create: []RowInput{{}}}, rowLedger.ChangeCreate},
		{BatchInput{Delete: []uuid.UUID{uuid.New()}}, rowLedger.ChangeDelete},
		{BatchInput{Create: []RowInput{{}}, Delete: []uuid.UUID{uuid.New()}}, rowLedger.ChangeEdit},
	}
	for _, c := range cases {
		if got := batchKind(c.in); got != c.want {
			t.Errorf("%+v: %s, want %s", c.in, got, c.want)
		}
	}
}

// The purges keep everything until the owner decides how long (O3): at 0
// days nothing past its retention goes, and neither purge asks the database
// for it (no database is connected here, so any such read fails the test).
// The trash's sweep only finishes a delete for good a stopped server left
// part way (V-121): it asks for that one once, and here there's none.
func TestThePurgesKeepEverythingAtZero(t *testing.T) {
	ctx := context.Background()
	asked := 0
	was := claimStalePurge
	claimStalePurge = func(context.Context, time.Time, time.Time) (model.Claim, bool, error) {
		asked++
		return model.Claim{}, false, nil
	}
	t.Cleanup(func() { claimStalePurge = was })
	if err := trashPurge(ctx, 0, time.Now()); err != nil {
		t.Errorf("the trash purge at 0: %v", err)
	}
	if asked != 1 {
		t.Errorf("the trash purge at 0 asked for a delete for good left part way %d times, want once", asked)
	}
	if err := historyPurge(ctx, 0, time.Now()); err != nil {
		t.Errorf("the history purge at 0: %v", err)
	}
}

// A change set's cells, for the AI columns that read them.
func TestTheCellsAChangeSetChanged(t *testing.T) {
	row, other := uuid.New(), uuid.New()
	cs := &model.ChangeSet{Changes: []model.Change{
		cellChange(row, "status", `"Open"`, `"Won"`), made(other), cellChange(row, model.FieldPosition, `"a0"`, `"a1"`),
		cellChange(row, model.LinksPrefix+"vendor", "", `{}`), cellChange(other, "notes", "", `"x"`),
	}}
	if got := changedIn(cs, row); !reflect.DeepEqual(got, map[string]bool{"status": true}) {
		t.Errorf("the row's changed cells are %v", got)
	}
}

// A field in the trash is read by nothing: a row's history leaves out its
// changes, to its cells and its links (kept for its restore, D14), and keeps
// the live fields', whole rows' and moves.
func TestAHistoryLeavesOutFieldsInTheTrash(t *testing.T) {
	row := uuid.New()
	live, trashed := uuid.New().String(), uuid.New().String()
	changes := []model.Change{
		cellChange(row, live, `1`, `2`),
		cellChange(row, trashed, `"a"`, `"b"`),
		{Row: row, Field: model.LinksPrefix + trashed},
		{Row: row, Field: model.LinksPrefix + live},
		{Row: row, Field: model.WholeRow},
		{Row: row, Field: model.FieldPosition},
	}
	got := liveChanges(changes, map[string]bool{live: true})
	want := []model.Change{changes[0], changes[3], changes[4], changes[5]}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the history keeps %+v", got)
	}
	if len(changes) != 6 || changes[1].Field != trashed {
		t.Error("leaving changes out rewrote the change set's own list")
	}
}

// Every computed read leaves out fields in the trash, whoever passes them, in
// order, and passes the list through as it is when none is.
func TestAComputedReadLeavesOutFieldsInTheTrash(t *testing.T) {
	now := time.Now()
	a, b, c := &model.Field{Id: uuid.New()}, &model.Field{Id: uuid.New(), DeletedAt: &now}, &model.Field{Id: uuid.New()}
	if got := liveOnly([]*model.Field{a, b, c}); !reflect.DeepEqual(got, []*model.Field{a, c}) {
		t.Errorf("live fields %v", got)
	}
	all := []*model.Field{a, c}
	if got := liveOnly(all); &got[0] != &all[0] {
		t.Error("a list with nothing in the trash was copied")
	}
}

// A column's change is no row's (V-455): a change set of columns alone tells
// no rows, and one with cells names only their rows.
func TestAColumnsChangeIsNoRowsEvent(t *testing.T) {
	field, row := uuid.New(), uuid.New()
	column := model.Change{Row: field, Field: model.FieldColumn, After: json.RawMessage(`{"name":"Size"}`)}
	alone := &model.ChangeSet{ID: uuid.New(), Kind: string(rowLedger.ChangeField), Changes: []model.Change{column}}
	if !columnsOnly(alone) {
		t.Error("a change set of a column alone isn't columns only")
	}
	created, updated, deleted, restored, _ := rowsByEvent(alone)
	if len(created)+len(updated)+len(deleted)+len(restored) != 0 {
		t.Errorf("a column's change told rows: %v %v %v %v", created, updated, deleted, restored)
	}
	both := &model.ChangeSet{ID: uuid.New(), Kind: string(rowLedger.ChangeField),
		Changes: []model.Change{column, cellChange(row, field.String(), `"12"`, `12`)}}
	if columnsOnly(both) || columnsOnly(&model.ChangeSet{}) {
		t.Error("a change set with cells, or with nothing, is columns only")
	}
	if got := rowsChanged(&model.DataTable{Id: uuid.New()}, both)["updated_row_ids"].([]string); !reflect.DeepEqual(got, []string{row.String()}) {
		t.Errorf("a field's change with its cells named rows %v", got)
	}
	if n := len(columnsIn(both.Changes)); n != 1 {
		t.Errorf("%d column changes", n)
	}
}

// An undo never puts back a column linking two tables alone (its other half
// lives in the other table); any other column it may.
func TestAColumnLinkingTwoTablesIsntPutBackByAnUndo(t *testing.T) {
	link := json.RawMessage(`{"name":"Vendor","type":"relation","config":{"relation_target":"table","table_id":"` + uuid.NewString() + `"}}`)
	trashed := json.RawMessage(`{"name":"Vendor","type":"relation","config":{"relation_target":"table","table_id":"` + uuid.NewString() + `"},"deleted":true}`)
	plain := json.RawMessage(`{"name":"Size","type":"text","config":{}}`)
	items := json.RawMessage(`{"name":"Refs","type":"relation","config":{"relation_target":"items"}}`)
	col := func(before, after json.RawMessage) []model.Change {
		return []model.Change{{Row: uuid.New(), Field: model.FieldColumn, Before: before, After: after}}
	}
	for name, c := range map[string]struct {
		changes []model.Change
		want    error
	}{
		"a link made":            {col(nil, link), errLinkedColumnMade},
		"a link brought back":    {col(trashed, link), errLinkedColumnMade},
		"a link deleted":         {col(link, trashed), errLinkedColumnDeleted},
		"a link renamed":         {col(link, link), errLinkedColumnChanged},
		"a column made a link":   {col(plain, link), errLinkedColumnChanged},
		"a plain column made":    {col(nil, plain), nil},
		"a link to items made":   {col(nil, items), nil},
		"a plain column renamed": {col(plain, plain), nil},
	} {
		if got := columnUndoProblem(c.changes); got != c.want {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
}
