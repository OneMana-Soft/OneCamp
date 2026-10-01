package business

// Curating the shared demo workspace.
//
// WHY THIS EXISTS. One account serves every visitor to the demo, and what they
// found was a workspace of keyboard mash: channels called dsfsdfsdfdf, jkkjj
// kjkjkj and yhdfghd, carrying messages that read "yo", "fdf", "zhh" and
// "horseys". That is the first thing a buyer sees of a product whose entire pitch
// is that it is careful with their data.
//
// It was not drift. A nightly job restores a golden snapshot so visitors cannot
// degrade the demo, and the snapshot itself contained the mash, so the job
// faithfully reinstated it every night. The fix is not to tidy up by hand — the
// next restore would undo it — but to have a curated state that can be produced
// on demand, checked into the repository, and captured as a new golden.
//
// WRITTEN THROUGH THE PRODUCT, not into the database. Posts live in Dgraph with
// rows in Postgres, a search document in OpenSearch and a notification fan-out
// over MQTT; SQL that wrote only the first two would produce content that exists
// but cannot be found, which is its own kind of broken demo. Every channel and
// message here goes through the same business calls the UI uses.
//
// IDEMPOTENT. Re-running adds nothing: a channel that already exists is left
// exactly as it is, because the second run must not double every message.

import (
	"context"
	"fmt"
	"html"
	"time"

	"github.com/google/uuid"

	channelAdapter "github.com/akashc777/OneCamp/adapter/Channel"
	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	postBusiness "github.com/akashc777/OneCamp/business/Post"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	channelDomain "github.com/akashc777/OneCamp/domain/Channel"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// conversation is one curated channel and what is said in it.
type conversation struct {
	Channel string
	Private bool
	// Posts are ordered oldest first. Plain sentences, not marketing: a demo
	// workspace that reads like a brochure is as unconvincing as one that reads
	// like a keyboard test.
	//
	// Speaker is which member of the cast says it, as an index that wraps. The
	// first version of this seeder posted everything as the admin, so three
	// channels each held one person talking to themselves for four messages —
	// tidier than the keyboard mash it replaced and just as obviously seeded. A
	// conversation needs more than one voice.
	Posts []Line
}

// Line is one message and who says it.
type Line struct {
	Speaker int
	Text    string
}

// Curated is the workspace a visitor should land in.
//
// Deliberately small. Three channels with a handful of real exchanges reads as a
// team that uses the product; forty channels of generated filler reads as a
// seeded database, and the visitor can tell the difference immediately.
//
// The content stays close to what this product is actually for, so the governance
// drill's channels sit beside it without looking like a different application.
var Curated = []conversation{
	{
		Channel: "engineering",
		Posts: []Line{
			{0, "Cutting 2.10.0 this afternoon. Two things in it worth calling out: the audit chain verifies again, and removing someone from a channel now takes effect immediately instead of up to half an hour."},
			{1, "The revoke one is the bigger deal. Membership was cached for thirty minutes behind every permission check, and only the ADD path cleared that cache."},
			{0, "So access was granted instantly and taken away slowly, which is the wrong way round for the only one of the two that is a security problem."},
			{2, "Did we confirm it against a real database rather than a unit test?"},
			{1, "Yes. Removed a member, asked the same question the post endpoint asks, and the answer was still yes. It is no now."},
			{0, "Migrations are unchanged at 158, so the upgrade is a rebuild rather than a schema change."},
		},
	},
	{
		Channel: "design",
		Posts: []Line{
			{1, "The admin audit card was showing one hash per row. On its own a hash proves nothing, so it now shows the previous one beside it: the link is the claim, not the fingerprint."},
			{2, "Makes sense. What happened to the verify button? It used to sit there for a while on bigger workspaces."},
			{1, "It recomputed the whole chain every time, which is instant on a new install and a timeout on one that has been running a year."},
			{0, "It checks the recent entries by default now and says so, with the full walk as a separate click."},
			{2, "Worth being careful with that wording. \"The last 500 verify\" and \"the log has not been altered\" are not the same claim."},
			{1, "Agreed, and it says which of the two it did."},
		},
	},
	{
		Channel: "general",
		Posts: []Line{
			{0, "Reminder that this workspace is the demo. Anything you write here is visible to whoever opens it next, and it is reset to a clean state every night."},
			{2, "Where does the governance drill live? I could not find it from the sidebar."},
			{0, "Admin, under AI and Agents. It asks an agent to post somewhere the person behind it cannot, and shows you the refusal, the audit rows, and the chain recomputation."},
			{1, "It fails closed on purpose. If the post ever succeeds, it tells you that in those words rather than quietly passing."},
		},
	},
}

// Junk is the exact set of channels this curation replaces.
//
// AN EXPLICIT LIST, NOT A PATTERN. It is tempting to archive anything that looks
// like keyboard mash, and a regular expression that decides what is junk will
// eventually archive a channel somebody meant. These are the names observed on
// the demo host, written down, reviewable in a diff. Anything not on this list is
// left alone, including channels added since.
var Junk = []string{
	"demoCH",
	"testing",
	"testing13",
	"dsfsdfsdfdf",
	"dilip test",
	"botTest",
	"jkkjj kjkjkj",
	"jjbkjbkjbkjbkj",
	"yhdfghd",
}

// Result reports what one run changed, so an operator can see the difference
// between "nothing to do" and "nothing happened".
type Result struct {
	ChannelsCreated  int
	PostsWritten     int
	ChannelsArchived int
	Skipped          []string
}

// Run curates the workspace as the given admin. Safe to run repeatedly.
func Run(ctx context.Context, admin userModels.UserInfo, refresh bool) (*Result, error) {
	res := &Result{}

	// REFRESH RETIRES THE CURATED SET FIRST, so the seeder can put a newer
	// version of the conversation in. Without it the run is a no-op on a
	// workspace that already has these channels, which is right for a repeat and
	// wrong for a change: the first version of this content had every message
	// from one speaker, and there was no way to replace it short of editing the
	// database by hand.
	if refresh {
		names := make([]string, 0, len(Curated))
		for _, c := range Curated {
			names = append(names, c.Channel)
		}
		n, err := retireByName(ctx, admin, names)
		res.ChannelsArchived += n
		if err != nil {
			return res, err
		}
	}

	// Three voices is enough for a channel to read as a conversation and few
	// enough that a small workspace can supply them.
	speakers := cast(ctx, admin, 3)

	for _, conv := range Curated {
		name := conv.Channel
		exists, err := channelBusiness.CheckIfChannelExist(ctx, &name)
		if err != nil {
			return res, fmt.Errorf("checking for #%s: %w", name, err)
		}
		if exists {
			// Left exactly as it is. A second run must not double the messages,
			// and a channel somebody has since used is not ours to rewrite.
			res.Skipped = append(res.Skipped, "#"+name)
			continue
		}

		err, channelUUID := channelBusiness.CreateChannel(ctx, &channelAdapter.InputCreateChannel{
			ChannelName:    conv.Channel,
			ChannelPrivate: conv.Private,
		}, &admin)
		if err != nil {
			return res, fmt.Errorf("creating #%s: %w", conv.Channel, err)
		}
		res.ChannelsCreated++

		written, err := writeConversation(ctx, speakers, channelUUID, conv.Posts)
		res.PostsWritten += written
		if err != nil {
			return res, fmt.Errorf("posting to #%s: %w", conv.Channel, err)
		}
	}

	archived, err := archiveByName(ctx, admin, Junk)
	res.ChannelsArchived += archived
	return res, err
}

// cast resolves the people the seeded conversation is attributed to.
//
// WHOEVER IS ALREADY HERE, never a hardcoded list. Putting real colleagues'
// names and addresses into the repository to make a demo look populated would be
// both a privacy decision nobody made and wrong on any other installation. So the
// seeder asks the workspace who is in it, puts the admin first so there is always
// at least one speaker, and rotates through the rest.
//
// Degrades rather than fails: a brand-new workspace has only the admin, and the
// conversation is then one voice, which is honest for a workspace with one
// person in it.
func cast(ctx context.Context, admin userModels.UserInfo, want int) []userModels.UserInfo {
	out := []userModels.UserInfo{admin}
	if want <= 1 {
		return out
	}
	others, _, err := userBusiness.GetAllUsersListFromDgraph(ctx, 0, 50)
	if err != nil {
		return out
	}
	for _, u := range others {
		if len(out) >= want {
			break
		}
		if u == nil || u.Uuid == "" || u.Uuid == admin.UserPostgresInfo.Id.String() {
			continue
		}
		// Bots speak for themselves elsewhere; a seeded human conversation
		// attributed to the AI would misrepresent what the product did.
		if u.IsBot {
			continue
		}
		pg, err := userDomain.GetUserByEmailId(ctx, &u.EmailID)
		if err != nil || pg == nil {
			continue
		}
		// Resolved again rather than reused. The list query selects user_uuid and
		// the name but NOT uid, so the rows it returns cannot be used to write an
		// edge: the mutation fails with "ID can't be empty", which is what it did.
		// Cheap, and the alternative is a principal that looks complete and is not.
		dg, err := userDomain.GetDgraphUserInfoByUUID(ctx, u.Uuid)
		if err != nil || dg == nil || dg.Uid == "" {
			continue
		}
		out = append(out, userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *dg})
	}
	return out
}

