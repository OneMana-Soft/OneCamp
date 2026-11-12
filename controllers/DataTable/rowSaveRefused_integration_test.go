//go:build integration

package controllers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	business "github.com/akashc777/OneCamp/business/DataTable"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/models/pgexec"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// A cell's save that Postgres refuses for space (53100) is answered as the
// server's, to try again: a 503 like the controller's other retry-later
// answers (helpers.WriteRetryLater), which the web keeps the edit for and
// offers Retry on, never a 400 "failed to update row" that read
// as the person's mistake and dropped their edit (V-583, main's ruling 1).
// Nothing is written, and the write runs once: the server never runs a write
// again.
func TestACellSaveRefusedForSpaceIsTheServersToTryAgain(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("connect: %v", err)
	}
	owner := uuid.New()
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `
		INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, 'owner@example.test', 'owner', 'Owner', NOW(), NOW())`, owner); err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	actor := business.Actor{UserID: owner, IsAdmin: true}
	table, err := business.CreateTable(ctx, business.TableInput{Name: "Accounts", Visibility: "workspace"}, actor)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	f, err := business.CreateField(ctx, table.Id, business.FieldInput{Name: "Stage", Type: model.FieldText}, actor)
	if err != nil {
		t.Fatalf("create field: %v", err)
	}
	stage := f.Id.String()
	row, err := business.CreateRow(ctx, table.Id, business.RowInput{Values: map[string]interface{}{stage: "Lead"}}, actor)
	if err != nil {
		t.Fatalf("create row: %v", err)
	}

	// Every write of the value "Won" to a row is refused for space, and
	// counts its runs in a sequence, which a rollback doesn't take back.
	for _, s := range []string{
		`CREATE SEQUENCE space_runs`,
		`CREATE FUNCTION space_refused() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.values::text LIKE '%Won%' THEN
				PERFORM nextval('space_runs');
				RAISE EXCEPTION 'could not resize shared memory segment "/PostgreSQL.1" to 8388608 bytes: No space left on device' USING ERRCODE = '53100';
			END IF;
			RETURN NEW;
		END $$`,
		`CREATE TRIGGER space_refused BEFORE INSERT OR UPDATE ON data_table_rows FOR EACH ROW EXECUTE FUNCTION space_refused()`,
	} {
		if _, err := pgexec.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}

	body, _ := json.Marshal(map[string]any{"values": map[string]any{stage: "Won"}})
	req := httptest.NewRequest(http.MethodPost, "/tables/"+table.Id.String()+"/rows/"+row.Id.String()+"/patch", strings.NewReader(string(body)))
	rc := chi.NewRouteContext()
	rc.URLParams.Add("id", table.Id.String())
	rc.URLParams.Add("rowId", row.Id.String())
	reqCtx := context.WithValue(req.Context(), chi.RouteCtxKey, rc)
	reqCtx = context.WithValue(reqCtx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: userModels.User{Id: owner, IsAdmin: true}})
	w := httptest.NewRecorder()
	PatchRow(w, req.WithContext(reqCtx))

	var answer struct {
		Msg string `json:"msg"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &answer)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(answer.Msg, "Try again") {
		t.Errorf("a cell's save refused for space: %d %+v; want 503, saying to try again", w.Code, answer)
	}
	if n, err := pgexec.Scalar[int64](ctx, `SELECT CASE WHEN is_called THEN last_value ELSE 0 END FROM space_runs`); err != nil || n != 1 {
		t.Errorf("the refused write ran %d times (%v); want once, never again by the server", n, err)
	}
	stored, err := model.GetRowByID(ctx, table.Id, row.Id)
	if err != nil || stored == nil || !strings.Contains(string(stored.Values), "Lead") || strings.Contains(string(stored.Values), "Won") {
		t.Errorf("after the refused save the row holds %v (%v); want Lead, nothing written", stored, err)
	}
}
