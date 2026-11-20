package business

// A task deleted while it waits in triage stops waiting (CR-288 F2). Nobody
// can decide a deleted task (Decide's read refuses it, and the queue leaves it
// out), so left waiting it would hold a place under MaxWaiting, where a pile
// of deleted spam from a public form would stop the project's triage taking
// work without a word, and it would stay in the waiting set every list reads.
// A task's deletion reaches here as its Work Change Log entry does every sink
// (business/WorkFeed sinks.go, through the Outbox): at least once, after a
// crash too. Its item stays, decided with no decision kept, since nobody made
// one: items are never deleted. A restored task comes back as the team's work,
// not into triage: a task goes through triage once, and whoever restored it
// chose to have it back.

import (
	"context"

	workFeed "github.com/akashc777/OneCamp/business/WorkFeed"
	triageModel "github.com/akashc777/OneCamp/models/postgres/Triage"
	"github.com/google/uuid"
)

// taskDeletedSink's name is stable: the Outbox remembers which sinks had an
// event by it.
var taskDeletedSink = workFeed.Sink{Name: "triage.task_deleted", Kinds: []string{workFeed.KindDeleted}, Run: endWaitOfDeleted}

func init() {
	workFeed.RegisterSink(taskDeletedSink)
}

var endWait = triageModel.EndWait

// endWaitOfDeleted ends the wait of the task e deleted, if it waited. An
// error is tried again with the Outbox's backoff.
func endWaitOfDeleted(ctx context.Context, _ string, e workFeed.Entry) error {
	id, err := uuid.Parse(e.ResourceUUID)
	if e.ResourceKind != "task" || err != nil {
		return nil
	}
	return endWait(ctx, id)
}
