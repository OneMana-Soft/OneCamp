package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Post"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	OpenSearchBulkModels "github.com/akashc777/OneCamp/models/openSearch/Bulk"
	OpenSearchModels "github.com/akashc777/OneCamp/models/openSearch/Post"
	models "github.com/akashc777/OneCamp/models/postgres/Post"
	"github.com/google/uuid"
)

const POST_COUNT = 10

func CreatePost(ctx context.Context, postUUID uuid.UUID, userUUID uuid.UUID, channelUUID uuid.UUID) (err error) {
	query := `
		INSERT INTO posts (id, post_channel, created_by)
		VALUES ($1, $2, $3)
	`
	err = models.CreatePost(query, postUUID, channelUUID, userUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreatePost Failed to create new post err: %+v",
			err)
		return
	}
	return
}

func CreatePostsBulk(ctx context.Context, postUUIDs []uuid.UUID, channelUUIDs []uuid.UUID, userUUID uuid.UUID) (err error) {
	// Row i pairs post i with channel i, so the two lists must agree. This check was already
	// here and was right to be; it now goes through the shared helper so every bulk writer
	// states the invariant the same way.
	if err := helpers.RequireSameLength("domain/CreatePostsBulk",
		helpers.NamedLen{Name: "postUUIDs", Len: len(postUUIDs)},
		helpers.NamedLen{Name: "channelUUIDs", Len: len(channelUUIDs)},
	); err != nil {
		return err
	}

	if len(postUUIDs) == 0 {
		return nil
	}

	query := `INSERT INTO posts (id, post_channel, created_by) VALUES `
	values := []interface{}{}
	placeholders := []string{}

	for i := 0; i < len(postUUIDs); i++ {
		placeholders = append(placeholders, fmt.Sprintf("($%d, $%d, $%d)", i*3+1, i*3+2, i*3+3))
		values = append(values, postUUIDs[i], channelUUIDs[i], userUUID)
	}

	query += strings.Join(placeholders, ",")

	err = models.BulkInsertPost(query, values...)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreatePostsBulk Failed to create posts err: %+v",
			err)
		return err
	}

	return nil
}

func UpdatePostByUUID(ctx context.Context, postUUID uuid.UUID, currentTime time.Time) (err error) {
	query := `
		UPDATE posts
        SET updated_at = $1
		WHERE id = $2
	`
	err = models.UpdatePostByUUID(query, currentTime, postUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UpdatePostByUUID Failed to update post err: %+v",
			err)
		return
	}
	return
}

func GetPostByUUID(ctx context.Context, postUUID uuid.UUID) (postInfo *models.Post, err error) {
	query := `
		SELECT id, post_channel, created_by, created_at, updated_at, deleted_at
        FROM posts
        WHERE id = $1
	`
	postInfo, err = models.GetPostByUUID(query, postUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetPostByUUID Failed to get post err: %+v",
			err)
		return
	}
	return
}

func SoftDeletePostByUUIUD(ctx context.Context, postUUID uuid.UUID, currentTime time.Time) (err error) {
	query := `
		UPDATE posts
        SET deleted_at = $1
		WHERE id = $2
	`
	err = models.UpdatePostByUUID(query, currentTime, postUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/SoftDeletePostByUUIUD Failed to delete post err: %+v",
			err)
		return
	}
	return
}

func GetLatestPostInChannelCountByUserID(ctx context.Context, userID uuid.UUID) (err error, channelPostCount map[string]*models.ChannelPostCount) {
	query := `
		SELECT lsc.channel_id, COUNT(p.id) AS post_count
		FROM last_seen_channel lsc
		LEFT JOIN posts p ON p.post_channel = lsc.channel_id
			AND p.created_at > lsc.user_last_seen
			AND p.deleted_at IS NULL
			AND p.created_by <> lsc.user_id
		WHERE lsc.user_id = $1
		GROUP BY lsc.channel_id;

	`
	err, channelPostCount = models.GetLatestPostInChannelCountByUserID(query, userID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetLatestPostInChannelCountByUserID Failed to get post count err: %+v",
			err)
		return
	}
	return
}