// writeConversation posts each message in order through the product's own path.
func writeConversation(ctx context.Context, speakers []userModels.UserInfo, channelUUID uuid.UUID, posts []Line) (int, error) {
	written := 0
	for _, line := range posts {
		who := speakers[line.Speaker%len(speakers)]

		// The author has to be in the channel, or the roster disagrees with the
		// transcript and a reader notices before they notice anything else. The
		// call is idempotent, so repeating it for each message costs nothing.
		if who.UserPostgresInfo.Id != speakers[0].UserPostgresInfo.Id {
			if err := channelBusiness.AddChannelMemberEdge(ctx, channelUUID,
				&who.UserDgraphInfo, who.UserPostgresInfo.Id); err != nil {
				return written, fmt.Errorf("adding %s to the channel: %w", who.UserDgraphInfo.UserName, err)
			}
		}

		// Read per speaker: the channel summary is cached per viewer, so the
		// author's own view is the one that has to say they are a member.
		dgraphChannel, err := channelBusiness.GetBasicDgraphChannelInfoByUUID(ctx, channelUUID, who.UserDgraphInfo.Uid)
		if err != nil {
			return written, err
		}
		if dgraphChannel == nil {
			return written, fmt.Errorf("channel %s is not readable after creation", channelUUID)
		}

		if _, err := postBusiness.CreatePost(ctx, &postAdapter.InputCreateOrUpdatePostInfo{
			HTMLText:    "<p>" + html.EscapeString(line.Text) + "</p>",
			ChannelUuid: channelUUID.String(),
			ChannelUUID: channelUUID,
		}, &who, []*dgraphStruct.DgraphUser{}, dgraphChannel); err != nil {
			return written, err
		}
		written++
	}
	return written, nil
}

