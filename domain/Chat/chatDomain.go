package domain

import (
	"context"
	"encoding/json"
	"fmt"
	readStateModels "github.com/akashc777/OneCamp/models/postgres/ReadState"
	"strconv"
	"time"

	messageDomain "github.com/akashc777/OneCamp/domain/Message"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/dgexec"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Chat"
	dgraphUserModels "github.com/akashc777/OneCamp/models/dgraph/User"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	OpenSearchBulkModels "github.com/akashc777/OneCamp/models/openSearch/Bulk"
	OpenSearchModels "github.com/akashc777/OneCamp/models/openSearch/Chat"
	"github.com/akashc777/OneCamp/models/pgexec"
	models "github.com/akashc777/OneCamp/models/postgres/Chat"
	"github.com/google/uuid"
)

const CHAT_COUNT = 10

// insertChat writes a message's row: $1 id, $2 author, $3 conversation, $4
// an imported message's original time (helpers.WithImportTimestamp) or NULL.
const insertChat = `
		INSERT INTO chats (id, created_by, grp_id, created_at, updated_at)
		VALUES ($1, $2, $3, COALESCE($4, NOW()), COALESCE($4, NOW()))`

func CreateChat(ctx context.Context, chatId uuid.UUID, createdBy uuid.UUID, grpId string) (err error) {
	// An imported message keeps its original time (helpers.WithImportTimestamp).
	_, err = pgexec.Exec(ctx, insertChat, chatId, createdBy, grpId, helpers.GetImportTimestamp(ctx))

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateChat Failed to create new chat err: %+v",
			err)
		return
	}
	return
}