func HardDeletePostByUUIUD(ctx context.Context, postUUID uuid.UUID) (err error) {
	query := `
		DELETE FROM posts
        WHERE id = $1
	`
	err = models.HardDeletePstByUUID(query, postUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domainHardDeletePostByUUIUD Failed to delete post err: %+v",
			err)
		return
	}
	return
}

func CreateOrUpdateDgraphPost(ctx context.Context, dgraphPost *dgraphStruct.DgraphPost) (postUid string, err error) {

	query := fmt.Sprintf(`query {
									  po as var(func: eq(post_uuid, "%+v"))
								  }`, dgraphPost.Uuid)

	postUid, err = dgraphModels.CreateOrUpdatePost(ctx, query, dgraphPost, "")

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateOrUpdateDgraphChannel Failed to create/update post err: %+v",
			err)
		return
	}
	return
}

func CreateOrUpdateDgraphPostWithMentions(ctx context.Context, dgraphPost *dgraphStruct.DgraphPost) (postUid string, err error) {

	query := fmt.Sprintf(`query {
									  po as var(func: eq(post_uuid, "%+v"))
									  me as var(func: eq(mention_post_uuid, %+v))
								  }`, dgraphPost.Uuid, dgraphPost.Uuid)

	delStringJSON := `{
		"uid": "uid(me)",
		"mention_users": null
	}`
	postUid, err = dgraphModels.CreateOrUpdatePost(ctx, query, dgraphPost, delStringJSON)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateOrUpdateDgraphChannel Failed to create/update post err: %+v",
			err)
		return
	}
	return
}

func CreateOrUpdateDgraphPostReaction(ctx context.Context, dgraphPost *dgraphStruct.DgraphPost) (reactionUid string, err error) {

	query := fmt.Sprintf(`query {
									  po as var(func: eq(post_uuid, "%+v"))
								  }`, dgraphPost.Uuid)

	reactionUid, err = dgraphModels.CreateOrUpdatePostReaction(ctx, query, dgraphPost)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateOrUpdateDgraphPostReaction Failed to create/update post reaction err: %+v",
			err)
		return
	}
	return
}

