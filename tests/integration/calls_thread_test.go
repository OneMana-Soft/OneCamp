//go:build integration

package integration_test

// The call's thread surface (calls CALLS-3b), on a real call server, graph and
// database: a channel's call writes its post as its starter when it starts;
// while it goes on, a member, someone let in by the invite and a guest read
// and write the call's thread through the surface; the invitee's reply leaves
// them out of the thread's people (CHAT-LEAD-2's condition 2); the call's end
// closes the surface and gives the post its length.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	callControlBusiness "github.com/akashc777/OneCamp/business/CallControl"
	callInviteBusiness "github.com/akashc777/OneCamp/business/CallInvite"
	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	callThreadBusiness "github.com/akashc777/OneCamp/business/CallThread"
	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	_ "github.com/akashc777/OneCamp/business/Huddle" // the call's post, linked as the server links it
	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	callStateModel "github.com/akashc777/OneCamp/models/postgres/CallState"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestCallThreadIsWrittenInByEveryoneInTheCall(t *testing.T) {
	ctx := context.Background()
	pg := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, pg.DSN); err != nil {
		t.Fatal(err)
	}
	lk := integration.SetupLiveKit(t)
	integration.SetupRedis(t)
	dg := integration.SetupDgraph(t)
	integration.StubMqttClient(t)
	if err := settingsBusiness.SetGuestAccessEnabled(true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = settingsBusiness.SetGuestAccessEnabled(false) })
	room := uuid.NewString() // a channel's call
	people := seedChannel(t, pg, dg, room, "maya", "ben", "jonas")
	maya, jonas := people["maya"], people["jonas"]
	// The channel is public in the graph too, as its row says, so its threads
	// read for anyone signed in, Jonas among them (V-1087; CR-372 F1).
	dg.Mutate(t, map[string]any{"uid": channelUID(t, dg, room), "ch_private": false})
	info := func(u *dgraphStruct.DgraphUser) *userModels.UserInfo {
		i := &userModels.UserInfo{UserDgraphInfo: *u}
		i.UserPostgresInfo.Id = uuid.MustParse(u.Uuid)
		return i
	}

	// Maya starts the channel's call: its post is written, as hers, and the call takes it.
	mayaToken, _, err := liveKitBusiness.CreateRoomAndGetToken(ctx, room, maya, false, "")
	if err != nil {
		t.Fatal(err)
	}
	joinSettled(t, room, maya.Uid, join(t, lk, mayaToken))
	connected(t, ctx, room, maya.Uid)
	var s *callStateBusiness.Session
	eventually(t, "the call to take its post", func() bool {
		s, err = callStateBusiness.Current(ctx, callStateBusiness.Ref{Kind: "channel", ID: room})
		return err == nil && s != nil && s.PostUUID != ""
	})
	post := s.PostUUID
	var card struct {
		P []struct {
			Kind   string                   `json:"post_kind"`
			System string                   `json:"post_system"`
			By     *dgraphStruct.DgraphUser `json:"post_by"`
		} `json:"p"`
	}
	readCard := func() {
		t.Helper()
		card.P = nil
		if err := json.Unmarshal(dg.Query(t, fmt.Sprintf(`{ p(func: eq(post_uuid, %q)) { post_kind post_system post_by { uid } } }`, post)), &card); err != nil || len(card.P) != 1 {
			t.Fatalf("the call's post: %+v, %v", card, err)
		}
	}
	readCard()
	if card.P[0].Kind != "huddle" || card.P[0].By == nil || card.P[0].By.Uid != maya.Uid {
		t.Errorf("the call's post: %+v", card.P[0])
	}

	// Jonas, not in the channel, comes in by Maya's invite; Gia, a guest, too.
	link, err := callInviteBusiness.Invite(ctx, room, maya)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := callInviteBusiness.Enter(ctx, callInviteBusiness.Door{Token: link.GuestToken}, jonas, "")
	if err != nil || entry.Token == "" {
		t.Fatalf("Jonas at the invite: %+v, %v", entry, err)
	}
	joinSettled(t, room, jonas.Uid, join(t, lk, entry.Token))
	connected(t, ctx, room, jonas.Uid)
	door, err := guestBusiness.OpenDoor(ctx, link.GuestToken)
	if err != nil {
		t.Fatal(err)
	}
	guestToken, _, err := guestBusiness.IssueGuestMeetingToken(ctx, door, "Gia", "")
	if err != nil {
		t.Fatal(err)
	}
	join(t, lk, guestToken)
	gia := ""
	eventually(t, "Gia to be in the call", func() bool {
		for _, id := range stillIn(ctx, room) {
			if len(id) > 6 && id[:6] == "guest-" {
				gia = id
				return true
			}
		}
		return false
	})
	connected(t, ctx, room, gia)
	giaAuth := "Bearer " + guestToken

	// Through any other route the public channel's thread reads for Jonas and
	// no more; on the call's surface, his invite lets him reply (CR-372 F1).
	thread := conversationBusiness.Ref{Kind: conversationBusiness.KindThread, ID: post}
	if d, err := conversationBusiness.Resolve(ctx, info(jonas), thread); err != nil || d.Level != conversationBusiness.LevelRead {
		t.Errorf("the call's thread through a member route: %+v, %v", d, err)
	}
	if d, err := conversationBusiness.Resolve(conversationBusiness.ForCallThread(ctx), info(jonas), thread); err != nil ||
		d.Level != conversationBusiness.LevelReply || len(d.Reasons) != 1 || d.Reasons[0] != conversationBusiness.ReasonCallInvite {
		t.Errorf("the call's thread, on its surface: %+v, %v", d, err)
	}

	// Jonas writes first, as himself and outside the conversation: the thread
	// has his reply, and he isn't one of its people.
	if err := callThreadBusiness.ReplyAsMember(ctx, info(jonas), s.ID, "Thanks for having me"); err != nil {
		t.Fatalf("Jonas's reply: %v", err)
	}
	eventually(t, "the thread to have Jonas's reply", func() bool {
		var at *time.Time
		err := pg.PG.QueryRow(`SELECT last_reply_at FROM threads WHERE id = $1`, post).Scan(&at)
		return err == nil && at != nil
	})
	if err := callThreadBusiness.ReplyAsMember(ctx, info(maya), s.ID, "Agenda's in the doc"); err != nil {
		t.Fatalf("Maya's reply: %v", err)
	}
	if err := callThreadBusiness.ReplyAsGuest(ctx, giaAuth, "Hi all"); err != nil {
		t.Fatalf("Gia's reply: %v", err)
	}
	role := func(u *dgraphStruct.DgraphUser) string {
		var r string
		if err := pg.PG.QueryRow(`SELECT role FROM thread_participants WHERE thread_id = $1 AND user_uuid = $2`, post, u.Uuid).Scan(&r); err != nil {
			return ""
		}
		return r
	}
	eventually(t, "Maya to be one of the thread's people", func() bool { return role(maya) != "" })
	if r := role(jonas); r != "" {
		t.Errorf("Jonas is one of the thread's people, as %q", r)
	}

	// Everyone in the call reads the same: the three replies, by name, and may write.
	for who, read := range map[string]func() (*guestBusiness.CallThread, error){
		"Maya": func() (*guestBusiness.CallThread, error) {
			return callThreadBusiness.ForMember(ctx, info(maya), s.ID, time.Now(), time.Time{})
		},
		"Jonas": func() (*guestBusiness.CallThread, error) {
			return callThreadBusiness.ForMember(ctx, info(jonas), s.ID, time.Now(), time.Time{})
		},
		"Gia": func() (*guestBusiness.CallThread, error) {
			return callThreadBusiness.ForGuest(ctx, giaAuth, time.Now(), time.Time{})
		},
	} {
		th, err := read()
		if err != nil || th.Post.ID != post || !th.CanReply || len(th.Replies) != 3 {
			t.Errorf("%s reads %+v, %v", who, th, err)
			continue
		}
		// Gia's is a guest's, by the name she gave; the members' are told
		// apart by a key, never by name or id.
		if r := th.Replies[2]; r.Author != "Gia" || !r.Guest || r.AuthorKey != "" || r.Text != "Hi all" {
			t.Errorf("%s reads Gia's reply as %+v", who, r)
		}
		if a, b := th.Replies[0].AuthorKey, th.Replies[1].AuthorKey; a == "" || b == "" || a == b {
			t.Errorf("%s reads the members' keys as %q and %q", who, a, b)
		}
		if th.Replies[0].Text != "Thanks for having me" || th.Replies[1].Text != "Agenda's in the doc" {
			t.Errorf("%s reads %+v", who, th.Replies)
		}
	}
	// Polls settle to carrying nothing. Gia's second reply, held back from
	// telling anyone, is still the thread's latest, so the next poll reads it.
	var latest time.Time
	eventually(t, "polls to carry nothing", func() bool {
		th, err := callThreadBusiness.ForGuest(ctx, giaAuth, time.Now(), latest)
		if errors.Is(err, callThreadBusiness.ErrUnchanged) {
			return true
		}
		if err == nil {
			latest = th.Latest
		}
		return false
	})
	if err := callThreadBusiness.ReplyAsGuest(ctx, giaAuth, "One more thing"); err != nil {
		t.Fatalf("Gia's second reply: %v", err)
	}
	eventually(t, "Jonas's poll to read Gia's second reply", func() bool {
		th, err := callThreadBusiness.ForMember(ctx, info(jonas), s.ID, time.Now(), latest)
		return err == nil && len(th.Replies) == 4 && th.Replies[3].Text == "One more thing"
	})

	// A call that isn't there takes nothing.
	if err := callThreadBusiness.ReplyAsMember(ctx, info(jonas), uuid.NewString(), "hi"); !errors.Is(err, callThreadBusiness.ErrNotFound) {
		t.Errorf("a call that isn't there: %v", err)
	}

	// The call ends: the surface answers nothing, and the post has its length.
	callControlBusiness.OnEnded(ctx, room, s.RoomSID, callStateModel.EndedByHost)
	if _, err := callThreadBusiness.ForMember(ctx, info(maya), s.ID, time.Now(), time.Time{}); !errors.Is(err, callThreadBusiness.ErrNotFound) {
		t.Errorf("Maya's read after the call: %v", err)
	}
	if err := callThreadBusiness.ReplyAsMember(ctx, info(jonas), s.ID, "late"); !errors.Is(err, callThreadBusiness.ErrNotFound) {
		t.Errorf("Jonas's reply after the call: %v", err)
	}
	if err := callThreadBusiness.ReplyAsGuest(ctx, giaAuth, "late"); !errors.Is(err, callThreadBusiness.ErrNotFound) {
		t.Errorf("Gia's reply after the call: %v", err)
	}
	eventually(t, "the call's post to have its end", func() bool {
		readCard()
		var facts struct {
			Ended *time.Time `json:"ended"`
		}
		return json.Unmarshal([]byte(card.P[0].System), &facts) == nil && facts.Ended != nil
	})
}

