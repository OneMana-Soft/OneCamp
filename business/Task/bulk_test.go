package business

import (
	"context"
	"errors"
	"testing"
	"time"

	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// PR2: one change to many tasks is one read, one check of what the person may
// do with all of them, and, for a priority or a tag, one write of each store
// for the lot, never a round per task. A task they can't see is "not found";
// one they may not change is refused in words; the rest change.
func TestABulkChangeReadsChecksAndWritesOnceForTheLot(t *testing.T) {
	ids := []uuid.UUID{uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()}
	outside := ids[4]
	live := time.Time{}
	gone := time.Now()
	label := "copy"
	tasks := []*dgraphStruct.DgraphTask{
		{Uid: "0x1", Uuid: ids[0].String(), Priority: "low", Label: &label, DeletedAt: &live},
		{Uid: "0x2", Uuid: ids[1].String(), Priority: "medium", DeletedAt: &live},
		{Uid: "0x3", Uuid: ids[2].String(), Priority: "low", DeletedAt: &gone},
		// ids[3] isn't read: the reader can't see it.
		// ids[4] is read, but its project isn't theirs: as if it weren't there.
		{Uid: "0x5", Uuid: ids[4].String(), Priority: "low", DeletedAt: &live},
	}
	old := bulkSeams
	t.Cleanup(func() { bulkSeams = old })
	reads, pgWrites, graphWrites := 0, 0, 0
	var wrote []*dgraphStruct.DgraphTask
	bulkSeams.read = func(_ context.Context, asked []string, _ string) ([]*dgraphStruct.DgraphTask, error) {
		reads++
		if reads == 1 && len(asked) != 5 {
			t.Fatalf("asked for %d tasks", len(asked))
		}
		return tasks, nil
	}
	bulkSeams.check = func(_ context.Context, _ *model.UserInfo, ts []*dgraphStruct.DgraphTask, need workAccess.Need) map[string]error {
		return map[string]error{ids[1].String(): errors.New("In this project you can change only the tasks assigned to you.")}
	}
	bulkSeams.touch = func(_ context.Context, ts []uuid.UUID, _ time.Time) error {
		pgWrites++
		if len(ts) != 1 {
			t.Fatalf("touched %d", len(ts))
		}
		return nil
	}
	bulkSeams.write = func(_ context.Context, ts []*dgraphStruct.DgraphTask) error {
		graphWrites++
		wrote = ts
		return nil
	}
	bulkSeams.index = func([]*dgraphStruct.DgraphTask) {}
	bulkSeams.visible = func(t *dgraphStruct.DgraphTask, _ string) bool { return t.Uuid != outside.String() }
	who := &model.UserInfo{UserDgraphInfo: dgraphStruct.DgraphUser{Uid: "0x9", Uuid: uuid.NewString()}}
	res, err := Bulk(context.Background(), who, ids, BulkChange{Op: BulkPriority, Value: "high"})
	if err != nil {
		t.Fatal(err)
	}
	if reads != 1 || pgWrites != 1 || graphWrites != 1 {
		t.Fatalf("reads %d, Postgres writes %d, graph writes %d: once each", reads, pgWrites, graphWrites)
	}
	if len(res.Changed) != 1 || res.Changed[0] != ids[0].String() {
		t.Fatalf("changed: %v", res.Changed)
	}
	why := map[string]string{}
	for _, r := range res.Refused {
		why[r.TaskUUID] = r.Message
	}
	if why[ids[1].String()] != "In this project you can change only the tasks assigned to you." || why[ids[2].String()] != "That task was deleted." || why[ids[3].String()] != "That task doesn't exist or you can't see it." || why[ids[4].String()] != "That task doesn't exist or you can't see it." {
		t.Fatalf("refusals: %v", why)
	}
	if len(wrote) != 1 || wrote[0].Uid != "0x1" || wrote[0].Priority != "high" || len(wrote[0].Activity) != 1 || wrote[0].Activity[0].PrevState != "low" {
		t.Fatalf("the graph write: %+v", wrote)
	}

	// Already so: nothing to write, and it counts as unchanged.
	tasks = tasks[:1]
	tasks[0].Priority = "high"
	pgWrites, graphWrites = 0, 0
	res, _ = Bulk(context.Background(), who, ids[:1], BulkChange{Op: BulkPriority, Value: "high"})
	if pgWrites != 0 || graphWrites != 0 || len(res.Unchanged) != 1 {
		t.Fatalf("already urgent: %d %d %+v", pgWrites, graphWrites, res)
	}
}

func TestABulkChangeHasLimits(t *testing.T) {
	who := &model.UserInfo{}
	many := make([]uuid.UUID, BulkMost+1)
	for i := range many {
		many[i] = uuid.New()
	}
	var be *BulkError
	if _, err := Bulk(context.Background(), who, many, BulkChange{Op: BulkPriority, Value: "low"}); !errors.As(err, &be) {
		t.Fatalf("over the cap: %v", err)
	}
	if _, err := Bulk(context.Background(), who, many[:1], BulkChange{Op: BulkPriority, Value: "loud"}); !errors.As(err, &be) {
		t.Fatalf("a priority that isn't one: %v", err)
	}
	if _, err := Bulk(context.Background(), who, many[:1], BulkChange{Op: "paint"}); !errors.As(err, &be) {
		t.Fatalf("a change there isn't: %v", err)
	}
}

// Tags: added to or taken from each task's own, normalised, and written once.
func TestABulkTagChangeKeepsEachTasksOwnTags(t *testing.T) {
	a, b := "copy, launch", ""
	live := time.Time{}
	ids := []uuid.UUID{uuid.New(), uuid.New()}
	old := bulkSeams
	t.Cleanup(func() { bulkSeams = old })
	bulkSeams.read = func(context.Context, []string, string) ([]*dgraphStruct.DgraphTask, error) {
		return []*dgraphStruct.DgraphTask{{Uid: "0x1", Uuid: ids[0].String(), Label: &a, DeletedAt: &live}, {Uid: "0x2", Uuid: ids[1].String(), Label: &b, DeletedAt: &live}}, nil
	}
	bulkSeams.check = func(context.Context, *model.UserInfo, []*dgraphStruct.DgraphTask, workAccess.Need) map[string]error {
		return nil
	}
	bulkSeams.touch = func(context.Context, []uuid.UUID, time.Time) error { return nil }
	var wrote []*dgraphStruct.DgraphTask
	bulkSeams.write = func(_ context.Context, ts []*dgraphStruct.DgraphTask) error { wrote = ts; return nil }
	bulkSeams.index = func([]*dgraphStruct.DgraphTask) {}
	bulkSeams.githubSync = func(context.Context, uuid.UUID, string, map[string]interface{}) {}
	bulkSeams.visible = func(*dgraphStruct.DgraphTask, string) bool { return true }
	who := &model.UserInfo{UserDgraphInfo: dgraphStruct.DgraphUser{Uid: "0x9"}}
	res, err := Bulk(context.Background(), who, ids, BulkChange{Op: BulkTagRemove, Value: "Launch"})
	if err != nil || len(res.Changed) != 1 || len(res.Unchanged) != 1 || len(wrote) != 1 || *wrote[0].Label != "copy" {
		t.Fatalf("remove: %+v %v %+v", res, err, wrote)
	}
	res, _ = Bulk(context.Background(), who, ids, BulkChange{Op: BulkTagAdd, Value: "review"})
	if len(res.Changed) != 2 || len(wrote) != 2 || *wrote[1].Label != "review" {
		t.Fatalf("add: %+v %+v", res, wrote)
	}
}

// GitHub's bulk link and unlink ask once for all their tasks (#13).
func TestChangeRefusalsAskOnceForAll(t *testing.T) {
	ids := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	old := bulkSeams
	t.Cleanup(func() { bulkSeams = old })
	reads, checks := 0, 0
	bulkSeams.read = func(context.Context, []string, string) ([]*dgraphStruct.DgraphTask, error) {
		reads++
		return []*dgraphStruct.DgraphTask{{Uuid: ids[0].String()}, {Uuid: ids[1].String()}}, nil
	}
	bulkSeams.visible = func(*dgraphStruct.DgraphTask, string) bool { return true }
	bulkSeams.check = func(context.Context, *model.UserInfo, []*dgraphStruct.DgraphTask, workAccess.Need) map[string]error {
		checks++
		return map[string]error{ids[1].String(): errors.New("Only this project's admins can change its tasks.")}
	}
	got, err := ChangeRefusals(context.Background(), &model.UserInfo{}, ids, workAccess.NeedEditTask)
	if err != nil || reads != 1 || checks != 1 {
		t.Fatalf("%d reads, %d checks: %v", reads, checks, err)
	}
	if len(got) != 2 || got[ids[1]] != "Only this project's admins can change its tasks." || got[ids[2]] != "That task doesn't exist or you can't see it." {
		t.Fatalf("%v", got)
	}
}
