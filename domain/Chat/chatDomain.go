package domain

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Chat"
	dgraphUserModels "github.com/akashc777/OneCamp/models/dgraph/User"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	OpenSearchBulkModels "github.com/akashc777/OneCamp/models/openSearch/Bulk"
	OpenSearchModels "github.com/akashc777/OneCamp/models/openSearch/Chat"
	models "github.com/akashc777/OneCamp/models/postgres/Chat"
	"github.com/google/uuid"
)

const CHAT_COUNT = 10

func CreateChat(ctx context.Context, chatId uuid.UUID, createdBy uuid.UUID, grpId string) (err error) {
	query := `
		INSERT INTO chats (id, created_by,  grp_id)
		VALUES ($1, $2, $3)
	`
	err = models.CreateChat(query, chatId, createdBy, grpId)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateChat Failed to create new chat err: %+v",
			err)
		return
	}
	return
}

func UpdateChatByUUID(ctx context.Context, chatUUID uuid.UUID) (err error) {
	query := `
		UPDATE chats
        SET updated_at = $1
		WHERE id = $2
	`
	err = models.UpdateChatByUUID(query, chatUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UpdateChatByUUID Failed to update chat err: %+v",
			err)
		return
	}
	return
}

func BulkUpdateGrpIdInChatAndAtachment(ctx context.Context, oldGrpID string, newGrpID string) (err error) {
	updateChatsQuery := `
		UPDATE chats
		SET grp_id = $1
		WHERE grp_id = $2
	`

	updateAttachmentsQuery := `
		UPDATE attachments
		SET src_value = $1
		WHERE src_value = $2 AND src_key = 'grpChat'
	`

	updateLastSeenChatQuery := `
		UPDATE last_seen_chat
		SET grp_id = $1
		WHERE grp_id = $2
	`

	tx, err := postgresInit.DBConn.SqlDB.BeginTx(ctx, nil)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/BulkUpdateGrpIdInChatAndAtachment Failed to begin transaction err: %+v",
			err)
		return
	}

	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	_, err = tx.ExecContext(ctx, updateChatsQuery, newGrpID, oldGrpID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/BulkUpdateGrpIdInChatAndAtachment Failed to update chats err: %+v",
			err)
		return
	}

	_, err = tx.ExecContext(ctx, updateAttachmentsQuery, newGrpID, oldGrpID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/BulkUpdateGrpIdInChatAndAtachment Failed to update attachments err: %+v",
			err)
		return
	}

	_, err = tx.ExecContext(ctx, updateLastSeenChatQuery, newGrpID, oldGrpID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/BulkUpdateGrpIdInChatAndAtachment Failed to update last_seen_chat err: %+v",
			err)
		return
	}

	err = tx.Commit()
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/BulkUpdateGrpIdInChatAndAtachment Failed to commit transaction err: %+v",
			err)
		return
	}
	return
}

func SoftDeleteChatByUUIUD(ctx context.Context, chatUUID uuid.UUID) (err error) {
	query := `
		UPDATE chats
        SET deleted_at = $1
		WHERE id = $2
	`
	err = models.UpdateChatByUUID(query, chatUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/SoftDeleteChatByUUIUD Failed to delete chat err: %+v",
			err)
		return
	}
	return
}

func HardDeleteChatByUUID(ctx context.Context, chatUUID uuid.UUID) (err error) {
	query := `
		DELETE FROM chats
        WHERE id = $1
	`
	err = models.HardDeleteChatByUUID(query, chatUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/HardDeleteChatByUUID Failed to delete chat err: %+v",
			err)
		return
	}
	return
}

func GetLatestChatMessageCountByUserID(ctx context.Context, userID uuid.UUID) (chatMessageCount map[string]*models.ChatMessageCount, err error) {
	// Three conditions the channel equivalent has had all along and this did not.
	// GetLatestPostInChannelCountByUserID is the reference; this query is now the
	// same shape, because the two answer the same question about different tables.
	//
	//   created_by <> user_id  A message you sent is not a message you have not
	//     read. Without it, CreateChat writes the row with created_at DEFAULT
	//     NOW(), which is Postgres time AFTER the Go currentTime it then stores as
	//     your last_seen. Combined with the >= below, your own message counted as
	//     one unread against you and stayed until you reopened the conversation.
	//     That is the badge that would not clear.
	//
	//   deleted_at IS NULL     A deleted message was counted forever, and no
	//     amount of reading could clear it, because there was nothing left to read.
	//
	//   >  rather than >=      The marker is set to the moment of reading, so a
	//     message stamped at exactly that instant is one you just saw.
	query := `
		SELECT lsc.grp_id, COUNT(c.id) AS chat_count
		FROM last_seen_chat lsc
		LEFT JOIN chats c ON c.grp_id = lsc.grp_id
			AND c.created_at > lsc.user_last_seen
			AND c.deleted_at IS NULL
			AND c.created_by <> lsc.user_id
		WHERE lsc.user_id = $1
		GROUP BY lsc.grp_id;
	`
	chatMessageCount, err = models.GetLatestChatMessageCountByUserID(query, userID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetLatestPostInChannelCountByUserID Failed to get post count err: %+v",
			err)
		return
	}
	return
}

