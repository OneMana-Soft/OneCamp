package business

// The chat kinds of the Access Resolver (PROGRAM.md §5 and CH4). Each kind is
// decided by one pure rule over where the person stands in the conversation
// (a standing), and the standings are read from one of two places
// (domain/Conversation source.go chooses, and says why):
//
//   - the graph, one read per id, as today's handlers read it: business/Channel
//     CanRead and business/Send MayPostIn over GetBasicDgraphChannelInfoByUUID
//     for a channel, a DM's two people and business/Send MayMessage, a group's
//     dm_is_member, a thread's root and its conversation, a call's room;
//   - the conversations index in Postgres (conversation_members), which reads a
//     whole list of channels or groups in one query, once it is known to agree
//     with the graph.
//
// The rules:
//   - a channel: its members post (an announcement channel's reply in threads),
//     its managers moderate, a public channel's others read; an archived channel
//     is read only;
//   - a DM: its two people read it, and post while the other can receive a
//     message;
//   - a group: its people post in it, and nobody else reads it;
//   - a thread: its root's conversation's members reply (never a public
//     channel's other readers), its managers moderate; a deleted root's or an
//     archived channel's thread is read only;
//   - a call: its room's conversation's members and people join.
//
// One difference between the two readers is deliberate: the graph lets a
// channel manager who is no longer a member read a private channel (an old
// edge; a removal takes both now), and the index, which keeps no such row,
// does not.

