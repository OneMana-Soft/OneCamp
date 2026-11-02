package business

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
)

// heard is what one project was told.
type heard struct {
	project string
	kind    int8
	data    any
}

func hearProjects(t *testing.T, restricted map[string]bool) *[]heard {
	t.Helper()
	var got []heard
	s, r := sendToProject, restrictedAmong
	t.Cleanup(func() { sendToProject, restrictedAmong = s, r })
	sendToProject = func(_ context.Context, projectID string, kind int8, data any, _ string) {
		got = append(got, heard{projectID, kind, data})
	}
	restrictedAmong = func(ids []string) map[string]bool {
		out := map[string]bool{}
		for _, id := range ids {
			if restricted[id] {
				out[id] = true
			}
		}
		return out
	}
	return &got
}

// A logged change goes to its project's open views with its number in the
// log, what kind of change it was, the fields' new values and who made it, so
// a board shows a teammate's change without reading its list again.
func TestAWorkChangeGoesToItsProjectWithItsNumberAndValues(t *testing.T) {
	got := hearProjects(t, nil)
	diff := json.RawMessage(`{"task_status":"done"}`)
	PublishWorkChange(context.Background(), WorkChange{Sequence: 42, Kind: "task.updated", TaskUUID: "t1", ProjectUUID: "p1", Diff: diff, By: "u1"})
	want := []heard{{"p1", mqttStruct.MESSAGE_TASK_CHANGED, &mqttStruct.MqttTaskChanged{
		TaskUuid: "t1", ProjectUuid: "p1", Sequence: 42, Kind: "task.updated", Diff: diff, By: "u1"}}}
	if !reflect.DeepEqual(*got, want) {
		t.Errorf("told %+v, want %+v", *got, want)
	}
}

// A change to a task only a few may see names nothing on the project's topic,
// which every member hears: no task, no number, no values. Each view reads its
// own lists again through the filter (publishToProject's one rule).
func TestARestrictedTasksChangeIsToldAsRereadNamingNothing(t *testing.T) {
	got := hearProjects(t, map[string]bool{"secret": true})
	PublishWorkChange(context.Background(), WorkChange{Sequence: 43, Kind: "task.updated", TaskUUID: "secret", ProjectUUID: "p1",
		Diff: json.RawMessage(`{"task_name":"Layoffs"}`), By: "u1"})
	want := []heard{{"p1", mqttStruct.MESSAGE_TASK_CHANGED, &mqttStruct.MqttTaskChanged{ProjectUuid: "p1", Reread: true}}}
	if !reflect.DeepEqual(*got, want) {
		t.Errorf("told %+v, want %+v", *got, want)
	}
}

// A change with no project is told to nobody.
func TestAWorkChangeWithNoProjectIsToldToNobody(t *testing.T) {
	got := hearProjects(t, nil)
	PublishWorkChange(context.Background(), WorkChange{Sequence: 44, Kind: "task.updated", TaskUUID: "t1"})
	if len(*got) != 0 {
		t.Errorf("told %+v", *got)
	}
}
