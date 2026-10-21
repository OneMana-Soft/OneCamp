package models

import (
	"encoding/json"
	"strings"
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// What only an answer to the app carries (keys, types, custom field values,
// what its reader may do) never goes into the graph, from the task or from a
// task it holds, and the task the caller has keeps it.
func TestATaskWrittenBackCarriesNothingOnlyTheAppGets(t *testing.T) {
	sub := &dgraphStruct.DgraphTask{Uuid: "s", Key: "Q4-3", WorkType: &dgraphStruct.TaskWorkType{Name: "Bug"}, Restricted: true}
	task := &dgraphStruct.DgraphTask{
		Uuid: "t", Name: "Write the launch post", Key: "Q4-2",
		WorkType:   &dgraphStruct.TaskWorkType{ID: "x", Builtin: "milestone", Name: "Milestone", Icon: "milestone"},
		Fields:     map[string]json.RawMessage{"f": json.RawMessage(`1`)},
		Access:     &dgraphStruct.TaskAccess{},
		ParentTask: &dgraphStruct.DgraphTask{Uuid: "p", Key: "Q4-1"},
		SubTasks:   []*dgraphStruct.DgraphTask{sub},
		BlockedBy:  []*dgraphStruct.DgraphTask{{Uuid: "b", Key: "Q4-4"}},
		// The lock (WA2b) is the filter's, laid over an answer.
		Restricted: true,
	}
	raw, err := json.Marshal(forDgraph(task))
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"task_key", "task_work_type", "task_fields", "task_access", "task_restricted", "Q4-"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("the write carries %q: %s", leak, raw)
		}
	}
	if !strings.Contains(string(raw), "Write the launch post") || !strings.Contains(string(raw), `"task_uuid":"s"`) {
		t.Errorf("the write lost the task itself: %s", raw)
	}
	if task.Key != "Q4-2" || sub.Key != "Q4-3" || task.WorkType == nil {
		t.Error("stripping the write changed the caller's task")
	}
	plain := &dgraphStruct.DgraphTask{Uuid: "u", SubTasks: []*dgraphStruct.DgraphTask{{Uuid: "v"}}}
	if locked := (&dgraphStruct.DgraphTask{Uuid: "w", Restricted: true}); forDgraph(locked) == locked {
		t.Error("a task with only the lock wasn't stripped")
	}
	if forDgraph(plain) != plain {
		t.Error("a task with nothing to strip was copied")
	}
}
