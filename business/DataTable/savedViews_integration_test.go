//go:build integration

package business

// Saved views (migration 512). A view of one's own is any reader's to make and
// theirs alone: no one else lists, changes or deletes it, a manager or an
// admin included (a guest's bundle carries no views at all: forGuests, held
// by TestTableGuestBundle). A shared view is listed for every reader, and
// only the table's managers (its creator, an admin) make, change, delete or
// lock one; a locked one takes no change until a manager unlocks it, even
// when the lock lands mid-change. A view is found only on its own table, and
// its config is bounded.
//
// Run: integ_rel.sh <tree> <log> '^(TestAViewTakesB4sLayouts|TestAViewOfOnesOwnIsTheirsAlone|TestASharedViewIsItsManagersToChange|TestALockedViewTakesNoChangeUntilUnlocked|TestALockThatLandsMidChangeHolds|TestAViewIsFoundOnlyOnItsOwnTable)$' with PKGS='./business/DataTable/'

import (
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// viewsSeenBy are the names of the views of table that reader's bundle lists.
func (w *relationWorld) viewsSeenBy(table uuid.UUID, reader Actor) []string {
	w.t.Helper()
	b, err := GetBundle(w.ctx, table, reader)
	if err != nil {
		w.t.Fatalf("open the table: %v", err)
	}
	var out []string
	for _, v := range b.Views {
		out = append(out, v.Name)
	}
	return out
}

func TestAViewOfOnesOwnIsTheirsAlone(t *testing.T) {
	w := newRelationWorld(t)
	tb, _ := w.table("Deals")
	admin := Actor{UserID: uuid.New(), IsAdmin: true}
	mine, err := CreateView(w.ctx, tb.Id, ViewInput{Name: "My open deals", Personal: true}, w.member)
	if err != nil {
		t.Fatalf("a member makes a view of their own: %v", err)
	}
	if mine.Owner == nil || *mine.Owner != w.member.UserID {
		t.Fatalf("the member's view is %v's", mine.Owner)
	}
	if got := w.viewsSeenBy(tb.Id, w.member); !slices.Contains(got, "My open deals") {
		t.Errorf("the member's views are %q; want their own among them", got)
	}
	for _, who := range []Actor{w.me, admin} {
		if got := w.viewsSeenBy(tb.Id, who); slices.Contains(got, "My open deals") {
			t.Errorf("%v sees the member's own view among %q", who, got)
		}
		if err := UpdateView(w.ctx, tb.Id, mine.Id, ViewInput{Name: "Taken"}, who); !errors.Is(err, errViewNotFound) {
			t.Errorf("%v changing the member's own view: %v; want it not found", who, err)
		}
		if err := DeleteView(w.ctx, tb.Id, mine.Id, who); !errors.Is(err, errViewNotFound) {
			t.Errorf("%v deleting the member's own view: %v; want it not found", who, err)
		}
	}
	if err := LockView(w.ctx, tb.Id, mine.Id, true, w.member); err == nil {
		t.Error("a view of one's own was locked")
	}
	if err := UpdateView(w.ctx, tb.Id, mine.Id, ViewInput{Name: "Open deals"}, w.member); err != nil {
		t.Errorf("the member renames their own view: %v", err)
	}
	if err := DeleteView(w.ctx, tb.Id, mine.Id, w.member); err != nil {
		t.Errorf("the member deletes their own view: %v", err)
	}

	// Someone who can't read a table makes no view of it.
	private, err := CreateTable(w.ctx, TableInput{Name: "Salaries", Visibility: "private"}, w.me)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CreateView(w.ctx, private.Id, ViewInput{Name: "Peek", Personal: true}, w.member); err == nil {
		t.Error("a member made a view of a private table they can't read")
	}
}

func TestASharedViewIsItsManagersToChange(t *testing.T) {
	w := newRelationWorld(t)
	tb, _ := w.table("Deals")
	admin := Actor{UserID: uuid.New(), IsAdmin: true}
	if _, err := CreateView(w.ctx, tb.Id, ViewInput{Name: "Team pipeline"}, w.member); !errors.Is(err, errForbidden) {
		t.Errorf("a member making a shared view: %v; want it refused", err)
	}
	team, err := CreateView(w.ctx, tb.Id, ViewInput{Name: "Team pipeline", Type: model.ViewChart}, w.me)
	if err != nil {
		t.Fatalf("the table's creator makes a shared chart: %v", err)
	}
	if got := w.viewsSeenBy(tb.Id, w.member); !slices.Contains(got, "Team pipeline") {
		t.Errorf("the member's views are %q; want the shared one among them", got)
	}
	if err := UpdateView(w.ctx, tb.Id, team.Id, ViewInput{Name: "Mine now"}, w.member); !errors.Is(err, errForbidden) {
		t.Errorf("a member changing a shared view: %v; want it refused", err)
	}
	if err := DeleteView(w.ctx, tb.Id, team.Id, w.member); !errors.Is(err, errForbidden) {
		t.Errorf("a member deleting a shared view: %v; want it refused", err)
	}
	if err := UpdateView(w.ctx, tb.Id, team.Id, ViewInput{Name: "Pipeline", Type: model.ViewChart}, admin); err != nil {
		t.Errorf("an admin renames a shared view: %v", err)
	}
	if err := DeleteView(w.ctx, tb.Id, team.Id, w.me); err != nil {
		t.Errorf("the table's creator deletes a shared view: %v", err)
	}
}

func TestALockedViewTakesNoChangeUntilUnlocked(t *testing.T) {
	w := newRelationWorld(t)
	tb, _ := w.table("Deals")
	team, err := CreateView(w.ctx, tb.Id, ViewInput{Name: "Board"}, w.me)
	if err != nil {
		t.Fatal(err)
	}
	if err := LockView(w.ctx, tb.Id, team.Id, true, w.member); !errors.Is(err, errForbidden) {
		t.Errorf("a member locking a shared view: %v; want it refused", err)
	}
	if err := LockView(w.ctx, tb.Id, team.Id, true, w.me); err != nil {
		t.Fatalf("the table's creator locks a shared view: %v", err)
	}
	if err := UpdateView(w.ctx, tb.Id, team.Id, ViewInput{Name: "Changed"}, w.me); !errors.Is(err, errViewLocked) {
		t.Errorf("changing a locked view: %v; want it refused as locked", err)
	}
	if err := DeleteView(w.ctx, tb.Id, team.Id, w.me); !errors.Is(err, errViewLocked) {
		t.Errorf("deleting a locked view: %v; want it refused as locked", err)
	}
	b, err := GetBundle(w.ctx, tb.Id, w.member)
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.IndexFunc(b.Views, func(v *model.View) bool { return v.Id == team.Id }); i < 0 || !b.Views[i].Locked {
		t.Error("a member's bundle doesn't say the view is locked")
	}
	if err := LockView(w.ctx, tb.Id, team.Id, false, w.member); !errors.Is(err, errForbidden) {
		t.Errorf("a member unlocking a shared view: %v; want it refused", err)
	}
	if err := LockView(w.ctx, tb.Id, team.Id, false, w.me); err != nil {
		t.Fatalf("the table's creator unlocks the view: %v", err)
	}
	if err := UpdateView(w.ctx, tb.Id, team.Id, ViewInput{Name: "Changed"}, w.me); err != nil {
		t.Errorf("changing the view once unlocked: %v", err)
	}
}

// A lock that lands between viewToChange's read and the write still holds:
// the store writes only an unlocked view, and the refusal says it's locked.
// The lock lands in the store first here, as it would in between.
func TestALockThatLandsMidChangeHolds(t *testing.T) {
	w := newRelationWorld(t)
	tb, _ := w.table("Deals")
	team, err := CreateView(w.ctx, tb.Id, ViewInput{Name: "Board"}, w.me)
	if err != nil {
		t.Fatal(err)
	}
	if err := model.LockView(w.ctx, tb.Id, team.Id, true); err != nil {
		t.Fatal(err)
	}
	raced := *team
	raced.Name = "Raced"
	if err := model.UpdateView(w.ctx, &raced); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("the store changed a locked view: %v", err)
	}
	if err := model.DeleteView(w.ctx, tb.Id, team.Id); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("the store deleted a locked view: %v", err)
	}
	if got := w.viewsSeenBy(tb.Id, w.member); !slices.Contains(got, "Board") {
		t.Errorf("the views are %q; want the locked one as it was", got)
	}
	if err := viewWriteErr(w.ctx, tb.Id, team.Id, sql.ErrNoRows); !errors.Is(err, errViewLocked) {
		t.Errorf("a change the lock stopped answers %v; want it refused as locked", err)
	}

	// A delete that lands in between: the change finds no view.
	if err := model.LockView(w.ctx, tb.Id, team.Id, false); err != nil {
		t.Fatal(err)
	}
	if err := DeleteView(w.ctx, tb.Id, team.Id, w.me); err != nil {
		t.Fatal(err)
	}
	if err := viewWriteErr(w.ctx, tb.Id, team.Id, sql.ErrNoRows); !errors.Is(err, errViewNotFound) {
		t.Errorf("a change a delete beat answers %v; want the view not found", err)
	}
}

