//go:build integration

package business_test

import (
	"context"
	"database/sql"
	"sync/atomic"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	dataTable "github.com/akashc777/OneCamp/business/DataTable"
	query "github.com/akashc777/OneCamp/business/QuerySpec"
	rowLedger "github.com/akashc777/OneCamp/business/RowLedger"
	rowQuery "github.com/akashc777/OneCamp/business/RowQuery"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// A write racing a kept field's first fill leaves no stale value
// (airtable-03, CR-87's M3). Projects.Paid adds up Vendors' Amount and has
// never been filled, so it reaches no table yet. A write to V1 is marked in
// its own transaction, and is held there, not committed, while the sweep
// fills Paid for the first time. Without a lock between them, the fill
// records Paid's reach after the write looked for it (so the write marked
// nothing), reads V1 as it was, and keeps P1's Paid at 100 with nothing
// waiting: stale until something else changes Paid. With the lock, a kept
// value is either current or waiting to be worked out again.
func TestAWriteRacingAFirstFillLeavesNoStaleValue(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("connect: %v", err)
	}
	integration.SetupDgraph(t)
	db := postgresInit.DBConn.SqlDB
	me := uuid.New()
	if _, err := db.ExecContext(ctx, `INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, 'kept-race@example.test', 'kept-race', 'kept-race', NOW(), NOW())`, me); err != nil {
		t.Fatal(err)
	}
	owner := dataTable.Actor{UserID: me}
	table := func(name string) *model.DataTable { return madeTable(ctx, t, name, owner) }
	add := func(tb *model.DataTable, name, typ string, cfg map[string]interface{}) *model.Field {
		f, err := dataTable.CreateField(ctx, tb.Id, dataTable.FieldInput{Name: name, Type: typ, Config: cfg}, owner)
		if err != nil {
			t.Fatalf("field %s: %v", name, err)
		}
		return f
	}
	vendors := table("Race vendors")
	amount := add(vendors, "Amount", model.FieldNumber, nil)
	projects := table("Race projects")
	pVendors := add(projects, "Vendors", model.FieldRelation, map[string]interface{}{"relation_target": "table", "table_id": vendors.Id.String(), "two_way": false})
	pPaid := add(projects, "Paid", model.FieldRollup, map[string]interface{}{"relation": pVendors.Id.String(), "field": amount.Id.String(), "aggregate": "sum"})
	v1, err := dataTable.CreateRow(ctx, vendors.Id, dataTable.RowInput{Values: map[string]interface{}{amount.Id.String(): 100}}, owner)
	if err != nil {
		t.Fatal(err)
	}
	p1, err := dataTable.CreateRow(ctx, projects.Id, dataTable.RowInput{}, owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := dataTable.ChangeLinks(ctx, projects.Id, p1.Id, pVendors.Id, []uuid.UUID{v1.Id}, nil, owner); err != nil {
		t.Fatal(err)
	}

	current := func() bool {
		var gen, filled int64
		var kind string
		if err := db.QueryRowContext(ctx, `SELECT gen, filled_gen, kind FROM data_table_computed WHERE field_id = $1`, pPaid.Id).Scan(&gen, &filled, &kind); err != nil {
			t.Fatal(err)
		}
		return filled == gen && kind != ""
	}
	kept := func() float64 {
		var n float64
		_ = db.QueryRowContext(ctx, `SELECT n FROM data_table_cell_index WHERE field_id = $1 AND row_id = $2 AND n IS NOT NULL`, pPaid.Id, p1.Id).Scan(&n)
		return n
	}
	waiting := func() (n int) {
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM data_table_computed_dirty WHERE field_id = $1 AND row_id = $2`, pPaid.Id, p1.Id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if current() {
		t.Fatal("Paid was filled before the sweep ran, so this is no first fill")
	}

	// hold keeps the next write to Vendors in its transaction, after the
	// observers before this one (the cell index and the store's marks) ran.
	var hold atomic.Bool
	held, release := make(chan struct{}), make(chan struct{})
	rowLedger.Observers.Register(rowLedger.Observer{Name: "test.kept-race." + uuid.NewString(), Committing: func(_ context.Context, _ *sql.Tx, cs *rowLedger.ChangeSet) error {
		if cs.Table == vendors.Id && hold.CompareAndSwap(true, false) {
			close(held)
			<-release
		}
		return nil
	}})
	hold.Store(true)
	wrote := make(chan error, 1)
	go func() {
		_, err := dataTable.PatchRow(ctx, vendors.Id, v1.Id, map[string]interface{}{amount.Id.String(): 500}, owner)
		wrote <- err
	}()
	select {
	case <-held:
	case err := <-wrote:
		t.Fatalf("the write to V1 ended before it was held in its transaction: %v", err)
	case <-time.After(time.Minute):
		t.Fatal("the write to V1 never reached its transaction's observers")
	}
	fillCtx, cancel := context.WithTimeout(ctx, time.Minute)
	raced := rowQuery.RecomputeKept(fillCtx)
	cancel()
	close(release)
	if err := <-wrote; err != nil {
		t.Fatalf("the write to V1: %v", err)
	}
	if raced != nil {
		t.Fatalf("the first fill, racing the write: %v", raced)
	}
	t.Logf("after the racing fill: Paid current %v, P1 kept %v, %d waiting", current(), kept(), waiting())
	if current() && kept() != 500 && waiting() == 0 {
		t.Errorf("the first fill raced the write to V1 and keeps P1's Paid at %v with nothing waiting, where it adds up to 500", kept())
	}

	if err := rowQuery.RecomputeKept(ctx); err != nil {
		t.Fatal(err)
	}
	if !current() {
		t.Errorf("Paid isn't kept current after the write committed and the sweep ran")
	}
	if n := kept(); n != 500 && waiting() == 0 {
		t.Errorf("P1's kept Paid is %v with nothing waiting after the sweep, where it adds up to 500", n)
	}
}

// A mark committed after the dirty pass read the marks waiting survives
// that pass, though it's numbered below a mark the pass read, and the next
// pass works it out (airtable-03, main's call, CR-88's M5). P1's Paid adds
// up V1's Amount. A write to V1 takes its mark and is held in its
// transaction; a write to P1 then takes a higher mark and commits. The
// sweep's dirty pass reads only P1's mark and works P1 out with V1 as it
// was, and the write to V1 commits before the pass answers what it read.
// Answering every mark up to the highest read deletes V1's mark as well,
// and P1's Paid stays at 100 with nothing waiting: stale until something
// else changes it.
func TestALateLowerMarkSurvivesTheDirtyPass(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("connect: %v", err)
	}
	integration.SetupDgraph(t)
	db := postgresInit.DBConn.SqlDB
	me := uuid.New()
	if _, err := db.ExecContext(ctx, `INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, 'kept-late-mark@example.test', 'kept-late-mark', 'kept-late-mark', NOW(), NOW())`, me); err != nil {
		t.Fatal(err)
	}
	owner := dataTable.Actor{UserID: me}
	table := func(name string) *model.DataTable { return madeTable(ctx, t, name, owner) }
	add := func(tb *model.DataTable, name, typ string, cfg map[string]interface{}) *model.Field {
		f, err := dataTable.CreateField(ctx, tb.Id, dataTable.FieldInput{Name: name, Type: typ, Config: cfg}, owner)
		if err != nil {
			t.Fatalf("field %s: %v", name, err)
		}
		return f
	}
	vendors := table("Late mark vendors")
	amount := add(vendors, "Amount", model.FieldNumber, nil)
	projects := table("Late mark projects")
	pVendors := add(projects, "Vendors", model.FieldRelation, map[string]interface{}{"relation_target": "table", "table_id": vendors.Id.String(), "two_way": false})
	pPaid := add(projects, "Paid", model.FieldRollup, map[string]interface{}{"relation": pVendors.Id.String(), "field": amount.Id.String(), "aggregate": "sum"})
	pNote := add(projects, "Note", model.FieldText, nil)
	v1, err := dataTable.CreateRow(ctx, vendors.Id, dataTable.RowInput{Values: map[string]interface{}{amount.Id.String(): 100}}, owner)
	if err != nil {
		t.Fatal(err)
	}
	p1, err := dataTable.CreateRow(ctx, projects.Id, dataTable.RowInput{}, owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := dataTable.ChangeLinks(ctx, projects.Id, p1.Id, pVendors.Id, []uuid.UUID{v1.Id}, nil, owner); err != nil {
		t.Fatal(err)
	}
	kept := func() float64 {
		var n float64
		_ = db.QueryRowContext(ctx, `SELECT n FROM data_table_cell_index WHERE field_id = $1 AND row_id = $2 AND n IS NOT NULL`, pPaid.Id, p1.Id).Scan(&n)
		return n
	}
	waiting := func() (n int) {
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM data_table_computed_dirty WHERE field_id = $1 AND row_id = $2`, pPaid.Id, p1.Id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	highest := func() (m int64) {
		if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(mark), 0) FROM data_table_computed_dirty WHERE field_id = $1 AND row_id = $2`, pPaid.Id, p1.Id).Scan(&m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	if err := rowQuery.RecomputeKept(ctx); err != nil {
		t.Fatal(err)
	}
	if n, w := kept(), waiting(); n != 100 || w != 0 {
		t.Fatalf("after the first sweep P1's Paid is kept at %v with %d waiting, where it adds up to 100 with none", n, w)
	}

	// hold keeps the next write to Vendors in its transaction once the
	// observers before this one (the cell index and the store's marks) ran,
	// and notes the mark it made on P1's Paid.
	var hold atomic.Bool
	var lower int64
	held, release := make(chan struct{}), make(chan struct{})
	released := false
	let := func() {
		if !released {
			released = true
			close(release)
		}
	}
	defer let()
	rowLedger.Observers.Register(rowLedger.Observer{Name: "test.kept-late-mark." + uuid.NewString(), Committing: func(octx context.Context, tx *sql.Tx, cs *rowLedger.ChangeSet) error {
		if cs.Table == vendors.Id && hold.CompareAndSwap(true, false) {
			_ = tx.QueryRowContext(octx, `SELECT COALESCE(MAX(mark), 0) FROM data_table_computed_dirty WHERE field_id = $1 AND row_id = $2`, pPaid.Id, p1.Id).Scan(&lower)
			close(held)
			<-release
		}
		return nil
	}})
	hold.Store(true)
	wrote := make(chan error, 1)
	go func() {
		_, err := dataTable.PatchRow(ctx, vendors.Id, v1.Id, map[string]interface{}{amount.Id.String(): 500}, owner)
		wrote <- err
	}()
	select {
	case <-held:
	case err := <-wrote:
		t.Fatalf("the write to V1 ended before it was held in its transaction: %v", err)
	case <-time.After(time.Minute):
		t.Fatal("the write to V1 never reached its transaction's observers")
	}
	if lower == 0 {
		t.Fatal("the held write to V1 marked nothing on P1's Paid")
	}
	noted := make(chan error, 1)
	go func() {
		_, err := dataTable.PatchRow(ctx, projects.Id, p1.Id, map[string]interface{}{pNote.Id.String(): "late"}, owner)
		noted <- err
	}()
	select {
	case err := <-noted:
		if err != nil {
			t.Fatalf("the write to P1: %v", err)
		}
	case <-time.After(time.Minute):
		t.Fatal("the write to P1 waited on the held write to V1")
	}
	higher := highest()
	if higher <= lower {
		t.Fatalf("the write to P1 marked P1's Paid %d, not above the held write's %d", higher, lower)
	}

	// The pass works P1 out from V1 as it was, then waits at P1's kept
	// entries, which this holds, while the write to V1 commits.
	lock, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback()
	var locked, lockPid int
	if err := lock.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT 1 FROM data_table_cell_index WHERE field_id = $1 AND row_id = $2 FOR UPDATE) l`, pPaid.Id, p1.Id).Scan(&locked); err != nil || locked == 0 {
		t.Fatalf("P1's kept Paid entry to hold: %d, %v", locked, err)
	}
	if err := lock.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&lockPid); err != nil {
		t.Fatal(err)
	}
	passed := make(chan error, 1)
	go func() {
		passCtx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		passed <- rowQuery.RecomputeKept(passCtx)
	}()
	for since := time.Now(); ; time.Sleep(10 * time.Millisecond) {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE $1::int = ANY(pg_blocking_pids(pid))`, lockPid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			break
		}
		select {
		case err := <-passed:
			t.Fatalf("the dirty pass ended (%v) without writing P1's kept entries", err)
		default:
		}
		if time.Since(since) > time.Minute {
			t.Fatal("the dirty pass never reached P1's kept entries")
		}
	}
	let()
	if err := <-wrote; err != nil {
		t.Fatalf("the write to V1: %v", err)
	}
	if err := lock.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-passed; err != nil {
		t.Fatalf("the dirty pass: %v", err)
	}
	t.Logf("after the racing pass: P1 kept %v, %d waiting (marks %d then %d)", kept(), waiting(), lower, higher)
	if n := kept(); n != 500 && waiting() == 0 {
		t.Errorf("the dirty pass answered the write to V1's mark %d, made before its %d but committed after it read the marks: P1's Paid is kept at %v with nothing waiting, where it adds up to 500", lower, higher, n)
	}
	if err := rowQuery.RecomputeKept(ctx); err != nil {
		t.Fatal(err)
	}
	if n := kept(); n != 500 {
		t.Errorf("P1's kept Paid is %v after the next pass, where it adds up to 500", n)
	}
}

// madeTable makes a table and waits out the background index walk that
// making it starts (indexSoon): the walk fills the table's kept fields when
// it runs after they're made, and a race test needs a field unfilled when it
// says so, and no batch at its rows but the ones it runs.
func madeTable(ctx context.Context, t *testing.T, name string, owner dataTable.Actor) *model.DataTable {
	t.Helper()
	tb, err := dataTable.CreateTable(ctx, dataTable.TableInput{Name: name, Visibility: "workspace"}, owner)
	if err != nil {
		t.Fatal(err)
	}
	for since := time.Now(); ; time.Sleep(20 * time.Millisecond) {
		var walked bool
		if err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `SELECT cell_index_at IS NOT NULL FROM data_tables WHERE id = $1`, tb.Id).Scan(&walked); err != nil {
			t.Fatal(err)
		}
		if walked {
			return tb
		}
		if time.Since(since) > time.Minute {
			t.Fatalf("%s's background index walk never ended", name)
		}
	}
}

// A batch that read a table's rows before another batch wrote their kept
// entries never writes over what that one wrote (airtable-03, M6). P1's
// Paid adds up V1's Amount. The sweep's dirty pass works P1 out at 200 and
// is held before it writes; meanwhile V1 becomes 500, a reader asks for
// Projects by Paid, and a second pass runs. Without one lock for the
// batches that write a table's kept entries, the reader's own work (or the
// second pass) writes 500 and answers the mark, and then the held pass
// writes its 200 over it: stale with nothing waiting. With the lock the
// reader never waits (it answers by its own read, P1 first), the second
// pass waits for the held one, and then reads the mark and writes 500.
func TestAnOlderBatchNeverWritesOverANewerKeptValue(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("connect: %v", err)
	}
	integration.SetupDgraph(t)
	db := postgresInit.DBConn.SqlDB
	me := uuid.New()
	if _, err := db.ExecContext(ctx, `INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, 'kept-older-batch@example.test', 'kept-older-batch', 'kept-older-batch', NOW(), NOW())`, me); err != nil {
		t.Fatal(err)
	}
	owner := dataTable.Actor{UserID: me}
	add := func(tb *model.DataTable, name, typ string, cfg map[string]interface{}) *model.Field {
		f, err := dataTable.CreateField(ctx, tb.Id, dataTable.FieldInput{Name: name, Type: typ, Config: cfg}, owner)
		if err != nil {
			t.Fatalf("field %s: %v", name, err)
		}
		return f
	}
	row := func(tb *model.DataTable, values map[string]interface{}) uuid.UUID {
		r, err := dataTable.CreateRow(ctx, tb.Id, dataTable.RowInput{Values: values}, owner)
		if err != nil {
			t.Fatal(err)
		}
		return r.Id
	}
	vendors := madeTable(ctx, t, "Older batch vendors", owner)
	amount := add(vendors, "Amount", model.FieldNumber, nil)
	projects := madeTable(ctx, t, "Older batch projects", owner)
	pVendors := add(projects, "Vendors", model.FieldRelation, map[string]interface{}{"relation_target": "table", "table_id": vendors.Id.String(), "two_way": false})
	pPaid := add(projects, "Paid", model.FieldRollup, map[string]interface{}{"relation": pVendors.Id.String(), "field": amount.Id.String(), "aggregate": "sum"})
	v1, v2 := row(vendors, map[string]interface{}{amount.Id.String(): 100}), row(vendors, map[string]interface{}{amount.Id.String(): 300})
	p1, p2 := row(projects, nil), row(projects, nil)
	for p, v := range map[uuid.UUID]uuid.UUID{p1: v1, p2: v2} {
		if _, _, err := dataTable.ChangeLinks(ctx, projects.Id, p, pVendors.Id, []uuid.UUID{v}, nil, owner); err != nil {
			t.Fatal(err)
		}
	}
	kept := func(p uuid.UUID) float64 {
		var n float64
		_ = db.QueryRowContext(ctx, `SELECT n FROM data_table_cell_index WHERE field_id = $1 AND row_id = $2 AND n IS NOT NULL`, pPaid.Id, p).Scan(&n)
		return n
	}
	waiting := func() (n int) {
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM data_table_computed_dirty WHERE field_id = $1`, pPaid.Id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	patch := func(v uuid.UUID, n float64) {
		if _, err := dataTable.PatchRow(ctx, vendors.Id, v, map[string]interface{}{amount.Id.String(): n}, owner); err != nil {
			t.Fatal(err)
		}
	}
	if err := rowQuery.RecomputeKept(ctx); err != nil {
		t.Fatal(err)
	}
	if a, b, w := kept(p1), kept(p2), waiting(); a != 100 || b != 300 || w != 0 {
		t.Fatalf("after the first sweep P1 and P2 keep Paid at %v and %v with %d waiting, where they add up to 100 and 300 with none", a, b, w)
	}
	patch(v1, 200)

	// The older pass is held once it has worked Projects' rows out, before
	// it writes their entries.
	var hold atomic.Bool
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
		if table == projects.Id && hold.CompareAndSwap(true, false) {
			close(held)
			<-release
		}
	})()
	hold.Store(true)
	older := make(chan error, 1)
	go func() { older <- rowQuery.RecomputeKept(ctx) }()
	select {
	case <-held:
	case err := <-older:
		t.Fatalf("the older pass ended (%v) before it worked P1 out", err)
	case <-time.After(time.Minute):
		t.Fatal("the older pass never worked P1 out")
	}
	patch(v1, 500)

	read := make(chan error, 1)
	var order []uuid.UUID
	go func() {
		res, err := query.Queries.Run(ctx, accessBusiness.Subject{UserUUID: me.String()}, query.Spec{
			Scope: query.Scope{Kind: query.ScopeTable, ID: projects.Id.String()},
			Sorts: []query.Sort{{Field: "field:" + pPaid.Id.String(), Desc: true}},
		}, query.Page{Size: 10})
		if err == nil {
			for _, raw := range res.IDs {
				order = append(order, uuid.MustParse(raw))
			}
		}
		read <- err
	}()
	select {
	case err := <-read:
		if err != nil {
			t.Fatalf("Projects by Paid, while a pass was held: %v", err)
		}
		if len(order) != 2 || order[0] != p1 || order[1] != p2 {
			t.Errorf("Projects by Paid, while a pass was held, read %v, where P1 (500) comes before P2 (300)", order)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a reader waited on the held pass")
	}

	newer := make(chan error, 1)
	go func() { newer <- rowQuery.RecomputeKept(ctx) }()
	newerDone := false
	for since := time.Now(); !newerDone; time.Sleep(10 * time.Millisecond) {
		select {
		case err := <-newer:
			if err != nil {
				t.Fatalf("the newer pass: %v", err)
			}
			newerDone = true
			continue
		default:
		}
		var waiters int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted`).Scan(&waiters); err != nil {
			t.Fatal(err)
		}
		if waiters > 0 {
			break
		}
		if time.Since(since) > time.Minute {
			t.Fatal("the newer pass neither ended nor waited for the held one")
		}
	}
	let()
	for name, ch := range map[string]chan error{"older": older, "newer": newer} {
		if name == "newer" && newerDone {
			continue
		}
		select {
		case err := <-ch:
			if err != nil {
				t.Fatalf("the %s pass: %v", name, err)
			}
		case <-time.After(time.Minute):
			t.Fatalf("the %s pass never ended", name)
		}
	}
	t.Logf("after both passes: P1 kept %v, %d waiting", kept(p1), waiting())
	if n := kept(p1); n != 500 && waiting() == 0 {
		t.Errorf("the older pass wrote P1's Paid as %v over the newer 500, with nothing waiting", n)
	}
	if err := rowQuery.RecomputeKept(ctx); err != nil {
		t.Fatal(err)
	}
	if n := kept(p1); n != 500 {
		t.Errorf("P1's kept Paid is %v after the next pass, where it adds up to 500", n)
	}
}

// TABLES-76 (30 Oct, the live walk at 100k): a reader that finds the table's
// kept lock held by a sweep or fill batch answered by its own read at once,
// and past 10,000 rows its own read is a refusal: 23 of 80 sorts by a formula
// were refused while 20 rows changed. A reader now waits for the lock up to
// about a second, then answers the marks waiting itself; a batch that holds
// the lock longer still never holds the reader past that wait.
func TestAReaderWaitsBrieflyForAHeldKeptLockAndAnswersTheMarks(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("connect: %v", err)
	}
	integration.SetupDgraph(t)
	db := postgresInit.DBConn.SqlDB
	me := uuid.New()
	if _, err := db.ExecContext(ctx, `INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, 'kept-lock-wait@example.test', 'kept-lock-wait', 'kept-lock-wait', NOW(), NOW())`, me); err != nil {
		t.Fatal(err)
	}
	owner := dataTable.Actor{UserID: me}
	lines := madeTable(ctx, t, "Kept lock wait", owner)
	amount, err := dataTable.CreateField(ctx, lines.Id, dataTable.FieldInput{Name: "Amount", Type: model.FieldNumber}, owner)
	if err != nil {
		t.Fatal(err)
	}
	double, err := dataTable.CreateField(ctx, lines.Id, dataTable.FieldInput{Name: "Double", Type: model.FieldFormula, Config: map[string]interface{}{"formula": "{Amount} * 2"}}, owner)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]uuid.UUID, 3)
	for i, n := range []float64{100, 300, 200} {
		r, err := dataTable.CreateRow(ctx, lines.Id, dataTable.RowInput{Values: map[string]interface{}{amount.Id.String(): n}}, owner)
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = r.Id
	}
	if err := rowQuery.RecomputeKept(ctx); err != nil {
		t.Fatal(err)
	}
	waiting := func() (n int) {
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM data_table_computed_dirty WHERE field_id = $1`, double.Id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if w := waiting(); w != 0 {
		t.Fatalf("after the first sweep %d marks wait, where none do", w)
	}
	// holdKept holds the table's kept lock, as a sweep or fill batch does,
	// for d; the key is model.LockKept's (keptLock).
	holdKept := func(d time.Duration) (held, done chan struct{}) {
		held, done = make(chan struct{}), make(chan struct{})
		go func() {
			defer close(done)
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Error(err)
				close(held)
				return
			}
			defer tx.Rollback()
			if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('onecamp.tables.kept:' || $1::text, 0))`, lines.Id); err != nil {
				t.Error(err)
			}
			close(held)
			time.Sleep(d)
			_ = tx.Commit()
		}()
		<-held
		return held, done
	}
	byDouble := func() (order []uuid.UUID, took time.Duration) {
		start := time.Now()
		res, err := query.Queries.Run(ctx, accessBusiness.Subject{UserUUID: me.String()}, query.Spec{
			Scope: query.Scope{Kind: query.ScopeTable, ID: lines.Id.String()},
			Sorts: []query.Sort{{Field: "field:" + double.Id.String(), Desc: true}},
		}, query.Page{Size: 10})
		took = time.Since(start)
		if err != nil {
			t.Fatalf("Lines by Double: %v", err)
		}
		for _, raw := range res.IDs {
			order = append(order, uuid.MustParse(raw))
		}
		return order, took
	}

	// A batch that holds the lock for 300 ms: the reader waits for it, then
	// answers the mark the write left, so nothing waits after the read.
	if _, err := dataTable.PatchRow(ctx, lines.Id, ids[0], map[string]interface{}{amount.Id.String(): 500}, owner); err != nil {
		t.Fatal(err)
	}
	if w := waiting(); w != 1 {
		t.Fatalf("after one write %d marks wait, where 1 does", w)
	}
	_, done := holdKept(300 * time.Millisecond)
	order, took := byDouble()
	<-done
	if len(order) != 3 || order[0] != ids[0] || order[1] != ids[1] || order[2] != ids[2] {
		t.Errorf("Lines by Double read %v, where the 500 line, then 300, then 200 come first", order)
	}
	if w := waiting(); w != 0 {
		t.Errorf("a reader that found the kept lock held for 300 ms left %d marks waiting (took %v), where it waits for the lock and answers them", w, took)
	}

	// A batch that holds the lock for 3 s: the reader waits no more than
	// about a second, then answers by its own read; the mark stays for the
	// next pass.
	if _, err := dataTable.PatchRow(ctx, lines.Id, ids[1], map[string]interface{}{amount.Id.String(): 50}, owner); err != nil {
		t.Fatal(err)
	}
	_, done = holdKept(3 * time.Second)
	order, took = byDouble()
	if took > 2500*time.Millisecond {
		t.Errorf("a reader waited %v on a batch holding the kept lock for 3 s, where it waits about a second at most", took)
	}
	if len(order) != 3 || order[0] != ids[0] || order[1] != ids[2] || order[2] != ids[1] {
		t.Errorf("Lines by Double, by its own read, read %v, where the 500 line, then 200, then 50 come first", order)
	}
	<-done
	if w := waiting(); w != 1 {
		t.Errorf("after a reader that gave up on the lock %d marks wait, where its write's 1 does", w)
	}
}
