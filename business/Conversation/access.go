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
	"strings"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	chatBusiness "github.com/akashc777/OneCamp/business/Chat"
	postBusiness "github.com/akashc777/OneCamp/business/Post"
	sendBusiness "github.com/akashc777/OneCamp/business/Send"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	channelDomain "github.com/akashc777/OneCamp/domain/Channel"
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
	// The graph's standings for many channels at once.
	readChannelStandings = channelDomain.ChannelStandings
	// The graph's account for someone, nil once they are deactivated.
	activeGraphUser = userDomain.GetActiveDgraphUserInfoByUUID
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
	public          bool // a public channel's, whose other readers read it
	archived        bool
	authorLeft      bool // a DM's or group's message whose person left
	rootDeleted     bool
}

// decideThread is a thread's rule. Pure.
func decideThread(s threadStanding) accessBusiness.Decision {
	if !s.member {
		if !s.channel {
			// Nothing, not even whether its root was deleted.
			return accessBusiness.Decision{}
		}
		// Someone not in the channel reads its threads as they read the
		// channel (decideChannel): a public one's, or as a manager who isn't
		// in it, read and never reply; anyone else's, nothing at all.
		return threadHeld(decideChannel(channelStanding{manager: s.manager, public: s.public}), s)
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
	return threadHeld(d, s)
}

// threadHeld is d as a thread's holds leave it, whatever opened it (the
// thread's rule, a call's invite): an archived channel's thread, or a deleted
// root's, is read only, and says which. What opened nothing stays nothing,
// not even whether its root was deleted. Pure.
func threadHeld(d accessBusiness.Decision, s threadStanding) accessBusiness.Decision {
	if d.Level == accessBusiness.LevelNone {
		return d
	}
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

// postThreadStanding is a channel thread as the person stands in it, its root
// as read for them. Pure.
func postThreadStanding(post *dgraphStruct.DgraphPost) threadStanding {
	ch := graphChannel(post.Channel)
	return threadStanding{
		member: ch.member, manager: ch.manager, public: ch.public, channel: true,
		archived:    ch.archived,
		rootDeleted: helpers.IsSoftDeleted(post.DeletedAt),
	}
}

// chatThreadStanding is a DM's or group's thread as the person stands in it,
// its root as read for them (decideThread: its participants reply, unless
// either person of the message has left); chat.DM is not nil. Pure.
func chatThreadStanding(chat *dgraphStruct.DgraphChat) threadStanding {
	return threadStanding{
		member:      chat.DM.ParticipantIsMember > 0,
		authorLeft:  left(chat.From) || left(chat.To),
		rootDeleted: helpers.IsSoftDeleted(chat.DeletedAt),
	}
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
	u, err := activeGraphUser(ctx, who.UserUUID)
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

// indexPlace is where me stands in a DM or group, conv, by the index: whether
// they are in it (a DM's two people always are), and whether the index has it
// at all. A channel's standing is indexChannelStanding's.
func indexPlace(ctx context.Context, me uuid.UUID, conv Ref) (member, found bool, err error) {
	switch conv.Kind {
	case KindGroup:
		in, err := indexGroups(ctx, me, []string{conv.ID})
		if err != nil {
			return false, false, err
		}
		participant, ok := in[conv.ID]
		return participant, ok, nil
	case KindDM:
		return conv.Other(me.String()) != "", true, nil
	}
	return false, false, nil
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
	if err != nil || uid == "" {
		return out, err
	}
	// The graph's standings for every channel asked, a batch a query (they
	// were read one query each: a Connected panel naming fifteen channels
	// made fifteen round trips).
	asked := map[string][]string{}
	for _, id := range ids {
		if cu, err := uuid.Parse(id); err == nil {
			asked[cu.String()] = append(asked[cu.String()], id)
		}
	}
	keys := make([]string, 0, len(asked))
	for k := range asked {
		keys = append(keys, k)
	}
	for from := 0; from < len(keys); from += channelDomain.ChannelStandingsBatch {
		chunk := keys[from:min(from+channelDomain.ChannelStandingsBatch, len(keys))]
		got, err := readChannelStandings(ctx, chunk, uid)
		if err != nil {
			return nil, err
		}
		for _, ch := range got {
			if ch == nil || ch.Uuid == "" {
				continue
			}
			for _, id := range asked[strings.ToLower(ch.Uuid)] {
				out[id] = channelDecision(ch)
			}
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
	// Each root's holds (archived, rootDeleted), decided or not, filled once
	// per root kind whichever source decides: what holds its thread for
	// someone a call's invite lets in (V-1138). Nothing else of a standing is
	// stored in it.
	roots := map[uuid.UUID]threadStanding{}
	// From the index, a channel post's root waits for its channel's facts,
	// which come for every root's channel in one read after the loop.
	type channelRoot struct {
		id, channel string
		s           threadStanding
	}
	var channelRoots []channelRoot
	var channelIDs []string
	channelSeen := map[string]bool{}
	for _, id := range ids {
		root, err := uuid.Parse(id)
		if err != nil || uid == "" {
			continue
		}
		// A root is a channel's post or a DM's or group's message, read from
		// the graph; who is in its conversation, from wherever decisions read.
		post, err := postBusiness.GetDgraphPostByUUID(ctx, id, uid)
		if err == nil && post != nil && post.Channel != nil {
			roots[root] = threadStanding{
				archived:    helpers.IsSoftDeleted(post.Channel.DeletedAt),
				rootDeleted: helpers.IsSoftDeleted(post.DeletedAt),
			}
			if !index {
				out[id] = decideThread(postThreadStanding(post))
				continue
			}
			s := roots[root]
			s.channel = true
			if !channelSeen[post.Channel.Uuid] {
				channelSeen[post.Channel.Uuid] = true
				channelIDs = append(channelIDs, post.Channel.Uuid)
			}
			channelRoots = append(channelRoots, channelRoot{id: id, channel: post.Channel.Uuid, s: s})
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
		roots[root] = threadStanding{rootDeleted: helpers.IsSoftDeleted(chat.DeletedAt)}
		if !index {
			out[id] = decideThread(chatThreadStanding(chat))
			continue
		}
		s := roots[root]
		s.authorLeft = left(chat.From) || left(chat.To)
		var found bool
		if s.member, found, err = indexPlace(ctx, me, ChatRef(chat.DM.GroupingId)); err != nil {
			return nil, err
		}
		if found {
			out[id] = decideThread(s)
		}
	}
	if len(channelIDs) > 0 {
		facts, err := indexChannels(ctx, me, channelIDs)
		if err != nil {
			return nil, err
		}
		for _, r := range channelRoots {
			if f, found := facts[r.channel]; found {
				ch := indexChannelStanding(f)
				r.s.member, r.s.manager, r.s.public = ch.member, ch.manager, ch.public
				out[r.id] = decideThread(r.s)
			}
		}
	}
	// A live call's thread, for someone let into the call through its invite:
	// never for someone the graph has no uid for (deactivated), whom no rule
	// above decides either.
	if uid != "" {
		if err := callInvites(ctx, who, out, ids, roots); err != nil {
			return nil, err
		}
	}
	// Then what a live call holds, over whichever reader decided.
	if err := chatHolds(ctx, who, out); err != nil {
		return nil, err
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
		// The rooms' channels in one read and their groups in one, whatever
		// their number (a table's call links, the call picker's pages); a
		// DM's room is its two people, and reads nothing (indexPlace's rule).
		channelRooms, groupRooms := map[string][]string{}, map[string][]string{}
		var channelIDs, groupIDs []string
		for _, room := range ids {
			channelID, groupingID := helpers.ClassifyRoom(room)
			switch conv := ChatRef(groupingID); {
			case channelID != "":
				if channelRooms[channelID] == nil {
					channelIDs = append(channelIDs, channelID)
				}
				channelRooms[channelID] = append(channelRooms[channelID], room)
			case groupingID == "":
			case conv.Kind == KindDM:
				out[room] = decideGroup(conv.Other(me.String()) != "")
			case conv.Kind == KindGroup:
				if groupRooms[conv.ID] == nil {
					groupIDs = append(groupIDs, conv.ID)
				}
				groupRooms[conv.ID] = append(groupRooms[conv.ID], room)
			}
		}
		if len(channelIDs) > 0 {
			facts, err := indexChannels(ctx, me, channelIDs)
			if err != nil {
				return nil, err
			}
			for id, rooms := range channelRooms {
				if f, ok := facts[id]; ok {
					for _, room := range rooms {
						out[room] = decideChannelCall(indexChannelStanding(f))
					}
				}
			}
		}
		if len(groupIDs) > 0 {
			in, err := indexGroups(ctx, me, groupIDs)
			if err != nil {
				return nil, err
			}
			for id, rooms := range groupRooms {
				if participant, ok := in[id]; ok {
					for _, room := range rooms {
						out[room] = decideGroup(participant)
					}
				}
			}
		}
		if err := callRoomInvites(ctx, who, out, ids); err != nil {
			return nil, err
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
	// A call someone was let into through a link they opened as themselves:
	// never for someone the graph has no uid for (deactivated).
	if uid != "" {
		if err := callRoomInvites(ctx, who, out, ids); err != nil {
			return nil, err
		}
	}
	return out, nil
}
