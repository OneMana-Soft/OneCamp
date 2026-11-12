//go:build integration

package business

// An API item's write and its audit row in one transaction (main's audit
// rule, 3 Nov 2026), against Postgres 12 with every migration. An item whose
// row can't be written, or whose commit fails once its row is written,
// leaves nothing behind: no field, no index entry, no notice to an open view
// or a linked table. A commit that fails after the row is followed by an
// "_failed" row, so the log says the change didn't take. An item that changed
// nothing writes no row. The same writes made outside a transaction (the
// app's, an import's) are there, and told, when they return, as before.
// Every write an item joins is refused that way: a field's, a view's, a
// primary change and a relation's pair; and a batch sent again finds what
// its first send made by the ids its key gives.
//
// Run: integ_rel.sh <tree> <log> '^(TestAnItemWhoseRowIsRefusedLeavesNothingBehind|TestAnItemThatFailsAtItsCommitIsRecordedAsFailed|TestWritesOutsideATransactionAreThereAndToldWhenTheyReturn|TestAViewItemWhoseRowIsRefusedLeavesTheViewAsItWas|TestAPrimaryChangeWhoseRowIsRefusedLeavesThePrimary|TestARelationPairWhoseRowIsRefusedIsAsItWas|TestABatchsRelationSentAgainIsTheOneItMade|TestALinksNoticeAWriteHandsOffIsWaitedFor)$' with PKGS='./business/DataTable/'

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	MQTT "github.com/eclipse/paho.mqtt.golang"
	"github.com/google/uuid"

	v1api "github.com/akashc777/OneCamp/business/PublicAPI"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// tableBroker stands in for the MQTT broker: it keeps what is published, by
// topic. What it doesn't override is an unconnected client's.
type tableBroker struct {
	MQTT.Client
	mu   sync.Mutex
	sent map[string][]string
}

func (b *tableBroker) Publish(topic string, _ byte, _ bool, payload interface{}) MQTT.Token {
	body, _ := payload.([]byte)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sent[topic] = append(b.sent[topic], string(body))
	return &MQTT.DummyToken{}
}

// itemWorld is a "Launch plan" of three rows with a relation to "Vendors",
// so a change to the plan's fields tells Vendors' readers, and a broker that
// keeps what the plan's open views are told.
type itemWorld struct {
	*relationWorld
	plan, vendors *model.DataTable
	vendor        *model.Field // the plan's relation to Vendors, one way
	broker        *tableBroker
	mu            sync.Mutex
	told          []uuid.UUID
}

func newItemWorld(t *testing.T) *itemWorld {
	w := &itemWorld{relationWorld: newRelationWorld(t)}
	w.broker = &tableBroker{Client: MQTT.NewClient(MQTT.NewClientOptions().AddBroker("tcp://127.0.0.1:1")), sent: map[string][]string{}}
	integration.UseMqttClient(t, w.broker)
	wasLinks, wasEvery := publishLinks, tellEvery
	publishLinks = func(id uuid.UUID) {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.told = append(w.told, id)
	}
	tellEvery = 0 // every notice at once: none waits on another's
	t.Cleanup(func() { publishLinks, tellEvery = wasLinks, wasEvery })
	// Cleanups run last first: the work this test's writes handed off ends,
	// telling this world, before the hooks go back. Left running, it would
	// book its notice under the restored tellEvery and tell the next test's
	// world a table it never made (b199's integ leg, 3 Nov).
	t.Cleanup(func() { helpers.SettleBackground(10 * time.Second) })
	w.plan, _ = w.table("Launch plan")
	w.vendors, _ = w.table("Vendors")
	w.vendor = w.field(w.plan, "Vendor", model.FieldRelation, map[string]interface{}{"relation_target": "table", "table_id": w.vendors.Id.String()})
	for range 3 {
		w.row(w.plan, map[string]interface{}{})
	}
	w.quiet()
	return w
}

// quiet lets the work a write handed off finish (some of it on a bare
// goroutine), then forgets what was told so far.
func (w *itemWorld) quiet() {
	helpers.SettleBackground(10 * time.Second)
	time.Sleep(300 * time.Millisecond)
	w.mu.Lock()
	w.told = nil
	w.mu.Unlock()
	w.broker.mu.Lock()
	w.broker.sent = map[string][]string{}
	w.broker.mu.Unlock()
}

// heard is what the plan's open views were told, and the tables told to read
// their links again, since the last quiet; it first lets handed-off work end.
func (w *itemWorld) heard() (views []string, linked []uuid.UUID) {
	helpers.SettleBackground(10 * time.Second)
	time.Sleep(300 * time.Millisecond)
	return w.sentToPlan(), w.linksTold()
}