// inviteCallEnv is a call's world: Postgres, LiveKit, Redis and the graph,
// with guest access on, as a call's invite is a guest link.
func inviteCallEnv(t *testing.T) (context.Context, *integration.Env, *integration.LiveKitEnv, *integration.DgraphEnv) {
	t.Helper()
	ctx := context.Background()
	pg := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, pg.DSN); err != nil {
		t.Fatal(err)
	}
	lk := integration.SetupLiveKit(t)
	integration.SetupRedis(t)
	dg := integration.SetupDgraph(t)
	integration.StubMqttClient(t)
	if err := settingsBusiness.SetGuestAccessEnabled(true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = settingsBusiness.SetGuestAccessEnabled(false) })
	return ctx, pg, lk, dg
}

// asPerson is u as a request's person.
func asPerson(u *dgraphStruct.DgraphUser) *userModels.UserInfo {
	i := &userModels.UserInfo{UserDgraphInfo: *u}
	i.UserPostgresInfo.Id = uuid.MustParse(u.Uuid)
	return i
}

// startCall has host start the call in room, the conversation ref's, and
// answers its session once the call has its post.
func startCall(t *testing.T, ctx context.Context, lk *integration.LiveKitEnv, room string, ref callStateBusiness.Ref, host *dgraphStruct.DgraphUser) *callStateBusiness.Session {
	t.Helper()
	token, _, err := liveKitBusiness.CreateRoomAndGetToken(ctx, room, host, false, "")
	if err != nil {
		t.Fatal(err)
	}
	joinSettled(t, room, host.Uid, join(t, lk, token))
	connected(t, ctx, room, host.Uid)
	var s *callStateBusiness.Session
	eventually(t, "the call to take its post", func() bool {
		s, err = callStateBusiness.Current(ctx, ref)
		return err == nil && s != nil && s.PostUUID != ""
	})
	return s
}

