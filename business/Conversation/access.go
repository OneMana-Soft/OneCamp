package business

// The chat kinds of the Access Resolver (PROGRAM.md §5 and CH4), answering
// with the rules today's handlers apply, each read through the query they
// read it with:
//
//   - a channel: business/Channel CanRead and business/Send MayPostIn, over
//     GetBasicDgraphChannelInfoByUUID (moderators moderate; an archived channel
//     is read only);
//   - a DM: its two people, and business/Send MayMessage for posting;
//   - a group: dm_is_member (GetDgraphDmBasicInfoFromDgraph);
//   - a thread: its root's conversation, as the thread routes decide (members
//     only, even in a public channel; a deleted root's thread is read only);
//   - a call: its room's conversation, as the call routes decide (members and
//     participants join).
//
// Each id is one read, as today's checks make it: on day 0 nothing decides
// more than one conversation at a time (Resolve, Pages, ReadState). A9
// replaces these with its conversation_members rows, which decide a whole
// list in one query; the kinds and levels stay.

import (
	"context"
	"errors"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	chatBusiness "github.com/akashc777/OneCamp/business/Chat"
	postBusiness "github.com/akashc777/OneCamp/business/Post"
	sendBusiness "github.com/akashc777/OneCamp/business/Send"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	chatDgraphModels "github.com/akashc777/OneCamp/models/dgraph/Chat"
	postDgraphModels "github.com/akashc777/OneCamp/models/dgraph/Post"
	"github.com/google/uuid"
)

func init() {
	accessBusiness.RegisterKind(KindChannel, channelRules{})
	accessBusiness.RegisterKind(KindDM, dmRules{})
	accessBusiness.RegisterKind(KindGroup, groupRules{})
	accessBusiness.RegisterKind(KindThread, threadRules{})
	accessBusiness.RegisterKind(KindCall, callRules{})
}

// Reasons, as codes the web app words.
const (
	reasonModerator   = "moderator"
	reasonMember      = "member"
	reasonPublic      = "public"
	reasonParticipant = "participant"
)

// uidOf is who's graph uid: as given (a request's person), else read for an
// active person by uuid. "" for someone who isn't an active person, who reads
// nothing.
func uidOf(ctx context.Context, who accessBusiness.Subject) (string, error) {
	if who.UserUID != "" {
		return who.UserUID, nil
	}
	if who.UserUUID == "" {
		return "", nil
	}
	u, err := userDomain.GetActiveDgraphUserInfoByUUID(ctx, who.UserUUID)
	if err != nil || u == nil {
		return "", err
	}
	return u.Uid, nil
}

// capAt is d held at most at l: what a deleted root or an archived channel
// leaves, with Was the level before.
func capAt(d accessBusiness.Decision, l accessBusiness.Level) accessBusiness.Decision {
	d.Was = d.Level
	d.Level = min(d.Level, l)
	return d
}

// ------------------------------------------------------------------ channels

type channelRules struct{}

func (channelRules) DecideMany(ctx context.Context, who accessBusiness.Subject, ids []string) (map[string]accessBusiness.Decision, error) {
	out := map[string]accessBusiness.Decision{}
	uid, err := uidOf(ctx, who)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		ch, err := channelOf(ctx, id, uid)
		if err != nil {
			return nil, err
		}
		if ch != nil {
			out[id] = channelDecision(ch)
		}
	}
	return out, nil
}

// channelOf is the channel as uid reads it, or nil when there's no such
// channel (or no such person).
func channelOf(ctx context.Context, id, uid string) (*dgraphStruct.DgraphChannel, error) {
	cu, err := uuid.Parse(id)
	if err != nil || uid == "" {
		return nil, nil
	}
	ch, err := channelBusiness.GetBasicDgraphChannelInfoByUUID(ctx, cu, uid)
	if errors.Is(err, channelBusiness.ErrNotFound) || (err == nil && (ch == nil || ch.Uuid == "")) {
		return nil, nil
	}
	return ch, err
}

