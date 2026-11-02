//go:build integration

package business

// V-121 (PROGRAM.md §13 O3; main's ruling of 30 Oct): a table goes to the
// trash softly, and only a delete for good, or its retention running out,
// removes it, with everything it holds outside its own tables, each through
// that thing's owner: its rows' pages (docs), their comment threads, its
// files (archived, so the attachments purge takes the bytes) and its guest
// links. A live table that linked to it stays whole.

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	tableAccess "github.com/akashc777/OneCamp/business/TableAccess"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	postgresStruct "github.com/akashc777/OneCamp/models/postgres"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	formModel "github.com/akashc777/OneCamp/models/postgres/Form"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	threadModels "github.com/akashc777/OneCamp/models/postgres/Thread"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// furnished is a table with something of each kind it holds elsewhere.
type furnished struct {
	table   *model.DataTable
	row     uuid.UUID
	page    uuid.UUID
	thread  uuid.UUID
	comment uuid.UUID
	file    uuid.UUID
	// The table's own form (no project; public at /f/<token>), its cover and
	// a file a visitor is still sending with it (V-121 F1).
	form   *formModel.Form
	cover  uuid.UUID
	upload uuid.UUID
}

func (w *relationWorld) purgeExec(q string, args ...any) {
	w.t.Helper()
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(w.ctx, q, args...); err != nil {
		w.t.Fatalf("%s: %v", q, err)
	}
}

func (w *relationWorld) purgeCount(q string, args ...any) int {
	w.t.Helper()
	var n int
	if err := postgresInit.DBConn.SqlDB.QueryRowContext(w.ctx, q, args...).Scan(&n); err != nil {
		w.t.Fatalf("%s: %v", q, err)
	}
	return n
}

// furnish makes a table with a row that has a page, a comment thread (a
// comment with a reaction), a file of the table, a guest link to the table
// and one to the row's page.
func (w *relationWorld) furnish(name, visibility string) furnished {
	w.t.Helper()
	tb, err := CreateTable(w.ctx, TableInput{Name: name, Visibility: visibility}, w.me)
	if err != nil {
		w.t.Fatalf("create %s: %v", name, err)
	}
	fields, err := model.ListFields(w.ctx, tb.Id)
	if err != nil || len(fields) == 0 {
		w.t.Fatalf("%s has no name field: %v", name, err)
	}
	f := furnished{table: tb, page: uuid.New(), thread: uuid.New(), comment: uuid.New(), file: uuid.New()}
	f.row = w.row(tb, map[string]interface{}{fields[0].Id.String(): "Launch"})
	w.purgeExec(`UPDATE data_table_rows SET doc_uuid = $2 WHERE id = $1`, f.row, f.page)
	now := time.Now()
	if _, err := threadModels.Insert(w.ctx, &threadModels.Row{ID: f.thread, ResourceKind: tableAccess.KindTableRow,
		ResourceUUID: f.row.String(), AnchorKind: "none", Status: "open", CreatedBy: w.me.UserID, CreatedAt: now, UpdatedAt: now}, false); err != nil {
		w.t.Fatalf("thread: %v", err)
	}
	if err := threadModels.InsertComment(w.ctx, &threadModels.Comment{ID: f.comment, ThreadID: f.thread,
		ResourceKind: tableAccess.KindTableRow, ResourceUUID: f.row.String(), AuthorUUID: w.me.UserID,
		AuthorKind: "person", Body: "Ship it", CreatedAt: now}); err != nil {
		w.t.Fatalf("comment: %v", err)
	}
	if _, _, err := threadModels.SetReaction(w.ctx, uuid.New(), f.comment, w.me.UserID, "thumbsup", now); err != nil {
		w.t.Fatalf("reaction: %v", err)
	}
	w.purgeExec(`INSERT INTO attachments (id, obj_key, src_key, src_value, created_by) VALUES ($1, $2, $3, $4, $5)`,
		f.file, "tables/"+f.file.String(), postgresStruct.ATTACHMENT_SRC_TABLE, tb.Id.String(), w.me.UserID)
	if _, err := guestModel.CreateGrant(w.ctx, []byte("purge-"+tb.Id.String()), tableAccess.KindTable, tb.Id.String(), "view", w.me.UserID, nil); err != nil {
		w.t.Fatalf("guest link: %v", err)
	}
	if _, err := guestModel.CreateGrant(w.ctx, []byte("purge-page-"+f.page.String()), guestModel.ResourceDoc, f.page.String(), "view", w.me.UserID, nil); err != nil {
		w.t.Fatalf("page guest link: %v", err)
	}
	f.form, f.cover, f.upload = w.form(uuid.Nil, tb.Id)
	return f
}

