package domain

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Channel"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	OpenSearchModels "github.com/akashc777/OneCamp/models/openSearch/Channel"
	models "github.com/akashc777/OneCamp/models/postgres/Channel"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	"github.com/google/uuid"
)

func CreateChannel(ctx context.Context, channelName string, userUUID uuid.UUID, channelPrivate bool, channelUUID uuid.UUID) (err error) {
	query := `
		INSERT INTO channels (id, ch_name, ch_private, created_by)
		VALUES ($1, $2, $3, $4)
	`
	err = models.CreateChannel(query, channelName, userUUID, channelUUID, channelPrivate)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateChannel Failed to create new channel err: %+v",
			err)
		return
	}
	return
}

func CheckIfChannelExist(ctx context.Context, uname *string) (exist bool, err error) {
	query := `
        SELECT EXISTS (
            SELECT 1
            FROM channels
            WHERE ch_name = $1
        );
    `

	exist, err = models.CheckIfChannelExist(query, *uname)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CheckIfChannelExist Failed to check channel by name err: %+v",
			err)
		return
	}

	return
}

func GetChannelByName(ctx context.Context, channelName string) (channelInfo *models.Channel, err error) {
	query := `
        SELECT id, ch_name, created_by, ch_private, created_at, updated_at, deleted_at
        FROM channels
        WHERE ch_name = $1 AND deleted_at IS NULL
        LIMIT 1
    `
	channelInfo, err = models.GetChannelByName(query, channelName)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetChannelByName Failed to get channel by name err: %+v",
			err)
		return
	}
	return
}

func GetChannelByHandle(ctx context.Context, channelHandle *string) (channelInfo *models.Channel, err error) {

	query := `
        SELECT id, ch_name, created_by, created_at, updated_at, deleted_at
        FROM channels
        WHERE ch_handle = $1
    `

	channelInfo, err = models.GetChannelByHandle(&query, channelHandle)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetChannelByName Failed to channel by ch_handle err: %+v",
			err)
		return
	}

	return
}

func GetChannelInfoByUUID(ctx context.Context, channelUUID uuid.UUID) (channelInfo *models.Channel, err error) {

	query := `
        SELECT id, ch_name, created_by, ch_private, created_at, updated_at, deleted_at
        FROM channels
        WHERE id = $1
    `

	channelInfo, err = models.GetChannelByUUID(&query, channelUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetChannelByName Failed to channel by ch_handle err: %+v",
			err)
		return
	}

	return
}

func SoftDeleteChannelInfo(ctx context.Context, channelName string, channelPrivate bool, currentTime time.Time, channelUUID uuid.UUID) (err error) {
	query := `
        UPDATE channels
        SET ch_name = $1, ch_private = $2, updated_at = $3, deleted_at = $3
		WHERE id = $4`

	err = models.SoftDeleteChannelInfo(query, channelName, channelPrivate, currentTime, channelUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/SoftDeleteChannelInfo Failed to soft delete channelInfo err: %+v",
			err)
		return
	}

	return
}

func UpdateChannelInfo(ctx context.Context, channelName string, channelPrivate bool, currentTime time.Time, channelUUID uuid.UUID) (err error) {
	query := `
        UPDATE channels
        SET ch_name = $1, ch_private = $2, updated_at = $3, deleted_at = null
		WHERE id = $4`

	err = models.UpdateChannelInfo(query, channelName, channelPrivate, currentTime, channelUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UpdateChannelInfo Failed to update user's delete time name err: %+v",
			err)
		return
	}

	InvalidateChannelBasicInfo(ctx, channelUUID.String())

	return
}

// SetChannelPostPolicy updates a channel's posting policy in Postgres + Dgraph
// and invalidates the basic-info cache so the change takes effect immediately.
func SetChannelPostPolicy(ctx context.Context, channelUUID uuid.UUID, policy string) (err error) {
	affected, err := models.SetChannelPostPolicy(channelUUID, policy)
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("channel not found")
	}

	// Mirror to Dgraph so the post-time read (GetBasicDgraphChannelInfoByUUID)
	// sees the new policy without a Postgres round-trip on the hot path.
	// Uid MUST be "uid(ch)" so the upsert binds to the existing channel node
	// (the var(func: eq(ch_uuid,...)) defined in CreateOrUpdateDgraphChannel).
	// Without it, Dgraph creates a blank node and rejects the txn with
	// "Some variables are defined but not used Defined:[ch] Used:".
	dgraphChannel := &dgraphStruct.DgraphChannel{
		Uid:        "uid(ch)",
		Uuid:       channelUUID.String(),
		PostPolicy: policy,
	}
	if _, derr := CreateOrUpdateDgraphChannel(ctx, dgraphChannel); derr != nil {
		helpers.LogErrorWithContext(ctx, "domain/SetChannelPostPolicy dgraph mirror failed: %+v", derr)
		// Non-fatal: Postgres is source of truth; cache invalidation + a
		// subsequent read will reconcile. But return the error so the caller
		// can surface a partial-failure warning.
		return derr
	}

	InvalidateChannelBasicInfo(ctx, channelUUID.String())
	return nil
}