// channelDecision is today's rule for a channel as read for the person (its
// member and moderator counts are theirs). Pure.
func channelDecision(ch *dgraphStruct.DgraphChannel) accessBusiness.Decision {
	var d accessBusiness.Decision
	switch {
	case !channelBusiness.CanRead(ch):
	case ch.IsMember > 0 && ch.IsAdmin > 0:
		d.Level, d.Reasons = LevelModerate, []string{reasonModerator}
	case ch.IsMember > 0 && sendBusiness.MayPostIn(live(ch)) != nil:
		// An announcement channel: its members reply in threads.
		d.Level, d.Reasons = LevelReply, []string{reasonMember}
	case ch.IsMember > 0:
		d.Level, d.Reasons = LevelPost, []string{reasonMember}
	default:
		// A public channel, or a moderator who left it: read, never post.
		d.Level, d.Reasons = LevelRead, []string{reasonPublic}
	}
	if helpers.IsSoftDeleted(ch.DeletedAt) {
		d = capAt(d, LevelRead)
		d.Archived = true
	}
	d.Was = max(d.Was, d.Level)
	return d
}

// live is ch as if it weren't archived: what its member could do, which an
// archive then holds at read.
func live(ch *dgraphStruct.DgraphChannel) *dgraphStruct.DgraphChannel {
	c := *ch
	c.DeletedAt = nil
	return &c
}

// ----------------------------------------------------------------------- DMs

type dmRules struct{}

func (dmRules) DecideMany(ctx context.Context, who accessBusiness.Subject, ids []string) (map[string]accessBusiness.Decision, error) {
	out := map[string]accessBusiness.Decision{}
	if uid, err := uidOf(ctx, who); err != nil || uid == "" {
		return out, err
	}
	for _, id := range ids {
		them := (Ref{Kind: KindDM, ID: id}).Other(who.UserUUID)
		if them == "" {
			out[id] = accessBusiness.Decision{}
			continue
		}
		to, err := userBusiness.GetDgraphUserInfoByUUID(ctx, them)
		if err != nil {
			return nil, err
		}
		out[id] = dmDecision(to)
	}
	return out, nil
}

// dmDecision is today's rule for a DM one is in, with the other person as
// read: both read it; a message goes only to someone who can receive one
// (MayMessage). Pure.
func dmDecision(to *dgraphStruct.DgraphUser) accessBusiness.Decision {
	d := accessBusiness.Decision{Level: LevelRead, Reasons: []string{reasonParticipant}}
	if to != nil && sendBusiness.MayMessage(to) == nil {
		d.Level = LevelPost
	}
	d.Was = d.Level
	return d
}

// --------------------------------------------------------------------- groups

type groupRules struct{}

func (groupRules) DecideMany(ctx context.Context, who accessBusiness.Subject, ids []string) (map[string]accessBusiness.Decision, error) {
	out := map[string]accessBusiness.Decision{}
	uid, err := uidOf(ctx, who)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if uid == "" {
			continue
		}
		dm, err := chatBusiness.GetDgraphDmBasicInfoFromDgraph(ctx, uid, id)
		if err != nil {
			return nil, err
		}
		if dm != nil && dm.GroupingId != "" {
			out[id] = groupDecision(dm)
		}
	}
	return out, nil
}

// groupDecision is today's rule for a group as read for the person: its
// participants post in it, and nobody else reads it. Pure.
func groupDecision(dm *dgraphStruct.DgraphDm) accessBusiness.Decision {
	if dm.ParticipantIsMember == 0 {
		return accessBusiness.Decision{}
	}
	return accessBusiness.Decision{Level: LevelPost, Was: LevelPost, Reasons: []string{reasonParticipant}}
}

// -------------------------------------------------------------------- threads

type threadRules struct{}

func (threadRules) DecideMany(ctx context.Context, who accessBusiness.Subject, ids []string) (map[string]accessBusiness.Decision, error) {
	out := map[string]accessBusiness.Decision{}
	uid, err := uidOf(ctx, who)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if _, err := uuid.Parse(id); err != nil || uid == "" {
			continue
		}
		// A root is a channel's post or a DM's or group's message.
		post, err := postBusiness.GetDgraphPostByUUID(ctx, id, uid)
		if err == nil && post != nil && post.Channel != nil {
			out[id] = postThreadDecision(post)
			continue
		}
		if err != nil && !errors.Is(err, postDgraphModels.ErrNotFound) {
			return nil, err
		}
		chat, err := chatBusiness.GetDgraphChatBasicByUUID(ctx, id, uid)
		if errors.Is(err, chatDgraphModels.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if chat != nil && chat.DM != nil {
			out[id] = chatThreadDecision(chat)
		}
	}
	return out, nil
}