func GetDgraphOldPostFromDgraph(ctx context.Context, channelUUID string, lastPostTime time.Time) (dgraphPosts []*dgraphStruct.DgraphPost, err error) {

	variables := make(map[string]string)
	variables["$time"] = lastPostTime.Format(time.RFC3339Nano)
	variables["$chId"] = channelUUID
	variables["$first"] = strconv.Itoa(POST_COUNT + 1)
	query := `query PostInfo($chId: string, $time: string, $first: int){
				postInfo(func: eq(ch_uuid, $chId)) {

					ch_posts @filter(lt(post_created_at, $time) AND not gt(post_deleted_at, "1970-01-01T00:00:00Z"))(first: $first, orderdesc: post_created_at) {
						post_uuid
						post_text
						post_created_at
						post_updated_at
						post_comment_count : count(post_comments @filter(not gt(comment_deleted_at, "1970-01-01T00:00:00Z")))
						post_comments (orderdesc: comment_created_at, first: 1){
							comment_created_at
						}
						post_by {
							user_uuid
							user_name
							user_profile_object_key
							is_bot
							user_status
						}
						post_attachments {
							attachment_uuid
							attachment_file_name
							attachment_obj_key
							attachment_width
							attachment_height
							attachment_size
							attachment_raw_type
							attachment_type
							attachment_duration
							attachment_created_at
						}
						post_reactions {
							uid
							reaction_emoji_id
							reaction_added_by {
								user_uuid
								user_name
							}
						}
					}
					
				}
			}`

	dgraphPosts, err = dgraphModels.GetDgraphPosts(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphUserInfoByUUID Failed to get user in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetDgraphNewPostFromDgraph(ctx context.Context, channelUUID string, lastPostTime time.Time) (dgraphPosts []*dgraphStruct.DgraphPost, err error) {

	variables := make(map[string]string)
	variables["$time"] = lastPostTime.Format(time.RFC3339Nano)
	variables["$chId"] = channelUUID
	variables["$first"] = strconv.Itoa(POST_COUNT + 1)
	query := `query PostInfo($chId: string, $time: string, $first: int){
				postInfo(func: eq(ch_uuid, $chId)) {

					ch_posts @filter(gt(post_created_at, $time) AND not gt(post_deleted_at, "1970-01-01T00:00:00Z"))(first: $first, orderasc: post_created_at) {
						post_uuid
						post_text
						post_created_at
						post_updated_at
						post_comment_count : count(post_comments @filter(not gt(comment_deleted_at, "1970-01-01T00:00:00Z")))
						post_comments (orderdesc: comment_created_at, first: 1){
							comment_created_at
						}
						post_by {
							user_uuid
							user_name
							user_profile_object_key
							is_bot
							user_status
						}
						post_attachments {
							attachment_uuid
							attachment_file_name
							attachment_obj_key
							attachment_width
							attachment_height
							attachment_size
							attachment_raw_type
							attachment_type
							attachment_duration
							attachment_created_at
						}
						post_reactions {
							uid
							reaction_emoji_id
							reaction_added_by {
								user_uuid
								user_name
							}
						}
					}
					
				}
			}`

	dgraphPosts, err = dgraphModels.GetDgraphPosts(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphUserInfoByUUID Failed to get user in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetDgraphNewPostIncludiongPostFromDgraph(ctx context.Context, channelUUID string, lastPostTime *time.Time) (dgraphPosts []*dgraphStruct.DgraphPost, err error) {

	variables := make(map[string]string)
	variables["$time"] = lastPostTime.Format(time.RFC3339Nano)
	variables["$chId"] = channelUUID
	variables["$first"] = strconv.Itoa(POST_COUNT + 1)

	query := `query PostInfo($chId: string, $time: string, $first: int){
				postInfo(func: eq(ch_uuid, $chId)) {

					ch_posts @filter(ge(post_created_at, $time) AND not gt(post_deleted_at, "1970-01-01T00:00:00Z"))(first: $first, orderasc: post_created_at) {
						post_uuid
						post_text
						post_created_at
						post_updated_at
						post_comment_count : count(post_comments @filter(not gt(comment_deleted_at, "1970-01-01T00:00:00Z")))
						post_comments (orderdesc: comment_created_at, first: 1){
							comment_created_at
						}
						post_by {
							user_uuid
							user_name
							user_profile_object_key
							is_bot
							user_status
						}
						post_attachments {
							attachment_file_name
							attachment_obj_key
						}
						post_reactions {
							uid
							reaction_emoji_id
							reaction_added_by {
								user_uuid
								user_name
							}
						}
					}
					
				}
			}`

	dgraphPosts, err = dgraphModels.GetDgraphPosts(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphNewPostIncludiongPostFromDgraph Failed to get user in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetDgraphPostOnlyTextDgraph(ctx context.Context, postUUID string) (dgraphPost *dgraphStruct.DgraphPost, err error) {
	variables := make(map[string]string)
	variables["$id"] = postUUID

	query := `query PostInfo($id: string){
				postInfo(func: eq(post_uuid, $id)) {
					post_uuid
					post_text
					post_created_at
					post_updated_at
					post_by {
						uid
						user_uuid
						user_name
						user_profile_object_key
						is_bot
						user_status
					}
				}
			}`

	dgraphPost, err = dgraphModels.GetDgraphPostInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphPostOnlyTextDgraph Failed to get post from dgraph err: %+v",
			err,
		)
		return
	}

	return

}

func GetDgraphLatestPostFromDgraph(ctx context.Context, channelUUID string) (dgraphPosts []*dgraphStruct.DgraphPost, err error) {

	variables := make(map[string]string)
	variables["$chId"] = channelUUID
	variables["$first"] = strconv.Itoa(POST_COUNT + 1)
	query := `query PostInfo($chId: string, $first: int){
					postInfo(func: eq(ch_uuid, $chId)) {

					ch_posts @filter(not gt(post_deleted_at, "1970-01-01T00:00:00Z") ) (orderdesc: post_created_at, first: $first) {
						post_uuid
						post_text
						post_created_at
						post_updated_at
						post_comment_count : count(post_comments @filter(not gt(comment_deleted_at, "1970-01-01T00:00:00Z")))
						post_by {
							user_uuid
							user_name
							user_profile_object_key
							is_bot
							user_status
						}
						post_attachments {
							attachment_uuid
							attachment_file_name
							attachment_obj_key
							attachment_width
							attachment_height
							attachment_size
							attachment_raw_type
							attachment_type
							attachment_duration
							attachment_created_at
						}
						post_comments (orderdesc: comment_created_at, first: 1){
							comment_created_at
						}
						post_reactions {
							uid
							reaction_emoji_id
							reaction_added_by {
								user_uuid
								user_name
							}
						}
						post_fwd_msg_post {
							post_uuid
							post_text
							post_created_at
							post_by {
								user_uuid
								user_name
								user_profile_object_key
								is_bot
								user_status
							}
							post_channel {
								ch_name
								ch_uuid
							}
						}
						post_fwd_msg_chat {
							chat_from: {
								user_uuid
								user_name
								user_profile_object_key
								is_bot
							}
							chat_created_at
							chat_body_text
							chat_uuid
						}
						post_reply_to @filter(not gt(post_deleted_at, "1970-01-01T00:00:00Z")) {
							post_uuid
							post_text
							post_created_at
							post_by {
								user_uuid
								user_name
								user_profile_object_key
								is_bot
								user_status
							}
						}
					}
					
				}
			}`

	dgraphPosts, err = dgraphModels.GetDgraphPosts(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphUserInfoByUUID Failed to get user in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

// GetDgraphRecentPostsForAI fetches lightweight recent posts for AI summarization.
// Returns only text, author, and timestamp — no attachments/reactions/comments.
func GetDgraphRecentPostsForAI(ctx context.Context, channelUUID string, count int) (dgraphPosts []*dgraphStruct.DgraphPost, channelName string, err error) {

	variables := make(map[string]string)
	variables["$chId"] = channelUUID
	variables["$first"] = strconv.Itoa(count)
	query := `query PostInfo($chId: string, $first: int){
				postInfo(func: eq(ch_uuid, $chId)) @filter(NOT has(ch_deleted_at)) {
					ch_name

					ch_posts @filter(not gt(post_deleted_at, "1970-01-01T00:00:00Z") ) (orderdesc: post_created_at, first: $first) {
						post_uuid
						post_text
						post_created_at
						post_by {
							user_uuid
							user_name
							user_full_name
						}
					}
				}
			}`

	// Use the same underlying DGraph query mechanism
	dgraphPosts, err = dgraphModels.GetDgraphPosts(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphRecentPostsForAI Failed to get posts from dgraph err: %+v",
			err,
		)
		return
	}

	// Channel name will be resolved from the user's DGraph channel list
	// since GetDgraphPosts only returns the posts array
	channelName = channelUUID

	return
}

func GetUnDeletedDgraphPostChannelBasicInfo(ctx context.Context, postUUID string) (dgraphPost *dgraphStruct.DgraphPost, err error) {

	variables := make(map[string]string)
	variables["$id"] = postUUID

	query := `query PostInfo($id: string){
				postInfo(func: eq(post_uuid, $id)) @filter(not gt(comment_deleted_at, "1970-01-01T00:00:00Z")) {
					uid
					post_uuid
					post_created_at
					post_by {
						uid
						user_uuid
						user_name
						user_status
					}
					post_channel {
						uid
						ch_uuid
						ch_is_member: count(ch_members @filter(uid($userId)))
						ch_is_admin: count(ch_moderators @filter(uid($userId)))
					}
				}
			}`

	dgraphPost, err = dgraphModels.GetDgraphPostInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetUnDeletedDgraphPostChannelBasicInfo Failed to get post in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetDgraphPostByUUIDWithAllComments(ctx context.Context, postUUID string, userDgraphUID string) (dgraphPost *dgraphStruct.DgraphPost, err error) {
	if strings.TrimSpace(postUUID) == "" {
		return nil, errors.New("post uuid is required")
	}

	variables := make(map[string]string)
	variables["$id"] = postUUID
	variables["$userId"] = dgraphUIDOrNone(userDgraphUID)
	query := `query PostInfo($id: string, $userId: string){
				postInfo(func: eq(post_uuid, $id)) {
					uid
					post_uuid
					post_text
					post_created_at
					post_by {
						uid
						user_uuid
						user_name
						user_profile_object_key
						is_bot
					}
					post_fwd_msg_post {
						post_uuid
						post_text
						post_created_at
						post_by {
							user_uuid
							user_name
							user_profile_object_key
							is_bot
						}
						post_channel {
							ch_name
							ch_uuid
						}
					}
					post_reply_to @filter(not gt(post_deleted_at, "1970-01-01T00:00:00Z")) {
						post_uuid
						post_text
						post_created_at
						post_by {
							user_uuid
							user_name
							user_profile_object_key
							is_bot
							user_status
						}
					}
					post_attachments {
						attachment_uuid
						attachment_file_name
						attachment_obj_key
						attachment_width
						attachment_height
						attachment_raw_type
						attachment_type
						attachment_size
						attachment_duration
						attachment_created_at
					}
					post_reactions {
						uid
						reaction_emoji_id
						reaction_added_by {
							user_uuid
							user_name
						}
					}
					post_channel {
						uid
						ch_uuid
						ch_is_member: count(ch_members @filter(uid($userId)))
						ch_is_admin: count(ch_moderators @filter(uid($userId)))
					}
					post_reaction_count: post_reactions @groupby(reaction_emoji_id) {
						count(uid)
					}
					post_comment_count : count(post_comments @filter(not gt(comment_deleted_at, "1970-01-01T00:00:00Z")))
					post_comments @filter(not gt(comment_deleted_at, "1970-01-01T00:00:00Z")) {
						comment_uuid
						comment_text
						comment_attachments {
							attachment_uuid
							attachment_file_name
							attachment_obj_key
							attachment_width
							attachment_height
							attachment_size
							attachment_raw_type
							attachment_type
							attachment_duration
							attachment_created_at
						}
						comment_reactions {
							uid
							reaction_emoji_id
							reaction_added_by {
								user_uuid
								user_name
							}
						}
						comment_by {
							user_uuid
							user_profile_object_key
							is_bot
							user_name
						}
						comment_created_at
					}
				}
			}`

	dgraphPost, err = dgraphModels.GetDgraphPostInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphPostByUUID Failed to get user in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetDgraphPostByUUIDWithChannelMembers(ctx context.Context, postUUID string, userDraphUid string) (dgraphPost *dgraphStruct.DgraphPost, err error) {
	if strings.TrimSpace(postUUID) == "" {
		return nil, errors.New("post uuid is required")
	}

	variables := make(map[string]string)
	variables["$id"] = postUUID
	variables["$userId"] = dgraphUIDOrNone(userDraphUid)
	query := `query PostInfo($id: string, $userId: string){
				postInfo(func: eq(post_uuid, $id)) {
					uid
					post_uuid
					post_text
					post_created_at
					post_updated_at
					post_by {
						uid
						user_uuid
						user_name
						user_profile_object_key
						is_bot
					}
					post_channel {
						uid
						ch_uuid
						ch_name
						ch_deleted_at
						ch_is_member: count(ch_members @filter(uid($userId)))
						ch_is_admin: count(ch_moderators @filter(uid($userId)))
					}
				}
			}`

	dgraphPost, err = dgraphModels.GetDgraphPostInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphPostByUUID Failed to get user in dgraph err: %+v",
			err,
		)
		return
	}

	return

}

func GetDgraphPostByUUID(ctx context.Context, postUUID string, userDraphUid string) (dgraphPost *dgraphStruct.DgraphPost, err error) {
	if strings.TrimSpace(postUUID) == "" {
		return nil, errors.New("post uuid is required")
	}

	variables := make(map[string]string)
	variables["$id"] = postUUID
	variables["$userId"] = dgraphUIDOrNone(userDraphUid)
	query := `query PostInfo($id: string, $userId: string){
				postInfo(func: eq(post_uuid, $id)) {
					uid
					post_uuid
					post_text
					post_created_at
					post_updated_at
					post_by {
						uid
						user_uuid
						user_name
						user_profile_object_key
						is_bot
					}
					post_channel {
						uid
						ch_uuid
						ch_name
						ch_deleted_at
						ch_is_member: count(ch_members @filter(uid($userId)))
						ch_is_admin: count(ch_moderators @filter(uid($userId)))
					}
				}
			}`

	dgraphPost, err = dgraphModels.GetDgraphPostInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphPostByUUID Failed to get user in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

// dgraphUIDOrNone returns uid if it is non-empty, otherwise a placeholder UID
// that matches no real node. Dgraph's uid() function refuses an empty string,
// so callers that do not need membership counts (and pass "") still need a
// valid, non-matching value for the query to execute.
func dgraphUIDOrNone(uid string) string {
	if strings.TrimSpace(uid) == "" {
		return "0x1"
	}
	return uid
}

func DeletePostReaction(ctx context.Context, postDgraphUUID string, reactionDgraphUUID string, postCreatedByUUID string, userDgraphUUID string) (err error) {
	delStringJSON := fmt.Sprintf(`
		[
			
			{
				"uid": "%s",
				"post_reactions": [
					{
						"uid": "%s"
					}
				]
			},
			{
				"uid": "%s",
				"reaction_added_by": null,
				"reaction_on_content_added_by": null,
				"reaction_added_at": null,
				"reaction_emoji_id": null
			
			},
			{
				"uid": "%s"
			}
		]
	`, postDgraphUUID, reactionDgraphUUID, reactionDgraphUUID, reactionDgraphUUID)

	err = dgraphModels.DeleteNodeOrEdges(ctx, delStringJSON)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/DeletePostReaction Failed to remove reaction in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func CreatePostInOpenSearch(openSearchPost *openSearchStruct.OpenSearchPost) {
	ctx := context.Background()
	err := OpenSearchModels.CreatePostInOpenSearch(ctx, openSearchPost)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreatePostInOpenSearch Failed to insert to openSearch err: %+v",
			err,
		)
		return
	}

}

func UpdatePostInOpenSearch(openSearchPost *openSearchStruct.OpenSearchPost) {
	ctx := context.Background()
	err := OpenSearchModels.UpdatePostInOpenSearch(ctx, openSearchPost)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UpdatePostInOpenSearch Failed to update in openSearch err: %+v",
			err,
		)
		return
	}

}

func getPostWithAttachmentBulkOperationStringForOpenSearch(ctx context.Context, openSearchPost *openSearchStruct.OpenSearchPost, openSearchAttachments []*openSearchStruct.OpenSearchAttachment) (bulkActionString string, err error) {

	postCreate := fmt.Sprintf("{ \"create\" : { \"_index\" : \"posts\", \"_id\" : \"%+v\" } }\n", openSearchPost.Uuid)
	postJsonData, err := json.Marshal(openSearchPost)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/getPostWithAttachmentString Error mashiling post struct to json err: %+v",
			err)
		return
	}

	postCreate += string(postJsonData) + "\n"

	bulkActionString += postCreate

	attachmentCreate := ""

	for _, attachment := range openSearchAttachments {
		tempAttachmentCreate := fmt.Sprintf("{ \"create\" : { \"_index\" : \"attachments\", \"_id\" : \"%+v\" } }\n", attachment.Uuid)
		attachmentJsonData, _ := json.Marshal(attachment)

		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"models/getPostWithAttachmentString Error mashiling attachment struct to json err: %+v",
				err)
			return
		}

		tempAttachmentCreate += string(attachmentJsonData) + "\n"
		attachmentCreate += tempAttachmentCreate
	}

	bulkActionString += attachmentCreate

	return
}

