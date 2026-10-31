package business

import (
	"context"
	"errors"
	"testing"
	"time"

	chatAdapter "github.com/akashc777/OneCamp/adapter/Chat"
	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

var now = time.Date(2026, 10, 22, 9, 30, 0, 0, time.UTC)

// fake answers one live channel call with its post, and records what was
// read, and whether the access question came marked as the call's surface.
func fake(t *testing.T, s *callStateBusiness.Session) (*[]string, *bool) {
	t.Helper()
	old := seams
	t.Cleanup(func() { seams = old })
	var reads []string
	marked := false
	seams.session = func(context.Context, string) (*callStateBusiness.Session, error) { return s, nil }
	seams.current = func(context.Context, callStateBusiness.Ref) (*callStateBusiness.Session, error) { return s, nil }
	seams.guestOf = func(raw string) (string, string, error) {
		if raw == "Bearer gia" {
			return "guest-g1-gia", s.Room, nil
		}
		return "", "", errors.New("not a call token")
	}
	seams.resolve = func(ctx context.Context, _ *userModels.UserInfo, ref conversationBusiness.Ref) (accessBusiness.Decision, error) {
		marked = conversationBusiness.AskedForCallThread(ctx)
		if ref.ID != s.PostUUID {
			return accessBusiness.Decision{}, conversationBusiness.ErrNotFound
		}
		return accessBusiness.Decision{Level: conversationBusiness.LevelReply}, nil
	}
	seams.read = func(_ context.Context, kind, post string, from, until time.Time) (*guestBusiness.CallThread, error) {
		reads = append(reads, kind+" "+post+" "+from.Format(time.Kitchen)+"-"+until.Format(time.Kitchen))
		return &guestBusiness.CallThread{Post: guestBusiness.GuestMessage{ID: post, CreatedAt: from}}, nil
	}
	seams.chatHeld = func(context.Context, *callStateBusiness.Session, string) (bool, error) { return false, nil }
	seams.livePost = func(context.Context, uuid.UUID) (bool, error) { return true, nil }
	return &reads, &marked
}

func liveCall() *callStateBusiness.Session {
	return &callStateBusiness.Session{
		ID: "s1", Room: "4f0c2a1e-3b7d-4c55-9a51-0d8b1f6a2c33", Live: true, StartedAt: now.Add(-20 * time.Minute), PostUUID: "p1",
		Resource: callStateBusiness.Ref{Kind: "channel", ID: "4f0c2a1e-3b7d-4c55-9a51-0d8b1f6a2c33"},
		People:   []callStateBusiness.Person{{Identity: "guest-g1-gia", Name: "Guest: Gia", Guest: true, In: true}},
	}
}

// A member, or someone let in by the invite, reads the call's thread from its
// start to now, asked as the call's surface; a guest in the call reads the
// same by their call token.
func TestTheCallsThreadIsReadAsTheCallsSurface(t *testing.T) {
	ctx := context.Background()
	reads, marked := fake(t, liveCall())
	if _, err := ForMember(ctx, &userModels.UserInfo{}, "s1", now, time.Time{}); err != nil || !*marked {
		t.Fatalf("a member: %v (marked %v)", err, *marked)
	}
	if _, err := ForGuest(ctx, "Bearer gia", now, time.Time{}); err != nil {
		t.Fatalf("a guest: %v", err)
	}
	if len(*reads) != 2 || (*reads)[0] != "channel p1 9:10AM-9:30AM" || (*reads)[1] != (*reads)[0] {
		t.Errorf("reads: %v", *reads)
	}
}

// Nothing is read for a call that's over, a call without its post, someone
// the thread isn't open to, a stranger's token, or a guest who left.
func TestTheCallsThreadIsNothingOutsideTheCall(t *testing.T) {
	ctx := context.Background()
	for name, change := range map[string]func(*callStateBusiness.Session){
		"over":    func(s *callStateBusiness.Session) { s.Live = false },
		"no post": func(s *callStateBusiness.Session) { s.PostUUID = "" },
	} {
		s := liveCall()
		change(s)
		reads, _ := fake(t, s)
		if _, err := ForMember(ctx, &userModels.UserInfo{}, "s1", now, time.Time{}); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s, a member: %v", name, err)
		}
		if _, err := ForGuest(ctx, "Bearer gia", now, time.Time{}); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s, a guest: %v", name, err)
		}
		if len(*reads) != 0 {
			t.Errorf("%s: read %v", name, *reads)
		}
	}
	s := liveCall()
	reads, _ := fake(t, s)
	seams.resolve = func(context.Context, *userModels.UserInfo, conversationBusiness.Ref) (accessBusiness.Decision, error) {
		return accessBusiness.Decision{}, conversationBusiness.ErrNotFound
	}
	if _, err := ForMember(ctx, &userModels.UserInfo{}, "s1", now, time.Time{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("someone it isn't open to: %v", err)
	}
	if _, err := ForGuest(ctx, "Bearer someone-else", now, time.Time{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("a stranger's token: %v", err)
	}
	s.People[0].In = false
	if _, err := ForGuest(ctx, "Bearer gia", now, time.Time{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("a guest who left: %v", err)
	}
	if len(*reads) != 0 {
		t.Errorf("read %v", *reads)
	}
}

// writes records what the surface wrote, as whom and how.
type writes struct {
	posts  []*postAdapter.InputCreateOrUpdateCommentToPost
	chats  []*chatAdapter.InputCreateOrUpdateCommentToChat
	guests []string
}

func writing(t *testing.T, w *writes, reasons []string) {
	t.Helper()
	seams.resolve = func(ctx context.Context, _ *userModels.UserInfo, _ conversationBusiness.Ref) (accessBusiness.Decision, error) {
		if !conversationBusiness.AskedForCallThread(ctx) {
			return accessBusiness.Decision{}, conversationBusiness.ErrNotFound
		}
		return accessBusiness.Decision{Level: conversationBusiness.LevelReply, Reasons: reasons}, nil
	}
	seams.post = func(context.Context, string, string) (*dgraphStruct.DgraphPost, error) {
		return &dgraphStruct.DgraphPost{Uuid: "p1", Channel: &dgraphStruct.DgraphChannel{Uuid: "4f0c2a1e-3b7d-4c55-9a51-0d8b1f6a2c33"}, PostBy: &dgraphStruct.DgraphUser{Uid: "0x2"}}, nil
	}
	seams.chat = func(context.Context, string, string) (*dgraphStruct.DgraphChat, error) {
		return &dgraphStruct.DgraphChat{Uuid: "p1", DM: &dgraphStruct.DgraphDm{GroupingId: "grp-1"}, From: &dgraphStruct.DgraphUser{Uid: "0x1"}}, nil
	}
	seams.comment = func(_ context.Context, in *postAdapter.InputCreateOrUpdateCommentToPost, _ *userModels.UserInfo, _ *dgraphStruct.DgraphPost) error {
		w.posts = append(w.posts, in)
		return nil
	}
	seams.chatComment = func(_ context.Context, in *chatAdapter.InputCreateOrUpdateCommentToChat, _ *userModels.UserInfo, _ *dgraphStruct.DgraphChat) error {
		w.chats = append(w.chats, in)
		return nil
	}
	seams.guestReply = func(_ context.Context, post, channel, name, text string) error {
		w.guests = append(w.guests, post+" "+channel+" "+name+": "+text)
		return nil
	}
}

// Someone let in by the invite replies in the call's thread as themselves,
// recorded as outside the conversation; a member of it replies as usual. Both
// write escaped text, never HTML they sent.
func TestARepliesInTheCallsThread(t *testing.T) {
	ctx := context.Background()
	fake(t, liveCall())
	var w writes
	writing(t, &w, []string{conversationBusiness.ReasonCallInvite})
	if err := ReplyAsMember(ctx, &userModels.UserInfo{}, "s1", "<b>hi</b>"); err != nil {
		t.Fatal(err)
	}
	writing(t, &w, []string{"member"})
	if err := ReplyAsMember(ctx, &userModels.UserInfo{}, "s1", "on it"); err != nil {
		t.Fatal(err)
	}
	// A web address becomes a link, as the conversation's own messages have it.
	if err := ReplyAsMember(ctx, &userModels.UserInfo{}, "s1", "draft at https://example.com/pricing\nthanks"); err != nil {
		t.Fatal(err)
	}
	if got := w.posts[2].HTMLText; got != `<p>draft at <a target="_blank" rel="noopener noreferrer nofollow" href="https://example.com/pricing">https://example.com/pricing</a><br>thanks</p>` {
		t.Errorf("a link: %s", got)
	}
	if th, err := ForMember(ctx, &userModels.UserInfo{}, "s1", now, time.Time{}); err != nil || !th.CanReply {
		t.Errorf("a member reads it as open: %+v, %v", th, err)
	}
	if len(w.posts) != 3 || !w.posts[0].Outsider || w.posts[1].Outsider || w.posts[0].PostUuid != "p1" || w.posts[0].HTMLText != "<p>&lt;b&gt;hi&lt;/b&gt;</p>" {
		t.Errorf("the replies: %+v %+v", w.posts[0], w.posts[1])
	}

	// A DM's or a group's call: the reply goes to the chat's thread.
	s := liveCall()
	s.Resource = callStateBusiness.Ref{Kind: "dm", ID: "a b"}
	fake(t, s)
	w = writes{}
	writing(t, &w, []string{conversationBusiness.ReasonCallInvite})
	if err := ReplyAsMember(ctx, &userModels.UserInfo{}, "s1", "hi"); err != nil || len(w.chats) != 1 || !w.chats[0].Outsider || w.chats[0].ChatUuid != "p1" {
		t.Errorf("a DM's call: %+v, %v", w.chats, err)
	}
}

// A guest in a channel's call replies as the guest bot, under the name they
// joined the call with, whatever they send. In a DM's or a group's call a
// guest reads the thread but doesn't write in it (a DM's thread takes only
// its people as writers), and the thread says so.
func TestAGuestRepliesInAChannelsCallThread(t *testing.T) {
	ctx := context.Background()
	fake(t, liveCall())
	var w writes
	writing(t, &w, nil)
	if err := ReplyAsGuest(ctx, "Bearer gia", "hello"); err != nil || len(w.guests) != 1 || w.guests[0] != "p1 4f0c2a1e-3b7d-4c55-9a51-0d8b1f6a2c33 Gia: hello" {
		t.Fatalf("a guest: %v, %v", w.guests, err)
	}
	if th, err := ForGuest(ctx, "Bearer gia", now, time.Time{}); err != nil || !th.CanReply {
		t.Errorf("a channel's call reads as open to the guest's reply: %+v, %v", th, err)
	}
	s := liveCall()
	s.Resource = callStateBusiness.Ref{Kind: "group", ID: "0123456789abcdef0123456789abcdef"}
	fake(t, s)
	writing(t, &w, nil)
	if err := ReplyAsGuest(ctx, "Bearer gia", "hello"); !errors.Is(err, ErrReadOnly) {
		t.Errorf("a group's call: %v", err)
	}
	if th, err := ForGuest(ctx, "Bearer gia", now, time.Time{}); err != nil || th.CanReply {
		t.Errorf("a group's call reads as closed to the guest: %+v, %v", th, err)
	}
	if len(w.guests) != 1 {
		t.Errorf("wrote %v", w.guests)
	}
}

// While the people running the call hold its chat (calls meet-12), a guest
// in it reads the thread as closed and is told why, and their reply is
// refused in those words without a write. A member whose thread access is
// held (business/Conversation) is refused alike.
func TestAHeldChatTakesNoReplies(t *testing.T) {
	ctx := context.Background()
	fake(t, liveCall())
	var w writes
	writing(t, &w, nil)
	var askedFor []string
	seams.chatHeld = func(_ context.Context, s *callStateBusiness.Session, identity string) (bool, error) {
		askedFor = append(askedFor, s.PostUUID+" "+identity)
		return true, nil
	}
	if err := ReplyAsGuest(ctx, "Bearer gia", "hello"); !errors.Is(err, ErrChatHeld) {
		t.Errorf("a guest's reply while chat is held: %v", err)
	}
	if th, err := ForGuest(ctx, "Bearer gia", now, time.Time{}); err != nil || th.CanReply || !th.ChatHeld {
		t.Errorf("the guest reads it held: %+v, %v", th, err)
	}
	if len(askedFor) != 2 || askedFor[0] != "p1 guest-g1-gia" {
		t.Errorf("asked about %v", askedFor)
	}
	seams.resolve = func(context.Context, *userModels.UserInfo, conversationBusiness.Ref) (accessBusiness.Decision, error) {
		return accessBusiness.Decision{Level: conversationBusiness.LevelRead, Was: conversationBusiness.LevelReply, Reasons: []string{"member", conversationBusiness.ReasonCallChatHeld}}, nil
	}
	if th, err := ForMember(ctx, &userModels.UserInfo{}, "s1", now, time.Time{}); err != nil || th.CanReply || !th.ChatHeld {
		t.Errorf("the member reads it held: %+v, %v", th, err)
	}
	if err := ReplyAsMember(ctx, &userModels.UserInfo{}, "s1", "on it"); !errors.Is(err, ErrChatHeld) {
		t.Errorf("a member's reply while chat is held: %v", err)
	}
	if len(w.guests) != 0 || len(w.posts) != 0 {
		t.Errorf("wrote %v %v", w.guests, w.posts)
	}
}

// The channel's reply routes (a reply, an edit, a reaction) ask the thread's
// own decision whether a live call's chat holds the writer: held only for the
// hold's reason, nothing for a thread they can't read, and a read that fails
// is an error, never an open thread. A post with no live call is never held,
// and never takes the thread's whole decision (CODE-REVIEWER-47's nit).
func TestTheChannelsReplyRoutesAskTheHold(t *testing.T) {
	fake(t, liveCall())
	down := errors.New("postgres is down")
	held, open, archived, gone, broken, quiet := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	answers := map[string]struct {
		d   accessBusiness.Decision
		err error
	}{
		held:     {d: accessBusiness.Decision{Level: conversationBusiness.LevelRead, Was: conversationBusiness.LevelReply, Reasons: []string{"member", conversationBusiness.ReasonCallChatHeld}}},
		open:     {d: accessBusiness.Decision{Level: conversationBusiness.LevelReply, Reasons: []string{"member"}}},
		archived: {d: accessBusiness.Decision{Level: conversationBusiness.LevelRead, Reasons: []string{"member"}, Archived: true}},
		gone:     {err: conversationBusiness.ErrNotFound},
		broken:   {err: down},
	}
	asked := map[string]bool{}
	seams.livePost = func(_ context.Context, post uuid.UUID) (bool, error) { return post.String() != quiet, nil }
	seams.resolve = func(_ context.Context, _ *userModels.UserInfo, ref conversationBusiness.Ref) (accessBusiness.Decision, error) {
		if ref.Kind != conversationBusiness.KindThread {
			t.Errorf("asked about a %s", ref.Kind)
		}
		asked[ref.ID] = true
		a := answers[ref.ID]
		return a.d, a.err
	}
	for post, want := range map[string]bool{held: true, open: false, archived: false, gone: false, quiet: false, "not a post": false} {
		if got, err := ChatHeldFor(context.Background(), &userModels.UserInfo{}, post); err != nil || got != want {
			t.Errorf("%s: held %v, %v; want %v", post, got, err, want)
		}
	}
	if asked[quiet] {
		t.Error("a post with no live call took the thread's whole decision")
	}
	if _, err := ChatHeldFor(context.Background(), &userModels.UserInfo{}, broken); !errors.Is(err, down) {
		t.Errorf("a decision that fails: %v", err)
	}
	seams.livePost = func(context.Context, uuid.UUID) (bool, error) { return false, down }
	if _, err := ChatHeldFor(context.Background(), &userModels.UserInfo{}, held); !errors.Is(err, down) {
		t.Errorf("a live-post read that fails: %v", err)
	}
}

// Nothing is written once the call is over, or by someone who may read the
// thread but not reply (its channel archived, or someone in its DM gone), who
// reads it as closed.
func TestNoReplyOutsideTheCall(t *testing.T) {
	ctx := context.Background()
	s := liveCall()
	s.Live = false
	fake(t, s)
	var w writes
	writing(t, &w, []string{conversationBusiness.ReasonCallInvite})
	if err := ReplyAsMember(ctx, &userModels.UserInfo{}, "s1", "late"); !errors.Is(err, ErrNotFound) {
		t.Errorf("after the end: %v", err)
	}
	if err := ReplyAsGuest(ctx, "Bearer gia", "late"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a guest after the end: %v", err)
	}
	fake(t, liveCall())
	writing(t, &w, nil)
	seams.resolve = func(context.Context, *userModels.UserInfo, conversationBusiness.Ref) (accessBusiness.Decision, error) {
		return accessBusiness.Decision{Level: conversationBusiness.LevelRead}, nil
	}
	if err := ReplyAsMember(ctx, &userModels.UserInfo{}, "s1", "hi"); !errors.Is(err, ErrReadOnly) {
		t.Errorf("a reader: %v", err)
	}
	if th, err := ForMember(ctx, &userModels.UserInfo{}, "s1", now, time.Time{}); err != nil || th.CanReply {
		t.Errorf("a reader reads it as closed: %+v, %v", th, err)
	}
	if len(w.posts)+len(w.chats)+len(w.guests) != 0 {
		t.Errorf("wrote %+v", w)
	}
}

// A poll carries the time of the newest reply its reader has. With nothing
// newer in the thread's index, it answers ErrUnchanged without reading the
// thread; something newer, or an index that can't be read, reads it in full.
// A call that's over answers not found, never unchanged.
func TestAPollReadsTheThreadOnlyWhenSomethingIsNew(t *testing.T) {
	ctx := context.Background()
	reads, _ := fake(t, liveCall())
	since := now.Add(-5 * time.Minute)
	latest, latestErr := since, error(nil)
	seams.latest = func(_ context.Context, post string) (time.Time, error) {
		if post != "p1" {
			t.Errorf("asked the index for %q", post)
		}
		return latest, latestErr
	}
	if _, err := ForMember(ctx, &userModels.UserInfo{}, "s1", now, since); !errors.Is(err, ErrUnchanged) {
		t.Errorf("a member's poll with nothing new: %v", err)
	}
	if _, err := ForGuest(ctx, "Bearer gia", now, since); !errors.Is(err, ErrUnchanged) {
		t.Errorf("a guest's poll with nothing new: %v", err)
	}
	if len(*reads) != 0 {
		t.Errorf("read the thread for nothing new: %v", *reads)
	}
	// What a read answers as latest is what the next poll sends back.
	latest = since.Add(time.Second)
	if th, err := ForMember(ctx, &userModels.UserInfo{}, "s1", now, since); err != nil || th == nil || !th.Latest.Equal(latest) {
		t.Errorf("a member's poll with a new reply: %+v, %v", th, err)
	}
	if _, err := ForMember(ctx, &userModels.UserInfo{}, "s1", now, latest); !errors.Is(err, ErrUnchanged) {
		t.Errorf("the next poll: %v", err)
	}
	latest, latestErr = time.Time{}, errors.New("postgres is away")
	if th, err := ForGuest(ctx, "Bearer gia", now, since); err != nil || th == nil || !th.Latest.Equal(liveCall().StartedAt) {
		t.Errorf("a guest's poll with the index away answers the post's time: %+v, %v", th, err)
	}
	// A thread with no reply yet: its post's time, which the next poll finds unchanged.
	latest, latestErr = time.Time{}, nil
	th, err := ForGuest(ctx, "Bearer gia", now, time.Time{})
	if err != nil || !th.Latest.Equal(liveCall().StartedAt) {
		t.Fatalf("a thread with no reply: %+v, %v", th, err)
	}
	if _, err := ForGuest(ctx, "Bearer gia", now, th.Latest); !errors.Is(err, ErrUnchanged) {
		t.Errorf("polling a thread with no reply: %v", err)
	}
	if len(*reads) != 3 {
		t.Errorf("reads: %v", *reads)
	}
	over := liveCall()
	over.Live = false
	fake(t, over)
	seams.latest = func(context.Context, string) (time.Time, error) { return time.Time{}, nil }
	if _, err := ForMember(ctx, &userModels.UserInfo{}, "s1", now, since); !errors.Is(err, ErrNotFound) {
		t.Errorf("a poll after the call: %v", err)
	}
}
