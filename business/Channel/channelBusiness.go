package business

import (
	"context"
	"fmt"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Channel"
	business "github.com/akashc777/OneCamp/business/LastSeenChannel"
	LiveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	userChannelNotificationBusiness "github.com/akashc777/OneCamp/business/UserChannelNotification"
	userFCMtokenBusiness "github.com/akashc777/OneCamp/business/UserFCMToken"
	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	domain "github.com/akashc777/OneCamp/domain/Channel"
	globalSearchDomain "github.com/akashc777/OneCamp/domain/GlobalSearch"
	postDomain "github.com/akashc777/OneCamp/domain/Post"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphChannelModels "github.com/akashc777/OneCamp/models/dgraph/Channel"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	postgressStruct "github.com/akashc777/OneCamp/models/postgres"
	aiModels "github.com/akashc777/OneCamp/models/postgres/AI"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	memoryModels "github.com/akashc777/OneCamp/models/postgres/WorkspaceMemory"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

func CreateChannel(ctx context.Context, channelInfo *adapter.InputCreateChannel, userInfo *userModels.UserInfo) (err error, channelUUID uuid.UUID) {

	// create channel uuid
	channelUUID = uuid.New()

	// create channel in postgres
	err = domain.CreateChannel(ctx, channelInfo.ChannelName, userInfo.UserPostgresInfo.Id, channelInfo.ChannelPrivate, channelUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreateChannel Failed to create new channel err: %+v",
			err)
		return
	}

	currentTime := helpers.CreateTimeOrNow(ctx)
	zeroUnixTime := time.Time{}

	// add channel in dgraph
	dgraphChannel := dgraphStruct.DgraphChannel{
		Uid:   "uid(ch)",
		DType: []string{"Channel"},
		CreatedBy: &dgraphStruct.DgraphUser{
			DType: []string{"User"},
			Uid:   userInfo.UserDgraphInfo.Uid,
			Channels: []*dgraphStruct.DgraphChannel{{
				Uid: "uid(ch)",
			}},
		},
		Uuid: channelUUID.String(),
		Moderators: []*dgraphStruct.DgraphUser{{
			Uid: userInfo.UserDgraphInfo.Uid,
		}},
		Members: []*dgraphStruct.DgraphUser{{
			Uid: userInfo.UserDgraphInfo.Uid,
		}},
		CreatedAt: &currentTime,
		UpdatedAt: &currentTime,
		IconObj:   channelInfo.ChannelProfileKey,
		Name:      channelInfo.ChannelName,
		IsPrivate: &channelInfo.ChannelPrivate,
		DeletedAt: &zeroUnixTime,
	}

	_, err = CreateOrUpdateDgraphChannel(ctx, &dgraphChannel)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreateChannel Failed to create new channel in dgraph err: %+v",
			err)

		// Reverse the Postgres insert. Leaving it made this operation PERMANENTLY
		// UNREPEATABLE: channels.ch_name is UNIQUE, every channel listing reads Dgraph, so
		// the row was a channel nobody could see that still owned the name — and each retry
		// failed on the constraint. One transient Dgraph error cost the user that name for
		// good.
		//
		// A hard delete, not a soft one: a soft-deleted row keeps the unique name, which is
		// the whole problem. Safe here because last-seen, notifications and the search
		// document are all written AFTER this point, so the row has no children yet and the
		// foreign keys on channels(id) cannot block it.
		_ = helpers.CompensateOnFailure(ctx, "channel row for "+channelInfo.ChannelName,
			func(undoCtx context.Context) error {
				return domain.HardDeleteChannel(undoCtx, channelUUID)
			})

		return
	}

	// update channel last seen
	err = business.CreateOrUpdateLastSeenChannel(ctx, userInfo.UserPostgresInfo.Id, channelUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreateChannel Failed to create entry in last seen channel err: %+v",
			err)
		return
	}

	go userChannelNotificationBusiness.CreateChannelNotificationType(userInfo.UserDgraphInfo.Uuid, channelUUID.String(), postgressStruct.NOTIFICATION_TYPE_ALL)

	go domain.CreateChannelInOpenSearch(&openSearchStruct.OpenSearchChannel{
		Uuid:             channelUUID.String(),
		ChannelName:      channelInfo.ChannelName,
		ChannelCreatedAt: currentTime.Unix(),
		ChannelDeletedAt: nil,
	})

	if helpers.IsBulkImport(ctx) {
		// Skip the per-channel webhook + last-seen + notification setup
		// during a bulk Slack import. Channels are created in bulk, and
		// the importer wires membership en masse via the channel
		// resolver afterwards. The webhook fan-out would otherwise emit
		// hundreds of "channel.created" events for historical content.
		return
	}

	go webhookBusiness.DispatchEvent(context.WithoutCancel(ctx), "channel.created", map[string]interface{}{
		"channel_id":   channelUUID.String(),
		"channel_name": channelInfo.ChannelName,
		"created_by":   userInfo.UserDgraphInfo.Uuid,
	})

	return

}

// recoverChannelMemoryCascade guards a fire-and-forget channel memory
// cascade goroutine so a panic in the best-effort AI projection layer can
// never crash the server. phase is "archive" or "restore" for the log.
func recoverChannelMemoryCascade(phase string) {
	if r := recover(); r != nil {
		helpers.MessageLogs.ErrorLog.Printf("panic in channel memory %s cascade: %v", phase, r)
	}
}