// form makes a form that adds rows to table: the table's own (project
// uuid.Nil) or a project's. It has an answer sent, a cover, and a file a
// visitor is still sending (waiting under their claim).
func (w *relationWorld) form(project, table uuid.UUID) (*formModel.Form, uuid.UUID, uuid.UUID) {
	w.t.Helper()
	cover, upload := uuid.New(), uuid.New()
	w.purgeExec(`INSERT INTO attachments (id, obj_key, src_key, src_value, created_by) VALUES ($1, $2, $3, $4, $5)`,
		cover, "forms/"+cover.String(), postgresStruct.ATTACHMENT_SRC_FORM, w.me.UserID.String(), w.me.UserID)
	fm, err := formModel.Create(w.ctx, formModel.Form{ProjectUUID: project, Token: "purge-" + uuid.NewString(), Title: "Intake",
		Fields: json.RawMessage(`[]`), Priority: "medium", Active: true, TargetTable: &table, CreatedBy: w.me.UserID, CoverID: &cover})
	if err != nil {
		w.t.Fatalf("form: %v", err)
	}
	if _, err := formModel.Record(w.ctx, fm.Id, nil, nil, json.RawMessage(`{"q1":"Launch"}`)); err != nil {
		w.t.Fatalf("answer: %v", err)
	}
	w.purgeExec(`INSERT INTO form_uploads (id, form_id, claim, question, obj_key, file_name, content_type, size)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		upload, fm.Id, "claim-"+upload.String(), "q2", "forms/uploads/"+upload.String(), "brief.pdf", "application/pdf", 1024)
	return fm, cover, upload
}

// formLeft says what of a form is still there, each kind 0 or 1: the form,
// its answers, its public link (formModel.ByToken), its live cover, and its
// waiting file while the uploads sweep wouldn't take it yet
// (formModel.Unsent, which takes it once it's gone or due).
func (w *relationWorld) formLeft(fm *formModel.Form, cover, upload uuid.UUID) map[string]int {
	w.t.Helper()
	link, err := formModel.ByToken(w.ctx, fm.Token)
	if err != nil {
		w.t.Fatalf("the form's link: %v", err)
	}
	now := time.Now()
	due, err := formModel.Unsent(w.ctx, now.Add(-24*time.Hour), now, 10000)
	if err != nil {
		w.t.Fatalf("the files the uploads sweep takes: %v", err)
	}
	waiting := w.purgeCount(`SELECT COUNT(*) FROM form_uploads WHERE id = $1`, upload)
	for _, u := range due {
		if u.ID == upload {
			waiting = 0
		}
	}
	linked := 0
	if link != nil {
		linked = 1
	}
	return map[string]int{
		"form":         w.purgeCount(`SELECT COUNT(*) FROM project_forms WHERE id = $1`, fm.Id),
		"form answers": w.purgeCount(`SELECT COUNT(*) FROM form_submissions WHERE form_id = $1`, fm.Id),
		"form link":    linked,
		"live cover":   w.purgeCount(`SELECT COUNT(*) FROM attachments WHERE id = $1 AND deleted_at IS NULL`, cover),
		"waiting file": waiting,
	}
}

// left says what of f is still there, empty when nothing is: the table, its
// rows, its row's thread, comment and reaction, its live file, its guest
// links and its row page's (V-121 S2), and its own form with what that holds
// (formLeft).
func (w *relationWorld) left(f furnished) map[string]int {
	w.t.Helper()
	left := map[string]int{}
	for what, n := range map[string]int{
		"table":           w.purgeCount(`SELECT COUNT(*) FROM data_tables WHERE id = $1`, f.table.Id),
		"rows":            w.purgeCount(`SELECT COUNT(*) FROM data_table_rows WHERE table_id = $1`, f.table.Id),
		"threads":         w.purgeCount(`SELECT COUNT(*) FROM threads WHERE resource_kind = $1 AND resource_uuid = $2`, tableAccess.KindTableRow, f.row.String()),
		"comments":        w.purgeCount(`SELECT COUNT(*) FROM thread_comments WHERE resource_kind = $1 AND resource_uuid = $2`, tableAccess.KindTableRow, f.row.String()),
		"reactions":       w.purgeCount(`SELECT COUNT(*) FROM thread_comment_reactions WHERE comment_id = $1`, f.comment),
		"live file":       w.purgeCount(`SELECT COUNT(*) FROM attachments WHERE id = $1 AND deleted_at IS NULL`, f.file),
		"guest link":      w.purgeCount(`SELECT COUNT(*) FROM guest_grants WHERE resource_type = $1 AND resource_id = $2`, tableAccess.KindTable, f.table.Id.String()),
		"page guest link": w.purgeCount(`SELECT COUNT(*) FROM guest_grants WHERE resource_type = $1 AND resource_id = $2`, guestModel.ResourceDoc, f.page.String()),
	} {
		if n > 0 {
			left[what] = n
		}
	}
	for what, n := range w.formLeft(f.form, f.cover, f.upload) {
		if n > 0 {
			left[what] = n
		}
	}
	return left
}

// seePurges records the pages handed to docs' purge and the tables' purge
// events, until the test ends.
func (w *relationWorld) seePurges() (pages func() []uuid.UUID, events func() []resourceEventBusiness.Event) {
	var mu sync.Mutex
	var gotPages []uuid.UUID
	var got []resourceEventBusiness.Event
	was := purgeHostedDocs
	purgeHostedDocs = func(_ context.Context, docs []uuid.UUID, _ resourceEventBusiness.Actor, _ string) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		gotPages = append(gotPages, docs...)
		return len(docs), nil
	}
	w.t.Cleanup(func() { purgeHostedDocs = was })
	resourceEventBusiness.Sinks.Register(resourceEventBusiness.Sink{Name: "test." + w.t.Name(),
		Wants: func(e resourceEventBusiness.Event) bool { return e.Kind == resourceEventBusiness.TablePurged },
		Run: func(_ context.Context, e resourceEventBusiness.Event) {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, e)
		}})
	settle := func() {
		helpers.SettleBackground(30 * time.Second)
		resourceEventBusiness.Sinks.Settle(10 * time.Second)
	}
	return func() []uuid.UUID {
			settle()
			mu.Lock()
			defer mu.Unlock()
			return append([]uuid.UUID(nil), gotPages...)
		}, func() []resourceEventBusiness.Event {
			settle()
			mu.Lock()
			defer mu.Unlock()
			return append([]resourceEventBusiness.Event(nil), got...)
		}
}

func subjectOf(a Actor) accessBusiness.Subject {
	return accessBusiness.Subject{UserUUID: a.UserID.String(), Admin: a.IsAdmin}
}

// The owner deletes a table in the trash for good: everything it holds goes
// through its owners, the event names who and why, and a live table that
// linked to its row keeps its own row, losing only the link.
func TestATableDeletedForGoodTakesWhatItHoldsWithIt(t *testing.T) {
	w := newRelationWorld(t)
	ctx := w.ctx
	pages, events := w.seePurges()
	f := w.furnish("Plans", model.VisibilityWorkspace)
	// Only the table's own forms go with it (main, 30 Oct): another table's
	// own form, and a project's form that adds rows to this table, stay whole.
	other := w.furnish("Other", model.VisibilityWorkspace)
	projectForm, projectCover, projectUpload := w.form(uuid.New(), f.table.Id)
	budget, bName := w.table("Budget")
	plan := w.field(budget, "Plan", model.FieldRelation, map[string]interface{}{"relation_target": "table", "table_id": f.table.Id.String()})
	kept := w.row(budget, map[string]interface{}{bName.Id.String(): "Q4"})
	w.purgeExec(`INSERT INTO data_table_links (field_id, from_row, to_row) VALUES ($1, $2, $3)`, plan.Id, kept, f.row)

	if err := DeleteTable(ctx, f.table.Id, w.me); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if left := w.left(f); left["threads"] == 0 || left["live file"] == 0 || left["guest link"] == 0 ||
		left["form"] == 0 || left["form answers"] == 0 || left["live cover"] == 0 || left["waiting file"] == 0 {
		t.Fatalf("a delete must stay soft, everything kept for a restore; left %v", left)
	}
	if err := PurgeTable(ctx, f.table.Id, subjectOf(w.me), w.me); err != nil {
		t.Fatalf("the owner's delete for good: %v", err)
	}
	if got := pages(); len(got) != 1 || got[0] != f.page {
		t.Errorf("docs' purge took pages %v, want the row's page %s", got, f.page)
	}
	if left := w.left(f); len(left) > 0 {
		t.Errorf("deleted for good, but left %v", left)
	}
	for name, kept := range map[string]map[string]int{
		"another table's own form":              w.formLeft(other.form, other.cover, other.upload),
		"a project's form that adds rows to it": w.formLeft(projectForm, projectCover, projectUpload),
	} {
		for what, n := range kept {
			if n != 1 {
				t.Errorf("%s: %s %d, want 1: only the table's own forms go with it", name, what, n)
			}
		}
	}
	got := events()
	if len(got) != 1 || got[0].ResourceUUID != f.table.Id.String() || got[0].Actor.UserUUID != w.me.UserID.String() ||
		got[0].Actor.System || got[0].Data["reason"] != "deleted for good" {
		t.Errorf("want one table.purged by the owner, reason \"deleted for good\"; got %+v", got)
	}
	if n := w.purgeCount(`SELECT COUNT(*) FROM data_tables WHERE id = $1 AND deleted_at IS NULL`, budget.Id); n != 1 {
		t.Errorf("the live table that linked to it: %d, want 1", n)
	}
	if n := w.purgeCount(`SELECT COUNT(*) FROM data_table_rows WHERE id = $1 AND deleted_at IS NULL`, kept); n != 1 {
		t.Errorf("the live table's row: %d, want 1", n)
	}
	if n := w.links(plan.Id, kept); n != 0 {
		t.Errorf("the live row still links to the purged row: %d links", n)
	}
}

// Only the owner, someone with full access or an admin deletes a table for
// good, and only from the trash (accessBusiness.OpenDeleted, as docs).
func TestOnlyFullAccessDeletesATableForGoodAndOnlyFromTheTrash(t *testing.T) {
	w := newRelationWorld(t)
	ctx := w.ctx
	shared := w.furnish("Shared", model.VisibilityWorkspace)
	private := w.furnish("Mine", model.VisibilityPrivate)
	live := w.furnish("Live", model.VisibilityWorkspace)
	for _, f := range []furnished{shared, private} {
		if err := DeleteTable(ctx, f.table.Id, w.me); err != nil {
			t.Fatalf("delete %s: %v", f.table.Name, err)
		}
	}
	if err := PurgeTable(ctx, shared.table.Id, subjectOf(w.member), w.member); !IsForbidden(err) {
		t.Errorf("a member who could only edit the shared table: %v, want forbidden", err)
	}
	if err := PurgeTable(ctx, private.table.Id, subjectOf(w.member), w.member); !IsNotFound(err) {
		t.Errorf("a member who never saw the private table: %v, want not found", err)
	}
	if err := PurgeTable(ctx, live.table.Id, subjectOf(w.me), w.me); !IsConflict(err) {
		t.Errorf("a live table: %v, want a conflict (only the trash deletes for good)", err)
	}
	helpers.SettleBackground(30 * time.Second)
	for _, f := range []furnished{shared, private, live} {
		if left := w.left(f); left["table"] != 1 || left["threads"] != 1 || left["live file"] != 1 || left["guest link"] != 1 {
			t.Errorf("%s was refused, so all of it stays; left %v", f.table.Name, left)
		}
	}
}

// The hourly purge removes, as a delete for good does, only tables in the
// trash past their retention, recorded as the server's own, and never one
// while the retention is 0 (keep).
func TestTheRetentionPurgeTakesOnlyTablesPastTheirDays(t *testing.T) {
	w := newRelationWorld(t)
	ctx := w.ctx
	_, events := w.seePurges()
	old := w.furnish("Old", model.VisibilityWorkspace)
	recent := w.furnish("Recent", model.VisibilityWorkspace)
	now := time.Now()
	for f, ago := range map[furnished]time.Duration{old: 31 * 24 * time.Hour, recent: 29 * 24 * time.Hour} {
		if err := DeleteTable(ctx, f.table.Id, w.me); err != nil {
			t.Fatalf("delete: %v", err)
		}
		w.purgeExec(`UPDATE data_tables SET deleted_at = $2 WHERE id = $1`, f.table.Id, now.Add(-ago))
	}
	if err := trashPurge(ctx, 0, now); err != nil {
		t.Fatalf("keep: %v", err)
	}
	if left := w.left(old); left["table"] != 1 || left["threads"] != 1 {
		t.Fatalf("retention 0 keeps everything; left %v", left)
	}
	if err := trashPurge(ctx, 30, now); err != nil {
		t.Fatalf("30 days: %v", err)
	}
	if left := w.left(old); len(left) > 0 {
		t.Errorf("31 days in the trash, but left %v", left)
	}
	if left := w.left(recent); left["table"] != 1 || left["threads"] != 1 || left["live file"] != 1 || left["guest link"] != 1 {
		t.Errorf("29 days in the trash stays whole; left %v", left)
	}
	got := events()
	if len(got) != 1 || got[0].ResourceUUID != old.table.Id.String() || !got[0].Actor.System || got[0].Data["reason"] != "retention" {
		t.Errorf("want one table.purged by the server, reason \"retention\"; got %+v", got)
	}
}

// A delete for good a stopped server left part way stays out of the trash
// (its list leaves it out and a restore can't find it), and the trash's
// sweep finishes it, even at retention 0 (keep), in the name of whoever
// started it. One claimed lately may still be running elsewhere, so the
// sweep leaves it until it's as stale.
func TestTheSweepFinishesADeleteForGoodAStoppedServerLeft(t *testing.T) {
	w := newRelationWorld(t)
	ctx := w.ctx
	pages, events := w.seePurges()
	stopped := w.furnish("Stopped", model.VisibilityWorkspace)
	running := w.furnish("Running", model.VisibilityWorkspace)
	now := time.Now()
	for f, ago := range map[furnished]time.Duration{stopped: purgeStale + time.Minute, running: 5 * time.Minute} {
		if err := DeleteTable(ctx, f.table.Id, w.me); err != nil {
			t.Fatalf("delete %s: %v", f.table.Name, err)
		}
		// The claim the owner's delete for good makes before it answers, as
		// if its server stopped right after.
		if claimed, err := model.ClaimTableForPurge(ctx, f.table.Id, w.me.UserID, now.Add(-ago)); err != nil || !claimed {
			t.Fatalf("claim %s: %v, %v", f.table.Name, claimed, err)
		}
	}
	if _, err := RestoreTable(ctx, stopped.table.Id, w.me); !IsNotFound(err) {
		t.Errorf("a restore of a table being deleted for good: %v, want not found", err)
	}
	trash, err := TablesTrash(ctx, w.me)
	if err != nil {
		t.Fatalf("the trash: %v", err)
	}
	for _, in := range trash {
		if in.Table.Id == stopped.table.Id || in.Table.Id == running.table.Id {
			t.Errorf("the trash lists %s, which is being deleted for good", in.Table.Name)
		}
	}

	if err := trashPurge(ctx, 0, now); err != nil {
		t.Fatalf("the sweep at 0: %v", err)
	}
	if left := w.left(stopped); len(left) > 0 {
		t.Errorf("the sweep finished the delete for good, but left %v", left)
	}
	if left := w.left(running); left["table"] != 1 || left["threads"] != 1 || left["live file"] != 1 || left["guest link"] != 1 {
		t.Errorf("a delete for good that may still be running is left to it; left %v", left)
	}
	if err := trashPurge(ctx, 0, now.Add(purgeStale)); err != nil {
		t.Fatalf("the sweep once it's as stale: %v", err)
	}
	if left := w.left(running); len(left) > 0 {
		t.Errorf("once as stale, the sweep finishes it too, but left %v", left)
	}

	if got := pages(); len(got) != 2 || got[0] != stopped.page || got[1] != running.page {
		t.Errorf("docs' purge took pages %v, want the stopped table's %s, then the other's %s", got, stopped.page, running.page)
	}
	got := events()
	byTable := map[string]resourceEventBusiness.Event{}
	for _, e := range got {
		byTable[e.ResourceUUID] = e
	}
	if len(got) != 2 || len(byTable) != 2 {
		t.Fatalf("want one table.purged for each; got %+v", got)
	}
	for _, f := range []furnished{stopped, running} {
		if e, ok := byTable[f.table.Id.String()]; !ok || e.Actor.UserUUID != w.me.UserID.String() || e.Actor.System ||
			e.Data["reason"] != "deleted for good" {
			t.Errorf("want %s's table.purged by the owner who started it, reason \"deleted for good\"; got %+v", f.table.Name, e)
		}
	}
}

// A table whose rows hold more pages than one pass hands docs (docs' limit on
// one trash operation; 1 here) goes for good across passes: each takes at
// most that many, the table stays claimed and out of the trash until its last
// page is gone, and the trash's sweep finishes it in the starter's name,
// leaving nothing behind.
func TestATableWithMorePagesThanOnePassGoesForGoodAcrossPasses(t *testing.T) {
	w := newRelationWorld(t)
	ctx := w.ctx
	was := hostedPagesPerPass
	hostedPagesPerPass = 1
	t.Cleanup(func() { hostedPagesPerPass = was })
	pages, events := w.seePurges()
	f := w.furnish("Notes", model.VisibilityWorkspace)
	fields, err := model.ListFields(ctx, f.table.Id)
	if err != nil || len(fields) == 0 {
		t.Fatalf("fields: %v", err)
	}
	all := map[uuid.UUID]bool{f.page: true}
	for _, name := range []string{"Second", "Third"} {
		row, page := w.row(f.table, map[string]interface{}{fields[0].Id.String(): name}), uuid.New()
		w.purgeExec(`UPDATE data_table_rows SET doc_uuid = $2 WHERE id = $1`, row, page)
		all[page] = true
	}
	if err := DeleteTable(ctx, f.table.Id, w.me); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := PurgeTable(ctx, f.table.Id, subjectOf(w.me), w.me); err != nil {
		t.Fatalf("the owner's delete for good: %v", err)
	}
	now := time.Now()
	for pass := 1; pass <= 3; pass++ {
		if pass > 1 {
			// The sweep takes a claim that has waited past purgeStale.
			if err := trashPurge(ctx, 0, now.Add(time.Duration(pass-1)*(purgeStale+time.Minute))); err != nil {
				t.Fatalf("the sweep's pass %d: %v", pass, err)
			}
		}
		if got := pages(); len(got) != pass {
			t.Fatalf("after pass %d docs' purge took %d pages, want %d (one a pass)", pass, len(got), pass)
		}
		if pass == 3 {
			break
		}
		if left := w.left(f); left["table"] != 1 || left["threads"] != 1 {
			t.Fatalf("after pass %d of 3 the table must still be claimed, whole but for its pages; left %v", pass, left)
		}
		if _, err := RestoreTable(ctx, f.table.Id, w.me); !IsNotFound(err) {
			t.Fatalf("after pass %d a restore must not find it: %v", pass, err)
		}
	}
	seen := map[uuid.UUID]bool{}
	for _, p := range pages() {
		if !all[p] || seen[p] {
			t.Errorf("docs' purge took %s, which isn't one of the table's pages or came twice", p)
		}
		seen[p] = true
	}
	if left := w.left(f); len(left) > 0 {
		t.Errorf("the last pass left %v", left)
	}
	got := events()
	if len(got) != 1 || got[0].ResourceUUID != f.table.Id.String() || got[0].Actor.UserUUID != w.me.UserID.String() ||
		got[0].Actor.System || got[0].Data["reason"] != "deleted for good" {
		t.Errorf("want one table.purged, at the last pass, by the owner who started it; got %+v", got)
	}
}

// One retention pass shares its page budget among the tables it claimed,
// fewest pages first: a table whose pages all fit goes for good in that pass,
// and the first that doesn't takes what is left of the budget and keeps its
// claim for the passes after (V-121 S1, CR-110's SHOULD). One big table must
// not hold back every table of its pass.
func TestARetentionPassFinishesTheTablesWhosePagesFit(t *testing.T) {
	w := newRelationWorld(t)
	ctx := w.ctx
	was := hostedPagesPerPass
	hostedPagesPerPass = 2
	t.Cleanup(func() { hostedPagesPerPass = was })
	pages, events := w.seePurges()
	big := w.furnish("Big", model.VisibilityWorkspace)
	small := w.furnish("Small", model.VisibilityWorkspace)
	fields, err := model.ListFields(ctx, big.table.Id)
	if err != nil || len(fields) == 0 {
		t.Fatalf("fields: %v", err)
	}
	for _, name := range []string{"Second", "Third"} {
		row := w.row(big.table, map[string]interface{}{fields[0].Id.String(): name})
		w.purgeExec(`UPDATE data_table_rows SET doc_uuid = $2 WHERE id = $1`, row, uuid.New())
	}
	now := time.Now()
	for _, f := range []furnished{big, small} {
		if err := DeleteTable(ctx, f.table.Id, w.me); err != nil {
			t.Fatalf("delete: %v", err)
		}
		w.purgeExec(`UPDATE data_tables SET deleted_at = $2 WHERE id = $1`, f.table.Id, now.Add(-31*24*time.Hour))
	}
	if err := trashPurge(ctx, 30, now); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if left := w.left(small); len(left) > 0 {
		t.Fatalf("the small table's one page fits pass 1 (2 pages a pass), but the pass left %v", left)
	}
	if left := w.left(big); left["table"] != 1 || left["threads"] != 1 {
		t.Fatalf("the big table (3 pages, 2 a pass) must stay claimed after pass 1; left %v", left)
	}
	if got := pages(); len(got) != 2 {
		t.Fatalf("pass 1 took %d pages, want 2: the small table's one and one of the big table's", len(got))
	}
	// The sweep takes the big table's claim once it has waited past purgeStale.
	if err := trashPurge(ctx, 0, now.Add(purgeStale+time.Minute)); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if left := w.left(big); len(left) > 0 {
		t.Errorf("the big table's last 2 pages fit pass 2, but it left %v", left)
	}
	if got := pages(); len(got) != 4 {
		t.Errorf("docs' purge took %d pages in all, want the 4 the two tables held", len(got))
	}
	got := events()
	if len(got) != 2 || got[0].ResourceUUID != small.table.Id.String() || got[1].ResourceUUID != big.table.Id.String() {
		t.Errorf("want table.purged for the small table in pass 1, then for the big one in pass 2; got %+v", got)
	}
}

// A purge forgets its rows' comment threads in chunks of 5,000 names, each
// chunk in one transaction, which may run 10 s (V-121 F2; CR-112 measured it
// on Postgres 12.22). By the rows' names alone, the comments' delete can't use
// their only index on the names (it is partial, WHERE deleted_at IS NULL), so
// with 400,000 comments one chunk took 20 s, the transaction timed out, and a
// big table's purge failed on every pass and stayed claimed. By the threads'
// ids both sides are indexed, and the chunk fits. Seeded as CR-112 did: one
// thread per resource, one comment per thread, half on table rows, one in ten
// comments already deleted (by their author: a deleted comment names who
// deleted it, thread_comments_deleted_state).
func TestForgettingRowsCommentsAtScaleFitsInOneTransaction(t *testing.T) {
	w := newRelationWorld(t)
	const n = 400000
	seed, by := uuid.NewString(), uuid.New()
	t.Cleanup(func() {
		w.purgeExec(`DELETE FROM thread_comments WHERE author_uuid = $1`, by)
		w.purgeExec(`DELETE FROM threads WHERE created_by = $1`, by)
	})
	w.purgeExec(`INSERT INTO threads (id, resource_kind, resource_uuid, created_by)
		SELECT md5($1::text || 't' || i)::uuid, CASE WHEN i % 2 = 0 THEN $2::text ELSE 'call' END, md5($1::text || 'r' || i)::uuid::text, $3
		FROM generate_series(1, $4::int) i`, seed, tableAccess.KindTableRow, by, n)
	w.purgeExec(`INSERT INTO thread_comments (id, thread_id, resource_kind, resource_uuid, author_uuid, body, deleted_at, deleted_by)
		SELECT md5($1::text || 'c' || i)::uuid, md5($1::text || 't' || i)::uuid, CASE WHEN i % 2 = 0 THEN $2::text ELSE 'call' END,
		       md5($1::text || 'r' || i)::uuid::text, $3, 'Ship it, comment ' || i, CASE WHEN i % 10 = 0 THEN NOW() END,
		       CASE WHEN i % 10 = 0 THEN $3::uuid END
		FROM generate_series(1, $4::int) i`, seed, tableAccess.KindTableRow, by, n)
	w.purgeExec(`ANALYZE threads`)
	w.purgeExec(`ANALYZE thread_comments`)

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(w.ctx, `SELECT resource_uuid FROM threads
		WHERE created_by = $1 AND resource_kind = $2 ORDER BY resource_uuid LIMIT 5000`, by, tableAccess.KindTableRow)
	if err != nil {
		t.Fatalf("the rows to forget: %v", err)
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("a row's name: %v", err)
		}
		names = append(names, name)
	}
	if err := rows.Close(); err != nil || len(names) != 5000 {
		t.Fatalf("want one chunk of 5,000 rows' names, got %d: %v", len(names), err)
	}

	start := time.Now()
	if err := threadModels.ForgetResources(w.ctx, tableAccess.KindTableRow, names); err != nil {
		t.Fatalf("forgetting one chunk of 5,000 rows' threads among %d comments failed after %s: %v", n, time.Since(start).Round(time.Millisecond), err)
	}
	t.Logf("one chunk of 5,000 rows' threads among %d comments: %s", n, time.Since(start).Round(time.Millisecond))
	if left := w.purgeCount(`SELECT COUNT(*) FROM thread_comments WHERE resource_kind = $1 AND resource_uuid = ANY($2)`,
		tableAccess.KindTableRow, pq.Array(names)); left != 0 {
		t.Errorf("%d comments on the forgotten rows are left, deleted ones included; want none", left)
	}
	if left := w.purgeCount(`SELECT COUNT(*) FROM threads WHERE resource_kind = $1 AND resource_uuid = ANY($2)`,
		tableAccess.KindTableRow, pq.Array(names)); left != 0 {
		t.Errorf("%d threads on the forgotten rows are left; want none", left)
	}
	if kept := w.purgeCount(`SELECT COUNT(*) FROM thread_comments WHERE author_uuid = $1`, by); kept != n-5000 {
		t.Errorf("%d other comments are left, want %d: only the forgotten rows' go", kept, n-5000)
	}
}
