package Domain

import (
	"context"

	"github.com/akashc777/OneCamp/models/dgexec"
)

// LiveProjects is which of the projects are there and not deleted, by id, in
// one read of the graph (the Cycle Scheduler leaves a deleted project's
// cycles alone).
func LiveProjects(ctx context.Context, ids []string) (map[string]bool, error) {
	return dgexec.LiveUUIDs(ctx, "project_uuid", "project_deleted_at", ids)
}
