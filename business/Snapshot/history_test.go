package business

// Docs H1: every change attributed (a version when the author changes), a
// year of history by the workspace's setting, and paging through it.

import (
	"context"
	"testing"
	"time"

	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	snapshotModels "github.com/akashc777/OneCamp/models/postgres/DocSnapshot"
)

func TestANewAuthorIsSomeoneTheLedgerDoesNotHold(t *testing.T) {
	ana, ben := Contributor{UserUUID: "ana"}, Contributor{UserUUID: "ben"}
	captain := Contributor{AgentUUID: "captain", AskerUUID: "ana"}
	for _, c := range []struct {
		name       string
		since, who []Contributor
		want       bool
	}{
		{"nobody yet", nil, []Contributor{ben}, false},
		{"the same author", []Contributor{ana}, []Contributor{ana}, false},
		{"someone else", []Contributor{ana}, []Contributor{ben}, true},
		{"one more beside", []Contributor{ana}, []Contributor{ana, ben}, true},
		{"an agent its asker started", []Contributor{ana}, []Contributor{captain}, true},
		{"a save holding nobody", []Contributor{ana}, []Contributor{{}}, false},
	} {
		if got := newAuthor(c.since, c.who); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// Ana writes, then Ben: the doc as Ana left it is a version credited to Ana
// alone, and Ben's edits wait for the next.
func TestAChangeOfAuthorKeepsTheVersionBeforeForThoseBefore(t *testing.T) {
	w := newWorld(t)
	save := func(before, body string, people ...string) {
		onDocSave(context.Background(), docBusiness.Change{DocUUID: docID, Before: before, Body: body, Contributors: people, SavedAt: w.clock})
	}
	save("", "<p>a</p>", "ana") // the first version
	w.clock = w.clock.Add(time.Minute)
	save("", "<p>ab</p>", "ana")
	w.clock = w.clock.Add(time.Minute)
	save("<p>ab</p>", "<p>abc</p>", "ben")

	all, _ := w.store.list(context.Background(), docID, time.Time{}, 10)
	if len(all) != 2 {
		t.Fatalf("%d versions, want 2", len(all))
	}
	handover := all[0]
	if w.store.bodies[handover.ID] != "<p>ab</p>" || len(handover.Contributors) != 1 || handover.Contributors[0].UserUUID != "ana" {
		t.Errorf("the version before Ben: %q credited %+v", w.store.bodies[handover.ID], handover.Contributors)
	}
	if held := ledger.Members(context.Background(), docKind, docID); len(held) != 1 || held[0].UserUUID != "ben" {
		t.Errorf("the ledger after: %+v, want Ben alone", held)
	}
	// Ben again: no new author, no new version.
	w.clock = w.clock.Add(time.Minute)
	save("<p>abc</p>", "<p>abcd</p>", "ben")
	if again, _ := w.store.list(context.Background(), docID, time.Time{}, 10); len(again) != 2 {
		t.Errorf("%d versions after Ben's second save, want 2", len(again))
	}
}

func TestHistoryIsKeptForTheWorkspacesSettingNeverBelowTheServersFloor(t *testing.T) {
	prevDays := historyDays
	t.Cleanup(func() { historyDays = prevDays; appliedDays.Store(0) })
	day := 24 * time.Hour
	setting := 365
	historyDays = func(context.Context) int { return setting }
	appliedDays.Store(0)
	ctx := context.Background()

	if got := scheduledDocPolicy(ctx).Retention; got != 365*day {
		t.Errorf("a year by default: %v", got)
	}
	// Lowered: the docs' own prunes keep the year until the scheduled prune
	// takes the new setting.
	setting = 30
	if got := docPolicy(ctx).Retention; got != 365*day {
		t.Errorf("lowered, before the scheduled prune: %v, want a year", got)
	}
	if got := scheduledDocPolicy(ctx).Retention; got != 30*day {
		t.Errorf("the scheduled prune: %v, want 30 days", got)
	}
	if got := docPolicy(ctx).Retention; got != 30*day {
		t.Errorf("after the scheduled prune: %v, want 30 days", got)
	}
	// Raised: kept at once.
	setting = 400
	if got := docPolicy(ctx).Retention; got != 400*day {
		t.Errorf("raised: %v, want 400 days", got)
	}
	// The server's floor.
	t.Setenv("DOC_SNAPSHOT_RETENTION_DAYS", "500")
	if got := scheduledDocPolicy(ctx).Retention; got != 500*day {
		t.Errorf("under the server's floor: %v, want 500 days", got)
	}
	// Before any scheduled prune, a doc's own prune removes nothing for age
	// short of ten years.
	appliedDays.Store(0)
	t.Setenv("DOC_SNAPSHOT_RETENTION_DAYS", "")
	setting = 30
	if got := docPolicy(ctx).Retention; got != longestDays*day {
		t.Errorf("before any scheduled prune: %v", got)
	}
}

func TestAYearOfVersionsIsReachedAPageAtATime(t *testing.T) {
	w := newWorld(t)
	start := w.clock
	for i := 0; i < ListPage+30; i++ {
		row := &snapshotModels.DocSnapshot{DocUUID: docID, Reason: snapshotModels.ReasonIdle, Kind: string(KindDoc), CreatedAt: start.Add(time.Duration(i) * time.Hour)}
		if _, err := w.store.put(context.Background(), row, "<p>v</p>"); err != nil {
			t.Fatal(err)
		}
	}
	first, err := docVersions{}.List(context.Background(), docID, time.Time{})
	if err != nil || len(first) != ListPage {
		t.Fatalf("first page: %d %v", len(first), err)
	}
	rest, err := docVersions{}.List(context.Background(), docID, first[len(first)-1].CreatedAt)
	if err != nil || len(rest) != 30 {
		t.Fatalf("second page: %d %v", len(rest), err)
	}
	seen := map[string]bool{}
	for _, v := range append(first, rest...) {
		if seen[v.ID] {
			t.Fatalf("version %s on two pages", v.ID)
		}
		seen[v.ID] = true
	}
	if !rest[0].CreatedAt.Before(first[len(first)-1].CreatedAt) {
		t.Errorf("the second page isn't older than the first")
	}
}