import (
	"context"
	"errors"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	chatBusiness "github.com/akashc777/OneCamp/business/Chat"
	postBusiness "github.com/akashc777/OneCamp/business/Post"
	sendBusiness "github.com/akashc777/OneCamp/business/Send"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	conversationDomain "github.com/akashc777/OneCamp/domain/Conversation"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	chatDgraphModels "github.com/akashc777/OneCamp/models/dgraph/Chat"
	postDgraphModels "github.com/akashc777/OneCamp/models/dgraph/Post"
	models "github.com/akashc777/OneCamp/models/postgres/Conversation"
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

// Where decisions read, and the index's reads: seams for tests.
var (
	accessSource  = conversationDomain.AccessSource
	indexChannels = models.ChannelFacts
	indexGroups   = models.GroupFacts
	indexPeople   = models.People
)

// fromIndex is whether this decision reads the index.
func fromIndex(ctx context.Context) bool { return accessSource(ctx).Index }

// capAt is d held at most at l: what a deleted root or an archived channel
// leaves, with Was the level before.
func capAt(d accessBusiness.Decision, l accessBusiness.Level) accessBusiness.Decision {
	d.Was = d.Level
	d.Level = min(d.Level, l)
	return d
}

// ------------------------------------------------------------------ standings

// channelStanding is a channel as one person stands in it.
type channelStanding struct {
	member, manager bool
	// public is a channel known to be public: one whose privacy didn't load
	// is private.
	public       bool
	announcement bool
	archived     bool
}

// decideChannel is a channel's rule. Pure.
func decideChannel(s channelStanding) accessBusiness.Decision {
	if !s.member && !s.manager && !s.public {
		// Nothing, not even whether it's archived.
		return accessBusiness.Decision{}
	}
	var d accessBusiness.Decision
	switch {
	case s.member && s.manager:
		d.Level, d.Reasons = LevelModerate, []string{reasonModerator}
	case s.member && s.announcement:
		// An announcement channel: its members reply in threads.
		d.Level, d.Reasons = LevelReply, []string{reasonMember}
	case s.member:
		d.Level, d.Reasons = LevelPost, []string{reasonMember}
	default:
		// A public channel, or a manager who left it: read, never post.
		d.Level, d.Reasons = LevelRead, []string{reasonPublic}
	}
	if s.archived {
		d = capAt(d, LevelRead)
		d.Archived = true
	}
	d.Was = max(d.Was, d.Level)
	return d
}

// decideDM is a DM's rule for one of its two people. Pure.
func decideDM(otherCanReceive bool) accessBusiness.Decision {
	d := accessBusiness.Decision{Level: LevelRead, Reasons: []string{reasonParticipant}}
	if otherCanReceive {
		d.Level = LevelPost
	}
	d.Was = d.Level
	return d
}

// decideGroup is a group's rule. Pure.
func decideGroup(participant bool) accessBusiness.Decision {
	if !participant {
		return accessBusiness.Decision{}
	}
	return accessBusiness.Decision{Level: LevelPost, Was: LevelPost, Reasons: []string{reasonParticipant}}
}

// threadStanding is a thread as one person stands in it: in its root's
// conversation, and the root's state.
type threadStanding struct {
	member, manager bool
	channel         bool // a channel's thread, whose managers moderate
	archived        bool
	authorLeft      bool // a DM's or group's message whose person left
	rootDeleted     bool
}

// decideThread is a thread's rule. Pure.
func decideThread(s threadStanding) accessBusiness.Decision {
	if !s.member {
		// Nothing, not even whether its root was deleted.
		return accessBusiness.Decision{}
	}
	var d accessBusiness.Decision
	switch {
	case s.channel && s.manager:
		d.Level, d.Reasons = LevelModerate, []string{reasonModerator}
	case s.channel:
		d.Level, d.Reasons = LevelReply, []string{reasonMember}
	default:
		d.Level, d.Reasons = LevelReply, []string{reasonParticipant}
		if s.authorLeft {
			d.Level = LevelRead
		}
	}
	d.Was = d.Level
	if s.archived {
		d = capAt(d, LevelRead)
		d.Archived = true
	}
	if s.rootDeleted {
		d = capAt(d, LevelRead)
		d.Deleted = true
	}
	d.Was = max(d.Was, d.Level)
	return d
}

// decideChannelCall is a channel call's rule: its members join, its managers
// moderate, a public channel's other readers don't. Pure.
func decideChannelCall(s channelStanding) accessBusiness.Decision {
	var d accessBusiness.Decision
	switch {
	case s.member && s.manager:
		d.Level, d.Reasons = LevelModerate, []string{reasonModerator}
	case s.member:
		d.Level, d.Reasons = LevelPost, []string{reasonMember}
	}
	d.Was = d.Level
	return d
}

// left is whether u is someone who has left (been deactivated). Pure.
func left(u *dgraphStruct.DgraphUser) bool {
	return u != nil && u.DeletedAt != nil && !u.DeletedAt.IsZero()
}

// ------------------------------------------------------- the graph's standings

// graphChannel is a channel as the graph reads it for the person. Pure.
func graphChannel(ch *dgraphStruct.DgraphChannel) channelStanding {
	return channelStanding{
		member:       ch.IsMember > 0,
		manager:      ch.IsAdmin > 0,
		public:       ch.IsPrivate != nil && !*ch.IsPrivate,
		announcement: ch.PostPolicy == "admins_only",
		archived:     helpers.IsSoftDeleted(ch.DeletedAt),
	}
}

// channelDecision is today's rule for a channel as read for the person (its
// member and moderator counts are theirs). Pure.
func channelDecision(ch *dgraphStruct.DgraphChannel) accessBusiness.Decision {
	if !channelBusiness.CanRead(ch) {
		return accessBusiness.Decision{}
	}
	// CanRead and the standing agree: member, manager or known public.
	return decideChannel(graphChannel(ch))
}

// dmDecision is today's rule for a DM one is in, with the other person as
// read: both read it; a message goes only to someone who can receive one
// (MayMessage). Pure.
func dmDecision(to *dgraphStruct.DgraphUser) accessBusiness.Decision {
	return decideDM(to != nil && sendBusiness.MayMessage(to) == nil)
}

// groupDecision is today's rule for a group as read for the person. Pure.
func groupDecision(dm *dgraphStruct.DgraphDm) accessBusiness.Decision {
	return decideGroup(dm.ParticipantIsMember > 0)
}

// postThreadDecision is today's rule for a channel thread, its root as read
// for the person. Pure.
func postThreadDecision(post *dgraphStruct.DgraphPost) accessBusiness.Decision {
	ch := post.Channel
	return decideThread(threadStanding{
		member: ch.IsMember > 0, manager: ch.IsAdmin > 0, channel: true,
		archived:    helpers.IsSoftDeleted(ch.DeletedAt),
		rootDeleted: helpers.IsSoftDeleted(post.DeletedAt),
	})
}

// chatThreadDecision is today's rule for a DM's or group's thread: its
// participants reply, unless either person of the message has left; a deleted
// root's thread is read only. Pure.
func chatThreadDecision(chat *dgraphStruct.DgraphChat) accessBusiness.Decision {
	if chat.DM == nil {
		return accessBusiness.Decision{}
	}
	return decideThread(threadStanding{
		member:      chat.DM.ParticipantIsMember > 0,
		authorLeft:  left(chat.From) || left(chat.To),
		rootDeleted: helpers.IsSoftDeleted(chat.DeletedAt),
	})
}

// channelCallDecision is today's rule for a channel's call. Pure.
func channelCallDecision(ch *dgraphStruct.DgraphChannel) accessBusiness.Decision {
	return decideChannelCall(graphChannel(ch))
}

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

// ------------------------------------------------------- the index's standings

// indexSubject is who as the index knows them: their id, and whether they are
// an active person (anyone else reads nothing).
func indexSubject(ctx context.Context, who accessBusiness.Subject) (uuid.UUID, bool, error) {
	id, err := uuid.Parse(who.UserUUID)
	if err != nil {
		return uuid.Nil, false, nil
	}
	got, err := indexPeople(ctx, []uuid.UUID{id})
	if err != nil {
		return uuid.Nil, false, err
	}
	return id, got[id].Active, nil
}

// indexChannelStanding is a channel as the index reads it. Pure.
func indexChannelStanding(f models.ChannelFact) channelStanding {
	return channelStanding{
		member: f.Member, manager: f.Manager, public: !f.Private,
		announcement: f.PostPolicy == "admins_only", archived: f.Archived,
	}
}

// canReceive is whether someone can receive a DM, as business/Principal
// CanReceiveDirectMessage and business/Send MayMessage say from the graph:
// an active account, and no external identity unless it's a bot. Pure.
func canReceive(p models.Person, found bool) bool {
	return found && p.Active && (!p.External || p.Bot)
}

// indexPlace is where me stands in conv by the index: whether they are in it
// (a DM's two people always are), whether they manage it, and whether the index
// has it at all.
func indexPlace(ctx context.Context, me uuid.UUID, conv Ref) (member, manager, found bool, err error) {
	switch conv.Kind {
	case KindChannel:
		facts, err := indexChannels(ctx, me, []string{conv.ID})
		if err != nil {
			return false, false, false, err
		}
		f, ok := facts[conv.ID]
		return f.Member, f.Manager, ok, nil
	case KindGroup:
		in, err := indexGroups(ctx, me, []string{conv.ID})
		if err != nil {
			return false, false, false, err
		}
		participant, ok := in[conv.ID]
		return participant, false, ok, nil
	case KindDM:
		return conv.Other(me.String()) != "", false, true, nil
	}
	return false, false, false, nil
}

// ------------------------------------------------------------------ channels

type channelRules struct{}

func (channelRules) DecideMany(ctx context.Context, who accessBusiness.Subject, ids []string) (map[string]accessBusiness.Decision, error) {
	out := map[string]accessBusiness.Decision{}
	if fromIndex(ctx) {
		me, active, err := indexSubject(ctx, who)
		if err != nil || !active {
			return out, err
		}
		facts, err := indexChannels(ctx, me, ids)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			if f, ok := facts[id]; ok {
				out[id] = decideChannel(indexChannelStanding(f))
			}
		}
		return out, nil
	}
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

// ----------------------------------------------------------------------- DMs

type dmRules struct{}

func (dmRules) DecideMany(ctx context.Context, who accessBusiness.Subject, ids []string) (map[string]accessBusiness.Decision, error) {
	out := map[string]accessBusiness.Decision{}
	if fromIndex(ctx) {
		me, active, err := indexSubject(ctx, who)
		if err != nil || !active {
			return out, err
		}
		others := map[string]uuid.UUID{}
		var ask []uuid.UUID
		for _, id := range ids {
			them, err := uuid.Parse((Ref{Kind: KindDM, ID: id}).Other(me.String()))
			if err != nil {
				out[id] = accessBusiness.Decision{}
				continue
			}
			others[id] = them
			ask = append(ask, them)
		}
		people, err := indexPeople(ctx, ask)
		if err != nil {
			return nil, err
		}
		for id, them := range others {
			p, found := people[them]
			out[id] = decideDM(canReceive(p, found))
		}
		return out, nil
	}
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

// --------------------------------------------------------------------- groups

type groupRules struct{}

func (groupRules) DecideMany(ctx context.Context, who accessBusiness.Subject, ids []string) (map[string]accessBusiness.Decision, error) {
	out := map[string]accessBusiness.Decision{}
	if fromIndex(ctx) {
		me, active, err := indexSubject(ctx, who)
		if err != nil || !active {
			return out, err
		}
		in, err := indexGroups(ctx, me, ids)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			if participant, ok := in[id]; ok {
				out[id] = decideGroup(participant)
			}
		}
		return out, nil
	}
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

// -------------------------------------------------------------------- threads

type threadRules struct{}

func (threadRules) DecideMany(ctx context.Context, who accessBusiness.Subject, ids []string) (map[string]accessBusiness.Decision, error) {
	out := map[string]accessBusiness.Decision{}
	uid, err := uidOf(ctx, who)
	if err != nil {
		return nil, err
	}
	var me uuid.UUID
	index := fromIndex(ctx)
	if index {
		var active bool
		if me, active, err = indexSubject(ctx, who); err != nil || !active {
			return out, err
		}
	}
	for _, id := range ids {
		if _, err := uuid.Parse(id); err != nil || uid == "" {
			continue
		}
		// A root is a channel's post or a DM's or group's message, read from
		// the graph; who is in its conversation, from wherever decisions read.
		post, err := postBusiness.GetDgraphPostByUUID(ctx, id, uid)
		if err == nil && post != nil && post.Channel != nil {
			if !index {
				out[id] = postThreadDecision(post)
				continue
			}
			s := threadStanding{
				channel: true, archived: helpers.IsSoftDeleted(post.Channel.DeletedAt),
				rootDeleted: helpers.IsSoftDeleted(post.DeletedAt),
			}
			var found bool
			if s.member, s.manager, found, err = indexPlace(ctx, me, Ref{Kind: KindChannel, ID: post.Channel.Uuid}); err != nil {
				return nil, err
			}
			if found {
				out[id] = decideThread(s)
			}
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
		if chat == nil || chat.DM == nil {
			continue
		}
		if !index {
			out[id] = chatThreadDecision(chat)
			continue
		}
		s := threadStanding{authorLeft: left(chat.From) || left(chat.To), rootDeleted: helpers.IsSoftDeleted(chat.DeletedAt)}
		var found bool
		if s.member, _, found, err = indexPlace(ctx, me, ChatRef(chat.DM.GroupingId)); err != nil {
			return nil, err
		}
		if found {
			out[id] = decideThread(s)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------- calls

type callRules struct{}

func (callRules) DecideMany(ctx context.Context, who accessBusiness.Subject, ids []string) (map[string]accessBusiness.Decision, error) {
	out := map[string]accessBusiness.Decision{}
	if fromIndex(ctx) {
		me, active, err := indexSubject(ctx, who)
		if err != nil || !active {
			return out, err
		}
		for _, room := range ids {
			channelID, groupingID := helpers.ClassifyRoom(room)
			switch {
			case channelID != "":
				facts, err := indexChannels(ctx, me, []string{channelID})
				if err != nil {
					return nil, err
				}
				if f, ok := facts[channelID]; ok {
					out[room] = decideChannelCall(indexChannelStanding(f))
				}
			case groupingID != "":
				member, _, found, err := indexPlace(ctx, me, ChatRef(groupingID))
				if err != nil {
					return nil, err
				}
				if found {
					out[room] = decideGroup(member)
				}
			}
		}
		return out, nil
	}
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
			out[room] = decideGroup(ChatRef(groupingID).Other(who.UserUUID) != "")
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