// channelDeletedAtOrNil turns a stored deleted_at into the pointer a restore needs.
//
// The Postgres column is nullable but the model carries a plain time.Time, so "never archived"
// arrives as the zero time rather than as an absence. Writing that zero time straight back would
// set deleted_at to year 1 — archiving a live channel while trying to undo a failed rename, and
// turning a recoverable failure into a channel that vanishes. nil restores SQL NULL.
func channelDeletedAtOrNil(deletedAt time.Time) *time.Time {
	if deletedAt.IsZero() {
		return nil
	}
	return &deletedAt
}

func UpdateChannelInfo(ctx context.Context, channelUpdateInfo *adapter.UpdateChannelInfo, channelUUID uuid.UUID) (err error) {

	currentTime := time.Now()

	// Snapshot before writing, so the Postgres half can be put back if the Dgraph half fails.
	// Best-effort on purpose: a channel the user asked to rename should not be refused because
	// a read failed, but losing the snapshot means losing the ability to compensate, so say so
	// rather than discovering it in the failure branch.
	priorChannel, snapshotErr := domain.GetChannelInfoByUUID(ctx, channelUUID)
	if snapshotErr != nil || priorChannel == nil {
		helpers.LogWarnWithContext(ctx,
			"business/UpdateChannelInfo could not snapshot channel %s before update; a Dgraph "+
				"failure will not be reversible: %+v", channelUUID, snapshotErr)
		priorChannel = nil
	}

	if !channelUpdateInfo.ChannelArchived {
		err = domain.UpdateChannelInfo(ctx, channelUpdateInfo.ChannelName, channelUpdateInfo.ChannelPrivate, currentTime, channelUUID)
	} else {
		err = domain.SoftDeleteChannelInfo(ctx, channelUpdateInfo.ChannelName, channelUpdateInfo.ChannelPrivate, currentTime, channelUUID)
	}

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/UpdateChannelInfo Failed to update channel info err: %+v",
			err)
		return
	}
	zeroUnixTime := time.Time{}

	dgraphChannel := dgraphStruct.DgraphChannel{
		Uid:       "uid(ch)",
		DType:     []string{"Channel"},
		Name:      channelUpdateInfo.ChannelName,
		IsPrivate: &channelUpdateInfo.ChannelPrivate,
		IconObj:   channelUpdateInfo.ChannelProfileKey,
		Uuid:      channelUpdateInfo.ChannelUuid,
		About:     channelUpdateInfo.ChannelAbout,
		UpdatedAt: &currentTime,
		DeletedAt: &zeroUnixTime,
	}

	if channelUpdateInfo.ChannelArchived {
		dgraphChannel.DeletedAt = &currentTime
	}

	_, err = CreateOrUpdateDgraphChannel(ctx, &dgraphChannel)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/UpdateChannelInfo Failed to update channel info in dgraph err: %+v",
			err)

		// Put the Postgres half back. Leaving it made the two stores disagree about a
		// channel's name, privacy or archived state — the API reported failure while Postgres
		// held the new value and Dgraph, which every listing reads, held the old one.
		//
		// The original TODO here said to REMOVE the channel from Postgres. That would have
		// been wrong: this is an update, not a create, so the row existed before and deleting
		// it would destroy a live channel over a failed rename. It is restored, not removed.
		if priorChannel != nil {
			_ = helpers.CompensateOnFailure(ctx, "channel update for "+channelUUID.String(),
				func(undoCtx context.Context) error {
					return domain.RestoreChannelRow(undoCtx, channelUUID,
						priorChannel.Name, priorChannel.IsPrivate,
						channelDeletedAtOrNil(priorChannel.DeletedAt))
				})
		} else {
			helpers.LogErrorWithContext(ctx,
				"business/UpdateChannelInfo channel %s now DIVERGES between postgres and dgraph "+
					"and cannot be reversed: no snapshot was taken", channelUUID)
		}

		return
	}

	var deletedAt *int64 = nil
	if channelUpdateInfo.ChannelArchived {
		deletedAt = helpers.Int64Pointer(currentTime.Unix())
		go globalSearchDomain.SyncCascadingDeletionInOpenSearch(
			[]string{"post_ch_id", "comment_channel_id", "attachment_channel_id"},
			channelUpdateInfo.ChannelUuid,
			*deletedAt,
			[]string{"posts", "comments", "attachments"},
			"cascade",
		)
		// Cascade the archive into the memory layer by SCOPE so every fact
		// derived from this channel (worker-extracted scope-keyed items AND
		// manual captures) stops surfacing as AI-queryable memory. Reversible
		// via the unarchive branch below. Own goroutine so a slow memory
		// store never extends the archive request.
		go func(chUUID string) {
			defer recoverChannelMemoryCascade("archive")
			ai.ArchiveMemoryByScope(context.Background(), memoryModels.ScopeRef{ChannelUUID: chUUID})
		}(channelUpdateInfo.ChannelUuid)
		go webhookBusiness.DispatchEvent(context.WithoutCancel(ctx), "channel.archived", map[string]interface{}{
			"channel_id":   channelUpdateInfo.ChannelUuid,
			"channel_name": channelUpdateInfo.ChannelName,
		})
	} else {
		go globalSearchDomain.SyncCascadingUnarchiveInOpenSearch(
			[]string{"post_ch_id", "comment_channel_id", "attachment_channel_id"},
			channelUpdateInfo.ChannelUuid,
			[]string{"posts", "comments", "attachments"},
			"cascade",
		)
		// Revive the memory items the archive cascade removed for this channel
		// (never items a user independently deleted). Mirrors the OpenSearch
		// unarchive above.
		go func(chUUID string) {
			defer recoverChannelMemoryCascade("restore")
			ai.RestoreMemoryByScope(context.Background(), memoryModels.ScopeRef{ChannelUUID: chUUID})
		}(channelUpdateInfo.ChannelUuid)
	}

	go domain.UpdateChannelInOpenSearch(&openSearchStruct.OpenSearchChannel{
		Uuid:             channelUpdateInfo.ChannelUuid,
		ChannelName:      channelUpdateInfo.ChannelName,
		ChannelDeletedAt: deletedAt,
	})

	// Persist the announcement (post-policy) setting in the same request so
	// the edit dialog saves everything atomically (one API call). Empty means
	// "leave untouched" — callers that don't manage the policy are unaffected.
	if channelUpdateInfo.PostPolicy != "" {
		if channelUpdateInfo.PostPolicy != ChannelPostPolicyEveryone && channelUpdateInfo.PostPolicy != ChannelPostPolicyAdminsOnly {
			return fmt.Errorf("invalid post policy")
		}
		if perr := domain.SetChannelPostPolicy(ctx, channelUUID, channelUpdateInfo.PostPolicy); perr != nil {
			helpers.LogErrorWithContext(ctx,
				"business/UpdateChannelInfo Failed to set post policy err: %+v", perr)
			return perr
		}
	}

	// Notify members live so the composer/header/archived-state update
	// without a manual refresh.
	updateAction := ChannelUpdateActionUpdated
	if channelUpdateInfo.ChannelArchived {
		updateAction = ChannelUpdateActionArchived
	}
	NotifyChannelUpdated(channelUpdateInfo.ChannelUuid, updateAction)

	return
}