// invitee is someone outside a live call's conversation, as they stand in
// its thread on the call's surface.
type invitee struct {
	t   *testing.T
	ctx context.Context
	s   *callStateBusiness.Session
	who *dgraphStruct.DgraphUser
}

func (c invitee) onSurface() (accessBusiness.Decision, error) {
	return conversationBusiness.Resolve(conversationBusiness.ForCallThread(c.ctx), asPerson(c.who),
		conversationBusiness.Ref{Kind: conversationBusiness.KindThread, ID: c.s.PostUUID})
}

// letIn has host let c's person into the call in room by its invite.
func (c invitee) letIn(lk *integration.LiveKitEnv, room string, host *dgraphStruct.DgraphUser) {
	c.t.Helper()
	link, err := callInviteBusiness.Invite(c.ctx, room, host)
	if err != nil {
		c.t.Fatal(err)
	}
	entry, err := callInviteBusiness.Enter(c.ctx, callInviteBusiness.Door{Token: link.GuestToken}, c.who, "")
	if err != nil || entry.Token == "" {
		c.t.Fatalf("%s at the invite: %+v, %v", c.who.UserName, entry, err)
	}
	joinSettled(c.t, room, c.who.Uid, join(c.t, lk, entry.Token))
	connected(c.t, c.ctx, room, c.who.Uid)
}

