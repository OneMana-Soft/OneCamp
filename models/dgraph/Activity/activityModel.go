package Activity

import (
	"context"
	"encoding/json"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

type RawReactionsActivity struct {
	dgraphStruct.DgraphReaction
	Chat    []*dgraphStruct.DgraphChat    `json:"chat,omitempty"`
	Post    []*dgraphStruct.DgraphPost    `json:"post,omitempty"`
	Comment []*dgraphStruct.DgraphComment `json:"comment,omitempty"`
}

type ReactionsActivity struct {
	dgraphStruct.DgraphReaction
	Chat    *dgraphStruct.DgraphChat    `json:"chat,omitempty"`
	Post    *dgraphStruct.DgraphPost    `json:"post,omitempty"`
	Comment *dgraphStruct.DgraphComment `json:"comment,omitempty"`
}

func GetComments(ctx context.Context, query string, variables map[string]string) (dgraphComment []*dgraphStruct.DgraphComment, actualLen int, err error) {
	txn := dgraphInit.DgraphClient.NewTxn()

	resp, err := txn.QueryWithVars(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetMentions failed to get user mention err: %+v",
			err)
		return
	}

	type Comments struct {
		CommentInfo []*dgraphStruct.DgraphComment `json:"comments"`
	}

	type RawComments struct {
		CommentInfo []Comments `json:"commentInfo"`
	}

	var commentInfo RawComments

	err = json.Unmarshal(resp.Json, &commentInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetMentions failed to unmarshal response json err: %+v",
			err)
		return
	}

	if len(commentInfo.CommentInfo) == 0 {
		return
	}

	actualLen = len(commentInfo.CommentInfo[0].CommentInfo)

	// Filter out orphaned comments where all parent content is nil (deleted/filtered)
	for _, c := range commentInfo.CommentInfo[0].CommentInfo {
		if c.Post == nil && c.Chat == nil && c.Task == nil && c.Doc == nil {
			continue
		}
		dgraphComment = append(dgraphComment, c)
	}
	actualLen = len(dgraphComment)
	return
}

func GetMentions(ctx context.Context, query string, variables map[string]string) (dgraphMention []*dgraphStruct.DgraphMentions, actualLen int, err error) {
	txn := dgraphInit.DgraphClient.NewTxn()

	resp, err := txn.QueryWithVars(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetMentions failed to get user mention err: %+v",
			err)
		return
	}

	type MentionsRawInfo struct {
		Mentions []*dgraphStruct.DgraphMentions `json:"mentions"`
	}

	type Mentions struct {
		MentionInfo []MentionsRawInfo `json:"mentionInfo"`
	}

	var mentionInfo Mentions

	err = json.Unmarshal(resp.Json, &mentionInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetMentions failed to unmarshal response json err: %+v",
			err)
		return
	}

	if len(mentionInfo.MentionInfo) == 0 {
		return
	}

	actualLen = len(mentionInfo.MentionInfo[0].Mentions)

	// Filter out orphaned mentions where all parent content is nil (deleted/filtered/self-mention)
	for _, m := range mentionInfo.MentionInfo[0].Mentions {
		if m.Post == nil && m.Chat == nil && m.Comment == nil {
			continue
		}
		dgraphMention = append(dgraphMention, m)
	}
	actualLen = len(dgraphMention)

	return
}

func GetReactions(ctx context.Context, query string, variables map[string]string) (dgraphReaction []*ReactionsActivity, actualLen int, err error) {
	txn := dgraphInit.DgraphClient.NewTxn()

	resp, err := txn.QueryWithVars(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetReactions failed to get user reaction err: %+v",
			err)
		return
	}

	type ReactionsRawInfo struct {
		Reactions []*RawReactionsActivity `json:"reactions"`
	}
	type Reactions struct {
		ReactionInfo []ReactionsRawInfo `json:"reactionInfo"`
	}

	var reactionInfo Reactions

	err = json.Unmarshal(resp.Json, &reactionInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetReactions failed to unmarshal response json err: %+v",
			err)
		return
	}

	if len(reactionInfo.ReactionInfo) == 0 {
		return
	}

	for _, rea := range reactionInfo.ReactionInfo[0].Reactions {

		filteredRea := &ReactionsActivity{
			DgraphReaction: dgraphStruct.DgraphReaction{
				AddedAt:   rea.AddedAt,
				EmojiUuid: rea.EmojiUuid,
				AddedBy:   rea.AddedBy,
			},
		}

		if rea.Post != nil {
			filteredRea.Post = rea.Post[0]
		}

		if rea.Chat != nil {
			filteredRea.Chat = rea.Chat[0]
		}

		if rea.Comment != nil {
			filteredRea.Comment = rea.Comment[0]
		}

		// Skip reactions where the parent content was deleted.
		// For comment reactions, the comment itself may exist while its parent
		// (post/chat/task/doc) was deleted, so we must check the comment's parent fields.
		if filteredRea.Post == nil && filteredRea.Chat == nil && filteredRea.Comment == nil {
			continue
		}
		if filteredRea.Comment != nil &&
			filteredRea.Comment.Post == nil &&
			filteredRea.Comment.Chat == nil &&
			filteredRea.Comment.Task == nil &&
			filteredRea.Comment.Doc == nil {
			continue
		}

		dgraphReaction = append(dgraphReaction, filteredRea)
	}

	actualLen = len(dgraphReaction)

	return
}

