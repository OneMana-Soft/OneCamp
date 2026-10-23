package business

// In-process event listeners.
//
// DispatchEvent already fans workspace events ("post.created", "task.created",
// …) out to configured OUTGOING webhooks. Some features need to react to those
// same events INSIDE the process — the Workflow engine, for example, runs
// "when a message is posted → reply / create task" rules. Rather than couple
// this package to those features (which would create an import cycle, since the
// Workflow engine creates posts/tasks and this package is imported by Post/Task
// business), we expose a tiny subscription registry: features register a
// listener at startup, and DispatchEvent invokes them.
//
// Listeners run in their own goroutine and must not block the dispatch path.

import (
	"context"
	"strings"
	"sync"

	"github.com/akashc777/OneCamp/helpers"
	restrictionModel "github.com/akashc777/OneCamp/models/postgres/TaskRestriction"
	"github.com/google/uuid"
)

// EventListener reacts to a workspace event. data is the same map passed to
// DispatchEvent (e.g. {"post_id", "channel_id", "text", ...}).
type EventListener func(ctx context.Context, eventType string, data map[string]interface{})

var (
	listenerMu sync.RWMutex
	listeners  []EventListener
	// seesRestricted says, for each listener, whether it is told of a
	// restricted task's events (RegisterEventListenerSeeingRestricted).
	seesRestricted []bool
)

// RegisterEventListener subscribes a listener to ALL dispatched events. The
// listener should filter by eventType itself. Called from feature packages'
// wiring at startup (before traffic), so no locking contention on the hot path
// beyond a read-lock.
func RegisterEventListener(l EventListener) {
	register(l, false)
}

// RegisterEventListenerSeeingRestricted is RegisterEventListener for a
// listener that is told of a restricted task's events too (WA2): one that
// acts only inside the task and tells no one of it, as a doc's to-dos synced
// to it, or an agent the task is assigned to. Every other listener (a rule
// that posts, a bridge that mirrors) is never told of them, so it can't
// carry a restricted task to people who may not see it.
func RegisterEventListenerSeeingRestricted(l EventListener) {
	register(l, true)
}

func register(l EventListener, seesRestrictedTasks bool) {
	if l == nil {
		return
	}
	listenerMu.Lock()
	listeners = append(listeners, l)
	seesRestricted = append(seesRestricted, seesRestrictedTasks)
	listenerMu.Unlock()
}

// restrictedTaskEvent is whether an event is a restricted task's: its data
// names a task (task_id, task_uuid) whose project keeps it to a few people.
// A restriction that can't be read counts as one; a task ctx marks open
// (helpers.WithOpenTasks: one of a batch just made) isn't read. A seam for
// tests.
var restrictedTaskEvent = func(ctx context.Context, data map[string]interface{}) bool {
	for _, key := range []string{"task_id", "task_uuid"} {
		raw, _ := data[key].(string)
		id, err := uuid.Parse(strings.TrimSpace(raw))
		if err != nil {
			continue
		}
		if helpers.TasksOpen(ctx, id.String()) {
			return false
		}
		r, err := restrictionModel.Of(ctx, id)
		return err != nil || r.Restricted
	}
	return false
}

// notifyEventListeners invokes every registered listener for an event. Each
// runs in its own goroutine with a panic guard so a misbehaving listener can
// neither block nor crash the dispatch path. ctx is detached from the request
// lifecycle so listener work isn't cancelled when the HTTP handler returns.
func notifyEventListeners(ctx context.Context, eventType string, data map[string]interface{}) {
	// Each event carries its own id, minted here once, so a listener that is
	// handed the same event twice (the hand-off delivers at least once) can
	// tell it is the same one: the rules engine runs a rule once per id.
	data = withEventID(data)
	// A process that runs no listeners (SERVICE_ROLE=api) hands the event to
	// the workers, which do (eventHandoff.go).
	if handOff(ctx, eventType, data) {
		return
	}
	listenerMu.RLock()
	defer listenerMu.RUnlock()
	if len(listeners) == 0 {
		return
	}
	detached := context.WithoutCancel(ctx)
	restricted := restrictedTaskEvent(detached, data)
	for i, l := range listeners {
		if restricted && (i >= len(seesRestricted) || !seesRestricted[i]) {
			continue
		}
		l := l
		go func() {
			defer func() {
				if r := recover(); r != nil {
					helpers.MessageLogs.ErrorLog.Printf("Webhook/eventListener panic recovered (event %s): %v", eventType, r)
				}
			}()
			l(detached, eventType, data)
		}()
	}
}

// withEventID is the event with an "event_id", a copy when it had none (the
// caller's map is left as it was: outgoing webhooks read it too).
func withEventID(data map[string]interface{}) map[string]interface{} {
	if id, _ := data["event_id"].(string); id != "" {
		return data
	}
	out := make(map[string]interface{}, len(data)+1)
	for k, v := range data {
		out[k] = v
	}
	out["event_id"] = uuid.NewString()
	return out
}
