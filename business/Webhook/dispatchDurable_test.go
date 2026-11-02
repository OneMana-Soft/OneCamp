package business

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	hooksBusiness "github.com/akashc777/OneCamp/business/Hooks"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	webhookModel "github.com/akashc777/OneCamp/models/postgres/Webhook"
	"github.com/google/uuid"
)

// linear-39: a source that runs an event until it is queued (an Outbox sink)
// gives it its own id, the same on every run: business/Hooks keeps one
// delivery per webhook and event, so a run again sends nothing twice, and the
// body says that id. A queue that fails is the source's to know, to run it
// again; an event no webhook is set up for is done.
func TestADurableEventKeepsItsIDAndSaysWhenItIsntQueued(t *testing.T) {
	w := stubReach(t)
	owner := uuid.New()
	w.members[owner] = true
	project := uuid.New()
	w.roles[project.String()] = map[uuid.UUID]workAccess.Role{owner: workAccess.RoleViewer}
	hook := &webhookModel.Webhook{Id: uuid.New(), Name: "changes", CreatedBy: owner, ScopeType: ScopeProject, ScopeEntityId: &project}
	saved, savedPublic := queueDeliveries, channelIsPublic
	t.Cleanup(func() { queueDeliveries, channelIsPublic = saved, savedPublic; dispatchCache.Purge() })
	channelIsPublic = func(context.Context, uuid.UUID) (bool, error) { return false, nil }
	var queuedID uuid.UUID
	var body string
	var fail error
	queueDeliveries = func(_ context.Context, id uuid.UUID, _ string, targets []hooksBusiness.Target) error {
		queuedID = id
		for _, tg := range targets {
			body = string(tg.Payload)
		}
		return fail
	}
	dispatchCache.Purge()
	dispatchCache.Set(dispatchCacheKey("task.updated", ScopeProject, &project), dispatchCacheEntry{generation: atomic.LoadUint64(&dispatchGeneration), webhooks: []*webhookModel.Webhook{hook}})

	event := uuid.New()
	data := map[string]interface{}{"task_id": uuid.NewString(), "project_id": project.String(), "fields": []string{"task_name"}}
	if err := DispatchDurable(context.Background(), event, "task.updated", data); err != nil {
		t.Fatal(err)
	}
	var sent struct {
		EventID string `json:"event_id"`
	}
	if err := json.Unmarshal([]byte(body), &sent); err != nil || queuedID != event || sent.EventID != event.String() {
		t.Errorf("queued as %s with body %s (err %v), want the event's own %s", queuedID, body, err, event)
	}
	if _, ok := data["event_id"]; ok {
		t.Errorf("the caller's data was written to: %v", data)
	}

	fail = errors.New("the queue is down")
	if err := DispatchDurable(context.Background(), event, "task.updated", data); !errors.Is(err, fail) {
		t.Errorf("a queue that failed answered %v, want its error", err)
	}
	other := uuid.New()
	dispatchCache.Set(dispatchCacheKey("task.updated", ScopeProject, &other), dispatchCacheEntry{generation: atomic.LoadUint64(&dispatchGeneration)})
	if err := DispatchDurable(context.Background(), event, "task.updated", map[string]interface{}{"task_id": "t", "project_id": other.String()}); err != nil {
		t.Errorf("an event with no webhook answered %v, want done", err)
	}
}

// Every try of a durable event tells the listeners its own id: the rules
// engine runs a rule once per id, so a sink tried again never runs one twice.
// A webhook lookup that fails answers its error, so the Outbox tries the
// event again; answering nothing would mark it done, its webhooks never sent it.
func TestEveryTryOfADurableEventTellsListenersItsIDAndALookupThatFailsIsTriedAgain(t *testing.T) {
	stubReach(t)
	heard := make(chan interface{}, 4)
	listenerMu.Lock()
	savedL, savedS := listeners, seesRestricted
	listeners, seesRestricted = nil, nil
	listenerMu.Unlock()
	savedRead, savedQueue := activeOutgoingWebhooks, queueDeliveries
	t.Cleanup(func() {
		listenerMu.Lock()
		listeners, seesRestricted = savedL, savedS
		listenerMu.Unlock()
		activeOutgoingWebhooks, queueDeliveries = savedRead, savedQueue
		dispatchCache.Purge()
	})
	RegisterEventListener(func(_ context.Context, _ string, data map[string]interface{}) { heard <- data["event_id"] })
	down := errors.New("postgres is down")
	activeOutgoingWebhooks = func(context.Context, string, string, *uuid.UUID) ([]*webhookModel.Webhook, error) { return nil, down }
	queueDeliveries = func(context.Context, uuid.UUID, string, []hooksBusiness.Target) error {
		t.Error("an event whose webhooks couldn't be read was queued")
		return nil
	}
	dispatchCache.Purge()

	event := uuid.New()
	data := map[string]interface{}{"task_id": uuid.NewString(), "project_id": uuid.NewString(), "fields": []string{"task_name"}}
	for try := 1; try <= 2; try++ {
		if err := DispatchDurable(context.Background(), event, "task.updated", data); !errors.Is(err, down) {
			t.Errorf("try %d: a webhook lookup that failed answered %v, want its error, to be tried again", try, err)
		}
		select {
		case id := <-heard:
			if id != event.String() {
				t.Errorf("try %d: the listeners were told event_id %v, want the event's own %s", try, id, event)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("try %d: no listener was told", try)
		}
	}
	if _, ok := data["event_id"]; ok {
		t.Errorf("the caller's data was written to: %v", data)
	}
}
