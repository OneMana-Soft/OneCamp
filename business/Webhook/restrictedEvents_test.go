package business

import (
	"context"
	"sync"
	"testing"
	"time"
)

// WA2: a restricted task's events reach only the listeners that act inside
// the task and tell no one of it (a doc's to-dos, an assigned agent); a rule
// that posts or a bridge that mirrors never hears of them.
func TestARestrictedTasksEventsReachOnlyTheListenersMeantForThem(t *testing.T) {
	var mu sync.Mutex
	heard := map[string][]string{}
	done := make(chan struct{}, 8)
	listen := func(name string) EventListener {
		return func(_ context.Context, kind string, _ map[string]interface{}) {
			mu.Lock()
			heard[name] = append(heard[name], kind)
			mu.Unlock()
			done <- struct{}{}
		}
	}
	listenerMu.Lock()
	savedL, savedS := listeners, seesRestricted
	listeners, seesRestricted = nil, nil
	listenerMu.Unlock()
	savedR := restrictedTaskEvent
	t.Cleanup(func() {
		listenerMu.Lock()
		listeners, seesRestricted = savedL, savedS
		listenerMu.Unlock()
		restrictedTaskEvent = savedR
	})
	RegisterEventListener(listen("rule"))
	RegisterEventListenerSeeingRestricted(listen("todos"))
	restrictedTaskEvent = func(_ context.Context, data map[string]interface{}) bool {
		return data["task_id"] == "secret"
	}
	wait := func(n int) {
		for i := 0; i < n; i++ {
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("a listener wasn't told")
			}
		}
	}

	notifyEventListeners(context.Background(), "task.updated", map[string]interface{}{"task_id": "open"})
	wait(2)
	notifyEventListeners(context.Background(), "task.updated", map[string]interface{}{"task_id": "secret"})
	wait(1)
	select {
	case <-done:
		t.Fatal("a listener not meant for a restricted task's events heard one")
	case <-time.After(150 * time.Millisecond):
	}
	mu.Lock()
	defer mu.Unlock()
	if len(heard["rule"]) != 1 || len(heard["todos"]) != 2 {
		t.Fatalf("heard: %v", heard)
	}
}

func TestAnEventNamesItsTask(t *testing.T) {
	if taskOfEvent(map[string]interface{}{"task_id": " t1 "}) != "t1" || taskOfEvent(map[string]interface{}{"task_uuid": "t2"}) != "t2" || taskOfEvent(map[string]interface{}{"post_id": "p"}) != "" {
		t.Fatal("taskOfEvent")
	}
}