func CheckIfChannelExist(ctx context.Context, channelHandle *string) (exist bool, err error) {

	exist, err = domain.CheckIfChannelExist(ctx, channelHandle)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CheckIfChannelExist Failed to check channel by ch_name err: %+v",
			err)
		return
	}

	return
}

func GetDgraphChannelInfoByUUIDAndModeratorInfo(ctx context.Context, channelUUID uuid.UUID, userUUID uuid.UUID, userDgraphUUID string, userModeratorUUID string) (channelInfo *dgraphStruct.DgraphChannel, err error) {

	channelInfo, err = domain.GetDgraphChannelInfoByUUIDAndModeratorInfo(ctx, channelUUID.String(), userDgraphUUID, userModeratorUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetDgraphChannelInfoByUUIDAndModeratorInfo Failed to get channel info by channel uuid err: %+v",
			err)
		return
	}

	return
}

func GetDgraphChannelInfoByUUIDAndMemberInfo(ctx context.Context, channelUUID uuid.UUID, userDgraphUUID string, userMemberUUID string) (channelInfo *dgraphStruct.DgraphChannel, err error) {

	channelInfo, err = domain.GetDgraphChannelInfoByUUIDAndMemberInfo(ctx, channelUUID.String(), userDgraphUUID, userMemberUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetDgraphChannelInfoByUUIDAndMemberInfo Failed to get channel info by channel uuid err: %+v",
			err)
		return
	}

	return
}

func GetChannelCallActiveStatus(ctx context.Context, channelId string) (exists bool, err error) {
	// Use human-participant detection, not room existence: the transcriber
	// agent and LiveKit's EmptyTimeout both keep the room object alive after
	// the last human leaves, which would otherwise keep the call dot lit.
	exists, err = LiveKitBusiness.IsCallActive(ctx, channelId)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetChannelCallActiveStatus Failed ro check channel call room err: %+v",
			err,
		)
		return
	}
	return
}

func GetBasicDgraphChannelInfoByUUIDAndUpdateLastSeen(ctx context.Context, channelUUID uuid.UUID, userUUID uuid.UUID, userDgraphUUID string) (channelInfo *dgraphStruct.DgraphChannel, err error) {

	channelInfo, err = domain.GetBasicDgraphChannelInfoByUUID(ctx, channelUUID.String(), userDgraphUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetBasicDgraphChannelInfoByUUIDAndUpdateLastSeen Failed to get channel info by channel uuid err: %+v",
			err)
		return
	}

	// update last seen
	err = business.CreateOrUpdateLastSeenChannel(ctx, userUUID, channelUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetBasicDgraphChannelInfoByUUIDAndUpdateLastSeen Failed to create entry in last seen channel err: %+v",
			err)
		return
	}

	return
}