func CreatePostWithAttachmentsInOpenSearch(openSearchPost *openSearchStruct.OpenSearchPost, dgraphAttachment []*dgraphStruct.DgraphAttachment) {
	ctx := context.Background()
	var openSearchAttachments []*openSearchStruct.OpenSearchAttachment

	for _, attachment := range dgraphAttachment {
		openSearchAttachments = append(openSearchAttachments, &openSearchStruct.OpenSearchAttachment{
			Uuid:                     attachment.Uuid,
			AttachmentFileName:       attachment.FileName,
			AttachmentByProfile:      openSearchPost.PostByProfile,
			AttachmentByUserFullName: openSearchPost.PostByUserFullName,
			AttachmentObjKey:         attachment.ObjectKey,
			AttachmentChannelUuid:    openSearchPost.PostChannelUuid,
			AttachmentPostUuid:       openSearchPost.Uuid,
			AttachmentChannelName:    openSearchPost.PostChannelName,
			AttachmentCreatedAt:      openSearchPost.PostCreatedAt,
			AttachmentDeletedAt:      nil,
		})
	}

	bulkOperationString, err := getPostWithAttachmentBulkOperationStringForOpenSearch(ctx, openSearchPost, openSearchAttachments)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreatePostWithAttachments Error getting bulk string for post's attachments err: %+v",
			err)
		return
	}

	err = OpenSearchBulkModels.BulkCreateInOpenSearch(bulkOperationString)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreatePostWithAttachments Error getting bulk string for post with attachments err: %+v",
			err)
		return
	}

}

