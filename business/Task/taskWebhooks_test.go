package business

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	workFeed "github.com/akashc777/OneCamp/business/WorkFeed"
	"github.com/google/uuid"
)

func updatedEntry(diff string) workFeed.Entry {
	return workFeed.Entry{Sequence: 42, Kind: workFeed.KindUpdated, ResourceKind: "task", ResourceUUID: "t1", ProjectUUID: "p1",
		Diff: json.RawMessage(diff), Actor: "u1", Source: "api", At: time.Date(2026, 11, 1, 7, 0, 0, 0, time.UTC)}
}

// linear-39: a webhook is sent what changed: the task, its project, the
// log's number for the change, the fields it changed in order and their new
// values, who changed them, from where and when. A change that names no
// field says nothing.
func TestTaskUpdatedNamesEveryFieldThatChanged(t *testing.T) {
	got := taskUpdatedEvent(updatedEntry(`{"task_status":"done","task_assignee":null,"task_priority":"high"}`))
	want := map[string]interface{}{
		"task_id": "t1", "project_id": "p1", "sequence": int64(42),
		"fields":          []string{"task_assignee", "task_priority", "task_status"},
		"changes":         map[string]interface{}{"task_status": "done", "task_assignee": nil, "task_priority": "high"},
		"updated_by_uuid": "u1", "source": "api", "changed_at": "2026-11-01T07:00:00Z",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("event %#v\nwant %#v", got, want)
	}
	// A custom field's value: the log keeps business/TaskField's own shape,
	// {field_id, value}, which the web's live views read; a webhook is sent it
	// as the task answer names it, task_fields by field id (null when cleared).
	for diff, value := range map[string]interface{}{
		`{"field_id":"f-77","value":{"amount":12.5,"currency":"EUR"}}`: map[string]interface{}{"amount": 12.5, "currency": "EUR"},
		`{"field_id":"f-77","value":null}`:                             nil,
	} {
		got := taskUpdatedEvent(updatedEntry(diff))
		if !reflect.DeepEqual(got["fields"], []string{"task_fields"}) || !reflect.DeepEqual(got["changes"], map[string]interface{}{"task_fields": map[string]interface{}{"f-77": value}}) {
			t.Errorf("custom field change %s went out as fields %v, changes %v; want task_fields {f-77: %v}", diff, got["fields"], got["changes"], value)
		}
	}
	for _, diff := range []string{`{}`, `null`, `[]`, `"x"`, ``} {
		if got := taskUpdatedEvent(updatedEntry(diff)); got != nil {
			t.Errorf("diff %q is %v, want nothing", diff, got)
		}
	}
	e := updatedEntry(`{"task_name":"Ship"}`)
	e.Actor = ""
	if _, ok := taskUpdatedEvent(e)["updated_by_uuid"]; ok {
		t.Error("a change nobody signed names someone")
	}
}

// knowsBefore stands in for the log's read of what a change replaced.
func knowsBefore(t *testing.T, replaced map[string]json.RawMessage, err error) {
	saved := replacedBy
	t.Cleanup(func() { replacedBy = saved })
	replacedBy = func(context.Context, workFeed.Entry) (map[string]json.RawMessage, error) { return replaced, err }
}

// linear-39 round 2: a task.updated says what each changed field was before
// (Linear's updatedFrom), named as its changes are: a custom field's under
// task_fields by field id. A field the log can't say is left out; previous
// is there, empty, when it can say none.
func TestATaskUpdatedSaysWhatItsFieldsWereBefore(t *testing.T) {
	ctx := context.Background()
	knowsBefore(t, map[string]json.RawMessage{"task_priority": json.RawMessage(`"low"`), "task_assignee": json.RawMessage(`null`)}, nil)
	data, err := taskEvent(ctx, updatedEntry(`{"task_priority":"high","task_assignee":{"user_uuid":"u2"},"task_status":"done"}`))
	if err != nil || !reflect.DeepEqual(data["previous"], map[string]interface{}{"task_priority": "low", "task_assignee": nil}) {
		t.Errorf("previous %#v (err %v), want task_priority low and task_assignee null, task_status unknown", data["previous"], err)
	}
	knowsBefore(t, map[string]json.RawMessage{"field_id": json.RawMessage(`"f-77"`), "value": json.RawMessage(`{"amount":3}`)}, nil)
	if data, _ := taskEvent(ctx, updatedEntry(`{"field_id":"f-77","value":{"amount":12.5}}`)); !reflect.DeepEqual(data["previous"],
		map[string]interface{}{"task_fields": map[string]interface{}{"f-77": map[string]interface{}{"amount": float64(3)}}}) {
		t.Errorf("a custom field's previous is %#v, want task_fields {f-77: {amount: 3}}", data["previous"])
	}
	knowsBefore(t, map[string]json.RawMessage{}, nil)
	if data, _ := taskEvent(ctx, updatedEntry(`{"task_name":"Ship"}`)); !reflect.DeepEqual(data["previous"], map[string]interface{}{}) {
		t.Errorf("with nothing known before, previous is %#v, want {}", data["previous"])
	}
	down := errors.New("the log is down")
	knowsBefore(t, nil, down)
	if data, err := taskEvent(ctx, updatedEntry(`{"task_name":"Ship"}`)); !errors.Is(err, down) || data != nil {
		t.Errorf("a log that can't be read answered %v, %v; want its error and nothing to send", data, err)
	}
}

func entryOfKind(kind, diff string) workFeed.Entry {
	e := updatedEntry(diff)
	e.Kind = kind
	return e
}