// MarkChannelSeen advances the user's last-seen marker for a channel WITHOUT
// fetching channel info. This is the in-session "I've read everything here"
// path: the FE calls it when the user leaves a channel (page unmount /
// channelId change) so messages that arrived while they were actively viewing
// (bot replies, other members) do not resurrect the unread badge on the next
// channel-list refetch. It reuses the same last-seen primitive that channel
// open and posting already advance, so the unread count (computed as
// COUNT(posts created_at > user_last_seen)) collapses to zero consistently.
func MarkChannelSeen(ctx context.Context, userID uuid.UUID, channelUUID uuid.UUID) (err error) {
	err = business.CreateOrUpdateLastSeenChannel(ctx, userID, channelUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/MarkChannelSeen Failed to advance last seen channel err: %+v",
			err)
		return
	}
	return
}

func GetChannelRecordingTranscript(ctx context.Context, channelUUID uuid.UUID, userDgraphUUID string, egressId string, pageIndex int, pageSize int) (dgraphChannel *dgraphStruct.DgraphChannel, err error) {
	dgraphChannel, err = domain.GetChannelRecordingTranscript(ctx, channelUUID.String(), userDgraphUUID, egressId, pageIndex, pageSize)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetChannelRecordingTranscript Failed to get channel recording transcript by channel uuid err: %+v",
			err,
		)
		return
	}

	return
}

// ErrNotFound is GetBasicDgraphChannelInfoByUUID's error for a channel that
// doesn't exist, as opposed to one the graph couldn't be asked about.
var ErrNotFound = dgraphChannelModels.ErrNotFound

func GetBasicDgraphChannelInfoByUUID(ctx context.Context, channelUUID uuid.UUID, userDgraphUUID string) (channelInfo *dgraphStruct.DgraphChannel, err error) {

	channelInfo, err = domain.GetBasicDgraphChannelInfoByUUID(ctx, channelUUID.String(), userDgraphUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetBasicDgraphChannelInfoByUUID Failed to get channel info by channel uuid err: %+v",
			err)
		return
	}

	return
}

// ChannelPostPolicyEveryone / AdminsOnly are the supported posting policies.
const (
	ChannelPostPolicyEveryone   = "everyone"
	ChannelPostPolicyAdminsOnly = "admins_only"
)

// SetChannelPostPolicy sets a channel's posting policy (announcement mode).
// Validates the value; persists to Postgres + Dgraph + cache via the domain.
func SetChannelPostPolicy(ctx context.Context, channelUUID uuid.UUID, policy string) error {
	if policy != ChannelPostPolicyEveryone && policy != ChannelPostPolicyAdminsOnly {
		return fmt.Errorf("invalid post policy")
	}
	if err := domain.SetChannelPostPolicy(ctx, channelUUID, policy); err != nil {
		return err
	}
	// Live-update members so the composer flips to/from the announcement
	// read-only notice without a manual refresh.
	NotifyChannelUpdated(channelUUID.String(), ChannelUpdateActionPostPolicy)
	return nil
}

// GetChannelAITokenCap returns a channel's per-day AI token cap (0 = no cap).
func GetChannelAITokenCap(ctx context.Context, channelUUID uuid.UUID) (int, error) {
	return domain.GetChannelAITokenCap(ctx, channelUUID)
}

// SetChannelAITokenCap sets a channel's per-day AI token cap (0 = no cap),
// the Claude-Tag-style channel-level cost control. Clamped non-negative.
func SetChannelAITokenCap(ctx context.Context, channelUUID uuid.UUID, cap int) error {
	if cap < 0 {
		cap = 0
	}
	return domain.SetChannelAITokenCap(ctx, channelUUID, cap)
}

// GetChannelAIModel returns a channel's pinned default AI model id (nil = no
// override; runs use the agent's pinned model, then the workspace default).
func GetChannelAIModel(ctx context.Context, channelUUID uuid.UUID) (*uuid.UUID, error) {
	return domain.GetChannelAIModel(ctx, channelUUID)
}

// SetChannelAIModel pins a channel's default AI model (or clears it with nil).
// A non-nil model must be a real, usable entry in the admin allowlist — so a
// channel can never be pinned to a disabled/unknown model that would then
// silently fall back — otherwise it is rejected.
func SetChannelAIModel(ctx context.Context, channelUUID uuid.UUID, modelID *uuid.UUID) error {
	if modelID != nil {
		am, err := aiModels.GetAuthorizedModel(ctx, *modelID)
		if err != nil || am == nil || !am.Usable() {
			return fmt.Errorf("that model isn't available — pick an enabled model from the allowlist")
		}
	}
	return domain.SetChannelAIModel(ctx, channelUUID, modelID)
}

func GetBasicChannelAndPostInfoByUUID(ctx context.Context, channelUUID string, postUUID string, userDgraphUUID string) (channelInfo *dgraphStruct.DgraphChannel, err error) {

	channelInfo, err = domain.GetBasicChannelAndPostInfoByUUID(ctx, channelUUID, postUUID, userDgraphUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetBasicChannelAndPostInfoByUUID Failed to get channel info by channel uuid err: %+v",
			err)
		return
	}

	return
}

type RecordingPagination struct {
	Recordings []*dgraphStruct.DgraphRecording `json:"recordings,omitempty"`
	HasMore    bool                            `json:"has_more"`
	IsMember   int                             `json:"is_member"`
}