// mayReply checks c's person replies in the call's thread, unheld.
func (c invitee) mayReply(when string) {
	c.t.Helper()
	if d, err := c.onSurface(); err != nil || d.Level != conversationBusiness.LevelReply || d.Archived || d.Deleted {
		c.t.Errorf("the call's thread, %s: %+v, %v", when, d, err)
	}
}

// heldAt checks c's person, in by the invite, reads the call's thread and no
// more, told why, and their reply is refused.
func (c invitee) heldAt(when string, archived, deleted bool) {
	c.t.Helper()
	d, err := c.onSurface()
	if err != nil || d.Level != conversationBusiness.LevelRead || d.Archived != archived || d.Deleted != deleted ||
		len(d.Reasons) != 1 || d.Reasons[0] != conversationBusiness.ReasonCallInvite {
		c.t.Errorf("the call's thread, %s: %+v, %v", when, d, err)
	}
	if err := callThreadBusiness.ReplyAsMember(c.ctx, asPerson(c.who), c.s.ID, "still here"); !errors.Is(err, callThreadBusiness.ErrReadOnly) {
		c.t.Errorf("%s's reply, %s: %v", c.who.UserName, when, err)
	}
}

// An invite opens no more of a call's thread than the thread's holds allow
// (V-1138): mid-call, an archived channel or a deleted root holds Jonas, let
// in by the invite, at read, told why, as it holds the channel's own members.
// Before he's let in, the public channel's thread reads for him on the call's
// surface and takes no reply (CR-380 N1).
func TestAnInviteOpensNoMoreOfACallsThreadThanItsHoldsAllow(t *testing.T) {
	ctx, pg, lk, dg := inviteCallEnv(t)
	room := uuid.NewString() // a channel's call
	people := seedChannel(t, pg, dg, room, "maya", "ben", "jonas")
	maya, jonas := people["maya"], people["jonas"]
	channel := channelUID(t, dg, room)
	dg.Mutate(t, map[string]any{"uid": channel, "ch_private": false})
	s := startCall(t, ctx, lk, room, callStateBusiness.Ref{Kind: "channel", ID: room}, maya)
	c := invitee{t: t, ctx: ctx, s: s, who: jonas}

	// Not let in, Jonas reads the public channel's call thread on its surface,
	// as anyone signed in reads it, and his reply is refused (CR-380 N1).
	if d, err := c.onSurface(); err != nil || d.Level != conversationBusiness.LevelRead {
		t.Errorf("the call's thread, not let in: %+v, %v", d, err)
	}
	if err := callThreadBusiness.ReplyAsMember(ctx, asPerson(jonas), s.ID, "hi"); !errors.Is(err, callThreadBusiness.ErrReadOnly) {
		t.Errorf("Jonas's reply, not let in: %v", err)
	}

	// Let in by Maya's invite, he may reply.
	c.letIn(lk, room, maya)
	c.mayReply("let in")

	// The channel is archived mid-call (a deletion time, as archiving leaves
	// it): read only, as for its members; restored, he replies again.
	const zero = "0001-01-01T00:00:00Z"
	dg.Mutate(t, map[string]any{"uid": channel, "ch_deleted_at": time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)})
	c.heldAt("its channel archived", true, false)
	dg.Mutate(t, map[string]any{"uid": channel, "ch_deleted_at": zero})
	c.mayReply("its channel restored")

	// The channel turns private, so its thread is nothing to Jonas but for
	// the invite; then the call's post is deleted (a deletion time, as
	// deleting one leaves it): read only.
	dg.Mutate(t, map[string]any{"uid": channel, "ch_private": true})
	c.mayReply("its channel private")
	dg.Mutate(t, map[string]any{"uid": graphUID(t, dg, "post_uuid", s.PostUUID), "post_deleted_at": time.Now().UTC().Format(time.RFC3339)})
	c.heldAt("its post deleted", false, true)
}

// A DM's call holds its invitee as a channel's does (CR-381 F1): Jonas,
// outside Maya and Ben's DM, is let into its call by the invite and replies
// in the call's thread; once the thread's root, the call's message in the
// DM, is deleted (a deletion time, as deleting one leaves it), he reads it
// and no more, told why, and his reply is refused.
func TestAnInviteOpensNoMoreOfADMCallsThreadThanItsDeletedRootAllows(t *testing.T) {
	ctx, pg, lk, dg := inviteCallEnv(t)
	dm, people := seedRingConversation(t, pg, dg, false, ringMember{name: "maya"}, ringMember{name: "ben"}, ringMember{name: "jonas", away: true})
	maya, jonas := people["maya"], people["jonas"]
	s := startCall(t, ctx, lk, dm, callStateBusiness.Ref{Kind: "dm", ID: dm}, maya)
	c := invitee{t: t, ctx: ctx, s: s, who: jonas}

	c.letIn(lk, dm, maya)
	c.mayReply("let in")
	dg.Mutate(t, map[string]any{"uid": graphUID(t, dg, "chat_uuid", s.PostUUID), "chat_deleted_at": time.Now().UTC().Format(time.RFC3339)})
	c.heldAt("its message deleted", false, true)
}
