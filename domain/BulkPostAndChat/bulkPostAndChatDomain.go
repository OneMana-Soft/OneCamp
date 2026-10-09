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

	dgraphChatAndPostUUID, err = dgraphModels.BulkAddChatAndPostToDgraph(ctx, forwardUpsertQuery(dgraphPosts, dgraphDMs), dgraphPosts, dgraphDMs)

	if err != nil {
		helpers.MessageLogs.ErrorLog.Printf(
			"domain/BulkAddChatAndPostToDgraph Failed to bulk add post and chat to dgraph err: %+v",
			err,
		)
		return
	}

	return

}

// forwardUpsertQuery is the query of a forward's upsert. Each post, chat and
// conversation the mutation writes is named by a variable (uid(po_0)), which
// this defines as whatever already has its id: nothing has a new post's or
// chat's, so its variable is empty and the mutation makes the node, while a
// conversation found by its grouping id is the one that exists. The variables
// are read off the mutation, so the two can't disagree: the query named a
// post's post_N where the mutation said po_N, and never defined a group
// chat's grpch_N, so the graph refused every forward into a channel or a
// group chat. The values are quoted, never pasted in bare.
func forwardUpsertQuery(posts []*dgraphStruct.DgraphPost, dms []*dgraphStruct.DgraphDm) string {
	var b strings.Builder
	b.WriteString("query {\n")
	defined := map[string]bool{}
	define := func(ref, predicate, value string) {
		name, ok := strings.CutPrefix(ref, "uid(")
		if !ok || !strings.HasSuffix(name, ")") {
			return // a node named by its uid, which needs no variable
		}
		name = strings.TrimSuffix(name, ")")
		if defined[name] {
			return
		}
		defined[name] = true
		fmt.Fprintf(&b, "%s as var(func: eq(%s, %q))\n", name, predicate, value)
	}
	for _, dm := range dms {
		define(dm.Uid, "dm_grouping_id", dm.GroupingId)
		for _, ch := range dm.Chats {
			define(ch.Uid, "chat_uuid", ch.Uuid)
		}
	}
	for _, p := range posts {
		define(p.Uid, "post_uuid", p.Uuid)
	}
	b.WriteString("}")
	return b.String()
}

// BulkRemoveChatAndPostFromPostgres takes back the rows a forward wrote before
// its graph write failed: posts and chats the graph doesn't have.
func BulkRemoveChatAndPostFromPostgres(ctx context.Context, postUUIDs, chatUUIDs []string) error {
	return models.BulkDeleteChatsAndPosts(ctx, postUUIDs, chatUUIDs)
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
