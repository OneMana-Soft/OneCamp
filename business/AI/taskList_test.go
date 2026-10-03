package business

import (
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

func TestTaskListRowsForAView(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	past, future := now.Add(-48*time.Hour), now.Add(48*time.Hour)
	custom := "QA"
	tasks := []*dgraphStruct.DgraphTask{
		{Uuid: "a", Name: "Ship it", Status: "inProgress", DueDate: &past, Project: &dgraphStruct.DgraphProject{Name: "Q4"}},
		{Uuid: "b", Name: " ", Status: "done", DueDate: &past},
		{Uuid: "c", Name: "Review", Status: "inReview", CustomStatusName: &custom, DueDate: &future},
		{Uuid: "d", Name: "Dropped", Status: "canceled", DueDate: &past},
		nil,
	}
	got := taskList("Your tasks", tasks, 2, "https://onecamp.acme.com", now)
	if got.Total != 4 || len(got.Tasks) != 4 {
		t.Fatalf("total %d rows %d", got.Total, len(got.Tasks))
	}
	if !got.Tasks[0].Overdue || got.Tasks[0].Project != "Q4" || got.Tasks[0].URL != "https://onecamp.acme.com/app/task/a" {
		t.Errorf("row 0: %+v", got.Tasks[0])
	}
	if got.Tasks[1].Overdue || got.Tasks[1].Name != "(untitled task)" {
		t.Errorf("a done task is never overdue, and a blank name reads as untitled: %+v", got.Tasks[1])
	}
	if got.Tasks[2].Status != "QA" || got.Tasks[2].Overdue {
		t.Errorf("custom status shows by name: %+v", got.Tasks[2])
	}
	if got.Tasks[3].Overdue {
		t.Error("a canceled task is finished, never overdue")
	}
}