func CreateDgraphChat(ctx context.Context, dgraphDm *dgraphStruct.DgraphDm, chatId string) (chatUid string, dmUID string, err error) {

	query := fmt.Sprintf(`query {
									  dm as var(func: eq(dm_grouping_id, %q))
									  ch as var(func: eq(chat_id, %+q))
								  }`, dgraphDm.GroupingId, chatId)

	chatUid, dmUID, err = dgraphModels.CreateChat(ctx, query, dgraphDm)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateDgraphChat Failed to create/update chat err: %+v",
			err)
		return
	}
	return
}

func CreateDM(ctx context.Context, dgraphDm *dgraphStruct.DgraphDm, grpID string) (chatUid string, dmUID string, err error) {

	query := fmt.Sprintf(`query {
									  dm as var(func: eq(dm_grouping_id, %q))
								  }`, grpID)

	chatUid, dmUID, err = dgraphModels.CreateChat(ctx, query, dgraphDm)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateDgraphChat Failed to create/update chat err: %+v",
			err)
		return
	}
	return
}

func UpdateDgraphChat(ctx context.Context, dgraphChat *dgraphStruct.DgraphChat) (err error) {

	query := fmt.Sprintf(`query {
									  cha as var(func: eq(chat_uuid, "%+v"))
									  me as var(func: eq(mention_chat_uuid, "%+v"))
								  }`, dgraphChat.Uuid, dgraphChat.Uuid)

	delStringJSON := `{
		"uid": "uid(me)",
		"mention_users": null
	}`

	err = dgraphModels.UpdateChat(ctx, query, dgraphChat, delStringJSON)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UpdateDgraphChat Failed to update chat err: %+v",
			err)
		return
	}
	return
}

func GetDgraphChatOnlyTextByUUID(ctx context.Context, chatUUID string) (dgraphChat *dgraphStruct.DgraphChat, err error) {
	variables := make(map[string]string)
	variables["$id"] = chatUUID

	query := `query ChatInfo($id: string){
				chatInfo(func: eq(chat_uuid, $id)) {
					uid
					chat_from {
						uid
						user_uuid
						user_name
						user_full_name
						user_profile_object_key
						is_bot
						user_deleted_at
					}
					chat_to {
						uid
						user_uuid
						user_name
						user_profile_object_key
						is_bot
						user_deleted_at
					}
					chat_dm {
						dm_grouping_id
					}
					chat_created_at
					chat_updated_at
					chat_deleted_at
					chat_body_text
					chat_attachments {
						attachment_file_name
						attachment_obj_key
					}
					chat_uuid
				}
			}`

	dgraphChat, err = dgraphModels.GetDgraphChatInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphChatOnlyTextByUUID Failed to get dgraph chat info err: %+v",
			err)
		return
	}
	return

}

// dmParticipantVisible is who a DM shows as its participants: everyone but
// external users, except bots. Bots are external by class (they are not
// members) but they are messageable, which is why the DM user picker and
// channel members already exempt them. Leaving them out here made a DM with
// OneCamp AI show no other participant, so the list named it after the reader
// themselves.
const dmParticipantVisible = "NOT eq(is_external, true) OR eq(is_bot, true)"

func GetDgraphChatBasicByUUID(ctx context.Context, chatUUID string, userDgraphUID string) (dgraphChat *dgraphStruct.DgraphChat, err error) {
	variables := make(map[string]string)
	variables["$id"] = chatUUID
	variables["$userId"] = userDgraphUID

	query := `query ChatInfo($id: string, $userId: string){
				chatInfo(func: eq(chat_uuid, $id)) {
					uid
					chat_from {
						uid
						user_uuid
						user_name
						user_full_name
						user_profile_object_key
						is_bot
						user_deleted_at
					}
					chat_dm {
						dm_grouping_id
						dm_is_member: count(dm_participants @filter(uid($userId)))
						dm_participants @filter(` + dmParticipantVisible + `) {
							user_uuid
							user_name
							user_full_name
							user_profile_object_key
							is_bot
						}
					}
					chat_to {
						uid
						user_uuid
						user_name
						user_profile_object_key
						is_bot
						user_deleted_at
					}
					chat_created_at
					chat_updated_at
					chat_deleted_at
					chat_uuid
				}
			}`

	dgraphChat, err = dgraphModels.GetDgraphChatInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphChatBasicByUUID Failed to get dgraph chat info err: %+v",
			err)
		return
	}

	return
}

func GetDgraphChatByUUID(ctx context.Context, chatUUID string) (dgraphChat *dgraphStruct.DgraphChat, err error) {
	variables := make(map[string]string)
	variables["$id"] = chatUUID
	query := `query ChatInfo($id: string){
				chatInfo(func: eq(chat_uuid, $id)) {
					uid
					chat_from {
						uid
						user_uuid
						user_name
						user_full_name
						user_profile_object_key
						is_bot
						user_deleted_at
					}
					chat_to {
						uid
						user_uuid
						user_name
						user_profile_object_key
						is_bot
						user_deleted_at
					}
					chat_created_at
					chat_updated_at
					chat_deleted_at
					chat_body_text
					chat_reactions  {
						uid
						reaction_emoji_id
						reaction_added_by {
							user_uuid
							user_name
						}
					}
					chat_attachments {
						attachment_file_name
						attachment_obj_key
					}
					chat_uuid
				}
			}`

	dgraphChat, err = dgraphModels.GetDgraphChatInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphChatByUUID Failed to get dgraph chat info err: %+v",
			err)
		return
	}
	return
}

