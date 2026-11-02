package business

// A task's webhooks (linear-39): every change the Work Change Log keeps to a
// task, made, changed, deleted or restored, reaches webhooks and the
// in-process listeners (workflows, agents, linked docs) through the Outbox
// (business/WorkFeed sinks.go), on the one path every task change takes. The
// change and its event are written in one transaction, the event is tried
// until its deliveries are queued, and business/Hooks tries each delivery for
// about a day, pausing a webhook that keeps failing. A change from anywhere
// (the app, the API, MCP, rules, forms, agents) is one event, since the log's
// one funnel (RecordTasks) logs every write once. The events are the log's
// kinds, named as webhooks have always been set up for them (task.created,
// task.updated, task.deleted, task.restored).

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	workFeed "github.com/akashc777/OneCamp/business/WorkFeed"
	"github.com/google/uuid"
)

// taskWebhooksSink is the log's sink for a task's events. Its name is
// stable: the Outbox remembers which sinks had an event by it.
var taskWebhooksSink = workFeed.Sink{Name: "task.webhooks", Run: sendTaskEvent,
	Kinds: []string{workFeed.KindCreated, workFeed.KindUpdated, workFeed.KindDeleted, workFeed.KindRestored}}

func init() {
	workFeed.RegisterSink(taskWebhooksSink)
}

// Seams for tests: business/Webhook's durable dispatch, and the log's read of
// what a change replaced.
var (
	dispatchDurable = webhookBusiness.DispatchDurable
	replacedBy      = workFeed.Replaced
)

// sendTaskEvent dispatches e as its kind's event with the Outbox event's id,
// the same on every try, so a try again sends no webhook anything twice and a
// listener runs once per id (the rules engine). A dispatch that queued
// nothing, or a log that can't say what a change replaced, is answered, to be
// tried again.
func sendTaskEvent(ctx context.Context, eventID string, e workFeed.Entry) error {
	data, err := taskEvent(ctx, e)
	if err != nil || data == nil {
		return err
	}
	id, _ := uuid.Parse(eventID)
	return dispatchDurable(ctx, id, e.Kind, data)
}

// taskEvent is what webhooks are sent of e, by its kind; nil sends nothing.
func taskEvent(ctx context.Context, e workFeed.Entry) (map[string]interface{}, error) {
	switch e.Kind {
	case workFeed.KindCreated:
		return taskCreatedEvent(e), nil
	case workFeed.KindUpdated:
		data := taskUpdatedEvent(e)
		if data == nil {
			return nil, nil
		}
		replaced, err := replacedBy(ctx, e)
		if err != nil {
			return nil, err
		}
		data["previous"] = previousOf(replaced)
		return data, nil
	case workFeed.KindDeleted:
		return taskDeletedEvent(e), nil
	case workFeed.KindRestored:
		return taskRestoredEvent(e), nil
	}
	return nil, nil
}

// previousOf is what a change's fields were before it (Linear's updatedFrom),
// named as its changes are: a custom field's under task_fields, by field id.
// A field the log can't say (set before it began, or pruned) is left out.
// Pure.
func previousOf(replaced map[string]json.RawMessage) map[string]interface{} {
	was := make(map[string]interface{}, len(replaced))
	for f, raw := range replaced {
		var v interface{}
		if json.Unmarshal(raw, &v) == nil {
			was[f] = v
		}
	}
	return asTheTaskAnswerNamesIt(was)
}

// lifecycleEvent is what a made, deleted or restored task's event says of
// the change: the task and its project, with the log's number, source and
// time, as task.updated has them. Its other keys are the ones each event has
// always had. Nil for an entry whose diff says nothing. Pure.
func lifecycleEvent(e workFeed.Entry) (map[string]interface{}, map[string]interface{}) {
	var diff map[string]interface{}
	if err := json.Unmarshal(e.Diff, &diff); err != nil || len(diff) == 0 {
		return nil, nil
	}
	return map[string]interface{}{
		"task_id": e.ResourceUUID, "project_id": e.ProjectUUID, "sequence": e.Sequence,
		"source": e.Source, "changed_at": e.At.UTC().Format(time.RFC3339Nano),
	}, diff
}