func GetChannelAllRecordingList(ctx context.Context, channelUUID string, userDgraphUUID string, startDate string, endDate string, pageIndex int, pageSize int) (recordingsPagination RecordingPagination, err error) {
	channelInfo, err := domain.GetChannelAllRecordingList(ctx, channelUUID, userDgraphUUID, startDate, endDate, pageIndex, pageSize)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetChannelAllRecordingList Failed to get channel recording info by channel uuid err: %+v",
			err)
		return
	}

	if channelInfo == nil {
		return
	}

	recordingsPagination.IsMember = int(channelInfo.IsMember)

	if len(channelInfo.Recordings) > pageSize {
		recordingsPagination.Recordings = channelInfo.Recordings[:pageSize]
	} else {
		recordingsPagination.Recordings = channelInfo.Recordings
	}

	recordingsPagination.HasMore = len(channelInfo.Recordings) > pageSize

	return
}

func GetDgraphChannelInfoByUUID(ctx context.Context, channelUUID uuid.UUID, userDgraphUUID string) (channelInfo *dgraphStruct.DgraphChannel, err error) {

	channelInfo, err = domain.GetDgraphChannelInfoByUUID(ctx, channelUUID.String(), userDgraphUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetChannelByUUID Failed to get channel info by channel uuid err: %+v",
			err)
		return
	}

	return
}

func GetDgraphChannelInfoByUUIDWithMemberAdminFlag(ctx context.Context, channelUUID uuid.UUID, userDgraphUUID string) (channelInfo *dgraphStruct.DgraphChannel, err error) {

	channelInfo, err = domain.GetDgraphChannelInfoByUUID(ctx, channelUUID.String(), userDgraphUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetDgraphChannelInfoByUUIDWithMemberAdminFlag Failed to get channel info by channel uuid err: %+v",
			err)
		return
	}

	adminIndex := 0
	adminLength := len(channelInfo.Moderators)

	if adminLength > 0 {

		for _, member := range channelInfo.Members {
			if member.Uuid == channelInfo.Moderators[adminIndex].Uuid {
				member.IsAdmin = true
				adminIndex = adminIndex + 1
			}

			if adminIndex == adminLength {
				break
			}
		}

	}

	channelInfo.Moderators = nil

	return
}

func GetUsersChannelListWithPublicChannel(ctx context.Context, userDgraphId string) (channelsInfo []*dgraphStruct.DgraphChannel, err error) {
	channelsInfo, err = domain.GetUsersChannelListWithPublicChannel(ctx, userDgraphId)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetUsersChannelListWithPublicChannel Failed to get users channels list err: %+v",
			err)
		return
	}

	return
}

func GetChannelListWithMemberFlag(ctx context.Context, userDgraphID string, channelDgraphUIDs []string) (dgraphChannels []*dgraphStruct.DgraphChannel, err error) {
	dgraphChannels, err = domain.GetChannelListWithMemberFlag(ctx, userDgraphID, channelDgraphUIDs)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetChannelListWithMemberFlag Failed to get channels list err: %+v",
			err)
		return
	}

	return
}

func GetChannelListWithLatestPostWithUserIdAndSearchText(ctx context.Context, userDgraphId string, searchText string, pageIndex int, pageSize int) (channelsInfo []*dgraphStruct.DgraphChannel, totalCount int64, err error) {
	channelsInfo, totalCount, err = domain.GetChannelListWithLatestPostWithUserIdAndSearchText(ctx, userDgraphId, searchText, pageIndex, pageSize)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetChannelListWithLatestPostWithUserIdAndSearchText Failed to get users channels list err: %+v",
			err)
		return
	}

	return
}

func GetAllActiveChannelListWithLatestPost(ctx context.Context, userDgraphId string, pageIndex int, pageSize int) (channelsInfo []*dgraphStruct.DgraphChannel, totalCount int64, err error) {
	channelsInfo, totalCount, err = domain.GetAllActiveChannelListWithLatestPost(ctx, userDgraphId, pageIndex, pageSize)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetAllActiveChannelListWithLatestPost Failed to get channels list err: %+v",
			err)
		return
	}

	return
}

func GetActiveChannelListWithLatestPostWithUserIdAndSearchText(ctx context.Context, userDgraphId string, searchText string, pageIndex int, pageSize int) (channelsInfo []*dgraphStruct.DgraphChannel, totalCount int64, err error) {
	channelsInfo, totalCount, err = domain.GetActiveChannelListWithLatestPostWithUserIdAndSearchText(ctx, userDgraphId, searchText, pageIndex, pageSize)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetActiveChannelListWithLatestPostWithUserIdAndSearchText Failed to get users channels list err: %+v",
			err)
		return
	}

	return
}

func GetArchivedChannelListWithLatestPostWithUserIdAndSearchText(ctx context.Context, userDgraphId string, searchText string, pageIndex int, pageSize int) (channelsInfo []*dgraphStruct.DgraphChannel, totalCount int64, err error) {
	channelsInfo, totalCount, err = domain.GetArchivedChannelListWithLatestPostWithUserIdAndSearchText(ctx, userDgraphId, searchText, pageIndex, pageSize)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetArchivedChannelListWithLatestPostWithUserIdAndSearchText Failed to get users channels list err: %+v",
			err)
		return
	}

	return
}

