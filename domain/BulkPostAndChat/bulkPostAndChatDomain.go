package domain

import (
	"context"
	"fmt"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/BulkPostAndChat"
	models "github.com/akashc777/OneCamp/models/postgres/BulkPostAndChatModel"
)

func BulkAddChatAndPostToDgraph(ctx context.Context, dgraphPosts []*dgraphStruct.DgraphPost, dgraphDMs []*dgraphStruct.DgraphDm) (dgraphChatAndPostUUID []string, err error) {

	// Build query
	queryBuilder := strings.Builder{}
	queryBuilder.WriteString("query {\n")
	for i := range dgraphDMs {
		queryBuilder.WriteString(fmt.Sprintf(
			`dm_%d as var(func: eq(dm_grouping_id, %s))`+"\n"+
				`ch_%d as var(func: eq(chat_uuid, %s))`+"\n",
			i, dgraphDMs[i].GroupingId, i, dgraphDMs[i].Chats[0].Uuid))
	}
	for i := range dgraphPosts {
		queryBuilder.WriteString(fmt.Sprintf(
			`post_%d as var(func: eq(post_uuid, %s))`+"\n",
			i, dgraphPosts[i].Uuid))

	}
	queryBuilder.WriteString("}")

	dgraphChatAndPostUUID, err = dgraphModels.BulkAddChatAndPostToDgraph(ctx, queryBuilder.String(), dgraphPosts, dgraphDMs)

	if err != nil {
		helpers.MessageLogs.ErrorLog.Printf(
			"domain/BulkAddChatAndPostToDgraph Failed to bulk add post and chat to dgraph err: %+v",
			err,
		)
		return
	}

	return

}

func BulkAddChatAndPostToPostgres(postUUIDs []string, channelUUIDs []string, chatUUIDs []string, chatGrpIDs []string, userUUID string) (err error) {

	// Two independent positional pairings live below: post i with channel i, and chat i with
	// grouping id i. Both are built by a caller that accumulates them under different
	// branches, so state the invariant here where it is relied upon.
	if err := helpers.RequireSameLength("domain/BulkAddChatAndPostToPostgres posts",
		helpers.NamedLen{Name: "postUUIDs", Len: len(postUUIDs)},
		helpers.NamedLen{Name: "channelUUIDs", Len: len(channelUUIDs)},
	); err != nil {
		return err
	}
	if err := helpers.RequireSameLength("domain/BulkAddChatAndPostToPostgres chats",
		helpers.NamedLen{Name: "chatUUIDs", Len: len(chatUUIDs)},
		helpers.NamedLen{Name: "chatGrpIDs", Len: len(chatGrpIDs)},
	); err != nil {
		return err
	}

	var queries []string
	var values []interface{}
	placeholderOffset := 0

	if len(postUUIDs) > 0 {
		postsQuery := "INSERT INTO posts (id, post_channel, created_by) VALUES "
		postsPlaceholders := make([]string, 0, len(postUUIDs))
		for i := 0; i < len(postUUIDs); i++ {
			postsPlaceholders = append(postsPlaceholders, fmt.Sprintf("($%d, $%d, $%d)", placeholderOffset+1, placeholderOffset+2, placeholderOffset+3))
			values = append(values, postUUIDs[i], channelUUIDs[i], userUUID)
			placeholderOffset += 3
		}
		postsQuery += strings.Join(postsPlaceholders, ",")
		//+ " RETURNING (SELECT COUNT(*) FROM posts WHERE id IN (" + strings.Join(postsPlaceholders, ",") + "))"
		queries = append(queries, postsQuery)
	}

	if len(chatUUIDs) > 0 {
		chatsQuery := "INSERT INTO chats (id, created_by, grp_id) VALUES "
		chatsPlaceholders := make([]string, 0, len(chatUUIDs))
		for i := 0; i < len(chatUUIDs); i++ {
			chatsPlaceholders = append(chatsPlaceholders, fmt.Sprintf("($%d, $%d, $%d)", placeholderOffset+1, placeholderOffset+2, placeholderOffset+3))
			values = append(values, chatUUIDs[i], userUUID, chatGrpIDs[i])
			placeholderOffset += 3
		}
		chatsQuery += strings.Join(chatsPlaceholders, ",")
		queries = append(queries, chatsQuery)
	}

	combinedQuery := strings.Join(queries, "; ")

	err = models.BulkInsertChatsAndPosts(combinedQuery, values)

	if err != nil {
		helpers.MessageLogs.ErrorLog.Printf(
			"domain/BulkAddChatAndPostToPostgres Failed to bulk add post and chat to postgres err: %+v",
			err,
		)
		return
	}

	return
}
