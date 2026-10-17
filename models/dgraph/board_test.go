package models

import (
	"encoding/json"
	"strings"
	"testing"
)

// Open columns are read unsorted (Dgraph sends them all); a closed one is its
// newest closedLimit, held to the maximum, or whole when closedLimit is 0.
func TestBoardPlacesQuery(t *testing.T) {
	q := BoardPlacesQuery("project_tasks", TASK_LIVE_FILTER, 99999)
	if n := strings.Count(q, "(orderdesc: task_created_at, first: 2000)"); n != 2 {
		t.Fatalf("closed columns sorted with first %d times, want 2:\n%s", n, q)
	}
	if strings.Count(q, "orderdesc") != 2 || strings.Contains(q, "orderasc") {
		t.Fatalf("only the closed columns may sort:\n%s", q)
	}
	for _, c := range BoardColumns {
		for _, alias := range []string{"places_" + c.Key + ":", "places_" + c.Key + "_count:", "places_" + c.Key + "_custom:"} {
			if !strings.Contains(q, alias) {
				t.Errorf("no %s", alias)
			}
		}
	}
	if q := BoardPlacesQuery("user_tasks", TASK_LIVE_FILTER, 0); strings.Contains(q, "order") {
		t.Fatalf("closedLimit 0 reads every column whole:\n%s", q)
	}
}

func TestParseBoardPlacesAndColumnCounts(t *testing.T) {
	block := json.RawMessage(`{
		"project_uuid": "p",
		"places_todo": [{"uid": "0x1", "task_uuid": "a"}, {"uid": "0x2", "task_uuid": "b", "task_custom_status": "ready"}],
		"places_todo_count": 1500,
		"places_todo_custom": [{"@groupby": [{"task_custom_status": "ready", "count": 400}, {"task_custom_status": "gone", "count": 7}]}],
		"places_done_count": 3
	}`)
	cols, err := ParseBoardPlaces(block)
	if err != nil {
		t.Fatal(err)
	}
	if got := cols["todo"]; len(got.Places) != 2 || got.Count != 1500 || got.Custom["ready"] != 400 || got.Custom["gone"] != 7 {
		t.Fatalf("todo = %+v", got)
	}
	if cols["done"].Count != 3 || cols["backlog"] == nil {
		t.Fatalf("done %+v, backlog %+v", cols["done"], cols["backlog"])
	}
	counts := ColumnCounts(cols, []string{"ready", "qa"})
	// "gone" is a status the project no longer has: its tasks show in To do.
	want := map[string]int{"todo": 1100, "ready": 400, "qa": 0, "done": 3, "inProgress": 0, "backlog": 0, "inReview": 0, "canceled": 0}
	if len(counts) != len(want) {
		t.Fatalf("counts %v, want %v", counts, want)
	}
	for k, v := range want {
		if counts[k] != v {
			t.Errorf("%s: %d, want %d", k, counts[k], v)
		}
	}
	// My Tasks draws only the built-in columns.
	if mine := ColumnCounts(cols, nil); mine["todo"] != 1500 || len(mine) != 6 {
		t.Fatalf("My Tasks counts %v", mine)
	}
}