func getDeletePostWithAttachmentBulkOperationStringForOpenSearch(ctx context.Context, openSearchPost *openSearchStruct.OpenSearchPost, openSearchAttachment []*openSearchStruct.OpenSearchAttachment) (bulkActionString string, err error) {

	postUpdate := fmt.Sprintf("{ \"update\": { \"_index\": \"posts\", \"_id\": \"%+v\" } }\n", openSearchPost.Uuid)
	postUpdateDoc := &openSearchStruct.BulkUpdate{
		Doc: openSearchPost,
	}
	postJsonData, err := json.Marshal(postUpdateDoc)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/getDeletePostWithAttachmentBulkOperationStringForOpenSearch Error mashiling post struct to json err: %+v",
			err)
		return
	}

	postUpdate += string(postJsonData) + "\n"

	bulkActionString += postUpdate

	attachmentCreate := ""

	for _, attachment := range openSearchAttachment {
		tempAttachmentCreate := fmt.Sprintf("{ \"update\" : { \"_index\" : \"attachments\", \"_id\" : \"%+v\" } }\n", attachment.Uuid)
		attachmentUpdateDoc := &openSearchStruct.BulkUpdate{
			Doc: attachment,
		}
		attachmentJsonData, _ := json.Marshal(attachmentUpdateDoc)

		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"models/getDeletePostWithAttachmentBulkOperationStringForOpenSearch Error mashiling attachment struct to json err: %+v",
				err)
			return
		}

		tempAttachmentCreate += string(attachmentJsonData) + "\n"
		attachmentCreate += tempAttachmentCreate
	}

	bulkActionString += attachmentCreate

	return
}