// GetChannelAITokenCap returns a channel's per-day AI token cap (0 = no cap).
func GetChannelAITokenCap(ctx context.Context, channelUUID uuid.UUID) (int, error) {
	return models.GetChannelAITokenCap(ctx, channelUUID)
}

// GetChannelAIModel returns a channel's pinned AI model id (nil = no override).
func GetChannelAIModel(ctx context.Context, channelUUID uuid.UUID) (*uuid.UUID, error) {
	return models.GetChannelAIModel(ctx, channelUUID)
}

// SetChannelAIModel pins (or clears with nil) a channel's default AI model.
func SetChannelAIModel(ctx context.Context, channelUUID uuid.UUID, modelID *uuid.UUID) error {
	affected, err := models.SetChannelAIModel(ctx, channelUUID, modelID)
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("channel not found")
	}
	return nil
}

// GetChannelNamesByUUIDs resolves channel UUIDs to names in one query (admin
// per-channel usage breakdown).
func GetChannelNamesByUUIDs(ctx context.Context, ids []string) (map[string]string, error) {
	return models.GetChannelNamesByUUIDs(ctx, ids)
}

// SetChannelAITokenCap sets a channel's per-day AI token cap (0 = no cap).
// Postgres is the source of truth; the cap is read only on the AI path, so no
// Dgraph mirror is needed.
func SetChannelAITokenCap(ctx context.Context, channelUUID uuid.UUID, cap int) error {
	affected, err := models.SetChannelAITokenCap(ctx, channelUUID, cap)
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("channel not found")
	}
	return nil
}

func CreateOrUpdateDgraphChannel(ctx context.Context, dgraphChannel *dgraphStruct.DgraphChannel) (channelUid string, err error) {
	dgraphChannel.DType = []string{"Channel"}
	query := fmt.Sprintf(`query {
									  ch as var(func: eq(ch_uuid, "%+v"))
								  }`, dgraphChannel.Uuid)

	channelUid, err = dgraphModels.CreateOrUpdateDgraphChannel(ctx, dgraphChannel, query)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateOrUpdateDgraphChannel Failed to create channel in dgraph err: %+v",
			err,
		)
		return
	}

	InvalidateChannelBasicInfo(ctx, dgraphChannel.Uuid)

	return
}

