package business

import (
	"context"
	"errors"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	conversationDomain "github.com/akashc777/OneCamp/domain/Conversation"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	models "github.com/akashc777/OneCamp/models/postgres/Conversation"
	"github.com/google/uuid"
)

// withCallInvites answers which posts are a live call's post (by room) and
// which call rooms the person was let into through an invite, for the test.
func withCallInvites(t *testing.T, posts map[uuid.UUID]string, rooms map[string]bool) *int {
	t.Helper()
	oldPosts, oldRooms := liveCallsOfPosts, roomsAdmittedTo
	t.Cleanup(func() { liveCallsOfPosts, roomsAdmittedTo = oldPosts, oldRooms })
	reads := 0
	liveCallsOfPosts = func(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
		reads++
		out := map[uuid.UUID]string{}
		for _, id := range ids {
			if room, ok := posts[id]; ok {
				out[id] = room
			}
		}
		return out, nil
	}
	roomsAdmittedTo = func(context.Context, uuid.UUID) (map[string]bool, error) {
		reads++
		return rooms, nil
	}
	return &reads
}

// A call's thread is open to someone let into the call through its invite,
// at reply, while the call goes on, and nothing else in its conversation is
// (calls HU13, agreed with CHAT-LEAD 21 Oct).
func TestACallsInviteeRepliesInItsThreadAndNowhereElse(t *testing.T) {
	ctx := ForCallThread(context.Background())
	jonas := accessBusiness.Subject{UserUUID: uuid.NewString()}
	callPost, otherThread := uuid.New(), uuid.New()

	// Let in: the call's thread at reply, for the invite; another thread of
	// the same conversation stays not found.
	reads := withCallInvites(t, map[uuid.UUID]string{callPost: "channel-1"}, map[string]bool{"channel-1": true})
	out := map[string]accessBusiness.Decision{}
	if err := callInvites(ctx, jonas, out, []string{callPost.String(), otherThread.String()}); err != nil {
		t.Fatal(err)
	}
	if d := out[callPost.String()]; d.Level != LevelReply || len(d.Reasons) != 1 || d.Reasons[0] != ReasonCallInvite {
		t.Errorf("the call's thread: %+v", d)
	}
	if _, found := out[otherThread.String()]; found {
		t.Errorf("another thread in the conversation: %+v", out[otherThread.String()])
	}
	if *reads != 2 {
		t.Errorf("%d reads; one for the posts' calls and one for the invites", *reads)
	}

	// Without an invite (never let in, or the invite revoked), and once the
	// call is over (ended, or the sweep ended it): not found.
	for name, c := range map[string]struct {
		posts map[uuid.UUID]string
		rooms map[string]bool
	}{
		"no invite":        {map[uuid.UUID]string{callPost: "channel-1"}, map[string]bool{}},
		"another call's":   {map[uuid.UUID]string{callPost: "channel-1"}, map[string]bool{"channel-2": true}},
		"the call is over": {map[uuid.UUID]string{}, map[string]bool{"channel-1": true}},
	} {
		withCallInvites(t, c.posts, c.rooms)
		out := map[string]accessBusiness.Decision{}
		if err := callInvites(ctx, jonas, out, []string{callPost.String()}); err != nil || len(out) != 0 {
			t.Errorf("%s: %+v, %v", name, out, err)
		}
	}

	// Someone a rule already decided keeps that decision, and no read is made.
	reads = withCallInvites(t, map[uuid.UUID]string{callPost: "channel-1"}, map[string]bool{"channel-1": true})
	member := map[string]accessBusiness.Decision{callPost.String(): {Level: LevelModerate, Was: LevelModerate, Reasons: []string{reasonModerator}}}
	if err := callInvites(ctx, jonas, member, []string{callPost.String()}); err != nil || member[callPost.String()].Level != LevelModerate || *reads != 0 {
		t.Errorf("a moderator: %+v, %v (%d reads)", member, err, *reads)
	}
}

// A call's invitee may join that call (the call kind), and only the rooms
// they were let into; a read that fails is an error, never a yes.
func TestACallsInviteeMayJoinOnlyItsCall(t *testing.T) {
	ctx := context.Background()
	jonas := accessBusiness.Subject{UserUUID: uuid.NewString()}
	withCallInvites(t, nil, map[string]bool{"channel-1": true})
	out := map[string]accessBusiness.Decision{}
	if err := callRoomInvites(ctx, jonas, out, []string{"channel-1", "channel-2"}); err != nil {
		t.Fatal(err)
	}
	if d := out["channel-1"]; d.Level != LevelPost || d.Reasons[0] != ReasonCallInvite {
		t.Errorf("the call they were let into: %+v", d)
	}
	if _, found := out["channel-2"]; found {
		t.Errorf("another call: %+v", out["channel-2"])
	}

	oldRooms := roomsAdmittedTo
	t.Cleanup(func() { roomsAdmittedTo = oldRooms })
	roomsAdmittedTo = func(context.Context, uuid.UUID) (map[string]bool, error) { return nil, errors.New("down") }
	if err := callRoomInvites(ctx, jonas, map[string]accessBusiness.Decision{}, []string{"channel-1"}); err == nil {
		t.Error("a failed read answered")
	}
}

