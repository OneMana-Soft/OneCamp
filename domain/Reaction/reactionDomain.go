package domain

import (
	"context"
	"fmt"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Reaction"
)

// GetReactionOnParent reads the reaction reactionUID only through the edge
// its parent holds it by (comment_reactions, post_reactions or
// chat_reactions), so a uid that is not a reaction on that parent reads as
// none (nil). It reads the node's types and who added it, for the caller to
// judge. Both uids must already be uids.
func GetReactionOnParent(ctx context.Context, edge, parentUID, reactionUID string) (*dgraphStruct.DgraphReaction, error) {
	switch edge {
	case "comment_reactions", "post_reactions", "chat_reactions":
	default:
		return nil, fmt.Errorf("no reactions edge %q", edge)
	}

	variables := map[string]string{
		"$parent":   parentUID,
		"$reaction": reactionUID,
	}
	query := `query ReactionOnParent($parent: string, $reaction: string){
				on(func: uid($parent)) {
					reactions: ` + edge + ` @filter(uid($reaction)) {
						uid
						dgraph.type
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
				}
			}`

	reaction, err := dgraphModels.GetReactionOnParent(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetReactionOnParent Failed to get reaction in dgraph err: %+v",
			err,
		)
		return nil, err
	}
	return reaction, nil
}
