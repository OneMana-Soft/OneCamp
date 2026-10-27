package business

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	domain "github.com/akashc777/OneCamp/domain/Attachment"
	"github.com/google/uuid"
)

// deletedWorld is a test's stores for the repair: each kind's items deleted
// in Postgres (sorted by id, as the query reads them), those the graph holds
// deleted, what taking each one's files off answers, and what the repair kept
// and recorded.
type deletedWorld struct {
	items    map[string][]uuid.UUID // by the kind's table
	gone     map[uuid.UUID]bool
	taken    map[uuid.UUID]int
	failing  map[uuid.UUID]bool
	readFail bool
	archived []uuid.UUID
	at       []time.Time
	progress deletedFilesProgress
	saves    int
	recorded []DeletedFilesReport
}

var repairClock = time.Date(2026, 10, 26, 7, 30, 0, 0, time.UTC)

func (w *deletedWorld) seams(t *testing.T) {
	t.Helper()
	prev := []any{deletedItems, deletedInGraph, archiveFilesOf, loadProgress, saveProgress, recordRepair, repairNow}
	t.Cleanup(func() {
		deletedItems = prev[0].(func(context.Context, domain.Holder, uuid.UUID, int) ([]uuid.UUID, error))
		deletedInGraph = prev[1].(func(context.Context, domain.Holder, []uuid.UUID) (map[uuid.UUID]bool, error))
		archiveFilesOf = prev[2].(func(context.Context, domain.Holder, uuid.UUID, time.Time) (int, error))
		loadProgress = prev[3].(func(string) (deletedFilesProgress, error))
		saveProgress = prev[4].(func(deletedFilesProgress) error)
		recordRepair = prev[5].(func(context.Context, DeletedFilesReport))
		repairNow = prev[6].(func() time.Time)
	})
	kindOf := map[domain.Holder]string{PostFiles: "posts", ChatFiles: "chats", CommentFiles: "comments"}
	deletedItems = func(_ context.Context, h domain.Holder, after uuid.UUID, n int) ([]uuid.UUID, error) {
		var out []uuid.UUID
		for _, id := range w.items[kindOf[h]] {
			if id.String() > after.String() && len(out) < n {
				out = append(out, id)
			}
		}
		return out, nil
	}
	deletedInGraph = func(_ context.Context, _ domain.Holder, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
		if w.readFail {
			return nil, errDown
		}
		out := map[uuid.UUID]bool{}
		for _, id := range ids {
			out[id] = w.gone[id]
		}
		return out, nil
	}
	archiveFilesOf = func(_ context.Context, _ domain.Holder, id uuid.UUID, at time.Time) (int, error) {
		if w.failing[id] {
			return 0, errDown
		}
		w.archived, w.at = append(w.archived, id), append(w.at, at)
		return w.taken[id], nil
	}
	loadProgress = func(string) (deletedFilesProgress, error) { return w.progress, nil }
	saveProgress = func(p deletedFilesProgress) error {
		w.progress = p
		w.progress.After = map[string]uuid.UUID{}
		for k, v := range p.After {
			w.progress.After[k] = v
		}
		w.saves++
		return nil
	}
	recordRepair = func(_ context.Context, r DeletedFilesReport) { w.recorded = append(w.recorded, r) }
	repairNow = func() time.Time { return repairClock }
}

// sortedIDs is n new ids in the order Postgres reads them.
func sortedIDs(n int) []uuid.UUID {
	ids := make([]uuid.UUID, n)
	for i := range ids {
		ids[i] = uuid.New()
	}
	slices.SortFunc(ids, func(a, b uuid.UUID) int {
		switch {
		case a.String() < b.String():
			return -1
		case a.String() > b.String():
			return 1
		}
		return 0
	})
	return ids
}