func (w *itemWorld) sentToPlan() []string {
	w.broker.mu.Lock()
	defer w.broker.mu.Unlock()
	return slices.Clone(w.broker.sent[helpers.GetMqttTopicForTable(w.plan.Id.String())])
}

func (w *itemWorld) linksTold() []uuid.UUID {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.told)
}

// waitTold waits for table's readers to be told to read their links again.
func (w *itemWorld) waitTold(table uuid.UUID) {
	w.t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if slices.Contains(w.linksTold(), table) {
			return
		}
	}
	w.t.Fatalf("%s's readers were never told; told %v", table, w.linksTold())
}

func (w *itemWorld) count(q string, args ...any) int {
	w.t.Helper()
	var n int
	if err := postgresInit.DBConn.SqlDB.QueryRowContext(w.ctx, q, args...).Scan(&n); err != nil {
		w.t.Fatalf("%s: %v", q, err)
	}
	return n
}

func (w *itemWorld) exec(q string) {
	w.t.Helper()
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(w.ctx, q); err != nil {
		w.t.Fatalf("%s: %v", q, err)
	}
}

// left is what a field id left in the store: its row (live or in the trash),
// its index entries, and the plan's index stale count (an index write that
// failed marks it).
func (w *itemWorld) left(field uuid.UUID) (rows, entries, staleGen int) {
	return w.count(`SELECT count(*) FROM data_table_fields WHERE id = $1`, field),
		w.count(`SELECT count(*) FROM data_table_cell_index WHERE field_id = $1`, field),
		w.count(`SELECT cell_index_stale_gen FROM data_tables WHERE id = $1`, w.plan.Id)
}

// rowsAbout are the actions of the audit rows about item id, in order, and
// their summaries.
func (w *itemWorld) rowsAbout(id uuid.UUID) (actions, summaries []string) {
	w.t.Helper()
	rs, err := postgresInit.DBConn.SqlDB.QueryContext(w.ctx,
		`SELECT action, summary FROM admin_audit_log WHERE metadata->>'id' = $1 ORDER BY seq`, id.String())
	if err != nil {
		w.t.Fatalf("read the audit rows: %v", err)
	}
	defer rs.Close()
	for rs.Next() {
		var a, s string
		if err := rs.Scan(&a, &s); err != nil {
			w.t.Fatal(err)
		}
		actions, summaries = append(actions, a), append(summaries, s)
	}
	return actions, summaries
}

// audit writes an item's rows as a fields batch of the plan does.
func (w *itemWorld) audit() itemAudit {
	return w.auditAs("tables.fields.batch")
}

// auditAs writes an item's rows as the plan's batch by route does.
func (w *itemWorld) auditAs(route string) itemAudit {
	return func(ctx context.Context, s batchStep, done itemDone, action string) error {
		return auditItem(ctx, w.me, route, "", w.plan.Id, s, done, action)
	}
}

// refuseRows has the audit log refuse every row until lift.
func (w *itemWorld) refuseRows() (lift func()) {
	was := recordItem
	recordItem = func(context.Context, *uuid.UUID, string, string, string, string, string, map[string]interface{}) error {
		return errors.New("the audit log refused the row")
	}
	lift = func() { recordItem = was }
	w.t.Cleanup(lift)
	return lift
}

func (w *itemWorld) fieldName(id uuid.UUID) (name string, live bool) {
	w.t.Helper()
	var deleted *time.Time
	if err := postgresInit.DBConn.SqlDB.QueryRowContext(w.ctx,
		`SELECT name, deleted_at FROM data_table_fields WHERE id = $1`, id).Scan(&name, &deleted); err != nil {
		w.t.Fatalf("read field %s: %v", id, err)
	}
	return name, deleted == nil
}