func GetUserArchivedChannelListWithLatestPost(ctx context.Context, userDgraphId string, userID uuid.UUID, pageIndex int, pageSize int) (channelsInfo []*dgraphStruct.DgraphChannel, totalCount int64, err error) {
	lastSeenTimestamps := make(map[string]int)
	channelsInfo, totalCount, err = domain.GetUserArchivedChannelListWithLatestPost(ctx, userDgraphId, lastSeenTimestamps, pageIndex, pageSize)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetUserArchivedChannelListWithLatestPost Failed to get users channels list err: %+v",
			err)
		return
	}

	return
}

func GetUserActiveChannelListWithLatestPost(ctx context.Context, userDgraphId string, userID uuid.UUID, pageIndex int, pageSize int) (channelsInfo []*dgraphStruct.DgraphChannel, totalCount int64, err error) {
	lastSeenTimestamps := make(map[string]int)
	channelsInfo, totalCount, err = domain.GetUserActiveChannelListWithLatestPost(ctx, userDgraphId, lastSeenTimestamps, pageIndex, pageSize)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetUserActiveChannelListWithLatestPost Failed to get users channels list err: %+v",
			err)
		return
	}

	err, channelPostCount := postDomain.GetLatestPostInChannelCountByUserID(ctx, userID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetUserActiveChannelListWithLatestPost Failed to post count for channels err: %+v",
			err)
		return
	}

	for ind := range channelsInfo {
		if channelsInfo[ind] == nil {
			continue
		}
		if count, ok := channelPostCount[channelsInfo[ind].Uuid]; ok {
			channelsInfo[ind].UnreadPostCount = count.PostCount
		}
	}

	return
}

func CreateOrUpdateDgraphChannel(ctx context.Context, dgraphChannel *dgraphStruct.DgraphChannel) (channelUid string, err error) {

	channelUid, err = domain.CreateOrUpdateDgraphChannel(ctx, dgraphChannel)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreateOrUpdateDgraphChannel Failed to create/update channel err: %+v",
			err)
		return
	}

	return
}

// DeleteChannelModeratorEdge demotes a channel admin. channelUUID is required
// for cache invalidation: ch_is_admin is cached per viewer, and the admins_only
// post gate reads it, so a demotion that skipped the purge left the demoted
// person posting in an announcement channel for the rest of the TTL.
func DeleteChannelModeratorEdge(ctx context.Context, channelDgraphUID string, userDgraphUID string, channelUUID string) (err error) {

	err = domain.DeleteChannelModeratorEdge(ctx, channelDgraphUID, userDgraphUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/DeleteChannelModeratorEdge Failed to remove channel moderator err: %+v",
			err)
		return
	}

	domain.InvalidateChannelBasicInfo(ctx, channelUUID)

	return
}

func AddChannelModeratorEdge(ctx context.Context, channelUUID string, userUUID string) (err error) {
	dgraphChannel := dgraphStruct.DgraphChannel{
		Uid:   "uid(ch)",
		DType: []string{"Channel"},
		Uuid:  channelUUID,
		Moderators: []*dgraphStruct.DgraphUser{{
			Uid:   userUUID,
			DType: []string{"User"},
		}},
	}

	_, err = CreateOrUpdateDgraphChannel(ctx, &dgraphChannel)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/UpdateChannelName Failed to add channel moderator in dgraph err: %+v",
			err)

		return
	}

	NotifyChannelUpdated(channelUUID, ChannelUpdateActionModerators)

	return
}

func AddChannelMemberEdge(ctx context.Context, channelUUID uuid.UUID, userDgraphInfo *dgraphStruct.DgraphUser, newMemberUUID uuid.UUID) (err error) {

	dgraphChannel := dgraphStruct.DgraphChannel{
		Uid:   "uid(ch)",
		DType: []string{"Channel"},
		Uuid:  channelUUID.String(),
		Members: []*dgraphStruct.DgraphUser{{
			Uid:   userDgraphInfo.Uid,
			DType: []string{"User"},
			Channels: []*dgraphStruct.DgraphChannel{{
				Uid: "uid(ch)",
			}},
		}},
	}

	_, err = CreateOrUpdateDgraphChannel(ctx, &dgraphChannel)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/AddChannelMemberEdge Failed to add channel member in dgraph err: %+v",
			err)

		return
	}

	// update channel last seen
	err = business.CreateOrUpdateLastSeenChannel(ctx, newMemberUUID, channelUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/AddChannelMemberEdge Failed to create entry in last seen channel err: %+v",
			err)
		return
	}

	go userChannelNotificationBusiness.CreateChannelNotificationType(userDgraphInfo.Uuid, channelUUID.String(), postgressStruct.NOTIFICATION_TYPE_ALL)

	if helpers.IsBulkImport(ctx) {
		// Skip the user.joined webhook fan-out during a bulk Slack
		// import. Adding 200 users to 50 channels would otherwise
		// dispatch 10k webhook events for historical membership.
		return
	}

	go webhookBusiness.DispatchEvent(context.WithoutCancel(ctx), "user.joined", map[string]interface{}{
		"user_id":    userDgraphInfo.Uuid,
		"channel_id": channelUUID.String(),
	})

	NotifyChannelUpdated(channelUUID.String(), ChannelUpdateActionMemberAdded)

	return
}

