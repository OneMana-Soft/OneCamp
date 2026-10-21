package business

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Channel"
	business "github.com/akashc777/OneCamp/business/LastSeenChannel"
	LiveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	userChannelNotificationBusiness "github.com/akashc777/OneCamp/business/UserChannelNotification"
	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	domain "github.com/akashc777/OneCamp/domain/Channel"
	conversationDomain "github.com/akashc777/OneCamp/domain/Conversation"
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

	// The maker is a member now. Their cached profile (kept an hour) lists
	// their channels, and search, the AI's reach and slash commands are
	// scoped by it: until it's dropped, the new channel is invisible to all
	// three for them.
	userDomain.InvalidateUserMemberships(ctx, userInfo.UserDgraphInfo.Uuid)
	conversationDomain.Opened(ctx, conversationDomain.KindChannel, channelUUID.String(), userInfo.UserDgraphInfo.Uuid, []string{userInfo.UserDgraphInfo.Uuid}, []string{userInfo.UserDgraphInfo.Uuid})

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

	// The maker's open apps hold no topic of a channel made just now.
	go mqttBusiness.PublishConversationAccess(mqttStruct.CONVERSATION_KIND_CHANNEL, channelUUID.String(), mqttStruct.CONVERSATION_ADDED, userInfo.UserDgraphInfo.Uuid)

	helpers.GoSafeNamed("webhookBusiness.DispatchEvent", func() {
		webhookBusiness.DispatchEvent(context.WithoutCancel(ctx), "channel.created", map[string]interface{}{
			"channel_id":   channelUUID.String(),
			"channel_name": channelInfo.ChannelName,
			"created_by":   userInfo.UserDgraphInfo.Uuid,
		})
	})

	return

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

// channelSearchMarks and channelMemoryCascades run each channel's search marks,
// and its memory's archive and restore, one at a time in the order the channel
// was archived and restored. Each started on a goroutine of its own, so an
// archive and a restore close together raced, and a restore whose unmark
// finished before the archive's mark left a live channel's whole history
// marked Archived (and its memory archived).
var channelSearchMarks, channelMemoryCascades helpers.InOrder