// Every kind's items deleted in both stores have their files taken off at the
// run's time, a page at a time, each page's place kept; an item the graph
// still shows is left as it is; the audit log gets the counts once.
func TestTheRepairTakesTheFilesOfItemsDeletedInBothStores(t *testing.T) {
	posts, chats, comments := sortedIDs(deletedFilesRepairPage+5), sortedIDs(2), sortedIDs(1)
	w := &deletedWorld{
		items: map[string][]uuid.UUID{"posts": posts, "chats": chats, "comments": comments},
		gone:  map[uuid.UUID]bool{}, taken: map[uuid.UUID]int{},
	}
	for _, id := range slices.Concat(posts, chats, comments) {
		w.gone[id], w.taken[id] = true, 1
	}
	shown := chats[1] // deleted in Postgres alone
	w.gone[shown] = false
	w.taken[comments[0]] = 0 // its files all held by items still there
	w.seams(t)

	rep, err := RepairDeletedFiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := DeletedFilesReport{Posts: len(posts), Chats: 1, Comments: 1, Files: len(posts) + 1}
	if rep != want || len(w.recorded) != 1 || w.recorded[0] != want {
		t.Fatalf("report %+v, recorded %+v; want %+v once", rep, w.recorded, want)
	}
	if slices.Contains(w.archived, shown) || len(w.archived) != len(posts)+2 {
		t.Fatalf("took files off %d items (the one the graph shows: %v)", len(w.archived), slices.Contains(w.archived, shown))
	}
	for _, at := range w.at {
		if !at.Equal(repairClock) {
			t.Fatalf("files taken at %v, want the run's time %v", at, repairClock)
		}
	}
	if w.progress.After["posts"] != posts[len(posts)-1] || w.progress.After["chats"] != chats[1] || w.saves != 4 {
		t.Fatalf("kept %+v after %d saves; want each kind's last item, a save a page (2 of posts, 1 each of the rest)", w.progress.After, w.saves)
	}
}

// A run goes on from where the last one got to, with what it had done.
func TestTheRepairGoesOnFromWhereItGotTo(t *testing.T) {
	posts := sortedIDs(4)
	w := &deletedWorld{items: map[string][]uuid.UUID{"posts": posts}, gone: map[uuid.UUID]bool{}, taken: map[uuid.UUID]int{}}
	for _, id := range posts {
		w.gone[id], w.taken[id] = true, 2
	}
	w.progress = deletedFilesProgress{After: map[string]uuid.UUID{"posts": posts[1]}, Report: DeletedFilesReport{Posts: 2, Files: 4}}
	w.seams(t)
	rep, err := RepairDeletedFiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(w.archived, posts[2:]) || rep != (DeletedFilesReport{Posts: 4, Files: 8}) {
		t.Fatalf("took %v, did %+v; want the last two and all four counted", w.archived, rep)
	}
}

// An item that fails stops the run there, every item before it done and kept;
// the next runs try it again, and on the fifth failure in a row the repair
// passes it, names it in the log, and goes on.
func TestAFailingItemStopsTheRunUntilItHasFailedFiveRuns(t *testing.T) {
	posts := sortedIDs(3)
	bad := posts[1]
	w := &deletedWorld{items: map[string][]uuid.UUID{"posts": posts}, gone: map[uuid.UUID]bool{}, taken: map[uuid.UUID]int{},
		failing: map[uuid.UUID]bool{bad: true}}
	for _, id := range posts {
		w.gone[id], w.taken[id] = true, 1
	}
	w.seams(t)
	for run := 1; run < deletedFilesTries; run++ {
		if _, err := RepairDeletedFiles(context.Background()); !errors.Is(err, errDown) {
			t.Fatalf("run %d: %v, want it to stop at the failing item", run, err)
		}
		if w.progress.After["posts"] != posts[0] || w.progress.Stuck != bad || w.progress.Tries != run {
			t.Fatalf("run %d kept %+v", run, w.progress)
		}
	}
	rep, err := RepairDeletedFiles(context.Background())
	if err != nil || rep != (DeletedFilesReport{Posts: 2, Files: 2, Passed: 1}) {
		t.Fatalf("the fifth run: %+v, %v; want the bad item passed and the rest done", rep, err)
	}
	if !slices.Equal(w.archived, []uuid.UUID{posts[0], posts[2]}) || len(w.recorded) != 1 {
		t.Fatalf("took %v, recorded %+v; want each good item once and one record", w.archived, w.recorded)
	}
}

// A store that can't be read stops the run with no item counted as failing,
// and nothing is recorded; nor is anything when nothing was deleted.
func TestAFailedReadOrNothingDeletedRecordsNothing(t *testing.T) {
	posts := sortedIDs(1)
	w := &deletedWorld{items: map[string][]uuid.UUID{"posts": posts}, gone: map[uuid.UUID]bool{posts[0]: true}, readFail: true}
	w.seams(t)
	if _, err := RepairDeletedFiles(context.Background()); !errors.Is(err, errDown) || w.progress.Tries != 0 || len(w.recorded) != 0 {
		t.Fatalf("a failed read: %v, kept %+v, recorded %+v", err, w.progress, w.recorded)
	}
	w = &deletedWorld{items: map[string][]uuid.UUID{}}
	w.seams(t)
	if rep, err := RepairDeletedFiles(context.Background()); err != nil || rep != (DeletedFilesReport{}) || len(w.recorded) != 0 {
		t.Fatalf("nothing deleted: %+v, %v, recorded %+v", rep, err, w.recorded)
	}
}
