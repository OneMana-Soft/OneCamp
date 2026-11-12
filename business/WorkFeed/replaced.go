package business

// What a change replaced (linear-39 round 2, Linear's updatedFrom): each
// entry keeps its fields' new values, so a field's value before a change is
// its value in the newest earlier entry of the same thing that names it. The
// log keeps every write once (RecordTasks), so that entry is the field's last
// change; pruning takes the oldest first, so a value found is always the
// latest.

import (
	"context"
	"encoding/json"
	"sort"

	taskChangeModels "github.com/akashc777/OneCamp/models/postgres/TaskChange"
)

// Seams: the store's two reads (task_changes, by resource and number).
var (
	latestValues     = taskChangeModels.LatestValues
	latestFieldValue = taskChangeModels.LatestFieldValue
)

// Replaced is what e replaced, in e's own shape: for each field e names, its
// value in the newest earlier entry of e's resource that names it. A custom
// field's change ({"field_id", "value"}, business/TaskField SetValue)
// answers {"field_id", "value"} with the value it had. A field no kept entry
// names (set before the log began, or pruned since) is left out, so an empty
// answer is "not known", never "empty". A change that tells no value
// ({"refetch": true}: a money field's, business/TaskField ForEveryone)
// replaced nothing anyone is told. One read; an error is the read's.
func Replaced(ctx context.Context, e Entry) (map[string]json.RawMessage, error) {
	var diff map[string]json.RawMessage
	if json.Unmarshal(e.Diff, &diff) != nil || len(diff) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	if _, refetch := diff["refetch"]; refetch {
		return map[string]json.RawMessage{}, nil
	}
	if id, ok := customFieldOf(diff); ok {
		value, found, err := latestFieldValue(ctx, e.ResourceUUID, e.Sequence, id)
		if err != nil || !found {
			return map[string]json.RawMessage{}, err
		}
		was, _ := json.Marshal(id)
		return map[string]json.RawMessage{"field_id": was, "value": value}, nil
	}
	fields := make([]string, 0, len(diff))
	for f := range diff {
		fields = append(fields, f)
	}
	sort.Strings(fields)
	return latestValues(ctx, e.ResourceUUID, e.Sequence, fields)
}

// customFieldOf is the field id of a custom field's change, which the log
// keeps in business/TaskField's shape, {"field_id", "value"}: a field id and
// a value, as business/Task's asTheTaskAnswerNamesIt reads it.
func customFieldOf(diff map[string]json.RawMessage) (string, bool) {
	raw, hasID := diff["field_id"]
	_, hasValue := diff["value"]
	if !hasID || !hasValue {
		return "", false
	}
	var id string
	if json.Unmarshal(raw, &id) != nil || id == "" {
		return "", false
	}
	return id, true
}
