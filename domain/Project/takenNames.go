package Domain

import (
	"context"

	models "github.com/akashc777/OneCamp/models/postgres/Project"
	"github.com/google/uuid"
)

// TakenProjectNamesQuery is which of these names ($2) a project in the team
// ($1) has, archived ones too ((project_name, team_id) is unique), in one
// statement (CheckIfProjectExistByProjectNameAndTeamID asks for one).
const TakenProjectNamesQuery = `SELECT DISTINCT project_name FROM projects WHERE team_id = $1 AND project_name = ANY($2)`

// TakenProjectNames is which of these names a project in the team has, in
// one read (the text here, run by the model).
func TakenProjectNames(ctx context.Context, team uuid.UUID, names []string) (map[string]bool, error) {
	return models.TakenProjectNames(ctx, TakenProjectNamesQuery, team, names)
}