// A view's id names a view of the table it's asked of, and nothing else: one
// table's view named on another isn't found, for the manager of both too. And
// a view's config, which every reader's bundle carries, is bounded.
func TestAViewIsFoundOnlyOnItsOwnTable(t *testing.T) {
	w := newRelationWorld(t)
	deals, _ := w.table("Deals")
	vendors, _ := w.table("Vendors")
	pipeline, err := CreateView(w.ctx, deals.Id, ViewInput{Name: "Pipeline"}, w.me)
	if err != nil {
		t.Fatal(err)
	}
	if err := UpdateView(w.ctx, vendors.Id, pipeline.Id, ViewInput{Name: "Moved"}, w.me); !errors.Is(err, errViewNotFound) {
		t.Errorf("changing a view through another table: %v; want it not found", err)
	}
	if err := LockView(w.ctx, vendors.Id, pipeline.Id, true, w.me); !errors.Is(err, errViewNotFound) {
		t.Errorf("locking a view through another table: %v; want it not found", err)
	}
	if err := DeleteView(w.ctx, vendors.Id, pipeline.Id, w.me); !errors.Is(err, errViewNotFound) {
		t.Errorf("deleting a view through another table: %v; want it not found", err)
	}
	big := map[string]interface{}{"rules": strings.Repeat("x", maxViewConfigBytes)}
	if _, err := CreateView(w.ctx, deals.Id, ViewInput{Name: "Big", Config: big, Personal: true}, w.member); err == nil {
		t.Error("a view with a config past the bound was made")
	}
}

