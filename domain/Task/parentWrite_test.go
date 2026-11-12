package Domain

import (
	"encoding/json"
	"testing"
	"time"
)

// Re-parenting writes the one relation (task_parent_task, task_sub_tasks)
// and the stamps that make two moves which meet conflict, nothing else.
func TestParentWriteMovesTheOneRelation(t *testing.T) {
	at := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	stamp := `"task_updated_at":"2026-11-06T09:00:00Z"`
	for _, c := range []struct {
		name, old, parent, set, del string
	}{
		{"from one parent to another", "0x2", "0x3",
			`[{"task_parent_task":{"uid":"0x3"},` + stamp + `,"uid":"0x1"},{"task_sub_tasks":[{"uid":"0x1"}],` + stamp + `,"uid":"0x3"},{` + stamp + `,"uid":"0x2"}]`,
			`[{"task_sub_tasks":[{"uid":"0x1"}],"uid":"0x2"}]`},
		{"from the top", "", "0x3",
			`[{"task_parent_task":{"uid":"0x3"},` + stamp + `,"uid":"0x1"},{"task_sub_tasks":[{"uid":"0x1"}],` + stamp + `,"uid":"0x3"}]`,
			`null`},
		{"out to the top", "0x2", "",
			`[{` + stamp + `,"uid":"0x1"},{` + stamp + `,"uid":"0x2"}]`,
			`[{"task_parent_task":{"uid":"0x2"},"uid":"0x1"},{"task_sub_tasks":[{"uid":"0x1"}],"uid":"0x2"}]`},
	} {
		set, del := parentWrite("0x1", c.old, c.parent, at)
		gotSet, _ := json.Marshal(set)
		gotDel, _ := json.Marshal(del)
		if string(gotSet) != c.set {
			t.Errorf("%s: sets %s, want %s", c.name, gotSet, c.set)
		}
		if string(gotDel) != c.del {
			t.Errorf("%s: deletes %s, want %s", c.name, gotDel, c.del)
		}
	}
}
