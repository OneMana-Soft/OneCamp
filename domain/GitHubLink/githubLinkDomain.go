package domain

import (
	"context"

	models "github.com/akashc777/OneCamp/models/postgres/GitHubLink"
	"github.com/google/uuid"
)

// UpdateAutomationRules persists a JSON-serialised automation rules
// blob for a single github_link. The model layer enforces the SQL
// shape; the controller / business layer only deals with parsed maps.
func UpdateAutomationRules(ctx context.Context, linkId uuid.UUID, rulesJSON string) error {
	return models.UpdateAutomationRules(ctx, linkId, rulesJSON)
}

// UpdateBranchFormat persists the branch-naming template for a link.
func UpdateBranchFormat(ctx context.Context, linkId uuid.UUID, branchFormat string) error {
	return models.UpdateBranchFormat(ctx, linkId, branchFormat)
}