func GetDgraphChannelInfoByUUIDAndModeratorInfo(ctx context.Context, channelUUID string, userDgraphUUID string, moderatorUUID string) (dgraphUser *dgraphStruct.DgraphChannel, err error) {

	variables := make(map[string]string)
	variables["$id"] = channelUUID
	variables["$userId"] = userDgraphUUID
	variables["$moderatorId"] = moderatorUUID

	query := `query ChannelInfo($id: string, $userId: string, $moderatorId: string){
				channelInfo(func: eq(ch_uuid, $id)) {
					uid
					ch_uuid
					ch_handle
					ch_is_member: count(ch_members @filter(uid($userId)))
					ch_is_admin: count(ch_moderators @filter(uid($userId)))
					ch_moderators @filter(eq(user_uuid, $moderatorId)){
						uid
						user_uuid
						user_name
						user_email_id
					}
					ch_created_by {
						user_uuid
						user_name
						user_email_id
					}
				}
			}`

	dgraphUser, err = dgraphModels.GetDgraphChannelInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphChannelInfoByUUIDAndModeratorInfo Failed to get user in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetDgraphChannelInfoByUUIDAndMemberInfo(ctx context.Context, channelUUID string, userDgraphUUID string, memberUUID string) (dgraphUser *dgraphStruct.DgraphChannel, err error) {
	if strings.TrimSpace(channelUUID) == "" {
		return nil, errors.New("channel uuid is required")
	}

	variables := make(map[string]string)
	variables["$id"] = channelUUID
	variables["$userId"] = dgraphUIDOrNone(userDgraphUUID)
	variables["$memberId"] = memberUUID

	query := `query ChannelInfo($id: string, $userId: string, $memberId: string){
				channelInfo(func: eq(ch_uuid, $id)) {
					uid
					ch_uuid
					ch_handle
					ch_deleted_at
					ch_post_policy
					ch_is_member: count(ch_members @filter(uid($userId)))
					ch_is_admin: count(ch_moderators @filter(uid($userId)))
					ch_members @filter(eq(user_uuid, $memberId)){
						uid
						user_uuid
						user_name
						user_email_id
					}
					ch_created_by {
						user_uuid
						user_name
						user_email_id
					}
				}
			}`

	dgraphUser, err = dgraphModels.GetDgraphChannelInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphChannelInfoByUUIDAndMemberInfo Failed to get user in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetBasicChannelAndPostInfoByUUID(ctx context.Context, channelUUID string, postUUID string, userDgraphUUID string) (dgraphChannel *dgraphStruct.DgraphChannel, err error) {
	variables := make(map[string]string)
	variables["$id"] = channelUUID
	variables["$userId"] = userDgraphUUID
	variables["$postId"] = postUUID

	query := `query ChannelInfo($id: string, $userId: string, $postId: string){
				channelInfo(func: eq(ch_uuid, $id)) {
					uid
					ch_uuid
					ch_handle
					ch_is_member: count(ch_members @filter(uid($userId)))
					ch_is_admin: count(ch_moderators @filter(uid($userId)))
					ch_posts @filter(eq(post_uuid, $postId)) {
						uid
						post_uuid
						post_created_at
						post_deleted_at
						post_created_by {
							user_uuid
							user_deleted_at
						}
					}
					ch_private
					ch_created_at
					ch_updated_at
					ch_deleted_at
				}
			}`

	dgraphChannel, err = dgraphModels.GetDgraphChannelInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetBasicChannelAndPostInfoByUUID Failed to get user in dgraph err: %+v",
			err,
		)
		return
	}

	if len(dgraphChannel.Posts) > 0 {
		dgraphChannel.Post = dgraphChannel.Posts[0]
	}

	return
}

func GetBasicDgraphChannelInfoByUUID(ctx context.Context, channelUUID string, userDgraphUUID string) (dgraphChannel *dgraphStruct.DgraphChannel, err error) {
	found, _ := redisStore.GetJSON(ctx, registry.ChannelBasicInfo, []string{channelUUID, userDgraphUUID}, &dgraphChannel)
	if found {
		return
	}

	variables := make(map[string]string)
	variables["$id"] = channelUUID
	variables["$userId"] = userDgraphUUID
	query := `query ChannelInfo($id: string, $userId: string){
				channelInfo(func: eq(ch_uuid, $id)) {
					uid
					ch_uuid
					ch_is_member: count(ch_members @filter(uid($userId)))
					ch_member_count: count(ch_members)
					ch_members @filter(NOT eq(is_external, true)) (first: 3) {
						user_name
						user_uuid
						user_profile_object_key
					}
					ch_is_admin: count(ch_moderators @filter(uid($userId)))
					ch_is_user_fav: count(~user_fav_channels @filter(uid($userId)))
					ch_created_by {
						user_uuid
						user_name
						user_enail_id
					}
					ch_icon
					ch_name
					ch_private
					ch_post_policy
					ch_created_at
					ch_updated_at
					ch_deleted_at
				}
			}`

	dgraphChannel, err = dgraphModels.GetDgraphChannelInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetBasicDgraphChannelInfoByUUID Failed to get user in dgraph err: %+v",
			err,
		)
		return
	}

	if dgraphChannel != nil {
		_ = redisStore.SetJSON(ctx, registry.ChannelBasicInfo, []string{channelUUID, userDgraphUUID}, dgraphChannel)
	}

	return
}