// retireName is the name a retired channel takes, freeing the original.
//
// ch_name is UNIQUE, which is the whole reason this function exists. Archiving
// alone sets a deleted_at and leaves the name taken, so the seeder's next step
// could not create the replacement and the refresh left the workspace with no
// curated channels at all. Found by running it.
//
// The suffix is the retirement time, so two refreshes in the same workspace
// cannot collide with each other either.
func retireName(name string, at time.Time) string {
	return fmt.Sprintf("%s-retired-%d", name, at.Unix())
}

// retireByName renames each channel out of the way and archives it.
//
// Rename rather than delete. The content is somebody's, even on the demo, and a
// product sold on being careful with data should not have a maintenance command
// that destroys a channel to free up its name.
func retireByName(ctx context.Context, admin userModels.UserInfo, names []string) (int, error) {
	retired := 0
	now := time.Now()
	for _, name := range names {
		n := name
		exists, err := channelBusiness.CheckIfChannelExist(ctx, &n)
		if err != nil || !exists {
			continue
		}
		row, err := channelDomain.GetChannelByName(ctx, n)
		if err != nil || row == nil {
			continue
		}
		ch, err := channelBusiness.GetBasicDgraphChannelInfoByUUID(ctx, row.Id, admin.UserDgraphInfo.Uid)
		if err != nil || ch == nil || ch.Uuid == "" {
			continue
		}
		if err := channelBusiness.UpdateChannelInfo(ctx, &channelAdapter.UpdateChannelInfo{
			ChannelUuid:     ch.Uuid,
			ChannelName:     retireName(n, now),
			ChannelArchived: true,
		}, row.Id); err != nil {
			return retired, fmt.Errorf("retiring #%s: %w", n, err)
		}
		retired++
	}
	return retired, nil
}

// archiveByName archives the listed channels. Archive, not delete: the content is
// recoverable, and a demo reset that destroyed data would be a poor advertisement
// for a product sold on being careful with it.
//
// One helper for both callers. The junk sweep and a refresh of the curated set
// are the same operation on different lists, and writing it twice is how the two
// would eventually disagree about what archiving means.
func archiveByName(ctx context.Context, admin userModels.UserInfo, names []string) (int, error) {
	archived := 0
	for _, name := range names {
		n := name
		exists, err := channelBusiness.CheckIfChannelExist(ctx, &n)
		if err != nil || !exists {
			continue
		}
		row, err := channelDomain.GetChannelByName(ctx, n)
		if err != nil || row == nil {
			continue
		}
		ch, err := channelBusiness.GetBasicDgraphChannelInfoByUUID(ctx, row.Id, admin.UserDgraphInfo.Uid)
		if err != nil || ch == nil || ch.Uuid == "" {
			continue
		}
		// Already archived: leave it, so a re-run reports honestly rather than
		// counting the same channel every time.
		if ch.DeletedAt != nil && !ch.DeletedAt.IsZero() {
			continue
		}
		if err := channelBusiness.UpdateChannelInfo(ctx, &channelAdapter.UpdateChannelInfo{
			ChannelUuid:     ch.Uuid,
			ChannelName:     n,
			ChannelArchived: true,
		}, row.Id); err != nil {
			return archived, fmt.Errorf("archiving #%s: %w", n, err)
		}
		archived++
	}
	return archived, nil
}
