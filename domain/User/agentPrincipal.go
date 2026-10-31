package domain

import (
	"context"

	"github.com/google/uuid"
)

// IsAgentPrincipal says whether a user is an AI teammate's own principal (a
// bot of kind agent, ClassifyBot), which work is given to as it is assigned
// in the app. Anyone not found is not one.
func IsAgentPrincipal(ctx context.Context, id uuid.UUID) bool {
	row, err := GetUserByUUID(ctx, id)
	return err == nil && row != nil && row.IsBot && ClassifyBot(row.EmailID) == BotKindAgent
}
