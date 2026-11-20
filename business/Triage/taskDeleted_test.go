package business

import (
	"context"
	"reflect"
	"testing"

	workFeed "github.com/akashc777/OneCamp/business/WorkFeed"
	"github.com/google/uuid"
)

// A deleted task's entry ends that task's wait, and nothing else does
// (CR-288 F2; tests/integration TestATaskDeletedWhileItWaitsLeavesTriage runs
// it through the Outbox against Postgres).
func TestADeletedTasksEntryEndsItsWait(t *testing.T) {
	saved := endWait
	t.Cleanup(func() { endWait = saved })
	var ended []uuid.UUID
	endWait = func(_ context.Context, id uuid.UUID) error {
		ended = append(ended, id)
		return nil
	}
	id := uuid.New()
	for _, e := range []workFeed.Entry{
		{Kind: workFeed.KindDeleted, ResourceKind: "task", ResourceUUID: id.String()},
		{Kind: workFeed.KindDeleted, ResourceKind: "task", ResourceUUID: "not a task"},
		{Kind: workFeed.KindDeleted, ResourceKind: "project", ResourceUUID: uuid.NewString()},
	} {
		if err := endWaitOfDeleted(context.Background(), "ev-1", e); err != nil {
			t.Fatalf("%+v: %v", e, err)
		}
	}
	if len(ended) != 1 || ended[0] != id {
		t.Fatalf("ended %v, want only %v", ended, id)
	}
	if want := []string{workFeed.KindDeleted}; !reflect.DeepEqual(taskDeletedSink.Kinds, want) {
		t.Fatalf("the sink is for %v, want %v", taskDeletedSink.Kinds, want)
	}
}
