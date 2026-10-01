package business

import (
	"context"

	slackImportBusiness "github.com/akashc777/OneCamp/business/SlackImport"
	"github.com/google/uuid"
)

// init wires the legacy SlackImport.RollbackImport into the generic
// rollback dispatcher so Slack jobs route to the existing channel-and-
// message rollback. We do this from the Import package (which depends
// on SlackImport) rather than from SlackImport (which would create a
// cycle by depending on Import).
func init() {
	invokeSlackRollback = func(ctx context.Context, jobId uuid.UUID) error {
		return slackImportBusiness.RollbackImport(ctx, jobId)
	}
}