// postThreadDecision is today's rule for a channel thread, its root as read
// for the person (PostByUUIDWithAllComments, CreateCommentInPost): its
// channel's members reply and its moderators moderate; nobody else reads it,
// even in a public channel. An archived channel's thread is read only; a
// deleted root's thread shows its replies under a placeholder and takes none.
// Pure.
func postThreadDecision(post *dgraphStruct.DgraphPost) accessBusiness.Decision {
	var d accessBusiness.Decision
	switch ch := post.Channel; {
	case ch.IsMember > 0 && ch.IsAdmin > 0:
		d.Level, d.Reasons = LevelModerate, []string{reasonModerator}
	case ch.IsMember > 0:
		d.Level, d.Reasons = LevelReply, []string{reasonMember}
	}
	d.Was = d.Level
	if helpers.IsSoftDeleted(post.Channel.DeletedAt) {
		d = capAt(d, LevelRead)
		d.Archived = true
	}
	if helpers.IsSoftDeleted(post.DeletedAt) {
		d = capAt(d, LevelRead)
		d.Deleted = true
	}
	d.Was = max(d.Was, d.Level)
	return d
}

// chatThreadDecision is today's rule for a DM's or group's thread
// (GetDgraphChatByUUIDWithAllComments, CreateChatComment): its participants
// reply, unless either person of the message has left; a deleted root's
// thread is read only. Pure.
func chatThreadDecision(chat *dgraphStruct.DgraphChat) accessBusiness.Decision {
	if chat.DM == nil || chat.DM.ParticipantIsMember == 0 {
		return accessBusiness.Decision{}
	}
	d := accessBusiness.Decision{Level: LevelReply, Was: LevelReply, Reasons: []string{reasonParticipant}}
	left := func(u *dgraphStruct.DgraphUser) bool { return u != nil && u.DeletedAt != nil && !u.DeletedAt.IsZero() }
	if left(chat.From) || left(chat.To) {
		d.Level = LevelRead
	}
	if helpers.IsSoftDeleted(chat.DeletedAt) {
		d = capAt(d, LevelRead)
		d.Deleted = true
	}
	d.Was = max(d.Was, d.Level)
	return d
}

// ---------------------------------------------------------------------- calls

type callRules struct{}

func (callRules) DecideMany(ctx context.Context, who accessBusiness.Subject, ids []string) (map[string]accessBusiness.Decision, error) {
	out := map[string]accessBusiness.Decision{}
	uid, err := uidOf(ctx, who)
	if err != nil {
		return nil, err
	}
	for _, room := range ids {
		channelID, groupingID := helpers.ClassifyRoom(room)
		switch {
		case channelID != "":
			ch, err := channelOf(ctx, channelID, uid)
			if err != nil {
				return nil, err
			}
			if ch != nil {
				out[room] = channelCallDecision(ch)
			}
		case ChatRef(groupingID).Kind == KindDM && uid != "":
			// A DM's room: its two people.
			if ChatRef(groupingID).Other(who.UserUUID) != "" {
				out[room] = accessBusiness.Decision{Level: LevelPost, Was: LevelPost, Reasons: []string{reasonParticipant}}
			} else {
				out[room] = accessBusiness.Decision{}
			}
		case groupingID != "" && uid != "":
			dm, err := chatBusiness.GetDgraphDmBasicInfoFromDgraph(ctx, uid, groupingID)
			if err != nil {
				return nil, err
			}
			if dm != nil && dm.GroupingId != "" {
				out[room] = groupDecision(dm)
			}
		}
		// An instant meeting (meet-…) is in no conversation: its guests join
		// by its link, which the guest routes decide.
	}
	return out, nil
}

// channelCallDecision is today's rule for a channel's call (MakeVideoChannelCall):
// its members join, and its moderators moderate; a public channel's other
// readers don't. Pure.
func channelCallDecision(ch *dgraphStruct.DgraphChannel) accessBusiness.Decision {
	var d accessBusiness.Decision
	switch {
	case ch.IsMember > 0 && ch.IsAdmin > 0:
		d.Level, d.Reasons = LevelModerate, []string{reasonModerator}
	case ch.IsMember > 0:
		d.Level, d.Reasons = LevelPost, []string{reasonMember}
	}
	d.Was = d.Level
	return d
}