func GetChannelRecordingTranscript(ctx context.Context, channelUUID string, userDgraphUUID string, egressId string, pageIndex int, pageSize int) (dgraphChannel *dgraphStruct.DgraphChannel, err error) {
	offset := pageIndex * pageSize
	variables := make(map[string]string)
	firstVal := strconv.Itoa(pageSize)
	offsetVal := strconv.Itoa(offset)

	variables["$id"] = channelUUID
	variables["$userId"] = userDgraphUUID
	variables["$egressId"] = egressId

	transcriptParams := fmt.Sprintf("(orderasc: transcript_timestamp, first: %v, offset: %v)", firstVal, offsetVal)

	query := fmt.Sprintf(`query ChannelInfo($id: string, $userId: string, $egressId: string){
		var(func: eq(recording_egress_id, $egressId)) {
			recUID as uid
		}
		channelInfo(func: eq(ch_uuid, $id)) {
			uid
			ch_uuid
			ch_handle
			ch_is_member: count(ch_members @filter(uid($userId)))
			ch_recording @filter( uid(recUID) AND gt(recording_ended_at, "1970-01-01T00:00:00Z") AND NOT eq(recording_transcript_only, true)) {
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
					}
				}
				recording_size
				recording_started_by {
					user_name
				}
			}
		}
	}`, transcriptParams)

	dgraphChannel, err = dgraphModels.GetDgraphChannelInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetChannelRecordingTranscript Failed to get channel in dgraph err: %+v",
			err,
		)
		return
	}

	return

}

func GetChannelAllRecordingList(ctx context.Context, channelUUID string, userDgraphUUID string, startDate string, endDate string, pageIndex int, pageSize int) (dgraphUser *dgraphStruct.DgraphChannel, err error) {

	offset := pageIndex * pageSize
	firstVal := strconv.Itoa(pageSize + 1)
	offsetVal := strconv.Itoa(offset)

	variables := make(map[string]string)
	variables["$id"] = channelUUID
	variables["$userId"] = userDgraphUUID
	variables["$first"] = firstVal
	variables["$offset"] = offsetVal

	recFilter := `gt(recording_ended_at, "1970-01-01T00:00:00Z") AND not gt(recording_deleted_at, "1970-01-01T00:00:00Z") AND NOT eq(recording_transcript_only, true)`
	dataVars := `$id: string, $userId: string, $first: int, $offset: int`

	if startDate != "" && endDate != "" {
		variables["$startDate"] = startDate
		variables["$endDate"] = endDate
		recFilter = fmt.Sprintf(`%s AND ge(recording_stared_at, $startDate) AND le(recording_stared_at, $endDate)`, recFilter)
		dataVars = fmt.Sprintf(`%s, $startDate: string, $endDate: string`, dataVars)
	}

	query := fmt.Sprintf(`query ChannelInfo(%s){
		channelInfo(func: eq(ch_uuid, $id)) {
			uid
			ch_uuid
			ch_handle
			ch_is_member: count(ch_members @filter(uid($userId)))
			ch_recording @filter( %s) (orderdesc: recording_stared_at, first: $first, offset: $offset) {
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
				recording_channel {
					ch_uuid
					ch_name
				}
			}
		}
	}`, dataVars, recFilter)

	dgraphUser, err = dgraphModels.GetDgraphChannelInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetChannelAllRecordingList Failed to get channel in dgraph err: %+v",
			err,
		)
		return
	}

	return

}