// B4 (migration 927): a view may be a gallery, a list, a timeline or a Gantt,
// saved and listed as any other, and the store refuses a layout the app
// doesn't draw even when the business check is passed by.
func TestAViewTakesB4sLayouts(t *testing.T) {
	w := newRelationWorld(t)
	tb, _ := w.table("Launch plan")
	for _, layout := range []string{model.ViewGallery, model.ViewList, model.ViewTimeline, model.ViewGantt} {
		v, err := CreateView(w.ctx, tb.Id, ViewInput{Name: "As " + layout, Type: layout}, w.me)
		if err != nil {
			t.Fatalf("a %s view: %v", layout, err)
		}
		if v.Type != layout {
			t.Errorf("a %s view saved as %q", layout, v.Type)
		}
	}
	got := w.viewsSeenBy(tb.Id, w.member)
	for _, name := range []string{"As gallery", "As list", "As timeline", "As gantt"} {
		if !slices.Contains(got, name) {
			t.Errorf("the member's views are %q; want %q among them", got, name)
		}
	}
	if _, err := CreateView(w.ctx, tb.Id, ViewInput{Name: "Form", Type: "form"}, w.me); err == nil {
		t.Error("a form view was taken; want it refused until forms land (B5)")
	}
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(w.ctx, `UPDATE data_table_views SET type = 'form' WHERE table_id = $1`, tb.Id); err == nil {
		t.Error("the store took a form view; want its check to refuse it")
	}
}
