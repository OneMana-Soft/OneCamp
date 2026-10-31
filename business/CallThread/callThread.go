package business

// The call's thread surface (calls CALLS-3b, agreed with CHAT-LEAD-2, 22 Oct):
// how someone outside a conversation reads its call's thread while the call
// goes on. A member let in by the call's invite asks as a member, under the
// surface's mark (conversation ForCallThread), which is the one place HU13's
// admission opens the thread; a guest in the call asks by the call token
// they joined with. Both read the same thing (business/Guest CallThread): the
// call's post and what was written while the call went on, and nothing of
// the conversation. Once the call ends, the surface answers nothing.
//
// They write there too, as plain text: a member as themselves, a guest in a
// channel's call as the guest bot under the name they joined with. Someone
// let in by the invite writes as outside the conversation: the reply is the
// thread's, but they don't become one of its people, so it never reaches
// their Threads list (CHAT-LEAD-2's condition 2).

import (
	"context"
	"errors"
	"slices"
	"time"

	chatAdapter "github.com/akashc777/OneCamp/adapter/Chat"
	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	callControlBusiness "github.com/akashc777/OneCamp/business/CallControl"
	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	chatBusiness "github.com/akashc777/OneCamp/business/Chat"
	contentcodec "github.com/akashc777/OneCamp/business/ContentCodec"
	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	postBusiness "github.com/akashc777/OneCamp/business/Post"
	threadIndex "github.com/akashc777/OneCamp/business/ThreadIndex"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	chatDgraphModels "github.com/akashc777/OneCamp/models/dgraph/Chat"
	postDgraphModels "github.com/akashc777/OneCamp/models/dgraph/Post"
	callStateModel "github.com/akashc777/OneCamp/models/postgres/CallState"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// ErrNotFound is a call that isn't going on, has no post yet, or whose thread
// isn't the asker's: all answer alike.
var ErrNotFound = errors.New("callthread: not found")

// ErrReadOnly is a call's thread the asker reads but may not write in: a
// guest in a DM's or a group's call (a DM's thread takes only its people as
// writers), or a conversation that takes no new replies (an archived
// channel, a DM someone has left).
var ErrReadOnly = errors.New("callthread: read only")

// ErrChatHeld is a call's thread whose chat the people running the call hold
// for everyone else, the writer included (calls meet-12).
var ErrChatHeld = errors.New("callthread: chat held")

// ChatHeldText is what a reply refused for a held chat says, on every route
// that refuses one.
const ChatHeldText = "The people running the call have turned off chat for now."

// ErrUnchanged is a poll's answer when nothing was written in the thread after
// the newest reply its reader has: read from the thread's index alone, so most
// of a call's polls never read the thread (CHAT-LEAD-2's condition on the
// poll).
var ErrUnchanged = errors.New("callthread: unchanged")

// The reads and writes, seams for tests.
var seams = struct {
	session     func(context.Context, string) (*callStateBusiness.Session, error)
	current     func(context.Context, callStateBusiness.Ref) (*callStateBusiness.Session, error)
	guestOf     func(string) (string, string, error)
	resolve     func(context.Context, *userModels.UserInfo, conversationBusiness.Ref) (accessBusiness.Decision, error)
	read        func(context.Context, string, string, time.Time, time.Time) (*guestBusiness.CallThread, error)
	latest      func(context.Context, string) (time.Time, error)
	post        func(context.Context, string, string) (*dgraphStruct.DgraphPost, error)
	chat        func(context.Context, string, string) (*dgraphStruct.DgraphChat, error)
	comment     func(context.Context, *postAdapter.InputCreateOrUpdateCommentToPost, *userModels.UserInfo, *dgraphStruct.DgraphPost) error
	chatComment func(context.Context, *chatAdapter.InputCreateOrUpdateCommentToChat, *userModels.UserInfo, *dgraphStruct.DgraphChat) error
	guestReply  func(ctx context.Context, post, channel, name, text string) error
	chatHeld    func(ctx context.Context, s *callStateBusiness.Session, identity string) (bool, error)
	livePost    func(context.Context, uuid.UUID) (bool, error)
}{
	session:     callStateBusiness.Get,
	current:     callStateBusiness.Current,
	guestOf:     callControlBusiness.GuestOf,
	resolve:     conversationBusiness.Resolve,
	livePost:    callStateBusiness.IsLivePost,
	read:        guestBusiness.GetCallThread,
	latest:      threadIndex.LatestReply,
	post:        postBusiness.GetDgraphPostByUUID,
	chat:        chatBusiness.GetDgraphChatBasicByUUID,
	comment:     commentOnPost,
	chatComment: commentOnChat,
	guestReply:  guestBusiness.ReplyInCallThread,
	chatHeld:    guestChatHeld,
}

// The two reply writes, as the conversation's own reply routes make them,
// without mentions: the surface takes plain text.
func commentOnPost(ctx context.Context, in *postAdapter.InputCreateOrUpdateCommentToPost, who *userModels.UserInfo, p *dgraphStruct.DgraphPost) error {
	_, err := postBusiness.CreatePostComment(ctx, in, who, nil, p)
	return err
}

func commentOnChat(ctx context.Context, in *chatAdapter.InputCreateOrUpdateCommentToChat, who *userModels.UserInfo, c *dgraphStruct.DgraphChat) error {
	_, err := chatBusiness.CreateChatComment(ctx, in, who, nil, c.DM.GroupingId, c.From.Uid, c)
	return err
}

// memberCall is the live call with its post, and the member's decision on its
// thread, asked as the call's surface. Anything else answers ErrNotFound.
func memberCall(ctx context.Context, who *userModels.UserInfo, session string) (*callStateBusiness.Session, accessBusiness.Decision, error) {
	var none accessBusiness.Decision
	if helpers.ActsThroughToken(ctx) {
		return nil, none, ErrNotFound
	}
	s, err := seams.session(ctx, session)
	if err != nil {
		return nil, none, err
	}
	if s == nil || !s.Live || s.PostUUID == "" {
		return nil, none, ErrNotFound
	}
	d, err := seams.resolve(conversationBusiness.ForCallThread(ctx), who, conversationBusiness.Ref{Kind: conversationBusiness.KindThread, ID: s.PostUUID})
	if errors.Is(err, conversationBusiness.ErrNotFound) {
		return nil, none, ErrNotFound
	}
	if err != nil {
		return nil, none, err
	}
	return s, d, nil
}

// guestCall is the live call with its post that the guest's call token is
// for, and the guest as the call has them, while they're in it.
func guestCall(ctx context.Context, token string) (*callStateBusiness.Session, *callStateBusiness.Person, error) {
	identity, room, err := seams.guestOf(token)
	if err != nil {
		return nil, nil, ErrNotFound
	}
	ref, _, ok := callStateBusiness.RefOfRoom(room)
	if !ok {
		return nil, nil, ErrNotFound
	}
	s, err := seams.current(ctx, ref)
	if err != nil {
		return nil, nil, err
	}
	if s == nil || !s.Live || s.Room != room || s.PostUUID == "" {
		return nil, nil, ErrNotFound
	}
	p := s.Person(identity)
	if p == nil || !p.In {
		return nil, nil, ErrNotFound
	}
	return s, p, nil
}

// poll reads the newest reply's time in the call's thread from its index, and
// says whether nothing came after since. It is read before the thread itself,
// so a reply landing between the two is read again, never missed. Only a new
// reply moves it: an edited or deleted one doesn't, so the app reads the
// whole thread every FULL_EVERY polls, about once a minute
// (lib/calls/callThread). No since, or an index that can't be read, reads
// the thread.
func poll(ctx context.Context, post string, since time.Time) (latest time.Time, unchanged bool) {
	latest, err := seams.latest(ctx, post)
	if err != nil {
		return time.Time{}, false
	}
	return latest, !since.IsZero() && !latest.After(since)
}

// ChatHeldFor is whether the people running a live call hold its chat for
// who in the thread of post: the thread's own decision (business/
// Conversation's chatHolds). A channel's reply route asks it too, since the
// call's chat is that thread and its view in the channel would otherwise be
// a way round the hold (calls meet-12). A thread who can't read holds
// nothing: the route's own checks answer them. Only a live call's post can be
// held, so one indexed read comes first, and most replies, edits and
// reactions never take the thread's whole decision.
func ChatHeldFor(ctx context.Context, who *userModels.UserInfo, post string) (bool, error) {
	id, err := uuid.Parse(post)
	if err != nil {
		return false, nil
	}
	if live, err := seams.livePost(ctx, id); err != nil || !live {
		return false, err
	}
	d, err := seams.resolve(ctx, who, conversationBusiness.Ref{Kind: conversationBusiness.KindThread, ID: post})
	if errors.Is(err, conversationBusiness.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return slices.Contains(d.Reasons, conversationBusiness.ReasonCallChatHeld), nil
}

// guestChatHeld is whether the call's chat is held for a guest in it: the
// same read a member's thread access makes (business/Conversation's
// chatHolds).
func guestChatHeld(ctx context.Context, s *callStateBusiness.Session, identity string) (bool, error) {
	post, err := uuid.Parse(s.PostUUID)
	if err != nil {
		return false, err
	}
	held, err := callStateModel.ChatHeldOver(ctx, []uuid.UUID{post}, nil, identity)
	return held[post], err
}

// answer is the thread read for its reader: whether they may write, why not
// when the call holds its chat, and the time their next poll sends back (the
// post's own while it has no reply).
func answer(t *guestBusiness.CallThread, canReply, held bool, latest time.Time) *guestBusiness.CallThread {
	t.CanReply = canReply && !held
	t.ChatHeld = held
	t.Latest = latest
	if t.Post.CreatedAt.After(latest) {
		t.Latest = t.Post.CreatedAt
	}
	return t
}

// ForMember is the call's thread for a member: someone in its conversation,
// or let into the call by its invite. since, when set, is the newest reply
// they have: ErrUnchanged when nothing came after it.
func ForMember(ctx context.Context, who *userModels.UserInfo, session string, now, since time.Time) (*guestBusiness.CallThread, error) {
	s, d, err := memberCall(ctx, who, session)
	if err != nil {
		return nil, err
	}
	t, latest, err := readSince(ctx, s, now, since)
	if err != nil {
		return nil, err
	}
	return answer(t, d.Level >= conversationBusiness.LevelReply, slices.Contains(d.Reasons, conversationBusiness.ReasonCallChatHeld), latest), nil
}

// ForGuest is the call's thread for a guest in the call, by their call token;
// since as ForMember's.
func ForGuest(ctx context.Context, token string, now, since time.Time) (*guestBusiness.CallThread, error) {
	s, p, err := guestCall(ctx, token)
	if err != nil {
		return nil, err
	}
	t, latest, err := readSince(ctx, s, now, since)
	if err != nil {
		return nil, err
	}
	// The call's own screen learns of a hold from the room's metadata at
	// once; a full read says it too.
	held, err := seams.chatHeld(ctx, s, p.Identity)
	if err != nil {
		return nil, err
	}
	return answer(t, s.Resource.Kind == "channel", held, latest), nil
}

// readSince reads a call's thread for ForMember and ForGuest, with its newest
// reply (latest): ErrUnchanged when nothing came after since.
func readSince(ctx context.Context, s *callStateBusiness.Session, now, since time.Time) (*guestBusiness.CallThread, time.Time, error) {
	latest, unchanged := poll(ctx, s.PostUUID, since)
	if unchanged {
		return nil, latest, ErrUnchanged
	}
	t, err := seams.read(ctx, s.Resource.Kind, s.PostUUID, s.StartedAt, now)
	return t, latest, err
}

// ReplyAsMember writes a member's reply in the call's thread, as them. Its
// text is plain, under 4,000 characters (guestBusiness.ErrGuestInput says
// what's wrong with it).
func ReplyAsMember(ctx context.Context, who *userModels.UserInfo, session, text string) error {
	text, err := guestBusiness.MessageText(text)
	if err != nil {
		return err
	}
	s, d, err := memberCall(ctx, who, session)
	if err != nil {
		return err
	}
	if slices.Contains(d.Reasons, conversationBusiness.ReasonCallChatHeld) {
		return ErrChatHeld
	}
	if d.Level < conversationBusiness.LevelReply {
		return ErrReadOnly
	}
	return writeReply(ctx, who, s, messageHTML(text), slices.Contains(d.Reasons, conversationBusiness.ReasonCallInvite))
}

// Report writes a call's report (business/Huddle) under its post as who, once
// the call has ended: someone who may still reply there, by the thread's own
// decision, as any reply of theirs would be written.
func Report(ctx context.Context, who *userModels.UserInfo, s *callStateBusiness.Session, body string) error {
	if s.Live || s.PostUUID == "" {
		return ErrNotFound
	}
	d, err := seams.resolve(ctx, who, conversationBusiness.Ref{Kind: conversationBusiness.KindThread, ID: s.PostUUID})
	if errors.Is(err, conversationBusiness.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if d.Level < conversationBusiness.LevelReply {
		return ErrReadOnly
	}
	return writeReply(ctx, who, s, body, false)
}

// writeReply writes body as who's reply under a call's post, as the
// conversation's own reply routes write one: outsider for someone let in by
// the call's invite (HU13).
func writeReply(ctx context.Context, who *userModels.UserInfo, s *callStateBusiness.Session, body string, outsider bool) error {
	uid := who.UserDgraphInfo.Uid
	switch s.Resource.Kind {
	case "channel":
		p, err := seams.post(ctx, s.PostUUID, uid)
		if errors.Is(err, postDgraphModels.ErrNotFound) || (err == nil && (p == nil || p.Channel == nil || p.PostBy == nil)) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		return seams.comment(ctx, &postAdapter.InputCreateOrUpdateCommentToPost{PostUuid: s.PostUUID, HTMLText: body, Outsider: outsider}, who, p)
	case "dm", "group":
		c, err := seams.chat(ctx, s.PostUUID, uid)
		if errors.Is(err, chatDgraphModels.ErrNotFound) || (err == nil && (c == nil || c.DM == nil || c.From == nil)) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		return seams.chatComment(ctx, &chatAdapter.InputCreateOrUpdateCommentToChat{ChatUuid: s.PostUUID, HTMLText: body, Outsider: outsider}, who, c)
	}
	return ErrNotFound
}

// messageHTML is a member's plain text as a message: its line breaks kept and
// its web addresses as links, as the conversation's own messages have them,
// and nothing in it read as markup (the content codec's plain-text reader).
// Pure.
func messageHTML(text string) string {
	return contentcodec.ToHTML(contentcodec.FromText(text))
}

// ReplyAsGuest writes a guest's reply in a channel's call thread, by their
// call token, under the name they joined the call with. A DM's or a group's
// call thread is read-only to a guest.
func ReplyAsGuest(ctx context.Context, token, text string) error {
	text, err := guestBusiness.MessageText(text)
	if err != nil {
		return err
	}
	s, p, err := guestCall(ctx, token)
	if err != nil {
		return err
	}
	if s.Resource.Kind != "channel" {
		return ErrReadOnly
	}
	held, err := seams.chatHeld(ctx, s, p.Identity)
	if err != nil {
		return err
	}
	if held {
		return ErrChatHeld
	}
	name := guestBusiness.NameInCall(p.Name)
	if name == "" {
		name = "Guest"
	}
	return seams.guestReply(ctx, s.PostUUID, s.Resource.ID, name, text)
}