func TestAnItemWhoseRowIsRefusedLeavesNothingBehind(t *testing.T) {
	w := newItemWorld(t)
	_, _, gen := w.left(uuid.Nil)
	lift := w.refuseRows()

	// A create: the field goes with its row, and nothing it would have told
	// or indexed happens.
	budget := uuid.New()
	create, err := fieldCreate(w.me, w.plan.Id, nil, map[string]any{"name": "Budget", "type": model.FieldNumber}, 0, budget)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applyItem(w.ctx, create, w.audit()); !errors.Is(err, errBatchAudit) {
		t.Fatalf("a create whose row was refused answered %v; want %v", err, errBatchAudit)
	}
	views, linked := w.heard()
	if rows, entries, g := w.left(budget); rows != 0 || entries != 0 || g != gen {
		t.Errorf("a create whose row was refused left %d field rows and %d index entries, stale count %d -> %d; want none, unchanged", rows, entries, gen, g)
	}
	if len(views)+len(linked) != 0 {
		t.Errorf("a create whose row was refused told the plan's views %q and the tables %v; want nothing", views, linked)
	}
	if actions, _ := w.rowsAbout(budget); len(actions) != 0 {
		t.Errorf("a create whose row was refused is in the log as %q", actions)
	}

	// Once rows are taken, the same create is there, indexed for the plan's
	// three rows, Vendors' readers told, with one row.
	lift()
	if _, err := applyItem(w.ctx, create, w.audit()); err != nil {
		t.Fatalf("the create once rows are taken: %v", err)
	}
	if rows, entries, _ := w.left(budget); rows != 1 || entries != 3 {
		t.Errorf("the create left %d field rows and %d index entries; want 1 and 3", rows, entries)
	}
	w.waitTold(w.vendors.Id)
	if actions, _ := w.rowsAbout(budget); !slices.Equal(actions, []string{"table.field.created"}) {
		t.Errorf("the create's rows are %q; want table.field.created", actions)
	}

	// An update and a delete whose rows are refused: the field keeps its name
	// and stays live, and no open view or linked table is told.
	w.quiet()
	lift = w.refuseRows()
	rename, err := fieldUpdate(w.me, w.plan.Id, nil, map[string]any{"field": budget.String(), "name": "Spend"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applyItem(w.ctx, rename, w.audit()); !errors.Is(err, errBatchAudit) {
		t.Fatalf("an update whose row was refused answered %v; want %v", err, errBatchAudit)
	}
	if _, err := applyItem(w.ctx, fieldDelete(w.me, w.plan.Id, nil, budget, 0), w.audit()); !errors.Is(err, errBatchAudit) {
		t.Fatalf("a delete whose row was refused answered %v; want %v", err, errBatchAudit)
	}
	views, linked = w.heard()
	if name, live := w.fieldName(budget); name != "Budget" || !live {
		t.Errorf("after an update and a delete whose rows were refused, the field is %q (live %v); want Budget, live", name, live)
	}
	if len(views)+len(linked) != 0 {
		t.Errorf("an update and a delete whose rows were refused told the plan's views %q and the tables %v; want nothing", views, linked)
	}

	// Once rows are taken, the update renames it and tells the plan's open
	// views, with a row naming what changed; sent again, it changes nothing,
	// writes no row and tells no one.
	lift()
	if _, err := applyItem(w.ctx, rename, w.audit()); err != nil {
		t.Fatalf("the update once rows are taken: %v", err)
	}
	if name, _ := w.fieldName(budget); name != "Spend" {
		t.Errorf("the update left the field named %q; want Spend", name)
	}
	if views, _ := w.heard(); !slices.ContainsFunc(views, func(m string) bool { return strings.Contains(m, `"action":"fields"`) }) {
		t.Errorf("the update told the plan's views %q; want its fields read again", views)
	}
	var changed string
	if err := postgresInit.DBConn.SqlDB.QueryRowContext(w.ctx, `SELECT metadata->>'changed' FROM admin_audit_log
		WHERE metadata->>'id' = $1 AND action = 'table.field.updated'`, budget.String()).Scan(&changed); err != nil || changed != `["name"]` {
		t.Errorf("the update's row says it changed %s (%v); want [\"name\"]", changed, err)
	}
	w.quiet()
	if _, err := applyItem(w.ctx, rename, w.audit()); err != nil {
		t.Fatalf("the same update again: %v", err)
	}
	if views, linked := w.heard(); len(views)+len(linked) != 0 {
		t.Errorf("an update that changed nothing told the plan's views %q and the tables %v", views, linked)
	}
	if actions, _ := w.rowsAbout(budget); !slices.Equal(actions, []string{"table.field.created", "table.field.updated"}) {
		t.Errorf("the field's rows are %q; want its create and one update", actions)
	}
}

func TestAnItemThatFailsAtItsCommitIsRecordedAsFailed(t *testing.T) {
	w := newItemWorld(t)
	// A check the store makes only at the commit, after the item's row is
	// written: Postgres refuses this field's name then.
	w.exec(`CREATE OR REPLACE FUNCTION test_item_refused_at_commit() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'refused at its commit' USING ERRCODE = 'check_violation'; END $$`)
	w.exec(`CREATE CONSTRAINT TRIGGER test_item_refused_at_commit AFTER INSERT ON data_table_fields
		DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.name = 'Refused at its commit')
		EXECUTE FUNCTION test_item_refused_at_commit()`)
	t.Cleanup(func() {
		w.exec(`DROP TRIGGER IF EXISTS test_item_refused_at_commit ON data_table_fields`)
		w.exec(`DROP FUNCTION IF EXISTS test_item_refused_at_commit()`)
	})
	_, _, gen := w.left(uuid.Nil)

	id := uuid.New()
	create, err := fieldCreate(w.me, w.plan.Id, nil, map[string]any{"name": "Refused at its commit", "type": model.FieldText}, 0, id)
	if err != nil {
		t.Fatal(err)
	}
	_, err = applyItem(w.ctx, create, w.audit())
	if !errors.Is(err, errItemSave) || err.Error() != "failed to save the change" {
		t.Fatalf("an item whose commit failed answered %v; want %q, the server's, in fixed words", err, errItemSave)
	}
	views, linked := w.heard()
	if rows, entries, g := w.left(id); rows != 0 || entries != 0 || g != gen {
		t.Errorf("an item whose commit failed left %d field rows and %d index entries, stale count %d -> %d; want none, unchanged", rows, entries, gen, g)
	}
	if len(views)+len(linked) != 0 {
		t.Errorf("an item whose commit failed told the plan's views %q and the tables %v; want nothing", views, linked)
	}
	actions, summaries := w.rowsAbout(id)
	if !slices.Equal(actions, []string{"table.field.created", "table.field.created_failed"}) {
		t.Fatalf("an item whose commit failed is in the log as %q; want its row, then its _failed row", actions)
	}
	if !strings.HasSuffix(summaries[1], "(it didn't take)") {
		t.Errorf("the _failed row says %q; want it to say the change didn't take", summaries[1])
	}
}

func TestWritesOutsideATransactionAreThereAndToldWhenTheyReturn(t *testing.T) {
	w := newItemWorld(t)

	// The app's create: there, indexed for the plan's three rows, when it
	// returns; Vendors' readers told.
	f, err := CreateField(w.ctx, w.plan.Id, FieldInput{Name: "Owner", Type: model.FieldText}, w.me)
	if err != nil {
		t.Fatal(err)
	}
	if rows, entries, _ := w.left(f.Id); rows != 1 || entries != 3 {
		t.Errorf("when the app's create returned there were %d field rows and %d index entries; want 1 and 3", rows, entries)
	}
	w.waitTold(w.vendors.Id)

	// The app's update: renamed, and the plan's open views already told, when
	// it returns.
	w.quiet()
	lead := "Lead"
	if _, err := UpdateField(w.ctx, w.plan.Id, f.Id, FieldPatch{Name: &lead}, w.me); err != nil {
		t.Fatal(err)
	}
	sent := w.sentToPlan()
	if name, _ := w.fieldName(f.Id); name != "Lead" || !slices.ContainsFunc(sent, func(m string) bool { return strings.Contains(m, `"action":"fields"`) }) {
		t.Errorf("when the app's update returned the field was %q and the plan's views were told %q; want Lead, told", name, sent)
	}
	w.waitTold(w.vendors.Id)

	// The app's delete: in the trash when it returns; Vendors' readers told.
	w.quiet()
	if err := DeleteField(w.ctx, w.plan.Id, f.Id, w.me); err != nil {
		t.Fatal(err)
	}
	if _, live := w.fieldName(f.Id); live {
		t.Errorf("when the app's delete returned the field was live")
	}
	w.waitTold(w.vendors.Id)

	// A view made, changed and deleted by the app: each there when it returns.
	v, err := CreateView(w.ctx, w.plan.Id, ViewInput{Name: "Plan", Type: model.ViewGrid}, w.me)
	if err != nil {
		t.Fatal(err)
	}
	if n := w.count(`SELECT count(*) FROM data_table_views WHERE id = $1 AND name = 'Plan' AND deleted_at IS NULL`, v.Id); n != 1 {
		t.Errorf("when the app's view create returned, %d views were there", n)
	}
	if err := UpdateView(w.ctx, w.plan.Id, v.Id, ViewInput{Name: "Roadmap", Type: model.ViewGrid}, w.me); err != nil {
		t.Fatal(err)
	}
	if n := w.count(`SELECT count(*) FROM data_table_views WHERE id = $1 AND name = 'Roadmap'`, v.Id); n != 1 {
		t.Errorf("when the app's view update returned, the view wasn't renamed")
	}
	if err := DeleteView(w.ctx, w.plan.Id, v.Id, w.me); err != nil {
		t.Fatal(err)
	}
	if n := w.count(`SELECT count(*) FROM data_table_views WHERE id = $1 AND deleted_at IS NOT NULL`, v.Id); n != 1 {
		t.Errorf("when the app's view delete returned, the view wasn't in the trash")
	}
}

// The notice a write hands off to a linked table's readers is work in
// flight: settling the background work waits for it, however long it
// takes. So a server shutting down sends it, and one test's notice can't
// land in the next test's world.
func TestALinksNoticeAWriteHandsOffIsWaitedFor(t *testing.T) {
	w := newItemWorld(t)
	code := w.field(w.plan, "Code", model.FieldText, nil)
	w.quiet()
	tell := publishLinks
	publishLinks = func(id uuid.UUID) {
		time.Sleep(time.Second) // slower than any grace a caller might allow
		tell(id)
	}
	name := "Code name"
	if _, err := UpdateField(w.ctx, w.plan.Id, code.Id, FieldPatch{Name: &name}, w.me); err != nil {
		t.Fatal(err)
	}
	helpers.SettleBackground(10 * time.Second)
	if told := w.linksTold(); !slices.Contains(told, w.vendors.Id) {
		t.Errorf("once the rename's background work settled, the tables told were %v; want Vendors (%s), which links to the plan", told, w.vendors.Id)
	}
}

// viewName is view id's name, and whether it is live (not in the trash).
func (w *itemWorld) viewName(id uuid.UUID) (name string, live bool) {
	w.t.Helper()
	var deleted *time.Time
	if err := postgresInit.DBConn.SqlDB.QueryRowContext(w.ctx,
		`SELECT name, deleted_at FROM data_table_views WHERE id = $1`, id).Scan(&name, &deleted); err != nil {
		w.t.Fatalf("read view %s: %v", id, err)
	}
	return name, deleted == nil
}

// shown is how many live fields of Vendors show the links of the plan's
// relation field of.
func (w *itemWorld) shown(of uuid.UUID) int {
	return w.count(`SELECT count(*) FROM data_table_fields WHERE table_id = $1 AND deleted_at IS NULL
		AND config->>'inverse_of' = $2`, w.vendors.Id, of.String())
}

// A view's create, update and delete whose rows are refused leave the view as
// it was: none made, its name kept, live. Once rows are taken, each is there
// with its row.
func TestAViewItemWhoseRowIsRefusedLeavesTheViewAsItWas(t *testing.T) {
	w := newItemWorld(t)
	audit := w.auditAs("tables.views.batch")
	lift := w.refuseRows()
	board := uuid.New()
	create, err := viewCreate(w.me, w.plan.Id, nil, map[string]any{"name": "Board", "type": model.ViewBoard}, 0, board)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applyItem(w.ctx, create, audit); !errors.Is(err, errBatchAudit) {
		t.Fatalf("a view's create whose row was refused answered %v; want %v", err, errBatchAudit)
	}
	if n := w.count(`SELECT count(*) FROM data_table_views WHERE id = $1`, board); n != 0 {
		t.Errorf("a view's create whose row was refused left %d views; want none", n)
	}
	lift()
	if _, err := applyItem(w.ctx, create, audit); err != nil {
		t.Fatalf("the view's create once rows are taken: %v", err)
	}
	if name, live := w.viewName(board); name != "Board" || !live {
		t.Fatalf("the view's create left %q (live %v); want Board, live", name, live)
	}

	lift = w.refuseRows()
	rename, err := viewUpdate(w.me, w.plan.Id, nil, map[string]any{"view": board.String(), "name": "Roadmap"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applyItem(w.ctx, rename, audit); !errors.Is(err, errBatchAudit) {
		t.Fatalf("a view's update whose row was refused answered %v; want %v", err, errBatchAudit)
	}
	remove := viewDelete(w.me, w.plan.Id, nil, board, 0)
	if _, err := applyItem(w.ctx, remove, audit); !errors.Is(err, errBatchAudit) {
		t.Fatalf("a view's delete whose row was refused answered %v; want %v", err, errBatchAudit)
	}
	if name, live := w.viewName(board); name != "Board" || !live {
		t.Errorf("after an update and a delete whose rows were refused, the view is %q (live %v); want Board, live", name, live)
	}

	lift()
	if _, err := applyItem(w.ctx, rename, audit); err != nil {
		t.Fatalf("the view's update once rows are taken: %v", err)
	}
	if name, _ := w.viewName(board); name != "Roadmap" {
		t.Errorf("the view's update left it named %q; want Roadmap", name)
	}
	if _, err := applyItem(w.ctx, remove, audit); err != nil {
		t.Fatalf("the view's delete once rows are taken: %v", err)
	}
	if _, live := w.viewName(board); live {
		t.Errorf("the view's delete left it live; want it in the trash")
	}
	if actions, _ := w.rowsAbout(board); !slices.Equal(actions, []string{"table.view.created", "table.view.updated", "table.view.deleted"}) {
		t.Errorf("the view's rows are %q; want its create, update and delete", actions)
	}
}

// A primary change whose row is refused leaves the primary where it was.
// Once rows are taken it moves in the item's transaction: SetPrimary joins
// it, where on its own it would wait on the item's lock on the field's row.
func TestAPrimaryChangeWhoseRowIsRefusedLeavesThePrimary(t *testing.T) {
	w := newItemWorld(t)
	primary := func() uuid.UUID {
		t.Helper()
		var id uuid.UUID
		if err := postgresInit.DBConn.SqlDB.QueryRowContext(w.ctx, `SELECT id FROM data_table_fields
			WHERE table_id = $1 AND is_primary AND deleted_at IS NULL`, w.plan.Id).Scan(&id); err != nil {
			t.Fatalf("read the plan's primary: %v", err)
		}
		return id
	}
	was := primary()
	code := w.field(w.plan, "Code", model.FieldText, nil)
	w.quiet()
	lift := w.refuseRows()
	change, err := fieldUpdate(w.me, w.plan.Id, nil, map[string]any{"field": code.Id.String(), "primary": true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applyItem(w.ctx, change, w.audit()); !errors.Is(err, errBatchAudit) {
		t.Fatalf("a primary change whose row was refused answered %v; want %v", err, errBatchAudit)
	}
	if p := primary(); p != was {
		t.Errorf("a primary change whose row was refused left %s the primary; want %s, as it was", p, was)
	}
	if views, linked := w.heard(); len(views)+len(linked) != 0 {
		t.Errorf("a primary change whose row was refused told the plan's views %q and the tables %v; want nothing", views, linked)
	}

	lift()
	if _, err := applyItem(w.ctx, change, w.audit()); err != nil {
		t.Fatalf("the primary change once rows are taken: %v", err)
	}
	if p := primary(); p != code.Id {
		t.Errorf("the primary change left %s the primary; want Code (%s)", p, code.Id)
	}
	if actions, _ := w.rowsAbout(code.Id); !slices.Equal(actions, []string{"table.field.updated"}) {
		t.Errorf("the primary change's rows are %q; want one table.field.updated", actions)
	}
}

// A relation's pair whose row is refused is as it was, and nothing is told:
// a two-way create leaves no field on either side, a delete of either side
// leaves both live, and a one-way relation turned two-way shows nothing in
// the other table. Once rows are taken, each is there.
func TestARelationPairWhoseRowIsRefusedIsAsItWas(t *testing.T) {
	w := newItemWorld(t)
	twoWay := map[string]any{"relation_target": "table", "table_id": w.vendors.Id.String(), "two_way": true}
	lift := w.refuseRows()
	link := uuid.New()
	create, err := fieldCreate(w.me, w.plan.Id, nil, map[string]any{"name": "Suppliers", "type": model.FieldRelation, "config": twoWay}, 0, link)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applyItem(w.ctx, create, w.audit()); !errors.Is(err, errBatchAudit) {
		t.Fatalf("a relation's create whose row was refused answered %v; want %v", err, errBatchAudit)
	}
	views, linked := w.heard()
	if rows, _, _ := w.left(link); rows != 0 || w.shown(link) != 0 {
		t.Errorf("a relation's create whose row was refused left %d fields here and %d in Vendors; want none", rows, w.shown(link))
	}
	if len(views)+len(linked) != 0 {
		t.Errorf("a relation's create whose row was refused told the plan's views %q and the tables %v; want nothing", views, linked)
	}
	lift()
	if _, err := applyItem(w.ctx, create, w.audit()); err != nil {
		t.Fatalf("the relation's create once rows are taken: %v", err)
	}
	if rows, _, _ := w.left(link); rows != 1 || w.shown(link) != 1 {
		t.Fatalf("the relation's create left %d fields here and %d in Vendors; want its pair", rows, w.shown(link))
	}
	theirs := w.otherSide(w.vendors, &model.Field{Id: link})

	// A delete of either side, and the one-way Vendor turned two-way, whose
	// rows are refused.
	w.quiet()
	lift = w.refuseRows()
	mine := fieldDelete(w.me, w.plan.Id, nil, link, 0)
	if _, err := applyItem(w.ctx, mine, w.audit()); !errors.Is(err, errBatchAudit) {
		t.Fatalf("a delete of the relation whose row was refused answered %v; want %v", err, errBatchAudit)
	}
	if _, err := applyItem(w.ctx, fieldDelete(w.me, w.vendors.Id, nil, theirs.Id, 0), w.audit()); !errors.Is(err, errBatchAudit) {
		t.Fatalf("a delete of the side showing it whose row was refused answered %v; want %v", err, errBatchAudit)
	}
	turn, err := fieldUpdate(w.me, w.plan.Id, nil, map[string]any{"field": w.vendor.Id.String(), "config": twoWay}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applyItem(w.ctx, turn, w.audit()); !errors.Is(err, errBatchAudit) {
		t.Fatalf("turning Vendor two-way, its row refused, answered %v; want %v", err, errBatchAudit)
	}
	views, linked = w.heard()
	_, liveMine := w.fieldName(link)
	_, liveTheirs := w.fieldName(theirs.Id)
	if !liveMine || !liveTheirs || w.shown(link) != 1 {
		t.Errorf("after deletes whose rows were refused, the relation is live %v and the side showing it live %v (%d shown); want both live", liveMine, liveTheirs, w.shown(link))
	}
	if n := w.shown(w.vendor.Id); n != 0 {
		t.Errorf("turning Vendor two-way, its row refused, left %d fields in Vendors showing it; want none", n)
	}
	if len(views)+len(linked) != 0 {
		t.Errorf("writes whose rows were refused told the plan's views %q and the tables %v; want nothing", views, linked)
	}

	// Once rows are taken: Vendors shows Vendor's links, and the delete takes
	// the pair.
	lift()
	if _, err := applyItem(w.ctx, turn, w.audit()); err != nil {
		t.Fatalf("turning Vendor two-way once rows are taken: %v", err)
	}
	if n := w.shown(w.vendor.Id); n != 1 {
		t.Errorf("turning Vendor two-way left %d fields in Vendors showing it; want 1", n)
	}
	if _, err := applyItem(w.ctx, mine, w.audit()); err != nil {
		t.Fatalf("the relation's delete once rows are taken: %v", err)
	}
	_, liveMine = w.fieldName(link)
	_, liveTheirs = w.fieldName(theirs.Id)
	if liveMine || liveTheirs {
		t.Errorf("the relation's delete left it live %v and the side showing it live %v; want both in the trash", liveMine, liveTheirs)
	}
}

// A batch's relation create that the store made but the batch's record
// missed is found, sent again, by the id the batch's key gives it: answered
// as made before with that id, and no second pair is made.
func TestABatchsRelationSentAgainIsTheOneItMade(t *testing.T) {
	w := newItemWorld(t)
	was := openItems
	t.Cleanup(func() { openItems = was })
	openItems = func(context.Context, string, string, string, string) (batchRecord, error) {
		return newRecord(), nil // a record of its own each send: the first send's is lost
	}
	info := userModels.UserInfo{}
	info.UserPostgresInfo.Id = w.me.UserID
	ctx := context.WithValue(w.ctx, helpers.UserInfoContextKey, info)
	send := func() apiBatchDone {
		t.Helper()
		out, err := serveFieldsBatch(ctx, v1api.Call{Args: map[string]any{"id": w.plan.Id.String(), "key": "suppliers-once",
			"create": []any{map[string]any{"name": "Suppliers", "type": model.FieldRelation,
				"config": map[string]any{"relation_target": "table", "table_id": w.vendors.Id.String(), "two_way": true}}}}})
		if err != nil {
			t.Fatalf("the batch: %v", err)
		}
		done, ok := out.(apiBatchDone)
		if !ok || len(done.Created) != 1 {
			t.Fatalf("the batch answered %+v; want one create", out)
		}
		return done
	}
	var made model.Field
	if err := json.Unmarshal(send().Created[0], &made); err != nil || made.Id == uuid.Nil {
		t.Fatalf("the first send made %s (%v); want the relation", made.Id, err)
	}
	var again apiMadeBefore
	if err := json.Unmarshal(send().Created[0], &again); err != nil || !again.MadeBefore || again.ID != made.Id.String() {
		t.Errorf("sent again, the create answered %+v (%v); want made_before with %s", again, err, made.Id)
	}
	if mine := w.count(`SELECT count(*) FROM data_table_fields WHERE table_id = $1 AND name LIKE 'Suppliers%'`, w.plan.Id); mine != 1 || w.shown(made.Id) != 1 {
		t.Errorf("after two sends there are %d Suppliers relations and %d fields showing it; want one pair", mine, w.shown(made.Id))
	}
	if n := w.count(`SELECT count(*) FROM data_table_fields WHERE table_id = $1 AND config->>'inverse_of' IS NOT NULL`, w.vendors.Id); n != 1 {
		t.Errorf("Vendors has %d fields showing a relation's links; want the one pair's", n)
	}
}

// A delete a send applied but whose record missed it (the record's write
// failed after the item's commit) is answered, when the same batch is sent
// again, as deleted before: it is in the trash already, and the resend must
// not refuse the whole batch over it, nor write a second audit row. A first
// send naming something already in the trash is refused as before: only a
// send of the same batch knows the delete was its own (V-529 (1)).
func TestABatchsDeleteSentAgainIsAnsweredAsDone(t *testing.T) {
	w := newItemWorld(t)
	budget := w.field(w.plan, "Budget", model.FieldNumber, nil)
	info := userModels.UserInfo{}
	info.UserPostgresInfo.Id = w.me.UserID
	ctx := context.WithValue(w.ctx, helpers.UserInfoContextKey, info)
	was := openItems
	t.Cleanup(func() { openItems = was })
	sends := 0
	openItems = func(context.Context, string, string, string, string) (batchRecord, error) {
		sends++
		r := newRecord()
		if sends == 1 {
			r.failMark[0] = true // the delete is done, but its record misses it
		} else {
			r.again = true // a send of this batch came before (its record holds no item)
		}
		return r, nil
	}
	views := func(key string, args map[string]any) (any, error) {
		args["id"], args["key"] = w.plan.Id.String(), key
		return serveViewsBatch(ctx, v1api.Call{Args: args})
	}
	out, err := views("make-board", map[string]any{"create": []any{map[string]any{"name": "Board", "type": model.ViewBoard}}})
	if err == nil {
		t.Fatalf("the view's create answered %+v; want it stopped by its record's write", out)
	}
	var board uuid.UUID
	if n := w.count(`SELECT count(*) FROM data_table_views WHERE table_id = $1 AND name = 'Board' AND deleted_at IS NULL`, w.plan.Id); n != 1 {
		t.Fatalf("the view's create left %d Board views; want one", n)
	}
	if err := postgresInit.DBConn.SqlDB.QueryRowContext(w.ctx, `SELECT id FROM data_table_views WHERE table_id = $1 AND name = 'Board'`, w.plan.Id).Scan(&board); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		what  string
		id    uuid.UUID
		send  func(key string, args map[string]any) (any, error)
		live  func() bool
		audit string
	}{
		{"field", budget.Id, func(key string, args map[string]any) (any, error) {
			args["id"], args["key"] = w.plan.Id.String(), key
			return serveFieldsBatch(ctx, v1api.Call{Args: args})
		}, func() bool { _, live := w.fieldName(budget.Id); return live }, "table.field.deleted"},
		{"view", board, views, func() bool { _, live := w.viewName(board); return live }, "table.view.deleted"},
	} {
		t.Run(c.what, func(t *testing.T) {
			sends = 0
			del := map[string]any{"delete": []any{c.id.String()}}
			if out, err := c.send("drop-"+c.what, del); err == nil {
				t.Fatalf("the first send answered %+v; want it stopped by its record's write", out)
			}
			if c.live() {
				t.Fatalf("the first send's delete didn't take: the %s is live", c.what)
			}
			out, err := c.send("drop-"+c.what, map[string]any{"delete": []any{c.id.String()}})
			if err != nil {
				t.Fatalf("the batch sent again: %v; want its delete answered as done before", err)
			}
			done, ok := out.(apiBatchDone)
			if !ok || len(done.Deleted) != 1 {
				t.Fatalf("the batch sent again answered %+v; want one delete", out)
			}
			var again apiDeleted
			if err := json.Unmarshal(done.Deleted[0], &again); err != nil || !again.Deleted || !again.DeletedBefore || again.ID != c.id {
				t.Errorf("sent again, the delete answered %s (%v); want deleted and deleted_before for %s", done.Deleted[0], err, c.id)
			}
			if actions, _ := w.rowsAbout(c.id); slices.Index(actions, c.audit) < 0 || len(slices.DeleteFunc(slices.Clone(actions), func(a string) bool { return a != c.audit })) != 1 {
				t.Errorf("the %s's audit rows are %v; want one %s", c.what, actions, c.audit)
			}
			sends = 0 // a first send of another batch: nothing came before it
			if out, err := c.send("drop-again-"+c.what, map[string]any{"delete": []any{c.id.String()}}); err == nil {
				t.Errorf("a first send naming a %s in the trash answered %+v; want it refused", c.what, out)
			}
		})
	}
}
