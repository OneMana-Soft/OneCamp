package domain

// What a call's recap keeps on its transcript's node (gcal-new-event-notes):
// the recap's words and its action items, whatever the notes-document
// setting, so an event's "Recordings & notes" show them for the event and
// every instance of its series (EventCallArtefacts), to whoever may read the
// call's transcript there: the caller's rule, the one that reads the
// transcript. A task made from an action item is noted beside the item, one
// value a task (recording_action_tasks), so two items made at once never
// write over each other.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Recording"
	"github.com/google/uuid"
)

// MaxActionItems is the most action items a call's recap keeps.
const MaxActionItems = 20

// ActionItem is one thing someone on a call took on, as its recap lists it:
// what (Text), who when the recap names them (Owner, as it names them), by
// when when it names a date (Due, YYYY-MM-DD), and the task made from it
// (Task, its uuid) once someone made one.
type ActionItem struct {
	Text  string `json:"text"`
	Owner string `json:"owner,omitempty"`
	Due   string `json:"due,omitempty"`
	Task  string `json:"task,omitempty"`
}

// CallRecap is what a call's recap says: its words (its summary and
// decisions) and its action items.
type CallRecap struct {
	Text  string
	Items []ActionItem
}

// IsRecapHeading is whether a line of a recap (trimmed) heads a section: a
// markdown heading, or a line in bold markers. The one test of it, for the
// recap's maker (business/AI) and its readers (business/Calendar). Pure.
func IsRecapHeading(l string) bool {
	return strings.HasPrefix(l, "#") || (len(l) > 4 && strings.HasPrefix(l, "**") && strings.HasSuffix(l, "**"))
}

// IsRecapNone is whether a line or an item of a recap says there is nothing:
// "None", "N/A" or "No action items", in any case, with or without its bold
// or italic markers and its full stop. Pure.
func IsRecapNone(t string) bool {
	t = strings.ToLower(strings.Trim(t, " .*_"))
	return t == "none" || t == "n/a" || t == "no action items"
}

// fields are the predicates a recap is kept in: none for a recap that says
// nothing. Its items are kept without tasks (a task is noted on its own) and
// at most MaxActionItems of them. Pure.
func (r CallRecap) fields() (map[string]interface{}, error) {
	out := map[string]interface{}{}
	if text := strings.TrimSpace(r.Text); text != "" {
		out["recording_recap"] = text
	}
	items := make([]ActionItem, 0, len(r.Items))
	for _, it := range r.Items {
		if it.Text = strings.TrimSpace(it.Text); it.Text == "" {
			continue
		}
		it.Task = ""
		items = append(items, it)
		if len(items) == MaxActionItems {
			break
		}
	}
	if len(items) > 0 {
		raw, err := json.Marshal(items)
		if err != nil {
			return nil, err
		}
		out["recording_action_items"] = string(raw)
	}
	return out, nil
}

// actionTaskValue is how the task made from a call's action item i is noted
// on its node: "<i>:<task uuid>". Pure.
func actionTaskValue(i int, task uuid.UUID) string {
	return strconv.Itoa(i) + ":" + task.String()
}

// notedTask reads one of a node's task notes (actionTaskValue's): the item
// and its task; false for a note that doesn't read. Pure.
func notedTask(v string) (int, uuid.UUID, bool) {
	at, id, ok := strings.Cut(v, ":")
	i, err := strconv.Atoi(at)
	if !ok || err != nil || i < 0 {
		return 0, uuid.Nil, false
	}
	task, err := uuid.Parse(id)
	return i, task, err == nil
}

// notedTasks are the tasks a node's notes name, each once. Pure.
func notedTasks(tasks []string) []string {
	var out []string
	for _, v := range tasks {
		if _, task, ok := notedTask(v); ok && !slices.Contains(out, task.String()) {
			out = append(out, task.String())
		}
	}
	return out
}

// itemsWithTasks are a node's action items (raw, as fields keeps them) with
// the task noted for each (tasks, actionTaskValue's) that is live (in live,
// by uuid): the least when two are; none for none, for a task deleted since
// (so the item can be made a task again), or for items that don't read. Pure.
func itemsWithTasks(raw string, tasks []string, live map[string]bool) []ActionItem {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var items []ActionItem
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		return nil
	}
	sorted := slices.Clone(tasks)
	slices.Sort(sorted)
	for _, v := range sorted {
		i, task, ok := notedTask(v)
		if !ok || i >= len(items) || items[i].Task != "" || !live[task.String()] {
			continue
		}
		items[i].Task = task.String()
	}
	return items
}

// RecordActionItemTask notes on a call's node (by its key) that task was made
// from its action item i. Nothing is written for a key no node has.
func RecordActionItemTask(ctx context.Context, key string, i int, task uuid.UUID) error {
	if key == "" || i < 0 || task == uuid.Nil {
		return fmt.Errorf("domain/RecordActionItemTask: a call, an item and a task, got %q, %d and %s", key, i, task)
	}
	query := `query Session($key: string) {
		session as var(func: eq(recording_egress_id, $key))
	}`
	return dgraphModels.SetFoundRecordingsFields(ctx, query, map[string]string{"$key": key}, "session",
		map[string]interface{}{"recording_action_tasks": actionTaskValue(i, task)})
}