func GetUserListWithLatestChatWithUserIdAndSearchText(ctx context.Context, userUUID string, searchText string) (dgraphUser *dgraphStruct.DgraphUser, err error) {

	variables := make(map[string]string)
	variables["$user_id"] = userUUID
	query := fmt.Sprintf(`query UserInfo($user_id: string){
				userInfo(func: eq(user_uuid, $user_id))  {
					user_uuid
                    user_dms @filter(anyofterms(dm_grouping_id, $user_id)) @cascade(dm_participants) {
						dm_grouping_id
						dm_chats (orderdesc: chat_created_at, first: 1) {
							chat_body_text
							chat_from {
								user_name
								user_full_name
							}
							chat_created_at
							chat_attachments {
								attachment_obj_key
								attachment_file_name
							}
						}
						dm_participants @filter(regexp(user_name,  /.*%s.*/i) AND (`+dmParticipantVisible+`)) {
							user_name
							user_full_name
							user_profile_object_key
							is_bot
							user_device_connected
							user_status
						}
					}
			  	}
			}`, searchText)

	dgraphUser, err = dgraphUserModels.GetDgraphUserInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetUserListWithLatestChatWithUserIdAndSearchText Failed to get user in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetUserChatListWithLatestChat(ctx context.Context, userUUID string) (dgraphUser *dgraphStruct.DgraphUser, err error) {

	variables := make(map[string]string)
	variables["$user_id"] = userUUID

	query := `query UserInfo($user_id: string){
				userInfo(func: eq(user_uuid, $user_id)) {
					user_uuid
					user_dms @cascade(dm_chats){
						dm_grouping_id
						dm_participants @filter(` + dmParticipantVisible + `) {
							user_uuid
							user_name
							user_full_name
							user_profile_object_key
							is_bot
							user_device_connected
							user_status
						}
						dm_chats @filter(not gt(chat_deleted_at, "1970-01-01T00:00:00Z")) (orderdesc:chat_created_at, first: 1) @cascade( chat_from){
							chat_uuid
							chat_body_text
							chat_created_at
							chat_from {
								user_uuid
								user_name
								user_full_name
								user_profile_object_key
								is_bot
								user_device_connected
								user_status
							}
							
							chat_attachments {
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
						}
					}
			  	}
			}`

	dgraphUser, err = dgraphUserModels.GetDgraphUserInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetUserChannelListWithLatestPost Failed to get user's chat from dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetDgraphOldChatFromDgraph(ctx context.Context, groupingId string, lastChatTime time.Time) (dgraphChats []*dgraphStruct.DgraphChat, err error) {

	variables := make(map[string]string)
	variables["$time"] = lastChatTime.Format(time.RFC3339Nano)
	variables["$grpId"] = groupingId
	variables["$first"] = strconv.Itoa(CHAT_COUNT + 1)
	query := `query DmInfo($grpId: string, $time: string, $first: int){
				dmInfo(func: eq(dm_grouping_id, $grpId)) {
					dm_chats @filter(lt(chat_created_at, $time) AND not gt(chat_deleted_at, "1970-01-01T00:00:00Z"))(first: $first, orderdesc: chat_created_at) {
						chat_uuid
						chat_body_text
						chat_created_at
						chat_from {
							user_uuid
							user_name
							user_full_name
							user_profile_object_key
							is_bot
							user_deleted_at
						}
						chat_reactions {
							uid
							reaction_emoji_id
							reaction_added_by {
								user_uuid
								user_name
							}
						}
						chat_comment_count : count(chat_comments @filter(not gt(comment_deleted_at, "1970-01-01T00:00:00Z")))
						chat_comments (orderdesc: comment_created_at, first: 1){
							comment_created_at
						}
						chat_to {
							user_uuid
							user_name
							user_profile_object_key
							is_bot
							user_deleted_at
						}
						chat_attachments {
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
						chat_fwd_msg_post {
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
						chat_fwd_msg_chat {
							chat_uuid
							chat_body_text
							chat_created_at
							chat_from {
								user_uuid
								user_name
								user_full_name
								user_profile_object_key
								is_bot
							}
						}
						chat_reply_to @filter(not gt(chat_deleted_at, "1970-01-01T00:00:00Z")) {
							chat_uuid
							chat_body_text
							chat_created_at
							chat_from {
								user_uuid
								user_name
								user_full_name
								user_profile_object_key
								is_bot
							}
						}
					}
				}
			}`

	dgraphChats, err = dgraphModels.GetDgraphDmsInfoByGrpId(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphOldChatFromDgraph Failed to get chats from dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetDgraphNewChatFromDgraph(ctx context.Context, groupingId string, lastChatTime time.Time) (dgraphChats []*dgraphStruct.DgraphChat, err error) {

	variables := make(map[string]string)
	variables["$time"] = lastChatTime.Format(time.RFC3339Nano)
	variables["$grpId"] = groupingId
	variables["$first"] = strconv.Itoa(CHAT_COUNT + 1)
	query := `
			query DmInfo($grpId: string, $time: string, $first: int){
				dmInfo(func: eq(dm_grouping_id, $grpId)) {
					dm_chats @filter(gt(chat_created_at, $time) AND not gt(chat_deleted_at, "1970-01-01T00:00:00Z"))(first: $first, orderasc: chat_created_at) {
						chat_uuid
						chat_body_text
						chat_created_at
						chat_from {
							user_uuid
							user_name
							user_full_name
							user_profile_object_key
							is_bot
							user_deleted_at
						}
						chat_reactions {
							uid
							reaction_emoji_id
							reaction_added_by {
								user_uuid
								user_name
							}
						}
						chat_comment_count : count(chat_comments @filter(not gt(comment_deleted_at, "1970-01-01T00:00:00Z")))
						chat_comments (orderdesc: comment_created_at, first: 1){
							comment_created_at
						}
						chat_to {
							user_uuid
							user_name
							user_profile_object_key
							is_bot
							user_deleted_at
						}
						chat_attachments {
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
						chat_fwd_msg_post {
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
						chat_fwd_msg_chat {
							chat_uuid
							chat_body_text
							chat_created_at
							chat_from {
								user_uuid
								user_name
								user_full_name
								user_profile_object_key
								is_bot
							}
						}
						chat_reply_to @filter(not gt(chat_deleted_at, "1970-01-01T00:00:00Z")) {
							chat_uuid
							chat_body_text
							chat_created_at
							chat_from {
								user_uuid
								user_name
								user_full_name
								user_profile_object_key
								is_bot
							}
						}
					}
				}
			}`

	dgraphChats, err = dgraphModels.GetDgraphDmsInfoByGrpId(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphNewPostFromDgraph Failed to get chats in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetDMRecordingTranscript(ctx context.Context, groupingId string, userId string, egressId string, pageIndex int, pageSize int) (dgraphDm *dgraphStruct.DgraphDm, err error) {
	offset := pageIndex * pageSize
	variables := make(map[string]string)
	firstVal := strconv.Itoa(pageSize)
	offsetVal := strconv.Itoa(offset)

	variables["$grpId"] = groupingId
	variables["$egressId"] = egressId
	variables["$userId"] = userId

	transcriptParams := fmt.Sprintf("(orderasc: transcript_timestamp, first: %v, offset: %v)", firstVal, offsetVal)

	query := fmt.Sprintf(`query DmInfo($grpId: string, $egressId: string, $userId: string){
			var(func: eq(recording_egress_id, $egressId)) {
				recUID as uid
			}
			dmInfo(func: eq(dm_grouping_id, $grpId)) {
				uid
				dm_is_member: count(dm_participants @filter(uid($userId)))
				dm_recording @filter( uid(recUID) AND gt(recording_ended_at, "1970-01-01T00:00:00Z") AND NOT eq(recording_transcript_only, true)) {
					recording_egress_id
					recording_stared_at
					recording_ended_at
					recording_duration
					recording_obj_key
					recording_transcript_count: count(recording_transcript)
					recording_transcript %s {
						transcript_text
						transcript_timestamp
						transcript_offset_ms
						transcript_from {
							user_name
							user_uuid
							user_profile_object_key
							is_bot
						}
					}
					recording_size
					recording_started_by {
						user_name
					}
			}
		}
	}`, transcriptParams)

	dgraphDm, err = dgraphModels.GetDgraphBasicDmsInfoByGrpId(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDMRecordingTranscript Failed to get dm in dgraph err: %+v",
			err,
		)
		return
	}

	return

}

func GetDgraphDmRecordingListFromDgraph(ctx context.Context, userId string, groupingId string, startDate string, endDate string, pageIndex int, pageSize int) (dgraphDm *dgraphStruct.DgraphDm, err error) {

	offset := pageIndex * pageSize
	firstVal := strconv.Itoa(pageSize + 1)
	offsetVal := strconv.Itoa(offset)

	variables := make(map[string]string)
	variables["$grpId"] = groupingId
	variables["$userId"] = userId
	variables["$first"] = firstVal
	variables["$offset"] = offsetVal

	recFilter := `gt(recording_ended_at, "1970-01-01T00:00:00Z") AND not gt(recording_deleted_at, "1970-01-01T00:00:00Z") AND NOT eq(recording_transcript_only, true)`
	dataVars := `$grpId: string, $userId: string, $first: int, $offset: int`

	if startDate != "" && endDate != "" {
		variables["$startDate"] = startDate
		variables["$endDate"] = endDate
		recFilter = fmt.Sprintf(`%s AND ge(recording_stared_at, $startDate) AND le(recording_stared_at, $endDate)`, recFilter)
		dataVars = fmt.Sprintf(`%s, $startDate: string, $endDate: string`, dataVars)
	}

	query := fmt.Sprintf(`
			query DmInfo(%s){
				dmInfo(func: eq(dm_grouping_id, $grpId)) {
					uid
					dm_recording @filter( %s) (orderdesc: recording_stared_at, first: $first, offset: $offset){
						recording_egress_id
						recording_stared_at
						recording_ended_at
						recording_duration
						recording_obj_key
						recording_transcript
						recording_size
						recording_started_by {
							user_name
						}
					}
					dm_is_member: count(dm_participants @filter(uid($userId)))
				}
			}`, dataVars, recFilter)

	dgraphDm, err = dgraphModels.GetDgraphBasicDmsInfoByGrpId(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphDmRecordingListFromDgraph Failed to get chat recording list in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetDgraphDmBasicInfoFromDgraph(ctx context.Context, userId string, groupingId string) (dgraphDm *dgraphStruct.DgraphDm, err error) {
	variables := make(map[string]string)
	variables["$grpId"] = groupingId
	variables["$userId"] = userId

	query := `
			query DmInfo($grpId: string, $userId: string){
				dmInfo(func: eq(dm_grouping_id, $grpId)) {
					uid
					dm_grouping_id
					dm_participants @filter(` + dmParticipantVisible + `) {
						user_uuid
						user_name
						user_full_name
						user_email_id
						user_job_title
						user_profile_object_key
						is_bot
					}
					dm_is_member: count(dm_participants @filter(uid($userId)))
				}
			}`

	dgraphDm, err = dgraphModels.GetDgraphBasicDmsInfoByGrpId(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphNewChatIncludingChatFromDgraph Failed to get chats in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetDgraphNewChatIncludingChatFromDgraph(ctx context.Context, groupingId string, lastChatTime time.Time) (dgraphChats []*dgraphStruct.DgraphChat, err error) {

	variables := make(map[string]string)
	variables["$time"] = lastChatTime.Format(time.RFC3339Nano)
	variables["$grpId"] = groupingId
	variables["$first"] = strconv.Itoa(CHAT_COUNT + 1)
	query := `
			query DmInfo($grpId: string, $time: string, $first: int){
				dmInfo(func: eq(dm_grouping_id, $grpId)) {
					dm_chats @filter(ge(chat_created_at, $time) AND not gt(chat_deleted_at, "1970-01-01T00:00:00Z"))(first: $first, orderasc: chat_created_at) {
						chat_uuid
						chat_body_text
						chat_created_at
						chat_from {
							user_uuid
							user_name
							user_full_name
							user_profile_object_key
							is_bot
							user_deleted_at
						}
						chat_reactions {
							uid
							reaction_emoji_id
							reaction_added_by {
								user_uuid
								user_name
							}
						}
						chat_comment_count : count(chat_comments @filter(not gt(comment_deleted_at, "1970-01-01T00:00:00Z")))
						chat_comments (orderdesc: comment_created_at, first: 1){
							comment_created_at
						}
						chat_to {
							user_uuid
							user_name
							user_profile_object_key
							is_bot
							user_deleted_at
						}
						chat_attachments {
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
						chat_fwd_msg_post {
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
						chat_fwd_msg_chat {
							chat_uuid
							chat_body_text
							chat_created_at
							chat_from {
								user_uuid
								user_name
								user_full_name
								user_profile_object_key
								is_bot
							}
						}
						chat_reply_to @filter(not gt(chat_deleted_at, "1970-01-01T00:00:00Z")) {
							chat_uuid
							chat_body_text
							chat_created_at
							chat_from {
								user_uuid
								user_name
								user_full_name
								user_profile_object_key
								is_bot
							}
						}
					}
				}
			}`

	dgraphChats, err = dgraphModels.GetDgraphDmsInfoByGrpId(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphNewChatIncludingChatFromDgraph Failed to get chats in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetDgraphLatestChatFromDgraph(ctx context.Context, groupingId string) (dgraphChats []*dgraphStruct.DgraphChat, err error) {

	variables := make(map[string]string)
	variables["$grpId"] = groupingId
	variables["$first"] = strconv.Itoa(CHAT_COUNT + 1)
	query := `query DmInfo($grpId: string, $time: string, $first: int){
				dmInfo(func: eq(dm_grouping_id, $grpId)) {
					dm_chats @filter(not gt(chat_deleted_at, "1970-01-01T00:00:00Z") ) (orderdesc: chat_created_at, first: $first) {
						chat_uuid
						chat_body_text
						chat_created_at
						chat_from {
							user_uuid
							user_name
							user_full_name
							user_profile_object_key
							is_bot
							user_deleted_at
						}
						chat_comment_count : count(chat_comments @filter(not gt(comment_deleted_at, "1970-01-01T00:00:00Z")))
						chat_comments (orderdesc: comment_created_at, first: 1){
							comment_created_at
						}
						chat_to {
							user_uuid
							user_name
							user_profile_object_key
							is_bot
							user_deleted_at
						}
						chat_reactions {
							uid
							reaction_emoji_id
							reaction_added_by {
								user_uuid
								user_name
							}
						}
						chat_attachments {
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
						chat_fwd_msg_post {
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
						chat_fwd_msg_chat {
							chat_uuid
							chat_body_text
							chat_created_at
							chat_from {
								user_uuid
								user_name
								user_full_name
								user_profile_object_key
								is_bot
							}
						}
						chat_reply_to @filter(not gt(chat_deleted_at, "1970-01-01T00:00:00Z")) {
							chat_uuid
							chat_body_text
							chat_created_at
							chat_from {
								user_uuid
								user_name
								user_full_name
								user_profile_object_key
								is_bot
							}
						}
					}
				}
			}`

	dgraphChats, err = dgraphModels.GetDgraphDmsInfoByGrpId(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphLatestPostFromDgraph Failed to get chats in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func CreateOrUpdateChatReaction(ctx context.Context, dgraphChat *dgraphStruct.DgraphChat) (reactionUID string, err error) {
	query := fmt.Sprintf(`query {
									  cha as var(func: eq(chat_uuid, "%+v"))
								  }`, dgraphChat.Uuid)

	reactionUID, err = dgraphModels.CreateOrUpdateChatReaction(ctx, query, dgraphChat)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateOrUpdateChatReaction Failed to get chats in dgraph err: %+v",
			err,
		)
		return
	}
	return
}

func DeleteChatReaction(ctx context.Context, chatDgraphUUID string, reactionDgraphUUID string) (err error) {
	delStringJSON := fmt.Sprintf(`
		[
			
			{
				"uid": "%s",
				"chat_reactions": [
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
	`, chatDgraphUUID, reactionDgraphUUID, reactionDgraphUUID, reactionDgraphUUID)

	err = dgraphModels.DeleteNodeOrEdgesRelatedToChats(ctx, delStringJSON)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/DeleteChannelMemberEdge Failed to remove channel member in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetDgraphChatByUUIDWithAllComments(ctx context.Context, chatUUID string) (dgraphChat *dgraphStruct.DgraphChat, err error) {

	variables := make(map[string]string)
	variables["$id"] = chatUUID
	query := `query ChatInfo($id: string){
				chatInfo(func: eq(chat_uuid, $id)) {
					uid
					chat_uuid
					chat_body_text
					chat_created_at
					chat_from {
						user_uuid
						user_name
						user_full_name
						user_profile_object_key
						is_bot
					}
					chat_to {
						user_uuid
						user_name
						user_profile_object_key
						is_bot
					}
					chat_attachments {
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
					chat_reactions {
						uid
						reaction_emoji_id
						reaction_added_by {
							user_uuid
							user_name
						}
					}
					chat_comment_count : count(chat_comments @filter(not gt(comment_deleted_at, "1970-01-01T00:00:00Z")))
					chat_comments @filter(not gt(comment_deleted_at, "1970-01-01T00:00:00Z")){
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

	dgraphChat, err = dgraphModels.GetDgraphChatInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphChatByUUIDWithAllComments Failed to get chat in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func UpdateChatInOpenSearch(openSearchChat *openSearchStruct.OpenSearchChat) {
	ctx := context.Background()
	err := OpenSearchModels.UpdateChatInOpenSearch(ctx, openSearchChat)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UpdateChatInOpenSearch Failed to insert to openSearch err: %+v",
			err,
		)
		return
	}
}

func MigrateChatGroupIDInOpensearch(oldGrpId string, newGrpId string) {
	ctx := context.Background()
	err := OpenSearchModels.MigrateChatGroupIDInOpensearch(ctx, oldGrpId, newGrpId)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UpdateChatInOpenSearch Failed to update grpId in openSearch err: %+v",
			err,
		)
		return
	}
}

func getChatWithAttachmentBulkOperationStringForOpenSearch(ctx context.Context, openSearchChat *openSearchStruct.OpenSearchChat, openSearchAttachments []*openSearchStruct.OpenSearchAttachment) (bulkActionString string, err error) {

	postCreate := fmt.Sprintf("{ \"create\" : { \"_index\" : \"chats\", \"_id\" : \"%+v\" } }\n", openSearchChat.Uuid)
	postJsonData, err := json.Marshal(openSearchChat)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/getChatWithAttachmentBulkOperationStringForOpenSearch Error mashiling post struct to json err: %+v",
			err)
		return
	}

	postCreate += string(postJsonData)

	bulkActionString += postCreate + "\n"

	attachmentCreate := ""

	for _, attachment := range openSearchAttachments {
		tempAttachmentCreate := fmt.Sprintf("{ \"create\" : { \"_index\" : \"attachments\", \"_id\" : \"%+v\" } }\n", attachment.Uuid)
		attachmentJsonData, _ := json.Marshal(attachment)

		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"models/getChatWithAttachmentBulkOperationStringForOpenSearch Error mashiling attachment struct to json err: %+v",
				err)
			return
		}

		tempAttachmentCreate += string(attachmentJsonData) + "\n"
		attachmentCreate += tempAttachmentCreate
	}

	bulkActionString += attachmentCreate

	return
}

func CreateChatWithAttachmentsInOpenSearch(openSearchChat *openSearchStruct.OpenSearchChat, dgraphAttachment []*dgraphStruct.DgraphAttachment) {

	ctx := context.Background()

	var openSearchAttachments []*openSearchStruct.OpenSearchAttachment

	for _, attachment := range dgraphAttachment {
		openSearchAttachments = append(openSearchAttachments, &openSearchStruct.OpenSearchAttachment{
			Uuid:                           attachment.Uuid,
			AttachmentFileName:             attachment.FileName,
			AttachmentByProfile:            openSearchChat.ChatByProfile,
			AttachmentByUserFullName:       openSearchChat.ChatByUserFullName,
			AttachmentObjKey:               attachment.ObjectKey,
			AttachmentChatGrpId:            openSearchChat.ChatGrpId,
			AttachmentChatUuid:             openSearchChat.Uuid,
			AttachmentCreatedAt:            openSearchChat.ChatCreatedAt,
			AttachmentChatFromUserUuid:     openSearchChat.ChatByUserUuid,
			AttachmentChatFromUserFullName: openSearchChat.ChatByUserFullName,
			AttachmentChatParticipants:     openSearchChat.ChatParticipants,
			AttachmentDeletedAt:            nil,
		})
	}

	bulkOperationString, err := getChatWithAttachmentBulkOperationStringForOpenSearch(ctx, openSearchChat, openSearchAttachments)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateChatWithAttachmentsInOpenSearch Error getting bulk string for chat's attachments err: %+v",
			err)
		return
	}

	err = OpenSearchBulkModels.BulkCreateInOpenSearch(bulkOperationString)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateChatWithAttachmentsInOpenSearch Error getting bulk string for chat with attachments err: %+v",
			err)
		return
	}

}

func getDeleteChatWithAttachmentBulkOperationStringForOpenSearch(ctx context.Context, openSearchChat *openSearchStruct.OpenSearchChat, openSearchAttachment []*openSearchStruct.OpenSearchAttachment) (bulkActionString string, err error) {

	postUpdate := fmt.Sprintf("{ \"update\": { \"_index\": \"chats\", \"_id\": \"%+v\" } }\n", openSearchChat.Uuid)
	postUpdateDoc := &openSearchStruct.BulkUpdate{
		Doc: openSearchChat,
	}
	postJsonData, err := json.Marshal(postUpdateDoc)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/getDeleteChatWithAttachmentBulkOperationStringForOpenSearch Error mashiling post struct to json err: %+v",
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
				"models/getDeleteChatWithAttachmentBulkOperationStringForOpenSearch Error mashiling attachment struct to json err: %+v",
				err)
			return
		}

		tempAttachmentCreate += string(attachmentJsonData) + "\n"
		attachmentCreate += tempAttachmentCreate
	}

	bulkActionString += attachmentCreate

	return
}

func DeleteChatWithAttachmentsInOpenSearch(openSearchChat *openSearchStruct.OpenSearchChat, dgraphAttachment []*dgraphStruct.DgraphAttachment) {
	ctx := context.Background()
	var openSearchUpdateDocs []*openSearchStruct.OpenSearchAttachment

	for _, attachment := range dgraphAttachment {
		openSearchUpdateDocs = append(openSearchUpdateDocs, &openSearchStruct.OpenSearchAttachment{
			Uuid:                attachment.Uuid,
			AttachmentDeletedAt: openSearchChat.ChatDeletedAt,
		})
	}

	bulkOperationString, err := getDeleteChatWithAttachmentBulkOperationStringForOpenSearch(ctx, openSearchChat, openSearchUpdateDocs)
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

// GetUserGroupChatListWithSearchText returns group chats (DMs with >2 participants) that the user
// belongs to and whose participants' names contain the search text.
func GetUserGroupChatListWithSearchText(ctx context.Context, userUUID string, userDgraphUID string, searchText string) (dgraphDms []*dgraphStruct.DgraphDm, err error) {
	variables := make(map[string]string)
	variables["$user_id"] = userUUID

	// Use regexp to filter participants by name. Group chats are DMs with >2 participants.
	query := fmt.Sprintf(`query UserInfo($user_id: string){
			userInfo(func: eq(user_uuid, $user_id)) {
				user_dms @filter(gt(count(dm_participants), 2)) {
					uid
					dm_grouping_id
					dm_participants @filter(regexp(user_name, /.*%s.*/i) AND NOT eq(user_uuid, $user_id) AND (`+dmParticipantVisible+`)) {
						uid
						user_uuid
						user_name
						user_full_name
						user_profile_object_key
						is_bot
					}
					dm_is_member: count(dm_participants @filter(uid(%s)))
				}
			}
		}`, searchText, userDgraphUID)

	dgraphUser, err := dgraphUserModels.GetDgraphUserInfoByUUID(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetUserGroupChatListWithSearchText Failed to get group chats err: %+v",
			err)
		return
	}

	if dgraphUser == nil {
		return
	}

	for _, dm := range dgraphUser.DMs {
		// Only include group chats where user is a confirmed member AND some participants matched the search
		if dm.ParticipantIsMember > 0 && len(dm.Participants) > 0 {
			dgraphDms = append(dgraphDms, dm)
		}
	}

	return
}

// BulkArchiveChatsInDgraph sets chat_deleted_at on multiple chats in a single Dgraph mutation (batched).
func BulkArchiveChatsInDgraph(ctx context.Context, chatUUIDs []string) error {
	if len(chatUUIDs) == 0 {
		return nil
	}
	now := time.Now()
	const batchSize = 500
	for start := 0; start < len(chatUUIDs); start += batchSize {
		end := start + batchSize
		if end > len(chatUUIDs) {
			end = len(chatUUIDs)
		}
		batch := chatUUIDs[start:end]
		query := "query {\n"
		chats := make([]*dgraphStruct.DgraphChat, len(batch))
		for i, uuidStr := range batch {
			query += fmt.Sprintf("  ch%d as var(func: eq(chat_uuid, \"%s\"))\n", i, uuidStr)
			chats[i] = &dgraphStruct.DgraphChat{
				DType:     []string{"Chat"},
				Uid:       fmt.Sprintf("uid(ch%d)", i),
				DeletedAt: &now,
			}
		}
		query += "}"
		err := dgraphModels.BulkSoftDeleteDgraphChats(ctx, chats, query)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "domain/BulkArchiveChatsInDgraph failed for batch %d-%d: %v", start, end, err)
			return err
		}
	}
	return nil
}

// BulkRestoreChatsInDgraph clears chat_deleted_at on multiple chats in a single Dgraph mutation (batched).
func BulkRestoreChatsInDgraph(ctx context.Context, chatUUIDs []string) error {
	if len(chatUUIDs) == 0 {
		return nil
	}
	zeroTime := time.Time{}
	const batchSize = 500
	for start := 0; start < len(chatUUIDs); start += batchSize {
		end := start + batchSize
		if end > len(chatUUIDs) {
			end = len(chatUUIDs)
		}
		batch := chatUUIDs[start:end]
		query := "query {\n"
		chats := make([]*dgraphStruct.DgraphChat, len(batch))
		for i, uuidStr := range batch {
			query += fmt.Sprintf("  ch%d as var(func: eq(chat_uuid, \"%s\"))\n", i, uuidStr)
			chats[i] = &dgraphStruct.DgraphChat{
				DType:     []string{"Chat"},
				Uid:       fmt.Sprintf("uid(ch%d)", i),
				DeletedAt: &zeroTime,
			}
		}
		query += "}"
		err := dgraphModels.BulkSoftDeleteDgraphChats(ctx, chats, query)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "domain/BulkRestoreChatsInDgraph failed for batch %d-%d: %v", start, end, err)
			return err
		}
	}
	return nil
}

// AddAttachmentToChatInDgraph appends a single DgraphAttachment to a
// chat's MediaObj edge. Used by the Slack-import retrofit pass after a
// historical attachment is downloaded.
//
// Implementation pattern mirrors AddAttachmentToPostInDgraph in the
// post domain: upsert by chat_uuid, append the new edge.
func AddAttachmentToChatInDgraph(ctx context.Context, chatUUID string, att *dgraphStruct.DgraphAttachment) error {
	if att == nil {
		return nil
	}

	query := fmt.Sprintf(`query {
									  cha as var(func: eq(chat_uuid, "%s"))
								  }`, chatUUID)

	dgraphChat := &dgraphStruct.DgraphChat{
		Uid:      "uid(cha)",
		Uuid:     chatUUID,
		DType:    []string{"Chat"},
		MediaObj: []*dgraphStruct.DgraphAttachment{att},
	}

	if err := dgraphModels.UpdateChat(ctx, query, dgraphChat, ""); err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/AddAttachmentToChatInDgraph failed chat=%s att=%s err=%+v",
			chatUUID, att.Uuid, err)
		return err
	}
	return nil
}

// GetDmParticipation answers "is this person a participant in this grouping" and
// nothing else.
//
// A DM and a group chat are the same thing here: both are a grouping with a
// participant set, distinguished only by how many people are in it. So one query
// answers for both, which is why the authorization layer needs one rule rather than
// two that must be kept in agreement.
//
// SINGLE-PURPOSE ON PURPOSE. GetDgraphDmBasicInfoWithChatInfoFromDgraph already
// computes dm_is_member, but it also pulls every chat in the grouping with both
// participants expanded on each one. That is a lot of message content to fetch in
// order to decide whether the caller may see any of it — and a caller about to be
// refused should not cause a read of the thing they are being refused.
//
// Returns a nil DM with no error when the grouping does not exist, so the caller
// distinguishes "no such grouping" from a lookup failure and can refuse both without
// telling them apart.
func GetDmParticipation(ctx context.Context, groupingId string, userDgraphUID string) (dgraphDm *dgraphStruct.DgraphDm, err error) {
	variables := make(map[string]string)
	variables["$grpId"] = groupingId
	variables["$userId"] = userDgraphUID

	// dm_is_member is the same predicate every other participation check in this
	// file uses. Reused verbatim so there is one definition of "is a participant".
	query := `query DmParticipation($grpId: string, $userId: string){
				dmInfo(func: eq(dm_grouping_id, $grpId)) {
					uid
					dm_grouping_id
					dm_is_member: count(dm_participants @filter(uid($userId)))
				}
			}`

	dgraphDm, err = dgraphModels.GetDgraphBasicDmsInfoByGrpId(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDmParticipation Failed to get dm participation in dgraph err: %+v",
			err,
		)
		return
	}
	return
}
