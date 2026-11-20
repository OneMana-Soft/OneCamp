package business

import (
	"context"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	triageBusiness "github.com/akashc777/OneCamp/business/Triage"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// Seam for tests: triage taking in a task just imported.
var arriveInTriage = triageBusiness.Arrive

// arriveFromImport puts an imported task that waited in its source's triage
// (importProvider.InTriage) into its project's, when the project has triage
// on, as asked for by whoever made it there (creator, nil when nobody the
// workspace knows). The status map still files it (Linear's default: backlog),
// so with triage off it is the team's work as before. held is it waiting.
func arriveFromImport(ctx context.Context, st *importProvider.SourceTask, kinds map[string]string, projectUUID, taskUUID uuid.UUID, creator *userModels.UserInfo) (held bool) {
	if !importProvider.InTriage(st.Status, kinds) {
		return false
	}
	var from triageBusiness.Requester
	if creator != nil && creator.UserPostgresInfo.Id != uuid.Nil {
		id := creator.UserPostgresInfo.Id
		from.UserUUID = &id
	}
	return arriveInTriage(ctx, "business/Import importOneTask", projectUUID, taskUUID, triageBusiness.FromImport, from)
}
