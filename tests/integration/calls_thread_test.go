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

	callControlBusiness "github.com/akashc777/OneCamp/business/CallControl"
	callInviteBusiness "github.com/akashc777/OneCamp/business/CallInvite"
	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	callThreadBusiness "github.com/akashc777/OneCamp/business/CallThread"
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
