package business

import (
	"testing"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

// I1: a source task's dependencies are kept with its mapping, to be linked
// once every task has landed (the dependencies stage), each one once and none
// on itself.
func TestATaskRemembersWhatItWaitsOnUntilAllTasksLand(t *testing.T) {
	md := taskMapMetadata(&importProvider.SourceTask{SourceID: "T-9", Name: "Ship", ProjectSourceID: "P",
		Dependencies: []importProvider.SourceDependency{{BlockerSourceID: "T-3"}, {BlockerSourceID: "T-3", Kind: "fs"}, {BlockerSourceID: "T-9"}, {BlockerSourceID: "T-4", Kind: "ss", LagDays: 2}}}, "todo", "medium")
	deps, _ := md["blocked_by"].([]importProvider.SourceDependency)
	if len(deps) != 2 || deps[0] != (importProvider.SourceDependency{BlockerSourceID: "T-3", Kind: "fs"}) || deps[1] != (importProvider.SourceDependency{BlockerSourceID: "T-4", Kind: "ss", LagDays: 2}) {
		t.Fatalf("blocked_by %+v", md["blocked_by"])
	}
	if _, ok := taskMapMetadata(&importProvider.SourceTask{SourceID: "T-1"}, "todo", "low")["blocked_by"]; ok {
		t.Fatal("a task with no dependencies keeps an empty list")
	}
}
