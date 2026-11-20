package business

import (
	"context"
	"testing"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	triageBusiness "github.com/akashc777/OneCamp/business/Triage"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// An imported task that waited in its source's triage (Linear's) waits in
// the project's, as the person who made it there asked for it; the rest of
// an import is the team's work at once (linear-21 H1). The Linear default
// map still files a triage issue as backlog.
func TestAnImportedTaskWaitingInItsSourcesTriageWaitsInTheProjects(t *testing.T) {
	type arrival struct {
		project, task uuid.UUID
		src           triageBusiness.Source
		from          triageBusiness.Requester
	}
	var got []arrival
	was := arriveInTriage
	t.Cleanup(func() { arriveInTriage = was })
	arriveInTriage = func(_ context.Context, _ string, project, task uuid.UUID, src triageBusiness.Source, from triageBusiness.Requester) bool {
		got = append(got, arrival{project, task, src, from})
		return true
	}
	creator := &userModels.UserInfo{}
	creator.UserPostgresInfo.Id = uuid.New()
	project, task := uuid.New(), uuid.New()
	kinds := map[string]string{"inbox": "triage"}
	if held := arriveFromImport(context.Background(), &importProvider.SourceTask{Status: "Todo"}, kinds, project, task, creator); held || len(got) != 0 {
		t.Fatalf("a todo task waits: %v %+v", held, got)
	}
	if held := arriveFromImport(context.Background(), &importProvider.SourceTask{Status: "Inbox"}, kinds, project, task, creator); !held || len(got) != 1 {
		t.Fatalf("a triage task: %v %+v", held, got)
	}
	a := got[0]
	if a.project != project || a.task != task || a.src != triageBusiness.FromImport || a.from.UserUUID == nil || *a.from.UserUUID != creator.UserPostgresInfo.Id {
		t.Errorf("arrived as %+v", a)
	}
	if held := arriveFromImport(context.Background(), &importProvider.SourceTask{Status: "triage"}, nil, project, task, nil); !held || got[1].from.UserUUID != nil {
		t.Errorf("no creator: %v %+v", held, got)
	}
}