func GetUnifiedActivity(ctx context.Context, query string, variables map[string]string) (
	mentions []*dgraphStruct.DgraphMentions,
	comments []*dgraphStruct.DgraphComment,
	reactions []*ReactionsActivity,
	err error,
) {
	txn := dgraphInit.DgraphClient.NewTxn()

	resp, err := txn.QueryWithVars(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetReactions failed to get user reaction err: %+v",
			err)
		return
	}

	type UnifiedResponse struct {
		MentionInfo []struct {
			Mentions []*dgraphStruct.DgraphMentions `json:"mentions"`
		} `json:"mentionInfo"`
		CommentInfo []struct {
			Comments []*dgraphStruct.DgraphComment `json:"comments"`
		} `json:"commentInfo"`
		ReactionInfo []struct {
			Reactions []*RawReactionsActivity `json:"reactions"`
		} `json:"reactionInfo"`
	}

	var unifiedResp UnifiedResponse
	err = json.Unmarshal(resp.Json, &unifiedResp)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetUnifiedActivity Failed to unmarshal response err: %+v", err)
		return
	}

	if len(unifiedResp.MentionInfo) > 0 {
		// Filter out orphaned mentions where all parent content is nil
		for _, m := range unifiedResp.MentionInfo[0].Mentions {
			if m.Post == nil && m.Chat == nil && m.Comment == nil {
				continue
			}
			mentions = append(mentions, m)
		}
	}
	if len(unifiedResp.CommentInfo) > 0 {
		// Filter out orphaned comments where all parent content is nil
		for _, c := range unifiedResp.CommentInfo[0].Comments {
			if c.Post == nil && c.Chat == nil && c.Task == nil && c.Doc == nil {
				continue
			}
			comments = append(comments, c)
		}
	}
	if len(unifiedResp.ReactionInfo) > 0 {
		// Convert RawReactions to CleanReactions (filtering empty parents)
		for _, rea := range unifiedResp.ReactionInfo[0].Reactions {

			filteredRea := &ReactionsActivity{
				DgraphReaction: dgraphStruct.DgraphReaction{
					AddedAt:   rea.AddedAt,
					EmojiUuid: rea.EmojiUuid,
					AddedBy:   rea.AddedBy,
				},
			}
			if rea.Post != nil {
				filteredRea.Post = rea.Post[0]
			}
			if rea.Chat != nil {
				filteredRea.Chat = rea.Chat[0]
			}
			if rea.Comment != nil {
				filteredRea.Comment = rea.Comment[0]
			}

			// Skip reactions where the parent content was deleted.
			// For comment reactions, the comment itself may exist while its parent
			// (post/chat/task/doc) was deleted, so we must check the comment's parent fields.
			if filteredRea.Post == nil && filteredRea.Chat == nil && filteredRea.Comment == nil {
				continue
			}
			if filteredRea.Comment != nil &&
				filteredRea.Comment.Post == nil &&
				filteredRea.Comment.Chat == nil &&
				filteredRea.Comment.Task == nil &&
				filteredRea.Comment.Doc == nil {
				continue
			}

			reactions = append(reactions, filteredRea)
		}
	}

	return

}

func GetTotalUnreadActivityCount(ctx context.Context, query string, variables map[string]string) (totalCount uint64, err error) {
	txn := dgraphInit.DgraphClient.NewTxn()

	resp, err := txn.QueryWithVars(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetTotalUnreadActivityCount failed to execute query err: %+v", err)
		return
	}

	type CountResp struct {
		MentionInfo []struct {
			Count uint64 `json:"count"`
		} `json:"mentionInfo"`
		CommentInfo []struct {
			Count uint64 `json:"count"`
		} `json:"commentInfo"`
		ReactionInfo []struct {
			Count uint64 `json:"count"`
		} `json:"reactionInfo"`
	}

	var countResp CountResp
	err = json.Unmarshal(resp.Json, &countResp)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetTotalUnreadActivityCount failed to unmarshal response err: %+v", err)
		return
	}

	if len(countResp.MentionInfo) > 0 {
		totalCount += countResp.MentionInfo[0].Count
	}
	if len(countResp.CommentInfo) > 0 {
		totalCount += countResp.CommentInfo[0].Count
	}
	if len(countResp.ReactionInfo) > 0 {
		totalCount += countResp.ReactionInfo[0].Count
	}

	return
}

type UnifiedActivityItem struct {
	ActivityType string                       `json:"activity_type"` // "MENTION", "COMMENT", "REACTION"
	Time         string                       `json:"time"`
	Priority     string                       `json:"priority,omitempty"` // "high" | "normal" | "low"
	Mention      *dgraphStruct.DgraphMentions `json:"mention,omitempty"`
	Comment      *dgraphStruct.DgraphComment  `json:"comment,omitempty"`
	Reaction     *ReactionsActivity           `json:"reaction,omitempty"`
	// ActorKind is who did it: "person", "agent" or "app", so a reader can
	// see what people did apart from what agents and automations did.
	ActorKind string `json:"actor_kind"`
}

type UnifiedActivityPagination struct {
	Activities []UnifiedActivityItem `json:"activities"`
	HasMore    bool                  `json:"has_more"`
}