func DeletePostWithAttachmentsInOpenSearch(openSearchPost *openSearchStruct.OpenSearchPost, dgraphAttachment []*dgraphStruct.DgraphAttachment) {
	ctx := context.Background()
	var openSearchUpdateDocs []*openSearchStruct.OpenSearchAttachment

	for _, attachment := range dgraphAttachment {
		openSearchUpdateDocs = append(openSearchUpdateDocs, &openSearchStruct.OpenSearchAttachment{
			Uuid:                attachment.Uuid,
			AttachmentDeletedAt: openSearchPost.PostDeletedAt,
		})
	}

	bulkOperationString, err := getDeletePostWithAttachmentBulkOperationStringForOpenSearch(ctx, openSearchPost, openSearchUpdateDocs)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/DeletePostWithAttachmentsInOpenSearch Error getting bulk string for post's attachments err: %+v",
			err)
		return
	}

	err = OpenSearchBulkModels.BulkCreateInOpenSearch(bulkOperationString)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/DeletePostWithAttachmentsInOpenSearch Error getting bulk string for post with attachments err: %+v",
			err)
		return
	}

}

// GetPostIdsOlderThan returns UUIDs of active posts created before the cutoff.
func GetPostIdsOlderThan(ctx context.Context, cutoff time.Time) ([]string, error) {
	query := `SELECT id::text FROM posts WHERE created_at < $1 AND deleted_at IS NULL`
	ids, err := models.GetEntityIdsOlderThan(query, cutoff)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetPostIdsOlderThan Failed err: %+v", err)
		return nil, err
	}
	return ids, nil
}