func GetDgraphChannelInfoByUUID(ctx context.Context, channelUUID string, userDgraphUUID string) (dgraphUser *dgraphStruct.DgraphChannel, err error) {
	if strings.TrimSpace(channelUUID) == "" {
		return nil, errors.New("channel uuid is required")
	}

	variables := make(map[string]string)
	variables["$id"] = channelUUID
	variables["$userId"] = dgraphUIDOrNone(userDgraphUUID)
	query := `query ChannelInfo($id: string, $userId: string){
				channelInfo(func: eq(ch_uuid, $id)) {
					uid
					ch_uuid
					ch_handle
					ch_is_member: count(ch_members @filter(uid($userId)))
					ch_is_admin: count(ch_moderators @filter(uid($userId)))
					ch_moderators (orderasc: user_name) {
						uid
						user_uuid
						user_name
						user_email_id
					}
			ch_members @filter(NOT eq(is_external, true) OR eq(is_bot, true)) (orderasc: user_name) {
				uid
				user_uuid
				user_name
				user_email_id
				user_profile_object_key
				is_bot
			}
					ch_icon
					ch_name
					ch_private
					ch_post_policy
					ch_created_at
					ch_updated_at
					ch_deleted_at
					ch_created_by {
						user_uuid
						user_name
						user_email_id
					}
				}
			}`

	dgraphUser, err = dgraphModels.GetDgraphChannelInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphChannelInfoByUUID Failed to get channel in dgraph err: %+v",
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

func GetUsersChannelListWithPublicChannel(ctx context.Context, userDgraphId string) (dgraphChannels []*dgraphStruct.DgraphChannel, err error) {

	variables := make(map[string]string)
	variables["$user_id"] = userDgraphId
	query := `query ChannelInfo($user_id: string){
			    var(func: uid($user_id)) {
					user_channels {
						ch as uid
					}
				}
				var(func: has(ch_uuid)) @filter(not eq(ch_private, true)) {
					p_ch as uid
				}
				channelInfo(func: uid(ch, p_ch)) {
					ch_uuid
					ch_name
			  	}
			}`

	dgraphChannels, err = dgraphModels.GetDgraphChannelsInfoByUserUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetUsersChannelListWithPublicChannel Failed to get channel fron dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetChannelListWithSearchText(ctx context.Context, userUUID string, searchText string) (dgraphChannels []*dgraphStruct.DgraphChannel, err error) {

	variables := make(map[string]string)
	variables["$user_id"] = userUUID
	query := fmt.Sprintf(`query ChannelInfo($user_id: string){
				channelInfo(func: has(ch_name)) @filter((uid_in(~user_channels, $user_id) OR not eq(ch_private, true)) AND regexp(ch_name,  /.*%s.*/i)) {
					uid
					ch_uuid
					ch_handle
					ch_icon
					ch_name
					ch_created_at
					ch_updated_at
					ch_deleted_at
					ch_private
			  	}
			}`, searchText)

	dgraphChannels, err = dgraphModels.GetDgraphChannelsInfoByUserUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetChannelListWithSearchText Failed to get channel from dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetChannelListWithMemberFlag(ctx context.Context, userDgraphID string, channelDgraphUIDs []string) (dgraphChannels []*dgraphStruct.DgraphChannel, err error) {

	dgraphUids := strings.Join(channelDgraphUIDs, ", ")
	variables := make(map[string]string)
	variables["$userId"] = userDgraphID
	query := fmt.Sprintf(`query ChannelInfo($userId: string){
				channelInfo(func: has(ch_uuid)) @filter(uid(%+v)) {
					uid
					ch_uuid
					ch_is_member: count(ch_members @filter(uid($userId)))

				}
			}`, dgraphUids)

	dgraphChannels, err = dgraphModels.GetDgraphChannelsInfoByUserUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetChannelListWithMemberFlag Failed to get channels from dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetArchivedChannelListWithLatestPostWithUserIdAndSearchText(ctx context.Context, userUUID string, searchText string, pageIndex int, pageSize int) (dgraphChannels []*dgraphStruct.DgraphChannel, totalCount int64, err error) {

	offset := pageIndex * pageSize
	firstVal := strconv.Itoa(pageSize)
	offsetVal := strconv.Itoa(offset)

	variables := make(map[string]string)
	variables["$user_id"] = userUUID
	filter := fmt.Sprintf(`(uid_in(~user_channels, $user_id) OR not eq(ch_private, true)) AND regexp(ch_name,  /.*%s.*/i) AND gt(ch_deleted_at, "1970-01-01T00:00:00Z")`, searchText)
	query := fmt.Sprintf(`query ChannelInfo($user_id: string){
			    var(func: has(ch_name)) @filter(%s) {
                    totalCountVar as count(uid)
                }
				channelInfo(func: has(ch_name), first: %v, offset: %v) @filter(%s) {
					ch_uuid
					ch_handle
					ch_icon
					ch_name
					ch_created_at
					ch_updated_at
					ch_deleted_at
					ch_private
					ch_created_by {
						user_name
						user_uuid
					}
					ch_posts @filter(not gt(post_deleted_at, "1970-01-01T00:00:00Z")) (orderdesc: post_created_at, first: 1) {
					  post_text
					  post_created_at
					  post_by {
						user_name
					  }
					post_attachments {
						attachment_file_name
					}
					}
			  	}
				totalCount() {
                    count: sum(val(totalCountVar))
                }
			}`, filter, firstVal, offsetVal, filter)

	dgraphChannels, totalCount, err = dgraphModels.GetDgraphChannelsInfoAndCount(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetArchivedChannelListWithLatestPostWithUserIdAndSearchText Failed to get channel from dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetAllActiveChannelListWithLatestPost(ctx context.Context, userDgraphId string, pageIndex int, pageSize int) (dgraphChannels []*dgraphStruct.DgraphChannel, totalCount int64, err error) {

	offset := pageIndex * pageSize
	firstVal := strconv.Itoa(pageSize)
	offsetVal := strconv.Itoa(offset)

	variables := make(map[string]string)
	variables["$user_id"] = userDgraphId
	filter := fmt.Sprintf(`(uid_in(~user_channels, $user_id) OR not eq(ch_private, true)) AND not gt(ch_deleted_at, "1970-01-01T00:00:00Z")`)
	query := fmt.Sprintf(`query ChannelInfo($user_id: string){
				var(func: has(ch_name)) @filter(%s) {
                    totalCountVar as count(uid)
                }
				channelInfo(func: has(ch_name), first: %v, offset: %v) @filter(%s) {
					ch_uuid
					ch_handle
					ch_icon
					ch_name
					ch_created_at
					ch_updated_at
					ch_deleted_at
					ch_private
					ch_created_by {
						user_name
						user_uuid
					}
					ch_posts @filter(not gt(post_deleted_at, "1970-01-01T00:00:00Z")) (orderdesc: post_created_at, first: 1) {
					  post_text
					  post_created_at
					  post_by {
						user_name
					  }
					post_attachments {
						attachment_file_name
					}
					}
			  	}
				totalCount() {
                    count: sum(val(totalCountVar))
                }
			}`, filter, firstVal, offsetVal, filter)

	dgraphChannels, totalCount, err = dgraphModels.GetDgraphChannelsInfoAndCount(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetAllActiveChannelListWithLatestPost Failed to get channel from dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetActiveChannelListWithLatestPostWithUserIdAndSearchText(ctx context.Context, userUUID string, searchText string, pageIndex int, pageSize int) (dgraphChannels []*dgraphStruct.DgraphChannel, totalCount int64, err error) {
	offset := pageIndex * pageSize
	firstVal := strconv.Itoa(pageSize)
	offsetVal := strconv.Itoa(offset)

	variables := make(map[string]string)
	variables["$user_id"] = userUUID
	filter := fmt.Sprintf(`(uid_in(~user_channels, $user_id) OR not eq(ch_private, true)) AND regexp(ch_name,  /.*%s.*/i) AND not gt(ch_deleted_at, "1970-01-01T00:00:00Z")`, searchText)
	query := fmt.Sprintf(`query ChannelInfo($user_id: string){
				var(func: has(ch_name)) @filter(%s) {
                    totalCountVar as count(uid)
                }
				channelInfo(func: has(ch_name), first: %v, offset: %v) @filter(%s) {
					ch_uuid
					ch_handle
					ch_icon
					ch_name
					ch_created_at
					ch_updated_at
					ch_deleted_at
					ch_private
					ch_created_by {
						user_name
						user_uuid
					}
					ch_posts @filter(not gt(post_deleted_at, "1970-01-01T00:00:00Z")) (orderdesc: post_created_at, first: 1) {
					  post_text
					  post_created_at
					  post_by {
						user_name
					  }
					post_attachments {
						attachment_file_name
					}
					}
			  	}
				totalCount() {
                    count: sum(val(totalCountVar))
                }
			}`, filter, firstVal, offsetVal, filter)

	dgraphChannels, totalCount, err = dgraphModels.GetDgraphChannelsInfoAndCount(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetActiveChannelListWithLatestPostWithUserIdAndSearchText Failed to get channel from dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetChannelListWithLatestPostWithUserIdAndSearchText(ctx context.Context, userUUID string, searchText string, pageIndex int, pageSize int) (dgraphChannels []*dgraphStruct.DgraphChannel, totalCount int64, err error) {

	offset := pageIndex * pageSize
	firstVal := strconv.Itoa(pageSize)
	offsetVal := strconv.Itoa(offset)

	variables := make(map[string]string)
	variables["$user_id"] = userUUID
	filter := fmt.Sprintf(`(uid_in(~user_channels, $user_id) OR not eq(ch_private, true)) AND regexp(ch_name,  /.*%s.*/i)`, searchText)
	query := fmt.Sprintf(`query ChannelInfo($user_id: string){
				var(func: has(ch_name)) @filter(%s) {
                    totalCountVar as count(uid)
                }
				channelInfo(func: has(ch_name), first: %v, offset: %v) @filter(%s) {
					ch_uuid
					ch_handle
					ch_icon
					ch_name
					ch_created_at
					ch_updated_at
					ch_deleted_at
					ch_private
					ch_created_by {
						user_name
						user_uuid
					}
					ch_posts @filter(not gt(post_deleted_at, "1970-01-01T00:00:00Z")) (orderdesc: post_created_at, first: 1) {
					  post_text
					  post_created_at
					  post_by {
						user_name
					  }
					post_attachments {
						attachment_file_name
					}
					}
			  	}
				totalCount() {
                    count: sum(val(totalCountVar))
                }
			}`, filter, firstVal, offsetVal, filter)

	dgraphChannels, totalCount, err = dgraphModels.GetDgraphChannelsInfoAndCount(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetChannelListWithLatestPostWithUserIdAndSearchText Failed to get channel from dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetUserArchivedChannelListWithLatestPost(ctx context.Context, userDgraphId string, lastSeenTimestamps map[string]int, pageIndex int, pageSize int) (dgraphChannels []*dgraphStruct.DgraphChannel, totalCount int64, err error) {

	offset := pageIndex * pageSize
	firstVal := strconv.Itoa(pageSize)
	offsetVal := strconv.Itoa(offset)

	variables := make(map[string]string)
	variables["$user_id"] = userDgraphId

	query := fmt.Sprintf(`query ChannelInfo($user_id: string){
			    var(func: has(ch_uuid)) @filter(uid_in(~user_channels, $user_id) AND gt(ch_deleted_at, "1970-01-01T00:00:00Z")) {
                    totalCountVar as count(uid)
                }
				channelInfo(func: has(ch_uuid), first: %v, offset: %v) @filter(uid_in(~user_channels, $user_id) AND gt(ch_deleted_at, "1970-01-01T00:00:00Z")) {
					ch_uuid
					ch_handle
					ch_icon
					ch_name
					ch_created_at
					ch_updated_at
					ch_deleted_at
					ch_private
					ch_created_by {
						user_name
						user_uuid
					}
					ch_posts @filter(not gt(post_deleted_at, "1970-01-01T00:00:00Z")) (orderdesc: post_created_at, first: 1) {
					  post_text
					  post_created_at
					  post_by {
						user_name
					  }
						post_attachments {
							attachment_file_name
						}
					}
			  	}
				totalCount() {
                    count: sum(val(totalCountVar))
                }
			}`, firstVal, offsetVal)

	dgraphChannels, totalCount, err = dgraphModels.GetDgraphChannelsInfoAndCount(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetUserArchivedChannelListWithLatestPost Failed to get user in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetUserActiveChannelListWithLatestPost(ctx context.Context, userDgraphId string, lastSeenTimestamps map[string]int, pageIndex int, pageSize int) (dgraphChannels []*dgraphStruct.DgraphChannel, totalCount int64, err error) {
	offset := pageIndex * pageSize
	firstVal := strconv.Itoa(pageSize)
	offsetVal := strconv.Itoa(offset)

	variables := make(map[string]string)
	variables["$user_id"] = userDgraphId

	query := fmt.Sprintf(`query ChannelInfo($user_id: string){
				var(func: has(ch_uuid)) @filter(uid_in(~user_channels, $user_id) AND not gt(ch_deleted_at, "1970-01-01T00:00:00Z")) {
                    totalCountVar as count(uid)
                }
				channelInfo(func: has(ch_uuid), first: %v, offset: %v) @filter(uid_in(~user_channels, $user_id) AND not gt(ch_deleted_at, "1970-01-01T00:00:00Z")) {
					ch_uuid
					ch_handle
					ch_icon
					ch_name
					ch_created_at
					ch_updated_at
					ch_deleted_at
					ch_private
					ch_created_by {
						user_name
						user_uuid
					}
					ch_posts @filter(not gt(post_deleted_at, "1970-01-01T00:00:00Z")) (orderdesc: post_created_at, first: 1) {
					  post_text
					  post_created_at
					  post_by {
						user_name
					  }
						post_attachments {
							attachment_file_name
						}
					}
			  	}
				totalCount() {
                    count: sum(val(totalCountVar))
                }
			}`, firstVal, offsetVal)

	dgraphChannels, totalCount, err = dgraphModels.GetDgraphChannelsInfoAndCount(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetUserActiveChannelListWithLatestPost Failed to get user in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func DeleteChannelModeratorEdge(ctx context.Context, channelDgraphUID string, userDgraphUID string) (err error) {

	delStringJSON := fmt.Sprintf(`{
		"uid": "%s",
		"ch_moderators": {
			"uid": "%s"
		}
	}`, channelDgraphUID, userDgraphUID)

	err = dgraphModels.DeleteChannelEdge(ctx, delStringJSON)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/DeleteChannelModeratorEdge Failed to remove channel moderator in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

// InvalidateChannelBasicInfo drops every cached per-viewer summary of one channel.
//
// ChannelBasicInfo is the read behind roughly two dozen authorization checks —
// create a post, open a channel, an agent's send_message, the admins_only gate —
// and it holds ch_is_member and ch_is_admin for THIRTY MINUTES. So whether a
// permission change takes effect is decided entirely by whether its write path
// calls this.
//
// It was only being called on the safe half. Adding a member goes through
// CreateOrUpdateDgraphChannel, which purges; removing one went straight to the
// Dgraph mutation and purged nothing. Granting access was instant and REVOKING
// IT TOOK UP TO HALF AN HOUR: an admin could remove somebody from a private
// channel, watch the roster update, and that person could still read it and
// still post to it, because every gate was asking a cache that had not been
// told. Exactly backwards — a stale grant is a breach, a stale denial is only an
// inconvenience.
//
// Patterned on the channel rather than keyed on the one viewer on purpose: a
// membership change also moves ch_member_count in everybody else's cached copy.
func InvalidateChannelBasicInfo(ctx context.Context, channelUUID string) {
	if channelUUID == "" {
		return
	}
	_ = redisStore.DeletePattern(ctx, registry.ChannelBasicInfo.Pattern(channelUUID))
}

func DeleteChannelMemberEdge(ctx context.Context, channelDgraphUID string, userDgraphUID string) (err error) {
	delStringJSON := fmt.Sprintf(`
		[
			{
				"uid": "%s",
				"ch_members": [
					{
						"uid": "%s"
					}
				]
			},
			{
				"uid": "%s",
				"user_channels": [
					{
						"uid": "%s"
					}
				]
			},
			{
				"uid": "%s",
				"user_fav_channels": [
					{
						"uid": "%s"
					}
				]
			},
			{
				"uid": "%s",
				"ch_moderators": [
					{
						"uid": "%s"
					}
				]
			}
		]
	`, channelDgraphUID, userDgraphUID, userDgraphUID, channelDgraphUID, userDgraphUID, channelDgraphUID, channelDgraphUID, userDgraphUID)

	err = dgraphModels.DeleteChannelEdge(ctx, delStringJSON)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/DeleteChannelMemberEdge Failed to remove channel member in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func CreateChannelInOpenSearch(openSearchChannel *openSearchStruct.OpenSearchChannel) {
	ctx := context.Background()
	err := OpenSearchModels.CreateChannelInOpenSearch(ctx, openSearchChannel)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateChannelInOpenSearch Failed to create channel in opensearch err: %+v",
			err)
		return
	}
	return
}

func UpdateChannelInOpenSearch(openSearchChannel *openSearchStruct.OpenSearchChannel) {
	ctx := context.Background()
	err := OpenSearchModels.UpdateChannelInOpenSearch(ctx, openSearchChannel)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UpdateChannelInOpenSearch Failed to update channel in opensearch err: %+v",
			err)
		return
	}
	return
}

// HardDeleteChannel removes a channel row outright, and clears its cache entry.
//
// Compensation only: see models.HardDeleteChannel for why a soft delete cannot serve here
// (ch_name is UNIQUE and a soft-deleted row keeps the name).
func HardDeleteChannel(ctx context.Context, channelUUID uuid.UUID) (err error) {
	query := `DELETE FROM channels WHERE id = $1`
	err = models.HardDeleteChannel(query, channelUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/HardDeleteChannel Failed to delete channel row err: %+v", err)
		return
	}
	// Same invalidation the update path performs. A cached entry for a row that no longer
	// exists would outlive it for the whole TTL.
	_ = redisStore.DeletePattern(ctx, registry.ChannelBasicInfo.Pattern(channelUUID.String()))
	return
}

// RestoreChannelRow puts a channel's previous name, privacy and deletion state back, and clears
// its cache entry.
//
// Compensation only, for an update whose Postgres half succeeded and whose Dgraph half did not.
// deletedAt is nil to restore SQL NULL, which is what a channel that was not archived needs.
//
// The cache invalidation is not optional. UpdateChannelInfo invalidates on the way in, so
// without this the cache would keep serving the value from the update that was rolled back —
// leaving the visible state wrong even though both stores now agree.
func RestoreChannelRow(ctx context.Context, channelUUID uuid.UUID, channelName string, channelPrivate bool, deletedAt *time.Time) (err error) {
	query := `
        UPDATE channels
        SET ch_name = $1, ch_private = $2, deleted_at = $3, updated_at = $4
		WHERE id = $5`
	err = models.RestoreChannelRow(query, channelName, channelPrivate, deletedAt, time.Now(), channelUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/RestoreChannelRow Failed to restore channel row err: %+v", err)
		return
	}
	_ = redisStore.DeletePattern(ctx, registry.ChannelBasicInfo.Pattern(channelUUID.String()))
	return
}
