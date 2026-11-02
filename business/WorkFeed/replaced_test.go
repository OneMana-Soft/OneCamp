package business

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

// replacedReads stands in for the store's two reads and counts them.
type replacedReads struct {
	fields   []string
	fieldIDs []string
	resource string
	before   int64
}

func readsReplaced(t *testing.T, values map[string]json.RawMessage, field json.RawMessage, found bool, err error) *replacedReads {
	r := &replacedReads{}
	savedValues, savedField := latestValues, latestFieldValue
	t.Cleanup(func() { latestValues, latestFieldValue = savedValues, savedField })
	latestValues = func(_ context.Context, resource string, before int64, fields []string) (map[string]json.RawMessage, error) {
		r.resource, r.before, r.fields = resource, before, fields
		return values, err
	}
	latestFieldValue = func(_ context.Context, resource string, before int64, id string) (json.RawMessage, bool, error) {
		r.resource, r.before, r.fieldIDs = resource, before, append(r.fieldIDs, id)
		return field, found, err
	}
	return r
}

// linear-39 round 2: what a change replaced is read in ONE read of the
// change's own resource, before its number: its fields by name, or a custom
// field's change by its field id, answered in the change's own shape.
func TestReplacedReadsTheChangesOwnFieldsBeforeIt(t *testing.T) {
	ctx := context.Background()
	e := Entry{Sequence: 42, ResourceUUID: "t1", Diff: json.RawMessage(`{"task_status":"done","task_priority":"high"}`)}
	r := readsReplaced(t, map[string]json.RawMessage{"task_priority": json.RawMessage(`"low"`)}, nil, false, nil)
	got, err := Replaced(ctx, e)
	if err != nil || !reflect.DeepEqual(got, map[string]json.RawMessage{"task_priority": json.RawMessage(`"low"`)}) {
		t.Errorf("replaced %v, %v", got, err)
	}
	if r.resource != "t1" || r.before != 42 || !reflect.DeepEqual(r.fields, []string{"task_priority", "task_status"}) || r.fieldIDs != nil {
		t.Errorf("read %+v, want t1's task_priority and task_status before 42, in one read", r)
	}

	e.Diff = json.RawMessage(`{"field_id":"f-77","value":{"amount":12.5}}`)
	r = readsReplaced(t, nil, json.RawMessage(`{"amount":3}`), true, nil)
	got, err = Replaced(ctx, e)
	if want := map[string]json.RawMessage{"field_id": json.RawMessage(`"f-77"`), "value": json.RawMessage(`{"amount":3}`)}; err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("a custom field's change replaced %v (err %v), want %v", got, err, want)
	}
	if !reflect.DeepEqual(r.fieldIDs, []string{"f-77"}) || r.fields != nil {
		t.Errorf("read %+v, want field f-77 alone", r)
	}
	readsReplaced(t, nil, nil, false, nil)
	if got, err := Replaced(ctx, e); err != nil || len(got) != 0 {
		t.Errorf("a custom field never set before replaced %v, %v; want nothing", got, err)
	}

	for _, diff := range []string{`{}`, `null`, `[]`, ``} {
		r = readsReplaced(t, nil, nil, false, nil)
		if got, err := Replaced(ctx, Entry{Sequence: 42, ResourceUUID: "t1", Diff: json.RawMessage(diff)}); err != nil || len(got) != 0 || r.fields != nil || r.fieldIDs != nil {
			t.Errorf("diff %q replaced %v (err %v) after reads %+v; want nothing, unread", diff, got, err, r)
		}
	}

	down := errors.New("the log is down")
	readsReplaced(t, nil, nil, false, down)
	e.Diff = json.RawMessage(`{"task_name":"Ship"}`)
	if _, err := Replaced(ctx, e); !errors.Is(err, down) {
		t.Errorf("a read that failed answered %v, want its error", err)
	}
}