// BulkArchivePosts soft-deletes posts created before the cutoff.
func BulkArchivePosts(ctx context.Context, cutoff time.Time) (int64, error) {
	query := `UPDATE posts SET deleted_at = NOW(), updated_at = NOW() WHERE created_at < $1 AND deleted_at IS NULL`
	count, err := models.BulkArchiveEntity(query, cutoff)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/BulkArchivePosts Failed err: %+v", err)
		return 0, err
	}
	return count, nil
}

// BulkArchivePostsInDgraph sets post_deleted_at on multiple posts in a single Dgraph mutation (batched).
func BulkArchivePostsInDgraph(ctx context.Context, postUUIDs []string) error {
	if len(postUUIDs) == 0 {
		return nil
	}
	now := time.Now()
	const batchSize = 500
	for start := 0; start < len(postUUIDs); start += batchSize {
		end := start + batchSize
		if end > len(postUUIDs) {
			end = len(postUUIDs)
		}
		batch := postUUIDs[start:end]
		query := "query {\n"
		posts := make([]*dgraphStruct.DgraphPost, len(batch))
		for i, uuidStr := range batch {
			query += fmt.Sprintf("  po%d as var(func: eq(post_uuid, \"%s\"))\n", i, uuidStr)
			posts[i] = &dgraphStruct.DgraphPost{
				DType:     []string{"Post"},
				Uid:       fmt.Sprintf("uid(po%d)", i),
				DeletedAt: &now,
			}
		}
		query += "}"
		err := dgraphModels.BulkSoftDeleteDgraphPosts(ctx, posts, query)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "domain/BulkArchivePostsInDgraph failed for batch %d-%d: %v", start, end, err)
			return err
		}
	}
	return nil
}

