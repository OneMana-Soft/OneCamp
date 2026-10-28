package business

// What the bridge may carry to Slack.
//
// Everything said in a linked channel goes to Slack: its posts, their replies
// and edits (outbound.go). Link took any channel an admin named and never
// asked whether they could read it, and outbound copied every post of a linked
// channel without asking either, so an admin outside a private channel could
// link it and read it in Slack. Outgoing webhooks (business/Webhook reach.go)
// and workflows (business/Workflow access.go) already hold what they send out
// to what their owner may read.
//
// So does the bridge now, with whoever linked a channel as its owner:
//   - Link refuses a channel the admin can't read (channelBusiness.ReadableBy:
//     a private channel's members and moderators, anyone for a public one), in
//     words that name no channel, before anything is asked of Slack;
//   - outbound sends only while the link's creator can still read the channel,
//     and nothing for a link with no creator or when that can't be read. That
//     also holds links made before Link asked. A deletion carries nothing out,
//     so it still takes down what went to Slack before;
//   - the admin page names a linked private channel only to an admin who can
//     read it (anyone else sees "private channel (linked by <name>)"), and
//     the audit log, which every admin reads, never names one.

import (
	"context"

	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	channelDomain "github.com/akashc777/OneCamp/domain/Channel"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/SlackBridge"
	"github.com/google/uuid"
)

// readsChannel is who may read a channel's messages (channelBusiness.ReadableBy):
// a seam for tests.
var readsChannel = channelBusiness.ReadableBy

// channelStandings reads many channels as one person sees them
// (channelDomain.ChannelStandings): a seam for tests.
var channelStandings = channelDomain.ChannelStandings

// PrivateChannelLabel stands in for a linked private channel's name on the
// admin page, for an admin who can't read it. The page prints it after a "#",
// as it prints a name, so it is lower case.
const PrivateChannelLabel = "private channel"

// linkedChannels reads the links' OneCamp channels as viewer sees them, in one
// query (channelStandings): each one's name, whether it's private and whether
// viewer is in it, by uuid. Nobody (viewer is nil), and someone who isn't
// active, are in none of them. A channel that's gone is left out; a read that
// fails is its error.
func linkedChannels(ctx context.Context, viewer *uuid.UUID, links []model.Link) (map[string]*dgraphStruct.DgraphChannel, error) {
	ids := make([]string, 0, len(links))
	for _, l := range links {
		ids = append(ids, l.ChannelUUID.String())
	}
	if len(ids) == 0 {
		return nil, nil
	}
	as := ""
	if viewer != nil {
		u, err := userDomain.GetActiveDgraphUserInfoByUUID(ctx, viewer.String())
		if err != nil {
			helpers.LogErrorWithContext(ctx, "SlackBridge status: %s couldn't be read, so no private channel is named to them: %v", viewer, err)
		} else if u != nil {
			as = u.Uid
		}
	}
	chans, err := channelStandings(ctx, ids, as)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "SlackBridge status: the linked channels couldn't be read, so none is named: %v", err)
		return nil, err
	}
	out := make(map[string]*dgraphStruct.DgraphChannel, len(chans))
	for _, ch := range chans {
		if ch != nil && ch.Uuid != "" {
			out[ch.Uuid] = ch
		}
	}
	return out, nil
}

// shownName is the name the admin page shows for a link's OneCamp channel ch,
// read as the viewer: its own when they can read it (channelBusiness.CanRead:
// anyone a public channel, a private one's members and moderators), otherwise
// "private channel (linked by <name>)", also when ch couldn't be read.
func shownName(ch *dgraphStruct.DgraphChannel, linkedBy string) (string, bool) {
	if channelBusiness.CanRead(ch) {
		return ch.Name, false
	}
	if linkedBy == "" {
		return PrivateChannelLabel, true
	}
	return PrivateChannelLabel + " (linked by " + linkedBy + ")", true
}

// linkersOf names whoever made each link, in one read
// (userDomain.ResolveUserDisplays), by uuid. A name that can't be read is
// left out.
func linkersOf(ctx context.Context, links []model.Link) map[string]string {
	ids := make([]string, 0, len(links))
	for _, l := range links {
		if l.CreatedBy != nil {
			ids = append(ids, l.CreatedBy.String())
		}
	}
	people, err := userDomain.ResolveUserDisplays(ctx, ids)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "SlackBridge status: who made the links couldn't be read: %v", err)
	}
	out := make(map[string]string, len(people))
	for id, p := range people {
		out[id] = p.Display
	}
	return out
}

// mayLink is nil when by may link the OneCamp channel channelUUID: they can
// read it. Anyone else, nobody (by is nil), a channel that isn't there and a
// lookup that fails are all ErrChannelUnreadable, which names no channel, as a
// workflow's scope is refused (business/Workflow checkScope).
func mayLink(ctx context.Context, by *uuid.UUID, channelUUID uuid.UUID) error {
	if by == nil {
		return ErrChannelUnreadable
	}
	ok, err := readsChannel(ctx, by.String(), channelUUID.String())
	if err != nil {
		helpers.LogErrorWithContext(ctx, "SlackBridge Link: whether %s may read %s couldn't be read, so it isn't linked: %v", by, channelUUID, err)
		return ErrChannelUnreadable
	}
	if !ok {
		return ErrChannelUnreadable
	}
	return nil
}

// creatorReads is whether the link's creator can read its channel now, which
// is what the bridge may send to Slack from it. A link with no creator, and a
// lookup that fails, send nothing.
func creatorReads(ctx context.Context, link model.Link) bool {
	if link.CreatedBy == nil {
		return false
	}
	ok, err := readsChannel(ctx, link.CreatedBy.String(), link.ChannelUUID.String())
	if err != nil {
		helpers.LogErrorWithContext(ctx, "SlackBridge outbound: whether the creator of the link for %s may read it couldn't be read, so nothing goes to Slack: %v", link.ChannelUUID, err)
		return false
	}
	return ok
}
