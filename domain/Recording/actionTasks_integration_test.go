//go:build integration

package domain

import (
	"context"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// V-1018 against the graph: an event's call reads each action item's task
// only while that task is live. A task deleted since leaves its item with
// none (Make task is offered again); a live one stays; one made again after
// the delete is the item's.
// Run: go test -tags=integration ./domain/Recording/ -run TestAnEventsCallReadsOnlyLiveTasks -v
func TestAnEventsCallReadsOnlyLiveTasks(t *testing.T) {
	ctx := context.Background()
	dg := integration.SetupDgraph(t)
	event := uuid.New()
	gone, kept, again := uuid.NewString(), uuid.NewString(), uuid.NewString()
	if again < gone {
		gone, again = again, gone // the deleted one sorts first, so taking the least is not enough
	}
	ended := time.Date(2026, 11, 12, 16, 30, 0, 0, time.UTC).Format(time.RFC3339)
	dg.Mutate(t, []map[string]any{
		{"uid": "_:gone", "dgraph.type": "Task", "task_uuid": gone, "task_deleted_at": "2026-11-13T09:00:00Z"},
		{"uid": "_:kept", "dgraph.type": "Task", "task_uuid": kept, "task_deleted_at": "0001-01-01T00:00:00Z"},
		{"uid": "_:again", "dgraph.type": "Task", "task_uuid": again},
		{"uid": "_:call", "dgraph.type": "Recording", "recording_egress_id": "call:v1018", "recording_event": event.String(),
			"recording_stared_at": "2026-11-12T16:01:00Z", "recording_ended_at": ended,
			"recording_action_items": `[{"text":"Send the deck"},{"text":"Book the room"},{"text":"Draft the FAQ"}]`,
			"recording_action_tasks": []string{"0:" + gone, "1:" + kept, "2:" + gone, "2:" + again}},
	})
	calls, err := EventCallArtefacts(ctx, []uuid.UUID{event})
	if err != nil || len(calls) != 1 || len(calls[0].ActionItems) != 3 {
		t.Fatalf("EventCallArtefacts = %+v, %v; want the one call with three items", calls, err)
	}
	items := calls[0].ActionItems
	if items[0].Task != "" || items[1].Task != kept || items[2].Task != again {
		t.Errorf("tasks = %q, %q, %q; want none (deleted), %s, %s", items[0].Task, items[1].Task, items[2].Task, kept, again)
	}
	one, err := EventCallArtefact(ctx, []uuid.UUID{event}, "call:v1018")
	if err != nil || one == nil || one.ActionItems[0].Task != "" {
		t.Errorf("EventCallArtefact = %+v, %v; want item 0 with no task, as Make task reads it", one, err)
	}
}
