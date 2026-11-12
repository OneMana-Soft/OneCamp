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

// A range write (a paste through Batch) or a move of rows that Postgres
// refuses for space (53100) is answered as the server's, to try again: the
// 503 the table's single writes answer (helpers.WriteRetryLater, V-583),
// never a 400 "failed to write the rows" that read as the person's mistake
// (V-617). Nothing is written, and each write runs once: the server never runs
// a write again, the 503 only tells the person they may.
func TestARangeWriteRefusedForSpaceIsTheServersToTryAgain(t *testing.T) {
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
	var rows []uuid.UUID
	for range 2 {
		row, err := business.CreateRow(ctx, table.Id, business.RowInput{Values: map[string]interface{}{stage: "Lead"}}, actor)
		if err != nil {
			t.Fatalf("create row: %v", err)
		}
		rows = append(rows, row.Id)
	}
	versions := func() []int64 {
		var out []int64
		for _, id := range rows {
			v, err := pgexec.Scalar[int64](ctx, `SELECT version FROM data_table_rows WHERE id = $1`, id)
			if err != nil {
				t.Fatalf("read row %s: %v", id, err)
			}
			out = append(out, v)
		}
		return out
	}
	before := versions()

	// From here every write to a row is refused for space, and counts its
	// runs in a sequence, which a rollback doesn't take back.
	for _, s := range []string{
		`CREATE SEQUENCE space_runs`,
		`CREATE FUNCTION space_refused() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			PERFORM nextval('space_runs');
			RAISE EXCEPTION 'could not resize shared memory segment "/PostgreSQL.1" to 8388608 bytes: No space left on device' USING ERRCODE = '53100';
		END $$`,
		`CREATE TRIGGER space_refused BEFORE INSERT OR UPDATE ON data_table_rows FOR EACH ROW EXECUTE FUNCTION space_refused()`,
	} {
		if _, err := pgexec.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}

	serve := func(handler http.HandlerFunc, path string, in any) *httptest.ResponseRecorder {
		body, _ := json.Marshal(in)
		req := httptest.NewRequest(http.MethodPost, "/tables/"+table.Id.String()+path, strings.NewReader(string(body)))
		rc := chi.NewRouteContext()
		rc.URLParams.Add("id", table.Id.String())
		reqCtx := context.WithValue(req.Context(), chi.RouteCtxKey, rc)
		reqCtx = context.WithValue(reqCtx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: userModels.User{Id: owner, IsAdmin: true}})
		w := httptest.NewRecorder()
		handler(w, req.WithContext(reqCtx))
		return w
	}
	const retryLater = "The table couldn't be read or changed just now. Try again in a moment."
	for i, c := range []struct {
		what    string
		handler http.HandlerFunc
		path    string
		in      any
	}{
		{"a paste over two rows", Batch, "/rows/batch", map[string]any{
			"kind":  "paste",
			"patch": []map[string]any{{"row": rows[0], "values": map[string]any{stage: "Won"}}, {"row": rows[1], "values": map[string]any{stage: "Won"}}},
		}},
		{"a move of a row", MoveRows, "/rows/move", map[string]any{"rows": []uuid.UUID{rows[1]}, "before": rows[0]}},
	} {
		w := serve(c.handler, c.path, c.in)
		var answer map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &answer)
		if w.Code != http.StatusServiceUnavailable || len(answer) != 1 || answer["msg"] != retryLater {
			t.Errorf("%s refused for space: %d %s; want 503 {\"msg\": %q}", c.what, w.Code, w.Body.String(), retryLater)
		}
		if n, err := pgexec.Scalar[int64](ctx, `SELECT CASE WHEN is_called THEN last_value ELSE 0 END FROM space_runs`); err != nil || n != int64(i+1) {
			t.Errorf("after %s the refused writes ran %d times (%v); want %d, each once, never again by the server", c.what, n, err, i+1)
		}
	}

	if after := versions(); after[0] != before[0] || after[1] != before[1] {
		t.Errorf("after the refused writes the rows are at versions %v; want %v, nothing written", after, before)
	}
	for _, id := range rows {
		stored, err := model.GetRowByID(ctx, table.Id, id)
		if err != nil || stored == nil || !strings.Contains(string(stored.Values), "Lead") || strings.Contains(string(stored.Values), "Won") {
			t.Errorf("after the refused paste row %s holds %v (%v); want Lead, nothing written", id, stored, err)
		}
	}
}