// An invite lets no agent into a call or its thread: not one on its own, and
// not one acting for someone who was let in. Nothing is decided, and nothing
// is read (CHAT-LEAD-2's review, 21 Oct).
func TestAnInviteLetsNoAgentIn(t *testing.T) {
	ctx := ForCallThread(context.Background())
	jonas, callPost := uuid.NewString(), uuid.New()
	posts, rooms := map[uuid.UUID]string{callPost: "channel-1"}, map[string]bool{"channel-1": true}
	for name, who := range map[string]accessBusiness.Subject{
		"an agent":                  {AgentUUID: uuid.NewString()},
		"an agent acting for Jonas": {UserUUID: jonas, AgentUUID: uuid.NewString()},
	} {
		reads := withCallInvites(t, posts, rooms)
		thread, call := map[string]accessBusiness.Decision{}, map[string]accessBusiness.Decision{}
		if err := callInvites(ctx, who, thread, []string{callPost.String()}); err != nil || len(thread) != 0 {
			t.Errorf("%s, the call's thread: %+v, %v", name, thread, err)
		}
		if err := callRoomInvites(ctx, who, call, []string{"channel-1"}); err != nil || len(call) != 0 {
			t.Errorf("%s, the call: %+v, %v", name, call, err)
		}
		if *reads != 0 {
			t.Errorf("%s: %d reads", name, *reads)
		}
	}

	// Jonas himself, with the same invite, is let in.
	withCallInvites(t, posts, rooms)
	thread, call := map[string]accessBusiness.Decision{}, map[string]accessBusiness.Decision{}
	me := accessBusiness.Subject{UserUUID: jonas}
	if err := callInvites(ctx, me, thread, []string{callPost.String()}); err != nil || thread[callPost.String()].Level != LevelReply {
		t.Errorf("Jonas, the call's thread: %+v, %v", thread, err)
	}
	if err := callRoomInvites(ctx, me, call, []string{"channel-1"}); err != nil || call["channel-1"].Level != LevelPost {
		t.Errorf("Jonas, the call: %+v, %v", call, err)
	}
}

// withGraphAccount makes decisions read the graph, where the person's account
// is the one given: nil is someone deactivated.
func withGraphAccount(t *testing.T, account *dgraphStruct.DgraphUser) {
	t.Helper()
	swapVar(t, &accessSource, func(context.Context) conversationDomain.Choice { return conversationDomain.Choice{Index: false} })
	swapVar(t, &activeGraphUser, func(context.Context, string) (*dgraphStruct.DgraphUser, error) { return account, nil })
}

// An invite someone deactivated still holds lets them into no call and no
// call's thread, on either path: the graph's, which has no uid for them, and
// the index's, which reads them inactive whatever the graph says (CHAT-LEAD-2's
// review, 21 Oct).
func TestAnInviteLetsNobodyDeactivatedIn(t *testing.T) {
	ctx := context.Background()
	me := uuid.New()
	who := accessBusiness.Subject{UserUUID: me.String()}
	callPost, channelRoom, meeting := uuid.New(), uuid.NewString(), "meet-"+uuid.NewString()
	posts, rooms := map[uuid.UUID]string{callPost: channelRoom}, map[string]bool{channelRoom: true, meeting: true}

	// The graph's path.
	withGraphAccount(t, nil)
	reads := withCallInvites(t, posts, rooms)
	if got, err := (threadRules{}).DecideMany(ctx, who, []string{callPost.String()}); err != nil || len(got) != 0 {
		t.Errorf("the graph, the call's thread: %+v, %v", got, err)
	}
	if got, err := (callRules{}).DecideMany(ctx, who, []string{channelRoom, meeting}); err != nil || len(got) != 0 {
		t.Errorf("the graph, the calls: %+v, %v", got, err)
	}
	if *reads != 0 {
		t.Errorf("the graph: %d reads", *reads)
	}
	// The same invite, held by someone active, lets them in.
	withGraphAccount(t, &dgraphStruct.DgraphUser{Uid: "0x9", Uuid: me.String()})
	if got, err := (callRules{}).DecideMany(ctx, who, []string{meeting}); err != nil || got[meeting].Level != LevelPost {
		t.Errorf("the graph, someone active: %+v, %v", got, err)
	}

	// The index's path.
	m := newIndexMode(t)
	m.people[me] = models.Person{Active: false}
	reads = withCallInvites(t, posts, rooms)
	if got, err := (threadRules{}).DecideMany(ctx, who, []string{callPost.String()}); err != nil || len(got) != 0 {
		t.Errorf("the index, the call's thread: %+v, %v", got, err)
	}
	if got, err := (callRules{}).DecideMany(ctx, who, []string{channelRoom, meeting}); err != nil || len(got) != 0 {
		t.Errorf("the index, the calls: %+v, %v", got, err)
	}
	if *reads != 0 {
		t.Errorf("the index: %d reads", *reads)
	}
	m.people[me] = models.Person{Active: true}
	if got, err := (callRules{}).DecideMany(ctx, who, []string{meeting}); err != nil || got[meeting].Level != LevelPost {
		t.Errorf("the index, someone active: %+v, %v", got, err)
	}
}

// The thread opens to an invitee only through the call's thread surface,
// whose handlers mark the context: asked anywhere else, the invite opens
// nothing and reads nothing, as for a thread that isn't there (calls
// CALLS-3b, CHAT-LEAD-2's condition).
func TestAnInviteOpensTheThreadOnlyToTheCallsSurface(t *testing.T) {
	jonas := accessBusiness.Subject{UserUUID: uuid.NewString()}
	callPost := uuid.New()
	reads := withCallInvites(t, map[uuid.UUID]string{callPost: "channel-1"}, map[string]bool{"channel-1": true})
	out := map[string]accessBusiness.Decision{}
	if err := callInvites(context.Background(), jonas, out, []string{callPost.String()}); err != nil || len(out) != 0 || *reads != 0 {
		t.Errorf("unmarked: %+v, %v (%d reads)", out, err, *reads)
	}
	if err := callInvites(ForCallThread(context.Background()), jonas, out, []string{callPost.String()}); err != nil || out[callPost.String()].Level != LevelReply {
		t.Errorf("the call's surface: %+v, %v", out, err)
	}
}
