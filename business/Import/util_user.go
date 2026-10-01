package business

import (
	"context"

	userBusiness "github.com/akashc777/OneCamp/business/User"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// getDgraphUserDirect is a thin wrapper over userBusiness.GetDgraphUserInfoByUUID.
// Lifted into its own file so team_project_stages.go can call it without
// pulling in the full user_resolver helper graph.
func getDgraphUserDirect(ctx context.Context, ocUUID uuid.UUID) (*dgraphStruct.DgraphUser, error) {
	return userBusiness.GetDgraphUserInfoByUUID(ctx, ocUUID.String())
}
