package business

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/google/uuid"
)

// A task whose link cannot be read is treated as unlinked.
//
// There is no database in this binary, so GetTaskGitHubURLs errors, and the
// answer must be "do not queue". Queueing anyway is precisely the behaviour that
// produced 67 of 69 recorded sync failures on beta, each one waking the worker
// and burning its retry budget to discover there was nothing to sync to.
func TestUnreadableLinkCountsAsNoLink(t *testing.T) {
	if TaskHasGitHubLink(context.Background(), uuid.New()) {
		t.Error("a task whose link could not be read reported as linked")
	}
}

// The origin guard must return before anything else happens. With no database
// configured, reaching the link lookup at all would be the only observable
// difference, so this asserts the call is inert rather than that it stored
// something.
func TestEnqueueIsInertForGitHubOriginatedWork(t *testing.T) {
	ctx := helpers.WithGitHubOrigin(context.Background())

	// Must not panic, block, or reach a store that does not exist.
	EnqueueGitHubSync(ctx, uuid.New(), "status", map[string]interface{}{"status": "done"})

	// And must not have woken the worker: a queued item signals, a dropped one
	// must not, or the worker spins for work that was never enqueued.
	select {
	case <-SyncSignal:
		t.Error("a GitHub-originated change signalled the sync worker")
	default:
	}
}

// Same for an unlinked task on an ordinary edit.
func TestEnqueueIsInertForAnUnlinkedTask(t *testing.T) {
	EnqueueGitHubSync(context.Background(), uuid.New(), "status", map[string]interface{}{"status": "done"})

	select {
	case <-SyncSignal:
		t.Error("an unlinked task signalled the sync worker")
	default:
	}
}

// Both guards must sit ahead of the insert. A source check because the true
// path needs a database, and because the ordering is the whole point: a guard
// after the insert would still queue the row it exists to prevent.
func TestBothGuardsPrecedeTheInsert(t *testing.T) {
	src, err := os.ReadFile("enqueue.go")
	if err != nil {
		t.Fatalf("read enqueue.go: %v", err)
	}
	body := string(src)

	origin := strings.Index(body, "helpers.IsGitHubOrigin(ctx)")
	link := strings.Index(body, "TaskHasGitHubLink(ctx, taskID)")
	insert := strings.Index(body, "AtomicInsertGitHubSyncQueueItem")

	if origin < 0 || link < 0 || insert < 0 {
		t.Fatalf("expected both guards and the insert in EnqueueGitHubSync (origin=%d link=%d insert=%d)", origin, link, insert)
	}
	if origin > insert {
		t.Error("the GitHub-origin guard runs after the insert, so echoes are still queued")
	}
	if link > insert {
		t.Error("the link guard runs after the insert, so unlinked tasks are still queued")
	}
}

// Nothing may reach the queue except through EnqueueGitHubSync, or a future
// caller inherits neither guard.
func TestTheQueueIsOnlyReachableThroughTheGuardedHelper(t *testing.T) {
	roots := []string{"../business", "../controllers", "../domain"}
	var offenders []string

	for _, root := range roots {
		_ = filepathWalk(root, func(path string, body string) {
			if strings.HasSuffix(path, "_test.go") ||
				strings.Contains(path, "GitHubSyncQueue") ||
				strings.HasSuffix(path, "business/enqueue.go") {
				return
			}
			if strings.Contains(body, "AtomicInsertGitHubSyncQueueItem") {
				offenders = append(offenders, path)
			}
		})
	}

	if len(offenders) > 0 {
		t.Errorf("these reach the sync queue without the origin and link guards:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// filepathWalk visits every .go file under root.
func filepathWalk(root string, fn func(path, body string)) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		p := root + "/" + e.Name()
		if e.IsDir() {
			_ = filepathWalk(p, fn)
			continue
		}
		if !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		b, readErr := os.ReadFile(p)
		if readErr != nil {
			continue
		}
		fn(p, string(b))
	}
	return nil
}