func DeleteChannelMemberEdge(ctx context.Context, channelDgraphUID string, userDgraphUID string, memberUUID string, channelUUID string) (err error) {

	err = domain.DeleteChannelMemberEdge(ctx, channelDgraphUID, userDgraphUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/DeleteChannelMemberEdge Failed to remove channel member err: %+v",
			err)
		return
	}

	// Before any of the best-effort fan-out below, and synchronously: until this
	// runs, every permission gate still reads a cached "yes", and search still
	// covers the channel for them.
	domain.InvalidateChannelBasicInfo(ctx, channelUUID)
	userDomain.InvalidateUserMemberships(ctx, memberUUID)

	go userChannelNotificationBusiness.DeleteNotificationTypeWhenUserIsRemovedFormChannel(memberUUID, channelUUID)
	go webhookBusiness.DispatchEvent(context.WithoutCancel(ctx), "user.left", map[string]interface{}{
		"user_id":    memberUUID,
		"channel_id": channelUUID,
	})

	NotifyChannelUpdated(channelUUID, ChannelUpdateActionMemberLeft)

	return
}

// AddChannelBotMemberEdge makes a bot principal (an AI agent's bot user) a real
// member of a channel (the ch_members edge), so "the AI is in this channel" is
// a true, queryable fact and the agent shows in the roster. It mirrors
// AddChannelMemberEdge but WITHOUT the human-onboarding side effects: no
// user.joined webhook (a bot joining must never trigger event-agents — that
// would be a cascade), and no last-seen / notification-type seeding (a
// non-login principal never reads or is notified). It still emits a
// channel-updated nudge so open clients revalidate. Idempotent.
func AddChannelBotMemberEdge(ctx context.Context, channelUUID uuid.UUID, botDgraphUID string) (err error) {
	if botDgraphUID == "" {
		return
	}
	dgraphChannel := dgraphStruct.DgraphChannel{
		Uid:   "uid(ch)",
		DType: []string{"Channel"},
		Uuid:  channelUUID.String(),
		Members: []*dgraphStruct.DgraphUser{{
			Uid:      botDgraphUID,
			DType:    []string{"User"},
			Channels: []*dgraphStruct.DgraphChannel{{Uid: "uid(ch)"}},
		}},
	}
	if _, err = CreateOrUpdateDgraphChannel(ctx, &dgraphChannel); err != nil {
		helpers.LogErrorWithContext(ctx, "business/AddChannelBotMemberEdge failed: %+v", err)
		return
	}
	NotifyChannelUpdated(channelUUID.String(), ChannelUpdateActionMemberAdded)
	return
}

// RemoveChannelBotMemberEdge removes a bot principal from a channel's members.
// Idempotent; resolves the channel's Dgraph uid first (membership deletion is
// keyed on node uids).
func RemoveChannelBotMemberEdge(ctx context.Context, channelUUID uuid.UUID, botDgraphUID string) (err error) {
	if botDgraphUID == "" {
		return
	}
	channelInfo, cerr := GetBasicDgraphChannelInfoByUUID(ctx, channelUUID, botDgraphUID)
	if cerr != nil || channelInfo == nil || channelInfo.Uid == "" {
		if cerr != nil {
			helpers.LogErrorWithContext(ctx, "business/RemoveChannelBotMemberEdge channel lookup failed: %+v", cerr)
		}
		return cerr
	}
	if err = domain.DeleteChannelMemberEdge(ctx, channelInfo.Uid, botDgraphUID); err != nil {
		helpers.LogErrorWithContext(ctx, "business/RemoveChannelBotMemberEdge failed: %+v", err)
		return
	}
	domain.InvalidateChannelBasicInfo(ctx, channelUUID.String())
	NotifyChannelUpdated(channelUUID.String(), ChannelUpdateActionMemberLeft)
	return
}

// Channel-update action labels published over MQTT so members revalidate
// their cached channel state in real time. Advisory only — the FE revalidates
// on any action value.
const (
	ChannelUpdateActionUpdated     = "updated"
	ChannelUpdateActionArchived    = "archived"
	ChannelUpdateActionUnarchived  = "unarchived"
	ChannelUpdateActionPostPolicy  = "post_policy"
	ChannelUpdateActionMemberAdded = "member_added"
	ChannelUpdateActionMemberLeft  = "member_removed"
	ChannelUpdateActionModerators  = "moderators_changed"
)

// NotifyChannelUpdated pushes a lightweight "this channel changed" signal to
// all members currently subscribed to the channel's message topic. Callers in
// the controller layer use this after a successful mutation that lacks the
// channel UUID at the business layer (e.g. moderator removal).
func NotifyChannelUpdated(channelUUID string, action string) {
	go mqttBusiness.PublishChannelUpdate(channelUUID, action)
}

func PublishTypingInChannel(userInfo *dgraphStruct.DgraphUser, channelId string) {
	mqttChannelTyping := mqttStruct.MqttChannelTyping{
		UserName:    userInfo.UserName,
		UserUUID:    userInfo.Uuid,
		ChannelUuid: channelId,
	}

	if userInfo.ProfileKey != nil {
		mqttChannelTyping.UserProfile = *userInfo.ProfileKey
	}
	go mqttBusiness.PublishChannelTyping(&mqttChannelTyping, channelId)
}

