package business

import (
	"context"
	domain "github.com/akashc777/OneCamp/domain/Reaction"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

func GetReactionNodeByDgraphUID(ctx context.Context, dgraphUID string) (dgraphReaction *dgraphStruct.DgraphReaction, err error) {
	dgraphReaction, err = domain.GetReactionNodeByDgraphUID(ctx, dgraphUID)

	if err != nil {
		helpers.MessageLogs.ErrorLog.Printf(
			"business/GetReactionNodeByDgraphUID Failed to get reaction from dgraph err: %+v",
			err)
		return
	}
	return
}
