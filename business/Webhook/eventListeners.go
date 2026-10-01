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
	"sync"

	"github.com/akashc777/OneCamp/helpers"
)

// EventListener reacts to a workspace event. data is the same map passed to
// DispatchEvent (e.g. {"post_id", "channel_id", "text", ...}).
type EventListener func(ctx context.Context, eventType string, data map[string]interface{})

var (
	listenerMu sync.RWMutex
	listeners  []EventListener
)

// RegisterEventListener subscribes a listener to ALL dispatched events. The
// listener should filter by eventType itself. Called from feature packages'
// wiring at startup (before traffic), so no locking contention on the hot path
// beyond a read-lock.
func RegisterEventListener(l EventListener) {
	if l == nil {
		return
	}
	listenerMu.Lock()
	listeners = append(listeners, l)
	listenerMu.Unlock()
}

// notifyEventListeners invokes every registered listener for an event. Each
// runs in its own goroutine with a panic guard so a misbehaving listener can
// neither block nor crash the dispatch path. ctx is detached from the request
// lifecycle so listener work isn't cancelled when the HTTP handler returns.
func notifyEventListeners(ctx context.Context, eventType string, data map[string]interface{}) {
	listenerMu.RLock()
	defer listenerMu.RUnlock()
	if len(listeners) == 0 {
		return
	}
	detached := context.WithoutCancel(ctx)
	for _, l := range listeners {
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
