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
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// seedChannelName is the channel every new workspace starts with. Lowercase and
// handle-safe because that is what the channel validator accepts. A var rather
// than a const because the existence check takes a pointer.
var seedChannelName = "general"

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
		HTMLText:    welcomeHTML(helpers.FeatureRegistered(helpers.FeatureNameAI)),
		ChannelUuid: channelUUID.String(),
		ChannelUUID: channelUUID,
	}, &admin, []*dgraphStruct.DgraphUser{}, dgraphChannel)
	return err
}

// welcomeHTML is the first post's body.
//
// It orients rather than welcomes. The failure mode of seeded content is copy
// that congratulates the reader on their new workspace and tells them nothing, so
// this says where the rest of the product is and names the two things that are
// genuinely worth doing before anyone else arrives. The closing line matters as
// much as the rest: it tells the owner this is an ordinary post, which is the
// difference between seeded content and chrome they cannot get rid of.
//
// withAI is the EDITION, not the current setting. On the AI-free edition the
// provider line would describe something that does not exist in the build; with
// AI present but unconfigured it describes exactly the thing they should go do.
func welcomeHTML(withAI bool) string {
	aiLine := ""
	if withAI {
		aiLine = "<li>Connect a model provider, so the workspace can answer questions about its own content. " +
			"Bring your own key or point it at a local model. Nothing leaves your server without one.</li>"
	}
	return "<p>This is #general. Every new member lands here, so it is the right place for anything " +
		"the whole workspace should see.</p>" +
		"<p>The rest is in the sidebar: docs for what is worth keeping, boards and tasks for work in " +
		"flight, a calendar that reads from both, and calls that start from any of them. Search covers " +
		"all of it at once.</p>" +
		"<p>Two things are worth doing before you invite anyone:</p>" +
		"<ul>" +
		"<li>Set up email, so invitations and password resets can leave the server.</li>" +
		aiLine +
		"</ul>" +
		"<p>This is an ordinary post in an ordinary channel. Reply to it, or delete it.</p>"
}