// CreateChatReadBySender is CreateChat with its author's read mark of the
// conversation moved up to readAt in the same statement, as
// LastSeenChat.CreateOrUpdateLastSeenChat moves it (never back): a sent
// message's two writes in one round trip, in ctx's transaction if it has one.
func CreateChatReadBySender(ctx context.Context, chatId uuid.UUID, createdBy uuid.UUID, grpId string, readAt time.Time) (err error) {
	_, err = pgexec.Exec(ctx, `
		WITH c AS (`+insertChat+`
		RETURNING created_by, grp_id)
		INSERT INTO last_seen_chat (user_id, grp_id, user_last_seen)
		SELECT created_by, grp_id, $5::timestamptz FROM c
		ON CONFLICT (user_id, grp_id)
		DO UPDATE SET user_last_seen = GREATEST(last_seen_chat.user_last_seen, EXCLUDED.user_last_seen)`,
		chatId, createdBy, grpId, helpers.GetImportTimestamp(ctx), readAt)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateChatReadBySender Failed to create new chat err: %+v",
			err)
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
	// The rule above is the read state's (models/postgres/ReadState), so the
	// count, the first unread and Mark unread agree.
	query := `
		SELECT lsc.grp_id, COUNT(m.id) AS chat_count, MAX(m.created_at) AS last_unread_at
		FROM last_seen_chat lsc
		LEFT JOIN chats m ON ` + readStateModels.Chat.Unread() + `
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
									  ch as var(func: eq(chat_uuid, %+q))
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
					chat_kind
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
					chat_kind
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
					chat_dm {
						dm_grouping_id
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
` + dmListFields + `					}
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

// pageChats reads a page of a DM's or group's messages, each with its
// thread's people (domain/Message KeepChatRepliers): every page query's one read.
// memberCount is the gate every page and thread read asks of a DM or a
// group, in its own query: how many of its participants are $userId (0 or
// 1). The one definition of "is a participant" the page reads use.
const memberCount = `dm_is_member: count(dm_participants @filter(uid($userId)))`

// chatPageFields is what each page of a DM's or a group's messages selects
// for every message on it, whichever way the page runs.
var chatPageFields = `{
						chat_uuid
						chat_body_text
						chat_created_at
						chat_kind
						chat_system
						chat_copy_of
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
						` + messageDomain.RepliersSelection("chat_comments") + `
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
						chat_fwd_msg_post ` + dgraphStruct.ForwardedPostFields + `
						chat_fwd_msg_chat ` + dgraphStruct.ForwardedChatFields + `
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
					}`

// DmPage is one page of a DM's or a group's messages: whether the
// conversation is there and, when it was read for someone, whether they are
// in it.
type DmPage struct {
	Found    bool
	IsMember bool
	Chats    []*dgraphStruct.DgraphChat
}

// dmPageRead is ChatPage's answer as the graph sends it.
type dmPageRead struct {
	DmInfo []dgraphStruct.DgraphDm `json:"dmInfo"`
}

// ChatPage reads one page of the conversation grouping's messages, the
// first CHAT_COUNT+1 of them in dir from at (Latest has no edge, and at is
// unused). With member (a person's graph uid) the same read says whether
// they are in it, so a page and its gate are one store call; "" asks
// nothing, for a caller that isn't answering a person.
func ChatPage(ctx context.Context, grouping string, dir messageDomain.Direction, at time.Time, member string) (DmPage, error) {
	vars := map[string]string{"$grpId": grouping, "$first": strconv.Itoa(CHAT_COUNT + 1)}
	if dir != messageDomain.Latest {
		vars["$time"] = at.Format(time.RFC3339Nano)
	}
	params, gate := "$grpId: string, $time: string, $first: int", ""
	if member != "" {
		vars["$userId"] = member
		params, gate = params+", $userId: string", memberCount
	}
	query := `query DmPage(` + params + `){
				dmInfo(func: eq(dm_grouping_id, $grpId)) {
					uid
					` + gate + `
					dm_chats ` + dir.Block("chat_created_at", "chat_deleted_at") + chatPageFields + `
				}
			}`
	got, err := dgexec.Read[dmPageRead](ctx, query, vars)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/ChatPage Failed to read a page of %s err: %+v", grouping, err)
		return DmPage{}, err
	}
	if len(got.DmInfo) == 0 {
		return DmPage{}, nil
	}
	dm := got.DmInfo[0]
	// A forwarded original deleted since shows as deleted, never its text.
	for _, c := range dm.Chats {
		c.HideDeletedForwards()
	}
	messageDomain.KeepChatRepliers(ctx, dm.Chats)
	return DmPage{Found: true, IsMember: dm.ParticipantIsMember > 0, Chats: dm.Chats}, nil
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

	// recUID is bound only under the conversation when the asker is in it, so
	// anyone else's read takes no recording and no line before the route's
	// 403 (V-1126 FIX): one round trip either way.
	query := fmt.Sprintf(`query DmInfo($grpId: string, $egressId: string, $userId: string){
			var(func: eq(dm_grouping_id, $grpId)) @filter(uid_in(dm_participants, $userId)) {
				dm_recording @filter(eq(recording_egress_id, $egressId)) {
					recUID as uid
				}
			}
			dmInfo(func: eq(dm_grouping_id, $grpId)) {
				uid
				dm_is_member: count(dm_participants @filter(uid($userId)))
				dm_recording @filter( uid(recUID) AND gt(recording_ended_at, "1970-01-01T00:00:00Z") AND not gt(recording_deleted_at, "1970-01-01T00:00:00Z")) {
					recording_egress_id
					recording_stared_at
					recording_ended_at
					recording_duration
					recording_obj_key
					recording_transcript_count: count(recording_transcript)
					recording_transcript %s {
						uid
						transcript_text
						transcript_timestamp
						transcript_offset_ms
						transcript_original_text
						transcript_corrected_at
						transcript_translations
						transcript_corrected_by { user_uuid user_full_name }
						transcript_from {
							user_name
							user_full_name
							user_uuid
							user_profile_object_key
							is_bot
						}
					}
					recording_size
					recording_transcript_only
					recording_status
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

	recFilter := `gt(recording_ended_at, "1970-01-01T00:00:00Z") AND not gt(recording_deleted_at, "1970-01-01T00:00:00Z")`
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
						recording_transcript_only
						recording_status
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
					dm_name
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

// GetDmCallOthers reads everyone in a DM's room but the caller, for the call
// rule (business/Chat RefuseCallIn, V-126): each one's uuid and whether it is
// a bot. Every member, unfiltered: it isn't a list anyone is shown, so
// dmParticipantVisible, which leaves external people out of the list a DM
// shows, doesn't apply, and an external guest is a person to call.
func GetDmCallOthers(ctx context.Context, groupingId string, callerUUID string) (others []*dgraphStruct.DgraphUser, err error) {
	query := `
			query DmCallOthers($grpId: string){
				dmInfo(func: eq(dm_grouping_id, $grpId)) {
					dm_participants {
						user_uuid
						is_bot
					}
				}
			}`

	dm, err := dgraphModels.GetDgraphBasicDmsInfoByGrpId(ctx, query, map[string]string{"$grpId": groupingId})
	if err != nil || dm == nil {
		return nil, err
	}

	for _, p := range dm.Participants {
		if p != nil && p.Uuid != callerUUID {
			others = append(others, p)
		}
	}

	return others, nil
}

// GetDgraphChatByUUIDWithAllComments reads a message with its thread. With
// member (a person's graph uid) the same read says, under chat_dm, whether
// they are in the message's conversation (memberCount); "" asks nothing.
func GetDgraphChatByUUIDWithAllComments(ctx context.Context, chatUUID string, member string) (dgraphChat *dgraphStruct.DgraphChat, err error) {
	variables := make(map[string]string)
	variables["$id"] = chatUUID
	params, gate := "$id: string", ""
	if member != "" {
		variables["$userId"] = member
		params, gate = params+", $userId: string", "chat_dm { dm_grouping_id "+memberCount+" }"
	}
	query := `query ChatInfo(` + params + `){
				chatInfo(func: eq(chat_uuid, $id)) {
					uid
					` + gate + `
					chat_uuid
					chat_body_text
					chat_created_at
					chat_updated_at
					chat_deleted_at
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
						comment_updated_at
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
