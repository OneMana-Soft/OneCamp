package clickup

import (
	"encoding/json"
	"testing"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

// I1: a ClickUp task keeps what it waits on, its time estimate and its sprint
// points; none of them came across.
func TestATaskKeepsItsWaitsEstimateAndPoints(t *testing.T) {
	var task clickupTask
	if err := json.Unmarshal([]byte(`{"id":"b","name":"Ship","status":{"status":"to do","type":"open"},
		"time_estimate":5400000,"points":3,
		"dependencies":[
			{"task_id":"b","depends_on":"a","type":1},
			{"task_id":"c","depends_on":"b","type":1}]}`), &task); err != nil {
		t.Fatal(err)
	}
	st := New().taskToSourceTask(task)
	if len(st.Dependencies) != 1 || st.Dependencies[0].BlockerSourceID != "a" {
		t.Fatalf("b waits on %+v, want only a (c waits on b)", st.Dependencies)
	}
	if st.EstimateMinutes != 90 {
		t.Fatalf("estimate %d minutes, want 90", st.EstimateMinutes)
	}
	var points *importProvider.SourceFieldValue
	for i := range st.Fields {
		if st.Fields[i].Field.Type == importProvider.FieldNumber && st.Fields[i].Field.Name == "Points" {
			points = &st.Fields[i]
		}
	}
	if points == nil || points.Value != 3.0 {
		t.Fatalf("points %+v in %+v", points, st.Fields)
	}
}
