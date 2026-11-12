package models

import (
	"encoding/json"
	"strings"
	"testing"

	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
)

// PR2 (the BE audit's #32): many tasks' search documents change in one bulk
// request, an update line and a doc line for each.
func TestManyTasksUpdateInOneBulkBody(t *testing.T) {
	body, err := bulkUpdateBody([]*openSearchStruct.OpenSearchTask{{Uuid: "a", TaskStatus: "done"}, {Uuid: "b", TaskPriority: "high"}, nil, {Uuid: ""}})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("two tasks, two lines each: %q", body)
	}
	var head struct {
		Update struct {
			Index string `json:"_index"`
			ID    string `json:"_id"`
		} `json:"update"`
	}
	if err := json.Unmarshal([]byte(lines[2]), &head); err != nil || head.Update.Index != "tasks" || head.Update.ID != "b" {
		t.Fatalf("the second task's line: %s %v", lines[2], err)
	}
	if !strings.Contains(lines[1], `"task_status":"done"`) || !strings.HasPrefix(lines[1], `{"doc":`) {
		t.Fatalf("the first task's doc: %s", lines[1])
	}
	if body, _ := bulkUpdateBody(nil); body != "" {
		t.Fatalf("nothing to write: %q", body)
	}
}

// UX-PROJECTS-H1-BE: a field taken off a task (No priority) is sent as null,
// since a field an update leaves out keeps its old value; the rest are sent
// as set.
func TestAFieldTakenOffIsSentEmpty(t *testing.T) {
	body, err := bulkUpdateBody([]*openSearchStruct.OpenSearchTask{{Uuid: "a", TaskUpdatedAt: 7}}, "task_priority")
	if err != nil {
		t.Fatal(err)
	}
	doc := strings.Split(strings.TrimRight(body, "\n"), "\n")[1]
	if !strings.Contains(doc, `"task_priority":null`) || !strings.Contains(doc, `"task_id":"a"`) || !strings.Contains(doc, `"updated_date":7`) {
		t.Fatalf("doc: %s", doc)
	}
}
