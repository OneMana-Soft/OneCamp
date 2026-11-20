package business

import (
	"context"

	domain "github.com/akashc777/OneCamp/domain/Project"
	"github.com/google/uuid"
)

// TakenProjectNames is which of these names a project in the team has
// (archived ones too: a name stays its project's), in one read.
func TakenProjectNames(ctx context.Context, team uuid.UUID, names []string) (map[string]bool, error) {
	return domain.TakenProjectNames(ctx, team, names)
}