// markInSearch marks a channel's posts, replies and files in search as in an
// archived channel or, archived false, clears the mark and brings back the
// history an archive made before the mark existed had marked deleted. A seam
// for tests.
var markInSearch = func(channelUUID string, archived bool) {
	globalSearchDomain.MarkChannelArchivedInOpenSearch(channelUUID, archived)
	if !archived {
		_ = globalSearchDomain.SyncCascadingUnarchiveInOpenSearch(
			[]string{"post_ch_id", "comment_channel_id", "attachment_channel_id"},
			channelUUID,
			[]string{"posts", "comments", "attachments"},
			"cascade",
		)
	}
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

	// Every gate reads the channel through a cache of it as each person sees
	// it (ChannelBasicInfo); a channel made private kept reading as public
	// to people outside it until that expired. Dropped before anything else,
	// the live-topic recheck below included.
	domain.InvalidateChannelBasicInfo(ctx, channelUUID.String())

	// Made private: anyone still on its live topics who isn't a member loses
	// them (a net under the cut made at each removal).
	if channelUpdateInfo.ChannelPrivate && (priorChannel == nil || !priorChannel.IsPrivate) {
		mqttBusiness.RecheckTopicSubscribers(helpers.GetMqttTopicForChannel(channelUUID.String()))
	}

	// Whether this update brings the channel back from the archive, by
	// whichever route (the restore button, or an edit that clears the
	// archive). Any other update (a rename, a privacy change) leaves its
	// history's marks as they are: each of those cleared the mark over the
	// channel's whole history, a settled update-by-query a time, though there
	// was nothing to clear. A channel whose snapshot didn't load counts as
	// archived, so a restore never leaves its history marked: that costs a
	// clear that changes nothing.
	restored := !channelUpdateInfo.ChannelArchived &&
		(priorChannel == nil || helpers.IsSoftDeleted(&priorChannel.DeletedAt))

	// The marks and the memory cascades run in the background, each channel's
	// in the order it was archived and restored (channelSearchMarks).
	chID := channelUUID.String()
	var deletedAt *int64 = nil
	switch {
	case channelUpdateInfo.ChannelArchived:
		deletedAt = helpers.Int64Pointer(currentTime.Unix())
		// The history stays searchable for the people in the channel, marked
		// Archived; it was marked deleted, out of everyone's search.
		channelSearchMarks.Go(chID, "channel.search-mark", func() { markInSearch(chID, true) })
		// Cascade the archive into the memory layer by SCOPE so every fact
		// derived from this channel (worker-extracted scope-keyed items AND
		// manual captures) stops surfacing as AI-queryable memory. Reversible
		// by the restore below.
		channelMemoryCascades.Go(chID, "channel.memory-archive", func() {
			ai.ArchiveMemoryByScope(context.Background(), memoryModels.ScopeRef{ChannelUUID: chID})
		})
		go webhookBusiness.DispatchEvent(context.WithoutCancel(ctx), "channel.archived", map[string]interface{}{
			"channel_id":   channelUpdateInfo.ChannelUuid,
			"channel_name": channelUpdateInfo.ChannelName,
		})
	case restored:
		// Back from the archive: the mark goes, and history an archive made
		// before the mark existed (marked deleted by the cascade) comes back.
		channelSearchMarks.Go(chID, "channel.search-unmark", func() { markInSearch(chID, false) })
		// Revive the memory items the archive cascade removed for this channel
		// (never items a user independently deleted).
		channelMemoryCascades.Go(chID, "channel.memory-restore", func() {
			ai.RestoreMemoryByScope(context.Background(), memoryModels.ScopeRef{ChannelUUID: chID})
		})
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

// TakenChannelNames is which of these channel names are taken.
func TakenChannelNames(ctx context.Context, names []string) (map[string]bool, error) {
	return domain.TakenChannelNames(ctx, names)
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

// ErrInvalidPostPolicy is a posting policy that is neither of the two: the
// caller's to fix, unlike a write that failed.
var ErrInvalidPostPolicy = errors.New("invalid post policy")

// SetChannelPostPolicy sets a channel's posting policy (announcement mode).
// Validates the value; persists to Postgres + Dgraph + cache via the domain.
func SetChannelPostPolicy(ctx context.Context, channelUUID uuid.UUID, policy string) error {
	if policy != ChannelPostPolicyEveryone && policy != ChannelPostPolicyAdminsOnly {
		return ErrInvalidPostPolicy
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

	// Each member is an admin when they are among the moderators, by id. The
	// two lists were walked side by side, which held only while both came
	// back in the same order with nobody in one and not the other.
	admins := make(map[string]bool, len(channelInfo.Moderators))
	for _, moderator := range channelInfo.Moderators {
		if moderator != nil {
			admins[moderator.Uuid] = true
		}
	}
	for _, member := range channelInfo.Members {
		if member != nil {
			member.IsAdmin = admins[member.Uuid]
		}
	}

	channelInfo.Moderators = nil

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

// KeepsAnAdmin is whether the channel would still have an admin if who (by
// uuid) were no longer one: they leave it, are removed, or step down. It
// mustn't be left with none, or nobody could add its members, or make anyone
// an admin, again; since its creator may leave, any admin can be its last.
// readerUID is the person asking, as the channel is read for them.
//
// It's read before the change, as the rest of the change's rules are: two
// admins stepping down in the same moment could still both pass.
func KeepsAnAdmin(ctx context.Context, channelUUID uuid.UUID, readerUID, who string) (bool, error) {
	ch, err := domain.GetDgraphChannelInfoByUUID(ctx, channelUUID.String(), readerUID)
	if err != nil {
		return false, err
	}
	isAdmin := false
	for _, m := range ch.Moderators {
		switch {
		case m == nil || m.Uuid == "":
		case strings.EqualFold(m.Uuid, who):
			isAdmin = true
		default:
			return true, nil
		}
	}
	return !isAdmin, nil
}

// DeleteChannelModeratorEdge demotes a channel admin. channelUUID is required
// for cache invalidation: ch_is_admin is cached per viewer, and the admins_only
// post gate reads it, so a demotion that skipped the purge left the demoted
// person posting in an announcement channel for the rest of the TTL.
func DeleteChannelModeratorEdge(ctx context.Context, channelDgraphUID string, userDgraphUID string, channelUUID string) (err error) {

	err = conversationDomain.Demote(ctx, conversationDomain.KindChannel, channelUUID, userDgraphUID, func() error { return domain.DeleteChannelModeratorEdge(ctx, channelDgraphUID, userDgraphUID) })

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
	conversationDomain.Promoted(ctx, conversationDomain.KindChannel, channelUUID, userUUID)

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

	// Joined, added, or let in by a default channel: their cached profile
	// still lists the channels they had, and search, the AI's reach and slash
	// commands are scoped by it, so the channel's history stayed out of their
	// search for up to the hour it's kept. Synchronously, before the answer.
	userDomain.InvalidateUserMemberships(ctx, newMemberUUID.String())
	conversationDomain.Added(ctx, conversationDomain.KindChannel, channelUUID.String(), conversationDomain.RoleMember, newMemberUUID.String())

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

	// Joined, added by an admin or by a default channel on the way in: the
	// new member's open apps hold no topic of this channel yet. The members
	// already there hear of it on the channel's own topic, below.
	go mqttBusiness.PublishConversationAccess(mqttStruct.CONVERSATION_KIND_CHANNEL, channelUUID.String(), mqttStruct.CONVERSATION_ADDED, newMemberUUID.String())

	go webhookBusiness.DispatchEvent(context.WithoutCancel(ctx), "user.joined", map[string]interface{}{
		"user_id":    userDgraphInfo.Uuid,
		"channel_id": channelUUID.String(),
	})

	NotifyChannelUpdated(channelUUID.String(), ChannelUpdateActionMemberAdded)

	return
}

func DeleteChannelMemberEdge(ctx context.Context, channelDgraphUID string, userDgraphUID string, memberUUID string, channelUUID string) (err error) {

	err = conversationDomain.Remove(ctx, conversationDomain.KindChannel, channelUUID, []string{memberUUID}, func() error { return domain.DeleteChannelMemberEdge(ctx, channelDgraphUID, userDgraphUID) })

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
	// And the broker: their open apps hold this channel's topics, and the
	// broker never asks again about a topic a client holds.
	mqttBusiness.DropConversation(mqttStruct.CONVERSATION_KIND_CHANNEL, channelUUID, memberUUID)

	helpers.GoSafeNamed("userChannelNotificationBusiness.DeleteNotificationTypeWhenUserIsRemovedFormChannel", func() {
		userChannelNotificationBusiness.DeleteNotificationTypeWhenUserIsRemovedFormChannel(memberUUID, channelUUID)
	})
	go webhookBusiness.DispatchEvent(context.WithoutCancel(ctx), "user.left", map[string]interface{}{
		"user_id":    memberUUID,
		"channel_id": channelUUID,
	})

	NotifyChannelUpdated(channelUUID, ChannelUpdateActionMemberLeft)

	// Out of the channel is out of its call. Their call token opens the room
	// named after the channel, and nothing used to put them out of it.
	helpers.GoSafeNamed("channel.leave-call", func() {
		err := LiveKitBusiness.RemoveFromCall(context.WithoutCancel(ctx), channelUUID, userDgraphUID, LiveKitBusiness.Remover{}, LiveKitBusiness.RemovedNotAMember)
		if err != nil && !errors.Is(err, LiveKitBusiness.ErrNotInCall) {
			helpers.LogErrorWithContext(ctx, "business/DeleteChannelMemberEdge could not put them out of the channel's call: %+v", err)
		}
	})

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
	conversationDomain.AddedByUID(ctx, conversationDomain.KindChannel, channelUUID.String(), conversationDomain.RoleMember, botDgraphUID)
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
	if err = conversationDomain.RemoveByUID(ctx, conversationDomain.KindChannel, channelUUID.String(), []string{botDgraphUID}, func() error { return domain.DeleteChannelMemberEdge(ctx, channelInfo.Uid, botDgraphUID) }); err != nil {
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
	helpers.GoSafeNamed("mqttBusiness.PublishChannelUpdate", func() { mqttBusiness.PublishChannelUpdate(channelUUID, action) })
}

func PublishTypingInChannel(userInfo *dgraphStruct.DgraphUser, channelId string) {
	mqttChannelTyping := mqttStruct.MqttChannelTyping{
		UserName:    userInfo.DisplayName(),
		UserUUID:    userInfo.Uuid,
		ChannelUuid: channelId,
	}

	if userInfo.ProfileKey != nil {
		mqttChannelTyping.UserProfile = *userInfo.ProfileKey
	}
	helpers.GoSafeNamed("mqttBusiness.PublishChannelTyping", func() { mqttBusiness.PublishChannelTyping(&mqttChannelTyping, channelId) })
}

func MakeVideoChannelCall(ctx context.Context, channelDraphInfo *dgraphStruct.DgraphChannel, userDraphInfo *dgraphStruct.DgraphUser, isAdmin bool, language string) (token string, alreadyExisted bool, err error) {

	token, alreadyExisted, err = LiveKitBusiness.CreateRoomAndGetToken(ctx, channelDraphInfo.Uuid, userDraphInfo, isAdmin, language)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/MakeVideoChannelCall Failed to make video call post err: %+v",
			err)

		return
	}

	// Its pushes and its call dot wait for the first person to really join
	// (callStartedPush.go).
	return
}

func PublishChannelCallStop(channelId string) {
	mqttChannelCall := mqttStruct.MqttChannelCall{
		CallActive:  mqttStruct.MESSAGE_CALL_INACTIVE,
		ChannelUUID: channelId,
	}

	helpers.GoSafeNamed("mqttBusiness.PublishChannelCall", func() { mqttBusiness.PublishChannelCall(&mqttChannelCall, channelId) })
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

	// 2. The push, and the desktop app's notice, to those of them the push
	// gate lets through (business/Notification Deliver). Each call rings
	// anew, so its notice is named by when it started.
	pushData := make(map[string]string)

	pushData[firebaseInit.FIREBASE_PUSH_DATA_TYPE] = firebaseInit.FIREBASE_PUSH_DATA_TYPE_CHANNEL_CALL
	pushData[firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID] = channelId
	pushData[firebaseInit.FIREBASE_PUSH_DATA_TITLE] = title
	pushData[firebaseInit.FIREBASE_PUSH_DATA_BODY] = body
	pushData[firebaseInit.FIREBASE_PUSH_DATA_USERNAME] = userDgraph.DisplayName()
	pushData[firebaseInit.FIREBASE_PUSH_DATA_ICON] = userBusiness.GetSignedProfileURL(ctx, userDgraph.ProfileKey)

	notificationBusiness.Deliver(ctx, eligibleUserIDs, pushData, fmt.Sprintf("channel_call:%s:%d", channelId, time.Now().UnixMilli()))

	// Email fan-out for channel call.
	notificationBusiness.DispatchChannelCall(
		userDgraph.Uuid,
		userDgraph.DisplayName(),
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

// GetDiscoverableChannels is a page of the public channels the person could
// join and isn't in, by name, searched on the server (domain).
func GetDiscoverableChannels(ctx context.Context, userDgraphId string, searchText string, pageIndex int, pageSize int) ([]*dgraphStruct.DgraphChannel, int64, error) {
	return domain.GetDiscoverableChannels(ctx, userDgraphId, searchText, pageIndex, pageSize)
}
