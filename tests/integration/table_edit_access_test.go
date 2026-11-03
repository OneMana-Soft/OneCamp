//go:build integration
// +build integration

package integration_test

// Who edits a table's rows, against Postgres 12 with every migration (V-170).
//
// Every row write asked only whether its actor could open the table, so every
// member who could open a workspace table could change it. A table's owner now
// chooses: everyone who can open it edits (the default), or only the people
// they choose, and the rest view and comment. A member who may only view is
// refused every row write with 403 {"msg": "You can view this table",
// "read_only": true}; each change to who edits is in the audit log, with what
// it was before and after; removing a person's grant is a soft delete. Forms,
// file links and the row ledger ask the same rule (tableAccess.Tables), so a
// form whose maker may only view can't be saved, and a live one stops taking
// answers.
//
// Run: go test -tags=integration ./tests/integration/ -run TestTableEditAccess -v

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	tableAccess "github.com/akashc777/OneCamp/business/TableAccess"
	dataTableController "github.com/akashc777/OneCamp/controllers/DataTable"
	v1Controller "github.com/akashc777/OneCamp/controllers/V1"
	dataTableModel "github.com/akashc777/OneCamp/models/postgres/DataTable"
)

// call serves one request as who, through the route pattern given, and
// returns the answer's code and its JSON body.
func (w *tableWorld) call(who, method, pattern, path string, h http.HandlerFunc, body string) (int, map[string]interface{}) {
	w.t.Helper()
	rec := httptest.NewRecorder()
	w.asMember(who, method, pattern, h).ServeHTTP(rec, httptest.NewRequest(method, path, bytes.NewBufferString(body)))
	var out map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestTableEditAccess(t *testing.T) {
	w := newTableWorld(t, "ana", "ben")
	ana, ben := w.people["ana"], w.people["ben"]
	vendors := w.table("ana", "Vendors")
	city := w.field("ana", vendors, "City", "text", nil)
	row, err := dataTableBusiness.CreateRow(w.ctx, vendors.Id, dataTableBusiness.RowInput{Values: map[string]interface{}{city: "Leeds"}}, ana)
	if err != nil {
		t.Fatalf("seed row: %v", err)
	}
	base := "/tables/" + vendors.Id.String()
	editing := func(who, body string) (int, map[string]interface{}) {
		return w.call(who, http.MethodPost, "/tables/{id}/editing", base+"/editing", dataTableController.SetTableEditing, body)
	}
	patch := func(who, value string) (int, map[string]interface{}) {
		return w.call(who, http.MethodPost, "/tables/{id}/rows/{rowId}/patch", base+"/rows/"+row.Id.String()+"/patch",
			dataTableController.PatchRow, `{"values":{"`+city+`":"`+value+`"}}`)
	}
	bundle := func(t *testing.T, who string) map[string]interface{} {
		t.Helper()
		code, out := w.call(who, http.MethodGet, "/tables/{id}", base, dataTableController.GetTable, "")
		if code != http.StatusOK {
			t.Fatalf("%s opening the table: %d %v", who, code, out)
		}
		data, _ := out["data"].(map[string]interface{})
		if data == nil {
			data = out
		}
		return data
	}
	audits := func(t *testing.T) int {
		t.Helper()
		var n int
		if err := w.env.PG.QueryRow(`SELECT count(*) FROM admin_audit_log WHERE action = 'table.editing.changed'
			AND metadata::jsonb->>'table_id' = $1`, vendors.Id.String()).Scan(&n); err != nil {
			t.Fatalf("count audit rows: %v", err)
		}
		return n
	}
	readOnly := func(t *testing.T, name string, code int, out map[string]interface{}) {
		t.Helper()
		if code != http.StatusForbidden || out["read_only"] != true || out["msg"] != dataTableBusiness.ReadOnlyWords {
			t.Errorf("%s: %d %v, want 403 read_only %q", name, code, out, dataTableBusiness.ReadOnlyWords)
		}
	}
	// rule is what ben may do as forms, file links and the ledger ask it
	// (business/Form's checkTable and rowsOpen refuse without Edit).
	rule := func(t *testing.T, name string, edit bool) {
		t.Helper()
		d, err := tableAccess.Tables.Resolve(w.ctx, tableAccess.Who{User: ben.UserID}, vendors.Id)
		if err != nil || !d.View || d.Edit != edit || d.Manage {
			t.Errorf("%s: the rule for ben %+v %v, want view, edit %v, no manage", name, d, err, edit)
		}
	}
	// levels is ben's level on the table and on its row as the Access
	// Resolver decides many at once (its "table" and "table_row" kinds read
	// his grants in one batch, EditGrantedAmong), so a removed grant must not
	// count there either.
	levels := func(t *testing.T, name string, want accessBusiness.Level) {
		t.Helper()
		onTable := accessBusiness.Resource{Kind: tableAccess.KindTable, UUID: vendors.Id.String()}
		onRow := accessBusiness.Resource{Kind: tableAccess.KindTableRow, UUID: row.Id.String()}
		got, err := accessBusiness.ResolveAll(w.ctx, accessBusiness.Subject{UserUUID: ben.UserID.String()}, []accessBusiness.Resource{onTable, onRow})
		if err != nil || got[onTable].Level != want || got[onRow].Level != want {
			t.Errorf("%s: the Access Resolver gives ben %v on the table and %v on its row (%v), want %v", name, got[onTable].Level, got[onRow].Level, err, want)
		}
	}

	t.Run("everyone who can open a table edits it until its owner chooses", func(t *testing.T) {
		if code, out := patch("ben", "York"); code != http.StatusOK {
			t.Fatalf("a member's edit on a table everyone edits: %d %v", code, out)
		}
		if b := bundle(t, "ben"); b["can_edit"] != true || b["editing"] != nil {
			t.Errorf("a member's bundle: can_edit %v, editing %v", b["can_edit"], b["editing"])
		}
		rule(t, "a table everyone edits", true)
		levels(t, "a table everyone edits", accessBusiness.LevelEdit)
	})

	t.Run("only the people its owner chooses: the rest view, and every row write is refused in words", func(t *testing.T) {
		if code, out := editing("ana", `{"general":"comment"}`); code != http.StatusOK || out["changed"] != true {
			t.Fatalf("the owner choosing: %d %v", code, out)
		}
		if n := audits(t); n != 1 {
			t.Errorf("audit rows after one change: %d", n)
		}
		code, out := patch("ben", "Hull")
		readOnly(t, "patch", code, out)
		code, out = w.call("ben", http.MethodPost, "/tables/{id}/rows", base+"/rows", dataTableController.CreateRow, `{"values":{"`+city+`":"Bath"}}`)
		readOnly(t, "create", code, out)
		for name, write := range map[string]func() error{
			"update": func() error {
				_, err := dataTableBusiness.UpdateRow(w.ctx, vendors.Id, row.Id, dataTableBusiness.RowInput{Values: map[string]interface{}{city: "Ely"}}, ben)
				return err
			},
			"delete": func() error { return dataTableBusiness.DeleteRow(w.ctx, vendors.Id, row.Id, ben) },
			"undo": func() error {
				_, err := dataTableBusiness.Undo(w.ctx, vendors.Id, nil, ben)
				return err
			},
			"redo": func() error {
				_, err := dataTableBusiness.Redo(w.ctx, vendors.Id, ben)
				return err
			},
		} {
			if err := write(); !dataTableBusiness.IsReadOnly(err) {
				t.Errorf("%s by a member who views: %v, want read-only", name, err)
			}
		}
		// The public API says it in the same words as the app.
		code, out = w.call("ben", http.MethodPost, "/v1/tables/{id}/rows", "/v1"+base+"/rows", v1Controller.CreateRow, `{"values":{"`+city+`":"Bath"}}`)
		readOnly(t, "create through /v1", code, out)
		if got := w.stored(vendors, row.Id)[city]; got != "York" {
			t.Errorf("the row after refused writes: %v", got)
		}
		b := bundle(t, "ben")
		if b["can_edit"] != false || b["editing"] != nil {
			t.Errorf("a viewer's bundle: can_edit %v, editing %v", b["can_edit"], b["editing"])
		}
		rule(t, "a member who views", false)
		levels(t, "a member who views", accessBusiness.LevelComment)
		if code, _ := w.call("ben", http.MethodGet, "/tables/{id}/editing", base+"/editing", dataTableController.GetTableEditing, ""); code != http.StatusForbidden {
			t.Errorf("a member reading who edits: %d, want 403", code)
		}
		if code, _ := editing("ben", `{"general":"edit"}`); code != http.StatusForbidden {
			t.Errorf("a member changing who edits: %d, want 403", code)
		}
		if b := bundle(t, "ana"); b["can_edit"] != true || b["editing"] == nil {
			t.Errorf("the owner's bundle: can_edit %v, editing %v", b["can_edit"], b["editing"])
		}
	})

	t.Run("a chosen member edits; removing them is a soft delete, and they view again", func(t *testing.T) {
		if code, out := editing("ana", `{"add":["`+ben.UserID.String()+`"]}`); code != http.StatusOK || out["changed"] != true {
			t.Fatalf("choosing ben: %d %v", code, out)
		}
		if code, out := patch("ben", "Hull"); code != http.StatusOK {
			t.Fatalf("a chosen member's edit: %d %v", code, out)
		}
		if b := bundle(t, "ben"); b["can_edit"] != true {
			t.Errorf("a chosen member's bundle: can_edit %v", b["can_edit"])
		}
		rule(t, "a chosen member", true)
		levels(t, "a chosen member", accessBusiness.LevelEdit)
		if code, out := editing("ana", `{"remove":["`+ben.UserID.String()+`"]}`); code != http.StatusOK || out["changed"] != true {
			t.Fatalf("removing ben: %d %v", code, out)
		}
		var live, gone int
		var by uuid.UUID
		if err := w.env.PG.QueryRow(`SELECT count(*) FILTER (WHERE deleted_at IS NULL), count(*) FILTER (WHERE deleted_at IS NOT NULL),
			COALESCE(max(deleted_by::text), '00000000-0000-0000-0000-000000000000')::uuid
			FROM data_table_grants WHERE table_id = $1 AND user_id = $2`, vendors.Id, ben.UserID).Scan(&live, &gone, &by); err != nil {
			t.Fatalf("read grants: %v", err)
		}
		if live != 0 || gone != 1 || by != ana.UserID {
			t.Errorf("ben's grants after removal: %d live, %d removed by %s", live, gone, by)
		}
		code, out := patch("ben", "Ripon")
		readOnly(t, "patch after removal", code, out)
		rule(t, "a member whose grant was removed", false)
		levels(t, "a member whose grant was removed", accessBusiness.LevelComment)
		if n := audits(t); n != 3 {
			t.Errorf("audit rows after three changes: %d", n)
		}
		// The same level again changes nothing, and nothing is recorded.
		if code, out := editing("ana", `{"general":"comment"}`); code != http.StatusOK || out["changed"] != false {
			t.Errorf("a change that changes nothing: %d %v", code, out)
		}
		if n := audits(t); n != 3 {
			t.Errorf("audit rows after a no-op: %d", n)
		}
	})

	// The store writes a change down before it commits (SetEditing's record
	// is the audit log), and a record that fails takes the change back.
	t.Run("a change that can't be recorded isn't made", func(t *testing.T) {
		before, err := dataTableModel.EditAccessOf(w.ctx, vendors.Id)
		if err != nil {
			t.Fatalf("who edits before: %v", err)
		}
		down := errors.New("the audit log is down")
		edit := dataTableModel.GeneralEdit
		_, _, err = dataTableModel.SetEditAccess(w.ctx, vendors.Id, ana.UserID,
			dataTableModel.EditChange{General: &edit, Add: []uuid.UUID{ben.UserID}},
			func(_, _ dataTableModel.EditAccess) error { return down })
		if !errors.Is(err, down) {
			t.Errorf("a change whose record fails: %v, want %v", err, down)
		}
		after, err := dataTableModel.EditAccessOf(w.ctx, vendors.Id)
		if err != nil || before.Changed(after) || after.General != dataTableModel.GeneralComment {
			t.Errorf("who edits after a change that wasn't recorded: %+v %v, want %+v", after, err, before)
		}
		rule(t, "a member whose grant wasn't recorded", false)
	})

	t.Run("refused changes say why, and store nothing", func(t *testing.T) {
		// person is an account that isn't a member: a bot, someone an import
		// brought in (external), or a removed account.
		person := func(name string, bot, external, removed bool) uuid.UUID {
			t.Helper()
			id := uuid.New()
			if _, err := w.env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, is_bot, is_external, deleted_at, created_at, updated_at)
				VALUES ($1, $2, $3, $4, $5, $6, CASE WHEN $7::boolean THEN NOW() END, NOW(), NOW())`,
				id, id.String()+"@example.test", name, name, bot, external, removed); err != nil {
				t.Fatalf("seed %s: %v", name, err)
			}
			return id
		}
		bot := person("bot", true, false, false)
		outsider := person("outsider", false, true, false)
		gone := person("gone", false, false, true)
		for name, body := range map[string]string{
			"the plain viewer, before sharing": `{"general":"view"}`,
			"a bot":                            `{"add":["` + bot.String() + `"]}`,
			"an external person":               `{"add":["` + outsider.String() + `"]}`,
			"a removed account":                `{"add":["` + gone.String() + `"]}`,
			"no one's id":                      `{"add":["` + uuid.NewString() + `"]}`,
		} {
			if code, out := editing("ana", body); code != http.StatusBadRequest {
				t.Errorf("%s: %d %v, want 400", name, code, out)
			}
		}
		var stored int
		if err := w.env.PG.QueryRow(`SELECT count(*) FROM data_table_grants WHERE table_id = $1 AND user_id IN ($2, $3, $4)`,
			vendors.Id, bot, outsider, gone).Scan(&stored); err != nil {
			t.Fatalf("read grants: %v", err)
		}
		if a, err := dataTableModel.EditAccessOf(w.ctx, vendors.Id); stored != 0 || err != nil || a.General != dataTableModel.GeneralComment || len(a.Editors) != 0 {
			t.Errorf("who edits after refused changes: %+v %v, %d grants to the refused", a, err, stored)
		}
		mine, err := dataTableBusiness.CreateTable(w.ctx, dataTableBusiness.TableInput{Name: "Mine", Visibility: dataTableModel.VisibilityPrivate}, ana)
		if err != nil {
			t.Fatalf("private table: %v", err)
		}
		code, out := w.call("ana", http.MethodPost, "/tables/{id}/editing", "/tables/"+mine.Id.String()+"/editing", dataTableController.SetTableEditing, `{"general":"comment"}`)
		if code != http.StatusBadRequest {
			t.Errorf("choosing who edits a private table: %d %v, want 400", code, out)
		}
		if n := audits(t); n != 3 {
			t.Errorf("audit rows after refused changes: %d", n)
		}
	})
}
