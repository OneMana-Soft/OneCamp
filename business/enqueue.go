package business

import (
	"context"
	"encoding/json"
	"runtime/debug"

	githubSyncQueueDomain "github.com/akashc777/OneCamp/domain/GitHubSyncQueue"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/google/uuid"
)

// SyncSignal wakes the GitHub sync worker immediately when a new item is enqueued.
// Buffered (size 1) so the sender never blocks.
var SyncSignal = make(chan struct{}, 1)

// EnqueueGitHubSync is the shared entry point for queueing GitHub sync operations.
//
// BOTH GUARDS LIVE HERE rather than at the fifteen call sites, because the call
// sites are ordinary task writes that have no reason to know GitHub exists, and
// one of them being forgotten is exactly how this went wrong.
//
// GUARD 1: THE TASK MUST ACTUALLY BE LINKED. Every task status, title,
// description, assignee, label and comment change queued a row, whether or not
// the task had anything to do with GitHub. On beta that was 67 of 69 recorded
// failures, all reading "task has no linked GitHub issue or PR", each one having
// woken the worker and burned its full retry budget with backoff first. Worse,
// the queue path marks the task's sync status, so a task that has never touched
// GitHub displayed a failed GitHub sync in its panel.
//
// GUARD 2: NOT IF THE CHANGE CAME FROM GITHUB. The webhook applies inbound
// changes through the same business functions a person's edit goes through, so
// GitHub renaming an issue made OneCamp PATCH the same title back. See
// helpers.GitHubOriginContextKey.
//
// The lookup costs one indexed read on a path that previously cost an insert, a
// worker wake, an API call and three retries.
func EnqueueGitHubSync(ctx context.Context, taskID uuid.UUID, syncType string, payload map[string]interface{}) {
	// Every one of the fifteen call sites invokes this as a bare `go`, so an
	// unrecovered panic here takes the process down rather than losing one sync.
	// Recovering inside covers them all without asking each to remember.
	defer func() {
		if r := recover(); r != nil {
			helpers.LogErrorWithContext(context.Background(),
				"EnqueueGitHubSync recovered panic for task %s type %s: %v\n%s",
				taskID, syncType, r, debug.Stack())
		}
	}()

	if helpers.IsGitHubOrigin(ctx) {
		return
	}

	// Detached from the caller's cancellation but keeping its values: this runs
	// in a goroutine that outlives the request that started it, and a cancelled
	// context would fail the insert for every write the user navigates away from.
	ctx = context.WithoutCancel(ctx)

	if !TaskHasGitHubLink(ctx, taskID) {
		return
	}

	payloadJSON, _ := json.Marshal(payload)
	id := uuid.New()
	inserted, err := githubSyncQueueDomain.AtomicInsertGitHubSyncQueueItem(ctx, id, taskID, syncType, string(payloadJSON))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "EnqueueGitHubSync failed: %v", err)
		return
	}
	if !inserted {
		helpers.MessageLogs.InfoLog.Printf("EnqueueGitHubSync skipped duplicate sync for task %s type %s", taskID.String(), syncType)
		return
	}

	// Signal the sync worker to wake immediately
	select {
	case SyncSignal <- struct{}{}:
	default:
	}
}

// TaskHasGitHubLink reports whether a task points at a GitHub issue or PR.
//
// Exported because the retry endpoint needs the same answer before it tells a
// user their sync is pending: without it, asking to retry a task that was never
// linked queues five doomed items and leaves the panel showing a failure.
//
// A lookup error reads as NOT linked. The alternative is queueing work that
// cannot succeed, which is the behaviour this guard exists to stop.
func TaskHasGitHubLink(ctx context.Context, taskID uuid.UUID) (linked bool) {
	// Recovers for its own sake, not just its caller's: the retry endpoint calls
	// this synchronously on a request goroutine, and the lookup dereferences a
	// database handle that is nil before initialisation finishes. A panic here
	// would answer a task question with a dead connection.
	defer func() {
		if r := recover(); r != nil {
			helpers.LogErrorWithContext(context.Background(),
				"TaskHasGitHubLink recovered panic for task %s: %v\n%s", taskID, r, debug.Stack())
			linked = false
		}
	}()

	issueURL, prURL, err := taskDomain.GetTaskGitHubURLs(ctx, taskID)
	if err != nil {
		return false
	}
	return (issueURL != nil && *issueURL != "") || (prURL != nil && *prURL != "")
}
