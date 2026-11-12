package business

import (
	"fmt"
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	releaseModel "github.com/akashc777/OneCamp/models/postgres/Release"
	"github.com/google/uuid"
)

// Mark released moves on the tasks still to do, as burndown counts them:
// open ones, of those its admin sees. Done and cancelled ones stay.
func TestOnlyUnfinishedTasksMoveWhenAReleaseGoesOut(t *testing.T) {
	ids := []uuid.UUID{uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()}
	var members []releaseModel.Member
	for _, id := range ids {
		members = append(members, releaseModel.Member{Task: id})
	}
	seen := map[string]*dgraphStruct.DgraphTask{}
	for i, status := range []string{dgraphStruct.TASK_STATUS_TODO, dgraphStruct.TASK_STATUS_DONE, dgraphStruct.TASK_STATUS_CANCELED, dgraphStruct.TASK_STATUS_INPROGRESS} {
		seen[ids[i].String()] = &dgraphStruct.DgraphTask{Uuid: ids[i].String(), Status: status}
	}
	// ids[4] isn't seen: deleted, or in another project now.
	if got := fmt.Sprint(unfinished(members, seen)); got != fmt.Sprint([]uuid.UUID{ids[0], ids[3]}) {
		t.Errorf("unfinished %s, want the to-do and the started one", got)
	}
}
