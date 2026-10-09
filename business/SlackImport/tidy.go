package business

// Leaving a Slack import's channels the way the team had them.
//
// Three things an import used to get wrong about channels:
//
//   - Slack's #general became #general-from-slack beside the #general every
//     new workspace starts with, so the team's main channel was split in two,
//     the old half full of history and the new half holding one welcome post.
//     Slack's #general now goes into the workspace's own when that one holds
//     nothing but its welcome post (anything more is the team's, and is left
//     alone).
//   - Channels archived in Slack came across live, so the sidebar filled with
//     channels nobody had used in years. They are archived once their history
//     is in, so search and the archive show them as they were.
//   - The admin running the import was put in every private channel, as the
//     person who created it: a member, and its admin, of conversations they
//     were never part of. They now leave the private channels they weren't in,
//     handing the channel's admin role to whoever made it in Slack (or its
//     first member who came across).

import (
	"context"
	"encoding/json"
	"fmt"

	channelAdapter "github.com/akashc777/OneCamp/adapter/Channel"
	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	channelDomain "github.com/akashc777/OneCamp/domain/Channel"
	"github.com/akashc777/OneCamp/helpers"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	importModels "github.com/akashc777/OneCamp/models/postgres/SlackImport"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// seedGeneralName is the channel every new workspace starts with
// (business/Onboarding seedChannelName).
const seedGeneralName = "general"

// seededGeneral is the workspace's own #general when Slack's #general can go
// into it: public, live, holding nothing but the one post the workspace
// started with, written by whoever made the channel, and shared with no guest
// (sharedWithGuests).
func seededGeneral(ctx context.Context) (uuid.UUID, bool) {
	ch, err := channelDomain.GetChannelByName(ctx, seedGeneralName)
	if err != nil || ch == nil || ch.Id == uuid.Nil || ch.IsPrivate {
		return uuid.Nil, false
	}
	total, others, err := importModels.CountChannelPosts(ctx, ch.Id, ch.CreatedBy)
	if err != nil || total > 1 || others > 0 || sharedWithGuests(ctx, ch.Id) {
		return uuid.Nil, false
	}
	return ch.Id, true
}

// sharedWithGuests reports whether a channel has a live guest link: Slack's
// #general poured into it would show its guests the team's whole Slack
// history, so it goes beside it instead. A grant that can't be checked counts
// as one.
func sharedWithGuests(ctx context.Context, channelUUID uuid.UUID) bool {
	grants, err := guestModel.ListActiveForResource(ctx, guestModel.ResourceChannel, channelUUID.String())
	return err != nil || len(grants) > 0
}

// generalSharedWithGuests reports whether the workspace's own #general has a
// live guest link, for the plan to say why Slack's goes beside it.
func generalSharedWithGuests(ctx context.Context) bool {
	ch, err := channelDomain.GetChannelByName(ctx, seedGeneralName)
	return err == nil && ch != nil && ch.Id != uuid.Nil && sharedWithGuests(ctx, ch.Id)
}

// heirOf is who becomes a private channel's admin when the importing admin
// leaves it: its Slack creator if they came across and are in it, else its
// first member who came across. Bots are passed over: an integration can't
// look after a channel. Pure apart from isBot.
func heirOf(sc *SlackChannel, resolved map[string]uuid.UUID, isBot func(slackID string) bool) (uuid.UUID, bool) {
	inChannel := make(map[string]bool, len(sc.Members))
	for _, m := range sc.Members {
		inChannel[m] = true
	}
	for _, candidate := range append([]string{sc.Creator}, sc.Members...) {
		id, ok := resolved[candidate]
		if !ok || id == uuid.Nil || !inChannel[candidate] || isBot(candidate) {
			continue
		}
		return id, true
	}
	return uuid.Nil, false
}

// slackBot reads whether the import recorded a Slack account as a bot.
func slackBot(ctx context.Context, importId uuid.UUID) func(string) bool {
	return func(slackID string) bool {
		raw, err := importModels.GetIdMapMetadata(ctx, importId, importModels.EntityUser, slackID)
		if err != nil || len(raw) == 0 {
			return false
		}
		var md struct {
			IsBot bool `json:"is_bot"`
		}
		_ = json.Unmarshal(raw, &md)
		return md.IsBot
	}
}

// wasMember reports whether the importing admin is one of the channel's
// members who came across. Pure.
func wasMember(sc *SlackChannel, resolved map[string]uuid.UUID, admin uuid.UUID) bool {
	for _, m := range sc.Members {
		if resolved[m] == admin {
			return true
		}
	}
	return false
}

// leavePrivateChannel takes the importing admin out of a private channel they
// weren't in, after handing its admin role on. When none of its members came
// across there is nobody to hand it to, and they stay, said in the import's
// warnings: a private channel with no member is one nobody can reach.
func leavePrivateChannel(ctx context.Context, importId, channelUUID uuid.UUID, sc *SlackChannel,
	admin *userModels.UserInfo, cache *dgraphUserCache) {

	resolved, err := importModels.LookupIdMappingsBatch(ctx, importId, importModels.EntityUser, append([]string{sc.Creator}, sc.Members...))
	if err != nil {
		helpers.LogWarnWithContext(ctx, "SlackImport private channel %s members lookup failed: %+v", channelUUID, err)
		return
	}
	if wasMember(sc, resolved, admin.UserPostgresInfo.Id) {
		return
	}
	heir, ok := heirOf(sc, resolved, slackBot(ctx, importId))
	if !ok {
		importModels.LogImportError(ctx, importId, nil, importModels.EntityChannel, sc.ID,
			importModels.SeverityWarning, "PRIVATE_CHANNEL_KEPT",
			fmt.Sprintf("You were left in the private channel %q: none of its members came across, so nobody else could open it.", sc.Name), nil)
		return
	}
	heirNode := cache.get(ctx, heir.String())
	ch, err := channelBusiness.GetBasicDgraphChannelInfoByUUID(ctx, channelUUID, "")
	if heirNode == nil || err != nil || ch == nil || ch.Uid == "" {
		helpers.LogWarnWithContext(ctx, "SlackImport private channel %s: could not hand it on: %+v", channelUUID, err)
		return
	}
	if err := channelBusiness.AddChannelModeratorEdge(ctx, channelUUID.String(), heirNode.Uid); err != nil {
		return // the admin stays rather than leave a channel without one
	}
	if err := channelBusiness.DeleteChannelModeratorEdge(ctx, ch.Uid, admin.UserDgraphInfo.Uid, channelUUID.String()); err != nil {
		return
	}
	_ = channelBusiness.DeleteChannelMemberEdge(ctx, ch.Uid, admin.UserDgraphInfo.Uid, admin.UserPostgresInfo.Id.String(), channelUUID.String())
}

// archiveArchivedChannels archives the channels this import made for Slack
// channels that were archived there. Run once their history is in: posts
// written into an archived channel would be indexed for search as live.
func archiveArchivedChannels(ctx context.Context, importId uuid.UUID) {
	owned, err := importModels.IdMappingsByTypeOwned(ctx, importId, importModels.EntityChannel)
	if err != nil {
		helpers.LogWarnWithContext(ctx, "SlackImport archive pass list failed job=%s err=%+v", importId, err)
		return
	}
	for _, e := range owned {
		raw, err := importModels.GetIdMapMetadata(ctx, importId, importModels.EntityChannel, e.SlackId)
		if err != nil || len(raw) == 0 {
			continue
		}
		var md struct {
			Archived  bool   `json:"is_archived"`
			FinalName string `json:"final_name"`
			Private   bool   `json:"is_private"`
		}
		if json.Unmarshal(raw, &md) != nil || !md.Archived || md.FinalName == "" {
			continue
		}
		if err := channelBusiness.UpdateChannelInfo(ctx, &channelAdapter.UpdateChannelInfo{
			ChannelUuid:     e.OnecampUUID.String(),
			ChannelName:     md.FinalName,
			ChannelPrivate:  md.Private,
			ChannelArchived: true,
		}, e.OnecampUUID); err != nil {
			importModels.LogImportError(ctx, importId, nil, importModels.EntityChannel, e.SlackId,
				importModels.SeverityWarning, "ARCHIVE_FAILED",
				fmt.Sprintf("#%s was archived in Slack and couldn't be archived here: %v", md.FinalName, err), nil)
		}
	}
}