// BulkRestorePostsInDgraph clears post_deleted_at on multiple posts in a single Dgraph mutation (batched).
func BulkRestorePostsInDgraph(ctx context.Context, postUUIDs []string) error {
	if len(postUUIDs) == 0 {
		return nil
	}
	zeroTime := time.Time{}
	const batchSize = 500
	for start := 0; start < len(postUUIDs); start += batchSize {
		end := start + batchSize
		if end > len(postUUIDs) {
			end = len(postUUIDs)
		}
		batch := postUUIDs[start:end]
		query := "query {\n"
		posts := make([]*dgraphStruct.DgraphPost, len(batch))
		for i, uuidStr := range batch {
			query += fmt.Sprintf("  po%d as var(func: eq(post_uuid, \"%s\"))\n", i, uuidStr)
			posts[i] = &dgraphStruct.DgraphPost{
				DType:     []string{"Post"},
				Uid:       fmt.Sprintf("uid(po%d)", i),
				DeletedAt: &zeroTime,
			}
		}
		query += "}"
		err := dgraphModels.BulkSoftDeleteDgraphPosts(ctx, posts, query)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "domain/BulkRestorePostsInDgraph failed for batch %d-%d: %v", start, end, err)
			return err
		}
	}
	return nil
}

// AddAttachmentToPostInDgraph appends a single DgraphAttachment to a
// post's MediaObj edge. Used by the Slack-import retrofit pass after a
// historical attachment is downloaded and persisted to MinIO+Postgres.
//
// Idempotency: the upsert query matches by post_uuid; running this
// twice with the same attachment uuid is a Dgraph-level no-op because
// BulkAddAttachmentsToDgraph (called separately) is the canonical
// creator and uses attachment_uuid for the attach-side upsert. Here we
// only add the edge — re-adding the same edge to the same post is a
// no-op in Dgraph's edge model.
//
// Why a separate function (vs. CreatePost): the Slack-import flow
// downloads files AFTER the post has been created, so we need a way
// to retroactively wire MediaObj. CreatePost is single-call and
// constructs the post body in one transaction; this is a pure edge
// append.
func AddAttachmentToPostInDgraph(ctx context.Context, postUUID string, att *dgraphStruct.DgraphAttachment) error {
	if att == nil {
		return nil
	}
	dgraphPost := &dgraphStruct.DgraphPost{
		Uid:      "uid(po)",
		Uuid:     postUUID,
		DType:    []string{"Post"},
		MediaObj: []*dgraphStruct.DgraphAttachment{att},
	}
	_, err := CreateOrUpdateDgraphPost(ctx, dgraphPost)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/AddAttachmentToPostInDgraph failed post=%s att=%s err=%+v",
			postUUID, att.Uuid, err)
		return err
	}
	return nil
}