func MakeVideoChannelCall(ctx context.Context, channelDraphInfo *dgraphStruct.DgraphChannel, userDraphInfo *dgraphStruct.DgraphUser, isAdmin bool, audioEnabled bool, videoEnabled bool) (token string, alreadyExisted bool, err error) {

	token, alreadyExisted, err = LiveKitBusiness.CreateRoomAndGetToken(ctx, channelDraphInfo.Uuid, userDraphInfo, isAdmin, audioEnabled, videoEnabled)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/MakeVideoChannelCall Failed to make video call post err: %+v",
			err)

		return
	}

	if !alreadyExisted {

		pushTitle := fmt.Sprintf("#%s - %s", channelDraphInfo.Name, userDraphInfo.UserName)
		body := "started call"

		mqttChannelCall := mqttStruct.MqttChannelCall{
			CallActive:  mqttStruct.MESSAGE_CALL_ACTIVE,
			ChannelUUID: channelDraphInfo.Uuid,
		}

		go mqttBusiness.PublishChannelCall(&mqttChannelCall, channelDraphInfo.Uuid)

		go sendChannelCallNotification(pushTitle, body, channelDraphInfo.Uuid, channelDraphInfo.Name, userDraphInfo)
	}

	return
}

func PublishChannelCallStop(channelId string) {
	mqttChannelCall := mqttStruct.MqttChannelCall{
		CallActive:  mqttStruct.MESSAGE_CALL_INACTIVE,
		ChannelUUID: channelId,
	}

	go mqttBusiness.PublishChannelCall(&mqttChannelCall, channelId)
}

func sendChannelCallNotification(title string, body string, channelId string, channelName string, userDgraph *dgraphStruct.DgraphUser) {

	mentionsUUIDList := []string{}

	ctx := context.Background()

	// 1. Get eligible users for notifications (based on preferences)
	eligibleUserIDs, err := userChannelNotificationBusiness.GetEligibleUsersForChannelActivity(ctx, channelId, userDgraph.Uuid, mentionsUUIDList, false)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/sendChannelCallNotification Failed to get eligible users err: %+v",
			err)
		return
	}

	if len(eligibleUserIDs) == 0 {
		return
	}

	// 2. Handle FCM Push Notifications
	tokens, err := userFCMtokenBusiness.GetFCMTokenByListOfUserId(ctx, eligibleUserIDs)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/sendChannelCallNotification Failed to gets user's fcm token err: %+v",
			err)
		return
	}

	pushData := make(map[string]string)

	pushData[firebaseInit.FIREBASE_PUSH_DATA_TYPE] = firebaseInit.FIREBASE_PUSH_DATA_TYPE_CHANNEL_CALL
	pushData[firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID] = channelId
	pushData[firebaseInit.FIREBASE_PUSH_DATA_TITLE] = title
	pushData[firebaseInit.FIREBASE_PUSH_DATA_BODY] = body
	pushData[firebaseInit.FIREBASE_PUSH_DATA_USERNAME] = userDgraph.UserName
	pushData[firebaseInit.FIREBASE_PUSH_DATA_ICON] = userBusiness.GetSignedProfileURL(ctx, userDgraph.ProfileKey)

	// Send notifications in batches of 500 tokens
	batchSize := 500
	for i := 0; i < len(tokens); i += batchSize {
		end := i + batchSize
		if end > len(tokens) {
			end = len(tokens)
		}
		tokenBatch := tokens[i:end]

		err = firebaseInit.FirebaseApp.MultiCastPush(ctx, pushData, tokenBatch)
		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"business/sendChannelCallNotification Failed to send push notification err: %+v",
				err)
			return
		}
	}

	// Email fan-out for channel call.
	notificationBusiness.DispatchChannelCall(
		userDgraph.Uuid,
		userDgraph.UserName,
		userBusiness.GetSignedProfileURL(ctx, userDgraph.ProfileKey),
		channelId,
		channelName,
		eligibleUserIDs,
	)
}

func StartRecordingChannelCall(ctx context.Context, channelId string, userDgraphInfo *dgraphStruct.DgraphUser) (err error) {
	egresssInfo, filepath, err := LiveKitBusiness.StartRecording(ctx, channelId, userDgraphInfo)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/MakeVideoChannelCall Failed to start recording err: %+v",
			err)

		return
	}

	zeroEpochTime := time.Time{}

	currentTime := time.Now()

	dgraphChannel := dgraphStruct.DgraphChannel{
		Uid:  "uid(ch)",
		Uuid: channelId,
		Recordings: []*dgraphStruct.DgraphRecording{

			{

				EgressId:  egresssInfo.EgressId,
				EndedAt:   &zeroEpochTime,
				StartedAt: &currentTime,
				ObjectKey: filepath,
				Channel: &dgraphStruct.DgraphChannel{
					Uid: "uid(ch)",
				},
				RecordingStartedBy: &dgraphStruct.DgraphUser{
					Uid: userDgraphInfo.Uid,
				},
			},
		},
	}

	_, err = CreateOrUpdateDgraphChannel(ctx, &dgraphChannel)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/MakeVideoChannelCall Failed to update channel dgraph err: %+v",
			err)

		return
	}

	return
}

func StopRecordingChannelCall(ctx context.Context, channelId string) (err error) {
	err = LiveKitBusiness.StopRecording(ctx, channelId)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/StopRecordingChannelCall Failed to stop recording err: %+v",
			err)

		return
	}

	return
}
