//go:build integration

package business_test

// A live view reads only the cells that changed (tables L1, Live Rows #65),
// over HTTP as the web asks (GET /tables/{id}/rows?ids=&fields=), against
// Postgres 12 with every migration: the cells named, with what every read
// works out (a formula that follows from them, the row's last change), each
// as the reader may see it; every other cell stays out, and a reader who
// can't open the table is refused. The live message names ids alone: values
// never ride a table's topic, which every reader of it gets.
// Run: integ_rel.sh <worktree> <log> '^TestALiveViewReadsOnlyTheCellsThatChanged$' with PKGS='./business/DataTable/'

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	dataTable "github.com/akashc777/OneCamp/business/DataTable"
	dataTableController "github.com/akashc777/OneCamp/controllers/DataTable"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// cellsOverHTTP asks for rows' cells as the web does, as who: the status,
// the rows' cells, and a refusal's words.
func cellsOverHTTP(t *testing.T, table, who uuid.UUID, query string) (int, []map[string]any, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/tables/"+table.String()+"/rows?"+query, nil)
	route := chi.NewRouteContext()
	route.URLParams.Add("id", table.String())
	rctx := context.WithValue(req.Context(), chi.RouteCtxKey, route)
	rctx = context.WithValue(rctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: userModels.User{Id: who}})
	rec := httptest.NewRecorder()
	dataTableController.ListRows(rec, req.WithContext(rctx))
	var out struct {
		Msg  string `json:"msg"`
		Data []struct {
			ID     string `json:"id"`
			Values string `json:"values"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	var rows []map[string]any
	for _, r := range out.Data {
		cells := map[string]any{}
		if err := json.Unmarshal([]byte(r.Values), &cells); err != nil {
			t.Fatalf("row %s values %q: %v", r.ID, r.Values, err)
		}
		cells["#id"] = r.ID
		rows = append(rows, cells)
	}
	return rec.Code, rows, out.Msg
}

func TestALiveViewReadsOnlyTheCellsThatChanged(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("connect: %v", err)
	}
	integration.SetupDgraph(t)
	maya, sam := uuid.New(), uuid.New()
	for _, u := range []struct {
		id   uuid.UUID
		name string
	}{{maya, "maya"}, {sam, "sam"}} {
		if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `
			INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2 || '@example.test', $2, $2, NOW(), NOW())`, u.id, u.name); err != nil {
			t.Fatalf("seed %s: %v", u.name, err)
		}
	}
	owner := dataTable.Actor{UserID: maya}
	table, err := dataTable.CreateTable(ctx, dataTable.TableInput{Name: "Salaries", Visibility: "private"}, owner)
	if err != nil {
		t.Fatal(err)
	}
	field := func(name, typ string, cfg map[string]interface{}) *model.Field {
		t.Helper()
		f, err := dataTable.CreateField(ctx, table.Id, dataTable.FieldInput{Name: name, Type: typ, Config: cfg}, owner)
		if err != nil {
			t.Fatalf("field %s: %v", name, err)
		}
		return f
	}
	name := field("Name", model.FieldText, nil)
	amount := field("Amount", model.FieldNumber, nil)
	note := field("Note", model.FieldText, nil)
	double := field("Double", model.FieldFormula, map[string]interface{}{"formula": "{Amount} * 2"})
	changed := field("Changed", model.FieldModifiedTime, nil)
	row, err := dataTable.CreateRow(ctx, table.Id, dataTable.RowInput{Values: map[string]interface{}{
		name.Id.String(): "Line 1", amount.Id.String(): 10.0, note.Id.String(): "first"}}, owner)
	if err != nil {
		t.Fatal(err)
	}
	heard := dataTable.RowMessagesOf(t, table.Id)
	if _, err := dataTable.PatchRow(ctx, table.Id, row.Id, map[string]interface{}{amount.Id.String(): 12.0, note.Id.String(): nil}, owner); err != nil {
		t.Fatalf("patch: %v", err)
	}

	// The cell edit's message names the row and the two cells it changed, by
	// id alone (no value rides the topic), and the reader asks for those.
	said := heard()
	if len(said) != 1 || said[0]["action"] != "updated" || said[0]["row_id"] != row.Id.String() {
		t.Fatalf("the edit's live messages: %v", said)
	}
	named, _ := said[0]["fields"].([]string)
	if len(named) != 2 || !slices.Contains(named, amount.Id.String()) || !slices.Contains(named, note.Id.String()) || len(said[0]) != 3 {
		t.Fatalf("the edit's message names %v, want Amount and Note's ids alone", said[0])
	}

	// The message named Amount and Note: those cells come, Note cleared as
	// null, with Double (worked out from the new Amount) and the row's last
	// change; Name, which didn't change, stays out.
	q := "ids=" + row.Id.String() + "&fields=" + strings.Join(named, ",")
	fieldReads := dataTable.FieldReadsOf(t, table.Id)
	code, rows, _ := cellsOverHTTP(t, table.Id, maya, q)
	if code != http.StatusOK || len(rows) != 1 {
		t.Fatalf("Maya's read: %d %v", code, rows)
	}
	// It reads the table's fields once, to work the row out and to narrow it
	// (CR-228 F1): every open viewer makes this read on every change.
	if n := fieldReads(); n != 1 {
		t.Errorf("a read of cells read the table's fields %d times, want 1", n)
	}
	got := rows[0]
	if got[amount.Id.String()] != 12.0 || got[double.Id.String()] != 24.0 {
		t.Errorf("the changed cell and its formula: %v", got)
	}
	if v, ok := got[note.Id.String()]; !ok || v != nil {
		t.Errorf("the cleared cell comes as null: %v (%v)", v, ok)
	}
	if _, ok := got[changed.Id.String()]; !ok {
		t.Errorf("the row's last change comes too: %v", got)
	}
	if _, ok := got[name.Id.String()]; ok {
		t.Errorf("a cell nobody changed stays out: %v", got)
	}

	// Without fields, the whole row, as before.
	if code, rows, _ := cellsOverHTTP(t, table.Id, maya, "ids="+row.Id.String()); code != http.StatusOK || len(rows) != 1 || rows[0][name.Id.String()] != "Line 1" {
		t.Errorf("a read by id alone is the whole row: %d %v", code, rows)
	}
	// Sam can't open Maya's private table, so he reads no cells of it.
	if code, rows, _ := cellsOverHTTP(t, table.Id, sam, q); code < 400 || len(rows) != 0 {
		t.Errorf("Sam read cells of a table he can't open: %d %v", code, rows)
	}
	// A list of fields that isn't one is refused, in the words of fields.
	if code, _, msg := cellsOverHTTP(t, table.Id, maya, "ids="+row.Id.String()+"&fields=Amount"); code != http.StatusBadRequest || msg != dataTable.ErrBadFieldIDs.Error() {
		t.Errorf("fields=Amount: %d %q", code, msg)
	}
}
