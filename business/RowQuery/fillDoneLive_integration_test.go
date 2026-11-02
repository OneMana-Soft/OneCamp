//go:build integration

package business_test

import (
	"context"
	"strings"
	"testing"

	dataTable "github.com/akashc777/OneCamp/business/DataTable"
	rowQuery "github.com/akashc777/OneCamp/business/RowQuery"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// V-124: a formula's fill, run by the server's own sweep, says once on its
// table's live topic when it ends, naming the formula alone (ids only), so an
// open view the server held for it reads again at once. A pass with nothing
// to fill says nothing. Who hears the topic is the broker's own reader rule
// (business/MqttAccess; tests/integration/live_access_test.go).
func TestAFormulasFillSaysOnceOnItsTablesTopicWhenItEnds(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN+"&application_name=fill_done_live"); err != nil {
		t.Fatalf("connect: %v", err)
	}
	statementsGetAMinute(t)
	integration.SetupDgraph(t)
	me := uuid.New()
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, 'fill-done@example.test', 'filldone', 'Fill Done', NOW(), NOW())`, me); err != nil {
		t.Fatal(err)
	}
	owner := dataTable.Actor{UserID: me}
	tb, err := dataTable.CreateTable(ctx, dataTable.TableInput{Name: "Ledger", Visibility: "workspace"}, owner)
	if err != nil {
		t.Fatal(err)
	}
	amount, err := dataTable.CreateField(ctx, tb.Id, dataTable.FieldInput{Name: "Amount", Type: model.FieldNumber}, owner)
	if err != nil {
		t.Fatal(err)
	}
	fill(ctx, t, tb.Id, me, []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}, func(i int) map[string]interface{} {
		return map[string]interface{}{amount.Id.String(): float64(i + 1)}
	})

	said, restore := rowQuery.SaidOnTableTopics()
	defer restore()
	// Only this table's: the sweep may fill other tests' tables meanwhile.
	mine := func() []string {
		var out []string
		for _, s := range said() {
			if strings.HasPrefix(s, tb.Id.String()+" ") {
				out = append(out, s)
			}
		}
		return out
	}
	if err := rowQuery.IndexTable(ctx, tb.Id); err != nil {
		t.Fatal(err)
	}
	if got := mine(); len(got) != 0 {
		t.Errorf("indexing a table with nothing kept to fill said %q on its topic, where nothing is", got)
	}
	double, err := dataTable.CreateField(ctx, tb.Id, dataTable.FieldInput{Name: "Double", Type: model.FieldFormula, Config: map[string]interface{}{"formula": "{Amount} * 2"}}, owner)
	if err != nil {
		t.Fatal(err)
	}
	sweepUntilFilled(ctx, t, double.Id, "Double")
	want := tb.Id.String() + ` {"action":"computed","fields":["` + double.Id.String() + `"]}`
	if got := mine(); len(got) != 1 || got[0] != want {
		t.Errorf("Double's fill said %q on its table's topic, where once %q is", got, want)
	}
}
