//go:build integration

package business_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	dataTable "github.com/akashc777/OneCamp/business/DataTable"
	query "github.com/akashc777/OneCamp/business/QuerySpec"
	rowQuery "github.com/akashc777/OneCamp/business/RowQuery"
	tableAccess "github.com/akashc777/OneCamp/business/TableAccess"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// airtable-03b (the live walk at 100k, 30 Oct): a person who sorted by a new
// formula over a big table while the store filled it for the first time was
// refused for two and a half minutes, as if the table could never be sorted
// so. While the fill runs, the read now says how far it has got (done of
// total rows), and once it's filled the same sort is answered.
func TestASortByAFormulaBeingFilledSaysHowFarTheFillHasGot(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("connect: %v", err)
	}
	integration.SetupDgraph(t)
	db := postgresInit.DBConn.SqlDB
	me := uuid.New()
	if _, err := db.ExecContext(ctx, `INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, 'fill-progress@example.test', 'fill-progress', 'fill-progress', NOW(), NOW())`, me); err != nil {
		t.Fatal(err)
	}
	owner := dataTable.Actor{UserID: me}
	lines := madeTable(ctx, t, "Fill progress", owner)
	amount, err := dataTable.CreateField(ctx, lines.Id, dataTable.FieldInput{Name: "Amount", Type: model.FieldNumber}, owner)
	if err != nil {
		t.Fatal(err)
	}
	const rows = rowQuery.MaxComputedScan + 1
	ids := make([]uuid.UUID, rows)
	for i := range ids {
		ids[i] = uuid.New()
	}
	fill(ctx, t, lines.Id, me, ids, func(i int) map[string]interface{} {
		return map[string]interface{}{amount.Id.String(): float64(i % 1000)}
	})
	if err := rowQuery.RecomputeKept(ctx); err != nil {
		t.Fatal(err)
	}
	double, err := dataTable.CreateField(ctx, lines.Id, dataTable.FieldInput{Name: "Double", Type: model.FieldFormula, Config: map[string]interface{}{"formula": "{Amount} * 2"}}, owner)
	if err != nil {
		t.Fatal(err)
	}
	byDouble := func() (query.Result, error) {
		return query.Queries.Run(ctx, accessBusiness.Subject{UserUUID: me.String()}, query.Spec{
			Scope: query.Scope{Kind: query.ScopeTable, ID: lines.Id.String()},
			Sorts: []query.Sort{{Field: "field:" + double.Id.String(), Desc: true}},
		}, query.Page{Size: 3})
	}

	// The fill is held in its second batch, once the first has committed.
	var batches atomic.Int32
	held, release := make(chan struct{}), make(chan struct{})
	released := false
	let := func() {
		if !released {
			released = true
			close(release)
		}
	}
	defer let()
	defer rowQuery.AfterKeptWork(func(_ context.Context, table uuid.UUID) {
		if table == lines.Id && batches.Add(1) == 2 {
			close(held)
			<-release
		}
	})()
	filling := make(chan error, 1)
	go func() { filling <- rowQuery.RecomputeKept(ctx) }()
	select {
	case <-held:
	case err := <-filling:
		t.Fatalf("the fill ended (%v) before its second batch", err)
	case <-time.After(2 * time.Minute):
		t.Fatal("the fill never reached its second batch")
	}

	// CR-95 M1: a view asks again every few seconds while a fill runs, so
	// the ask says how far before any work: it reads no row of the table,
	// where it worked out the first 10,000 to find there were more.
	worked := rowQuery.WorkedOut
	// (Counted by the work asked, as a read past its limit hands back no row.)
	var works, upTo atomic.Int32
	rowQuery.WorkedOut = func(ctx context.Context, who tableAccess.Who, table uuid.UUID, fields []*model.Field, max int) ([]*model.Row, bool, map[string]string, error) {
		works.Add(1)
		upTo.Add(int32(max))
		return worked(ctx, who, table, fields, max)
	}
	_, err = byDouble()
	rowQuery.WorkedOut = worked
	if n := works.Load(); n != 0 {
		t.Errorf("Lines by Double, while its fill runs, worked out rows %d times (up to %d rows) to say how far the fill has got, where it reads none", n, upTo.Load())
	}
	var working *rowQuery.StillWorkingOut
	if !errors.As(err, &working) {
		t.Fatalf("Lines by Double, while its fill runs, answered %v, where it says how far the fill has got", err)
	}
	if working.Field != "Double" || working.Total != rows || working.Done <= 0 || working.Done >= rows {
		t.Errorf("Lines by Double, while its fill runs, says %+v, where Double has a first batch of its %d rows done", *working, rows)
	}
	if !errors.Is(err, rowQuery.ErrTooManyToWorkOut) {
		t.Errorf("a fill still running is %v, where it is ErrTooManyToWorkOut to what only knows that", err)
	}

	let()
	select {
	case err := <-filling:
		if err != nil {
			t.Fatalf("the fill: %v", err)
		}
	case <-time.After(2 * time.Minute):
		t.Fatal("the fill never ended")
	}
	res, err := byDouble()
	if err != nil {
		t.Fatalf("Lines by Double, once filled: %v", err)
	}
	if len(res.IDs) == 0 {
		t.Fatal("Lines by Double, once filled, read no rows")
	}
}