// words is diff's string at key, "" for none, as the events always sent them.
func words(diff map[string]interface{}, key string) string {
	s, _ := diff[key].(string)
	return s
}

// taskCreatedEvent is task.created: the task's name, description, status and
// priority as it was made, and who made it (a user uuid). A sub-task's is sent
// too, naming its parent (parent_task_id, Linear's parentId), so a receiver
// can tell one from a top-level task. The rules engine and agents still run
// on a top-level task's making only (business/Workflow wants, business/AIAgent
// eventWanted), as they always have: a template's sub-tasks would otherwise
// start them once each. Pure.
func taskCreatedEvent(e workFeed.Entry) map[string]interface{} {
	data, diff := lifecycleEvent(e)
	if data == nil {
		return nil
	}
	data["name"], data["description"] = words(diff, "task_name"), words(diff, "task_description")
	data["status"], data["priority"], data["created_by"] = words(diff, "task_status"), words(diff, "task_priority"), e.Actor
	if parent, _ := diff["task_parent_task"].(map[string]interface{}); parent != nil {
		if id := words(parent, "task_uuid"); id != "" {
			data["parent_task_id"] = id
		}
	}
	return data
}

// taskDeletedEvent is task.deleted: the task's name and status as it went,
// and who deleted it, by their user uuid (it was a graph uid, which nothing
// outside can read). Pure.
func taskDeletedEvent(e workFeed.Entry) map[string]interface{} {
	data, diff := lifecycleEvent(e)
	if data == nil {
		return nil
	}
	data["name"], data["status"], data["deleted_by"] = words(diff, "task_name"), words(diff, "task_status"), e.Actor
	return data
}

// taskRestoredEvent is task.restored: the task's name, and who restored it,
// by their user uuid (as task.deleted). Pure.
func taskRestoredEvent(e workFeed.Entry) map[string]interface{} {
	data, diff := lifecycleEvent(e)
	if data == nil {
		return nil
	}
	data["name"], data["restored_by"] = words(diff, "task_name"), e.Actor
	return data
}

// taskUpdatedEvent is what a webhook is sent of e: the task and its project,
// the log's number for the change (rising, one per change, so a receiver can
// order and de-duplicate them), the fields it changed in order and their new
// values as the app's task answer carries them (someone it names by id, names
// and picture, never their email), who changed them, from where (app, api or
// agent) and when. A change that names no field says nothing. Pure.
func taskUpdatedEvent(e workFeed.Entry) map[string]interface{} {
	var changes map[string]interface{}
	if err := json.Unmarshal(e.Diff, &changes); err != nil || len(changes) == 0 {
		return nil
	}
	changes = asTheTaskAnswerNamesIt(changes)
	fields := make([]string, 0, len(changes))
	for f := range changes {
		fields = append(fields, f)
	}
	sort.Strings(fields)
	data := map[string]interface{}{
		"task_id": e.ResourceUUID, "project_id": e.ProjectUUID, "sequence": e.Sequence,
		"fields": fields, "changes": changes, "source": e.Source, "changed_at": e.At.UTC().Format(time.RFC3339Nano),
	}
	if e.Actor != "" {
		data["updated_by_uuid"] = e.Actor
	}
	return data
}

// asTheTaskAnswerNamesIt is a logged change named as the app's task answer
// names it, the one place a log entry's names become the event's. The log
// keeps a custom field's value in business/TaskField's own shape,
// {"field_id", "value"} (SetValue), which the web's live views read
// (lib/tasks/workChanges.ts); the answer carries it as task_fields, by field
// id. Every other change already names task fields, and comes back as it is.
func asTheTaskAnswerNamesIt(changes map[string]interface{}) map[string]interface{} {
	id, _ := changes["field_id"].(string)
	value, hasValue := changes["value"]
	if id == "" || !hasValue {
		return changes
	}
	out := make(map[string]interface{}, len(changes)-1)
	for k, v := range changes {
		if k != "field_id" && k != "value" {
			out[k] = v
		}
	}
	out["task_fields"] = map[string]interface{}{id: value}
	return out
}
