package asana

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

// I1: an Asana task's dependencies (the tasks it waits on) come across; they
// were never asked for.
func TestATaskKeepsWhatItWaitsOn(t *testing.T) {
	q, _ := url.QueryUnescape(taskOptFields)
	if !strings.Contains(q, "dependencies.gid") {
		t.Fatalf("tasks aren't asked for their dependencies: %s", q)
	}
	var task asanaTask
	if err := json.Unmarshal([]byte(`{"gid":"9","name":"Ship","dependencies":[{"gid":"3"},{"gid":"4"}]}`), &task); err != nil {
		t.Fatal(err)
	}
	deps := buildSourceTask(&task, "p", nil, "").Dependencies
	if len(deps) != 2 || deps[0].BlockerSourceID != "3" || deps[1].BlockerSourceID != "4" {
		t.Fatalf("dependencies %+v", deps)
	}
}
