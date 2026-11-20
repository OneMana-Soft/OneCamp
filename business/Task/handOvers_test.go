package business

import (
	"context"
	"reflect"
	"testing"
)

// A bulk run's hand-overs reach the AI index in one write when the run is
// done, not one per task; a task handed over twice in it keeps the last. A
// hand-over outside a run writes at once.
func TestABulkRunsHandOversAreOneAIWrite(t *testing.T) {
	var writes []map[string]string
	was := aiAssigneeWrite
	aiAssigneeWrite = func(_ context.Context, to map[string]string) error {
		c := make(map[string]string, len(to))
		for k, v := range to {
			c[k] = v
		}
		writes = append(writes, c)
		return nil
	}
	t.Cleanup(func() { aiAssigneeWrite = was })

	ctx, write := HandOvers(context.Background())
	aiAssignee(ctx, "t1", "u1")
	aiAssignee(ctx, "t2", "")
	aiAssignee(ctx, "t3", "u2")
	aiAssignee(ctx, "t1", "u3")
	if len(writes) != 0 {
		t.Fatalf("a bulk run of 4 hand-overs wrote %d times before it was done, want 0", len(writes))
	}
	write(ctx)
	want := map[string]string{"t1": "u3", "t2": "", "t3": "u2"}
	if len(writes) != 1 || !reflect.DeepEqual(writes[0], want) {
		t.Fatalf("a bulk run of 4 hand-overs wrote %v, want one write of %v", writes, want)
	}
	write(ctx)
	if len(writes) != 1 {
		t.Fatalf("writing the run again wrote %d times in all, want its one", len(writes))
	}

	writes = nil
	_, none := HandOvers(context.Background())
	none(context.Background())
	if len(writes) != 0 {
		t.Fatalf("a run with no hand-overs wrote %v", writes)
	}

	aiAssignee(context.Background(), "t4", "u4")
	if len(writes) != 1 || !reflect.DeepEqual(writes[0], map[string]string{"t4": "u4"}) {
		t.Fatalf("one hand-over alone wrote %v, want one write of t4", writes)
	}
}
