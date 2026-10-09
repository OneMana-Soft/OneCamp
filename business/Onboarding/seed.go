package business

// The first thing a new workspace contains.
//
// WHY IT EXISTS. Setup created an admin account and dropped them into a product
// with sixty-eight modules and nothing in any of them. Every surface a new owner
// could open was empty, because every surface in this product is multiplayer and
// they were the only person there. The checklist next to that emptiness told them
// to go invite their team, which is to say: do the organisational work first, and
// this becomes useful later. Nothing in the first ninety seconds showed that the
// thing they had just paid for was alive.
//
// So the workspace now starts with a channel that has a post in it. Not a tour,
// not a wizard, not chrome that behaves differently from the real thing: an
// ordinary post in an ordinary channel, which the owner can reply to, react to,
// or delete. Its job is to make the first screen look like the product working
// rather than the product waiting.
//
// BEST EFFORT, ALWAYS. Every failure here is swallowed. A workspace that could
// not be seeded is a slightly emptier workspace; an admin account that could not
// be created because seeding failed is a customer who cannot log in at all. The
// account is the thing that matters and it already exists by the time this runs.

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	channelAdapter "github.com/akashc777/OneCamp/adapter/Channel"
	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	postBusiness "github.com/akashc777/OneCamp/business/Post"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// seedChannelName is the channel every new workspace starts with, and the one
// new members are put in until an admin chooses others
// (channelBusiness.DefaultChannels). Lowercase and handle-safe because that is
// what the channel validator accepts. A var rather than a const because the
// existence check takes a pointer.
var seedChannelName = channelBusiness.GeneralChannelName

// SeedWorkspace gives a brand-new workspace its first channel and first post.
//
// Safe to call more than once: it does nothing when the workspace already has a
// channel, so a retried setup cannot produce "general" and "general-2", and an
// existing installation that upgrades into this code is never touched.
func SeedWorkspace(ctx context.Context, admin userModels.UserInfo) {
	// Two guards, because they answer different questions. The name check is the
	// workspace-level one and is what actually prevents a duplicate: CreateChannel
	// does not reject a taken name, and a retried setup would otherwise leave
	// "general" beside "general-2". The membership check catches the wider case of
	// a workspace already in use, where seeding would be a stranger posting in
	// somebody's space.
	//
	// Both fail CLOSED: an error checking either one skips the seed. Not seeding a
	// new workspace costs a slightly emptier first screen, and seeding one already
	// in use writes into a stranger's channel list.
	if exists, err := channelBusiness.CheckIfChannelExist(ctx, &seedChannelName); err != nil || exists {
		return
	}
	if hasAnyChannel(ctx, admin) {
		return
	}

	channelUUID, err := createSeedChannel(ctx, admin)
	if err != nil {
		helpers.LogWarnWithContext(ctx, "Onboarding seed channel failed err: %+v", err)
		return
	}
	// Pinned by id: new members go here until an admin chooses other
	// channels, whatever it is renamed to (channelBusiness.DefaultChannels).
	if err := settingsBusiness.PinGeneralChannel(channelUUID.String()); err != nil {
		helpers.LogWarnWithContext(ctx, "Onboarding seed could not pin #general err: %+v", err)
	}
	if err := postWelcome(ctx, admin, channelUUID); err != nil {
		// The channel is the larger half of the value and it already exists, so a
		// failed post leaves the workspace better off than not seeding at all.
		helpers.LogWarnWithContext(ctx, "Onboarding seed post failed err: %+v", err)
	}
}

// createSeedChannel makes the channel and returns its id.
func createSeedChannel(ctx context.Context, admin userModels.UserInfo) (uuid.UUID, error) {
	err, channelUUID := channelBusiness.CreateChannel(ctx, &channelAdapter.InputCreateChannel{
		ChannelName:    seedChannelName,
		ChannelPrivate: false,
	}, &admin)
	if err != nil {
		return uuid.Nil, err
	}
	if channelUUID == uuid.Nil {
		return uuid.Nil, fmt.Errorf("channel created without an id")
	}
	return channelUUID, nil
}

// postWelcome writes the first post as the admin themselves.
//
// Authored by the owner rather than by a system account on purpose: a workspace
// whose first message comes from "OneCamp Bot" reads as a product demo, and one
// whose first message comes from the person who set it up reads as their
// workspace. It is also one fewer account to explain, and it deletes cleanly.
func postWelcome(ctx context.Context, admin userModels.UserInfo, channelUUID uuid.UUID) error {
	dgraphChannel, err := channelBusiness.GetBasicDgraphChannelInfoByUUID(ctx, channelUUID, admin.UserDgraphInfo.Uid)
	if err != nil {
		return err
	}
	if dgraphChannel == nil {
		return fmt.Errorf("channel %s not readable after creation", channelUUID)
	}

	_, err = postBusiness.CreatePost(ctx, &postAdapter.InputCreateOrUpdatePostInfo{
		HTMLText:    welcomeHTML(),
		ChannelUuid: channelUUID.String(),
		ChannelUUID: channelUUID,
	}, &admin, []*dgraphStruct.DgraphUser{}, dgraphChannel)
	return err
}

// welcomeHTML is the first post's body.
//
// WRITTEN FOR THE PEOPLE WHO JOIN, because they are who read it. New members
// are put in #general (channelBusiness.JoinDefaultChannels) and open on it,
// unless an admin chooses other channels for them, so this post is the first
// thing a teammate sees. It used to address the
// owner, telling every newcomer to "set up email" and "connect a model
// provider" before inviting anyone: work only an admin can do, which the
// admin's setup checklist already lists, and which on OneCamp Cloud is done for
// them. So it says what a member can do here, and nothing about setup.
//
// The same in both editions and on every install: it names nothing that only
// one edition, or only a configured workspace, has.
func welcomeHTML() string {
	return "<p>This is #general. New members are added here unless an admin has chosen other channels " +
		"for them, so it is a good place for anything the whole team should see.</p>" +
		"<p>New here? Say hello below: a line about who you are and what you work on helps everyone " +
		"put a name to a face.</p>" +
		"<p>The rest is in the sidebar: channels for conversations, docs for what is worth keeping, " +
		"projects and tasks for work in flight, and a calendar that reads from both. Browse channels " +
		"shows the ones you are not in yet, and search covers all of it at once.</p>"
}