// linear-39 round 2: made, deleted and restored tasks reach webhooks from
// their log entries, with the keys each event has always had, plus the log's
// number, source and time. Who did it is their user uuid (task.deleted and
// task.restored sent a graph uid). A sub-task's making is sent too (main,
// 1 Nov: every change), naming its parent; a top-level task's names none.
func TestMadeDeletedAndRestoredTasksKeepTheirEventsKeys(t *testing.T) {
	common := map[string]interface{}{"task_id": "t1", "project_id": "p1", "sequence": int64(42), "source": "api", "changed_at": "2026-11-01T07:00:00Z"}
	with := func(more map[string]interface{}) map[string]interface{} {
		out := map[string]interface{}{}
		for k, v := range common {
			out[k] = v
		}
		for k, v := range more {
			out[k] = v
		}
		return out
	}
	made := taskCreatedEvent(entryOfKind(workFeed.KindCreated,
		`{"task_name":"Release notes","task_description":"Draft them","task_status":"todo","task_priority":"low","task_parent_task":null,"task_label":null}`))
	if want := with(map[string]interface{}{"name": "Release notes", "description": "Draft them", "status": "todo", "priority": "low", "created_by": "u1"}); !reflect.DeepEqual(made, want) {
		t.Errorf("task.created %#v\nwant %#v", made, want)
	}
	if bare := taskCreatedEvent(entryOfKind(workFeed.KindCreated, `{"task_name":"Bare","task_description":null,"task_priority":null}`)); bare["description"] != "" || bare["priority"] != "" {
		t.Errorf("a task made with no description or priority says %#v, want them empty, as task.created always sent them", bare)
	}
	sub := taskCreatedEvent(entryOfKind(workFeed.KindCreated,
		`{"task_name":"Step 1","task_description":null,"task_status":"todo","task_priority":null,"task_parent_task":{"task_uuid":"t0","task_name":"Launch"}}`))
	if want := with(map[string]interface{}{"name": "Step 1", "description": "", "status": "todo", "priority": "", "created_by": "u1", "parent_task_id": "t0"}); !reflect.DeepEqual(sub, want) {
		t.Errorf("a sub-task's task.created %#v\nwant %#v: sent like any task's, naming its parent", sub, want)
	}
	gone := taskDeletedEvent(entryOfKind(workFeed.KindDeleted, `{"task_deleted_at":"2026-11-01T07:00:00Z","task_name":"Release notes","task_status":"todo"}`))
	if want := with(map[string]interface{}{"name": "Release notes", "status": "todo", "deleted_by": "u1"}); !reflect.DeepEqual(gone, want) {
		t.Errorf("task.deleted %#v\nwant %#v", gone, want)
	}
	back := taskRestoredEvent(entryOfKind(workFeed.KindRestored, `{"task_deleted_at":null,"task_name":"Release notes","task_status":"todo"}`))
	if want := with(map[string]interface{}{"name": "Release notes", "restored_by": "u1"}); !reflect.DeepEqual(back, want) {
		t.Errorf("task.restored %#v\nwant %#v", back, want)
	}
	for _, f := range []func(workFeed.Entry) map[string]interface{}{taskCreatedEvent, taskDeletedEvent, taskRestoredEvent} {
		if got := f(entryOfKind(workFeed.KindCreated, `{}`)); got != nil {
			t.Errorf("an entry that says nothing sent %v", got)
		}
	}
}

// The sink is for every kind of task change, dispatches each as its kind with
// the Outbox event's id, the same on every try, and hands back a dispatch that
// didn't queue it, to be tried again.
func TestTheTaskWebhooksSinkIsTriedUntilQueued(t *testing.T) {
	knowsBefore(t, map[string]json.RawMessage{}, nil)
	saved := dispatchDurable
	t.Cleanup(func() { dispatchDurable = saved })
	var kinds []string
	var ids []uuid.UUID
	fail := errors.New("the queue is down")
	dispatchDurable = func(_ context.Context, id uuid.UUID, kind string, data map[string]interface{}) error {
		kinds, ids = append(kinds, kind), append(ids, id)
		if data["task_id"] != "t1" {
			t.Errorf("dispatched %v", data)
		}
		return fail
	}
	event := uuid.New()
	for _, kind := range []string{workFeed.KindCreated, workFeed.KindUpdated, workFeed.KindDeleted, workFeed.KindRestored} {
		if err := sendTaskEvent(context.Background(), event.String(), entryOfKind(kind, `{"task_name":"Ship"}`)); !errors.Is(err, fail) {
			t.Errorf("a %s whose dispatch failed answered %v, want its error", kind, err)
		}
	}
	if err := sendTaskEvent(context.Background(), event.String(), updatedEntry(`{}`)); err != nil {
		t.Errorf("an empty change answered %v", err)
	}
	if want := []string{"task.created", "task.updated", "task.deleted", "task.restored"}; !reflect.DeepEqual(kinds, want) || ids[0] != event || ids[3] != event {
		t.Errorf("dispatched %v with %v, want %v, each with %s", kinds, ids, want, event)
	}
	if want := []string{workFeed.KindCreated, workFeed.KindUpdated, workFeed.KindDeleted, workFeed.KindRestored}; !reflect.DeepEqual(taskWebhooksSink.Kinds, want) || taskWebhooksSink.Name != "task.webhooks" {
		t.Errorf("the sink %q is for %v, want task.webhooks for %v", taskWebhooksSink.Name, taskWebhooksSink.Kinds, want)
	}
}
