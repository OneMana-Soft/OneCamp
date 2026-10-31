//go:build integration

package business_test

// V-114 (b) (main, 30 Oct): a formula made over a live 100,000-row table
// waited for the fill sweep's next tick, up to a minute, before its fill
// began, while its reader's view said "starting" (the fill itself took 6 s
// locally once V-114 (a) wrote in place). A read that finds a formula still
// waiting for its fill wakes the sweep (sweepBusiness.Jobs.Wake), which fills
// it at once. The sweeps run here as the server runs them
// (sweepBusiness.Jobs.StartAll), ticking once a minute; the formula is made
// over HTTP once their first pass is over, and asked for over HTTP, as a view
// asks, until it's answered.
//
// Run: integ23.sh <worktree> <log> '^TestAFormulaItsReaderWaitsOnIsFilledAtOnce$' with PKGS='./business/RowQuery/'

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	dataTable "github.com/akashc777/OneCamp/business/DataTable"
	query "github.com/akashc777/OneCamp/business/QuerySpec"
	rowQuery "github.com/akashc777/OneCamp/business/RowQuery"
	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	dataTableController "github.com/akashc777/OneCamp/controllers/DataTable"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// wokenRows is more rows than one read works out, so a reader of a formula not
// yet filled waits for its fill.
const wokenRows = 2 * rowQuery.MaxComputedScan

func TestAFormulaItsReaderWaitsOnIsFilledAtOnce(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("connect: %v", err)
	}
	statementsGetAMinute(t)
	integration.SetupDgraph(t)
	me := uuid.New()
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, 'woken@example.test', 'woken', 'Woken', NOW(), NOW())`, me); err != nil {
		t.Fatal(err)
	}
	owner := dataTable.Actor{UserID: me}

	// imported makes a table of rows rows, each with an Amount, indexed whole.
	imported := func(name string, rows int) *model.DataTable {
		t.Helper()
		tb, err := dataTable.CreateTable(ctx, dataTable.TableInput{Name: name, Visibility: "workspace"}, owner)
		if err != nil {
			t.Fatal(err)
		}
		amount, err := dataTable.CreateField(ctx, tb.Id, dataTable.FieldInput{Name: "Amount", Type: model.FieldNumber}, owner)
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]uuid.UUID, rows)
		for i := range ids {
			ids[i] = uuid.New()
		}
		fill(ctx, t, tb.Id, me, ids, func(i int) map[string]interface{} {
			return map[string]interface{}{amount.Id.String(): float64(i % 1000)}
		})
		if err := rowQuery.IndexTable(ctx, tb.Id); err != nil {
			t.Fatal(err)
		}
		return tb
	}
	// filled is whether field's entries are current.
	filled := func(field uuid.UUID) bool {
		states, err := model.ComputedStates(ctx, []uuid.UUID{field})
		return err == nil && states[field].Current() && states[field].Kind != ""
	}
	ledger := imported("Ledger", wokenRows)

	// A formula made before the sweeps start is filled by their first pass,
	// which shows that pass is over before the one asked for is made.
	tally := imported("Tally", 10)
	canary, err := dataTable.CreateField(ctx, tally.Id, dataTable.FieldInput{Name: "Triple", Type: model.FieldFormula, Config: map[string]interface{}{"formula": "{Amount} * 3"}}, owner)
	if err != nil {
		t.Fatal(err)
	}
	sweeps, stop := context.WithCancel(ctx)
	t.Cleanup(stop)
	sweepBusiness.Jobs.StartAll(sweeps)
	for start := time.Now(); !filled(canary.Id); time.Sleep(100 * time.Millisecond) {
		if time.Since(start) > 3*time.Minute {
			t.Fatal("the sweeps' passes never filled a formula made before they started")
		}
	}

	// Double, made as the web makes it, then asked for as a view asks: sorted
	// by it, again every half second while its fill is under way (422).
	rec := overHTTP(t, dataTableController.CreateField, "/tables/"+ledger.Id.String()+"/fields", ledger.Id, me,
		map[string]any{"name": "Double", "type": model.FieldFormula, "config": map[string]any{"formula": "{Amount} * 2"}})
	var made struct {
		Data struct {
			ID uuid.UUID `json:"id"`
		} `json:"data"`
	}
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &made) != nil || made.Data.ID == uuid.Nil {
		t.Fatalf("POST /tables/{id}/fields Double: %d %s", rec.Code, rec.Body.String())
	}
	at := time.Now()
	f := "field:" + made.Data.ID.String()
	ask := map[string]any{"spec": query.Spec{Sorts: []query.Sort{{Field: f, Desc: true}}}, "n": 50, "count": true}
	var answered time.Duration
	waiting := 0
	for {
		code, out := queryOverHTTP(t, ledger.Id, me, ask)
		if code == http.StatusOK {
			answered = time.Since(at)
			if out.Data.Total != wokenRows || len(out.Data.Rows) != 50 {
				t.Errorf("answered with %d rows of %d, where the view asked for 50 of %d", len(out.Data.Rows), out.Data.Total, wokenRows)
			}
			break
		}
		if code != http.StatusUnprocessableEntity {
			t.Fatalf("asked for the view sorted by Double: %d %q, where a fill under way answers 422 until it's done", code, out.Msg)
		}
		waiting++
		if time.Since(at) > 90*time.Second {
			t.Fatalf("Double, over %d rows, still wasn't answered 90 s after it was made (%d asks): %q", wokenRows, waiting, out.Msg)
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Logf("Double over %d rows answered %.1f s after it was made, after %d asks that waited for its fill", wokenRows, answered.Seconds(), waiting)
	// The fill takes a second or two here: a sweep that waits for its next
	// tick answers a minute after it started, whatever the fill takes.
	if answered > 20*time.Second {
		t.Errorf("Double, over %d rows, was answered %.1f s after it was made, where its reader waking the sweep fills it at once (the sweep ticks once a minute)", wokenRows, answered.Seconds())
	}
	if !filled(made.Data.ID) {
		t.Error("Double was answered, and its entries aren't current")
	}
}
