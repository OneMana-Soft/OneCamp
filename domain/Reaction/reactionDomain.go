package domain

import (
	"context"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Reaction"
)

func GetReactionNodeByDgraphUID(ctx context.Context, dgraphUUID string) (dgraphReaction *dgraphStruct.DgraphReaction, err error) {

	variables := make(map[string]string)
	variables["$Id"] = dgraphUUID
	query := `query ReactionInfo($Id: string){
				reactionInfo(func: uid($Id)) {
					uid
					reaction_added_at
					reaction_emoji_id
					reaction_added_by {
						uid
						user_uuid
					}
					reaction_on_content_added_by {
						uid
						user_uuid
					}
					
				}
			}`

	dgraphReaction, err = dgraphModels.GetDgraphPostInfoByUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetReactionNodeByDgraphUID Failed to get reaction in dgraph err: %+v",
			err,
		)
		return
	}

	return
}
