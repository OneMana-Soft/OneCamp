//go:build integration

package integration_test

// A leave ends a call only when nobody is left in it (V-85, 29 Oct 2026). A
// person whose page closed without Leave comes back on another connection (a
// reload, their phone, a second device) while the call server still holds the
// first one: LiveKit puts the first out for the second, under the same
// identity, and says it left after they are back. The server took that late
// leave as the person's, found nobody else, and ended the call under them and
// deleted its room: the guests at an event call's door knocked into a dead
// call. A connection is told apart by its SID, and only a person keeps a call
// going: not the agent, a recorder or a hidden watcher.
//
// A real call server holds the connections. LiveKit's participant_left reaches
// the server's own webhook handler, signed as LiveKit signs it (lkWebhook), and
// the call is read through its own route, /calls/current, as its host's screen
// reads it.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"

	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	callsController "github.com/akashc777/OneCamp/controllers/Calls"
	"github.com/akashc777/OneCamp/initializers/livekitInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/tests/integration"
)

// leftAs tells the server's webhook handler, as LiveKit does, that the
// connection p (as LiveKit last listed it) left the room.
func leftAs(t *testing.T, lk *integration.LiveKitEnv, room, roomSID string, p *livekit.ParticipantInfo) {
	t.Helper()
	p.State = livekit.ParticipantInfo_DISCONNECTED
	lkWebhook(t, lk, &livekit.WebhookEvent{Event: "participant_left", Room: &livekit.Room{Name: room, Sid: roomSID}, Participant: p})
	settle(t)
}

// readCallAs reads a channel's call through its own route, as who: the
// answer's code, whether the call is live, the call, and who's part in it.
func readCallAs(t *testing.T, room string, who *dgraphStruct.DgraphUser) (int, bool, *callStateBusiness.Session, string) {
	t.Helper()
	code, body := callRead(t, callsController.CurrentCall, http.MethodGet, "/calls/current?kind=channel&id="+room, who, "", "")
	var got struct {
		Live    bool                       `json:"live"`
		Session *callStateBusiness.Session `json:"session"`
		You     struct {
			Role string `json:"role"`
		} `json:"you"`
	}
	if err := json.Unmarshal(body["data"], &got); err != nil {
		t.Fatalf("the call read as %s: %d %s: %v", who.UserName, code, body["data"], err)
	}
	return code, got.Live, got.Session, got.You.Role
}

// inCallAs is identity's entry in a call, or nil.
func inCallAs(s *callStateBusiness.Session, identity string) *callStateBusiness.Person {
	if s == nil {
		return nil
	}
	for i := range s.People {
		if s.People[i].Identity == identity {
			return &s.People[i]
		}
	}
	return nil
}

// roomDeleted waits for the call server to have deleted room.
func roomDeleted(t *testing.T, ctx context.Context, room string) {
	t.Helper()
	eventually(t, room+" to be deleted", func() bool {
		_, err := livekitInit.LiveKitService.GetRoom(ctx, room)
		return errors.Is(err, livekitInit.ErrRoomNotFound)
	})
}

func TestALeaveEndsACallOnlyWhenNobodyIsLeftInIt(t *testing.T) {
	ctx, pg, dg, lk := breakoutEnv(t)

	t.Run("a late leave from the connection she replaced ends nothing; her last leave ends the call", func(t *testing.T) {
		room := uuid.NewString()
		people := seedChannel(t, pg, dg, room, "maya", "outsider")
		maya := people["maya"]
		s, _ := enterCall(t, ctx, lk, room, people, "maya")
		r, err := livekitInit.LiveKitService.GetRoom(ctx, room)
		if err != nil {
			t.Fatal(err)
		}
		first, err := livekitInit.LiveKitService.Participant(ctx, room, maya.Uid)
		if err != nil || first == nil {
			t.Fatalf("Maya's first connection: %+v, %v", first, err)
		}

		// Her laptop's page closes without Leave, and she comes back on her
		// phone, alone in the call: the call server puts the first connection
		// out for the second, under the same identity.
		token, err := livekitInit.LiveKitService.GenerateToken(room, maya.Uid, title("maya"), false, "", livekitInit.CallPublishSources)
		if err != nil {
			t.Fatal(err)
		}
		phone := join(t, lk, token)
		var second *livekit.ParticipantInfo
		eventually(t, "Maya's phone to take her first connection's place", func() bool {
			second, err = livekitInit.LiveKitService.Participant(ctx, room, maya.Uid)
			return err == nil && second != nil && second.Sid != first.Sid
		})
		joinSettled(t, room, maya.Uid, phone)
		connected(t, ctx, room, maya.Uid)

		// (a) The first connection's leave comes late, after she is back.
		leftAs(t, lk, room, r.Sid, first)
		if _, err := livekitInit.LiveKitService.GetRoom(ctx, room); err != nil {
			t.Fatalf("the call was ended under a person who is still in it: its room is gone (%v)", err)
		}
		code, live, now, role := readCallAs(t, room, maya)
		if code != http.StatusOK || !live || now == nil || now.ID != s.ID {
			t.Fatalf("the call was ended under a person who is still in it: %d, live %v, call %+v (want %s)", code, live, now, s.ID)
		}
		if me := inCallAs(now, maya.Uid); me == nil || !me.In || me.LeftAt != nil || role != "host" {
			t.Errorf("the late leave was taken as hers: %+v, her part %q (want in the call, host)", me, role)
		}
		if p, _ := livekitInit.LiveKitService.Participant(ctx, room, maya.Uid); p == nil || p.Sid != second.Sid {
			t.Errorf("her phone's connection was put out: %+v", p)
		}

		// (b) Her phone leaves too: nobody is left, so the call ends and its
		// room is deleted.
		last, err := livekitInit.LiveKitService.Participant(ctx, room, maya.Uid)
		if err != nil || last == nil {
			t.Fatalf("Maya's phone: %+v, %v", last, err)
		}
		phone.room.Disconnect()
		eventually(t, "Maya to be gone from "+room, func() bool {
			p, _ := livekitInit.LiveKitService.Participant(ctx, room, maya.Uid)
			return p == nil
		})
		leftAs(t, lk, room, r.Sid, last)
		if ended, err := callStateBusiness.Get(ctx, s.ID); err != nil || ended == nil || ended.Live {
			t.Errorf("its last person left, and the call goes on: %+v (%v)", ended, err)
		}
		if code, live, _, _ := readCallAs(t, room, maya); code != http.StatusOK || live {
			t.Errorf("the ended call still reads live: %d, live %v", code, live)
		}
		roomDeleted(t, ctx, room)
	})

	t.Run("a hidden watcher doesn't keep a call going", func(t *testing.T) {
		room := uuid.NewString()
		people := seedChannel(t, pg, dg, room, "omar", "passerby")
		omar := people["omar"]
		s, in := enterCall(t, ctx, lk, room, people, "omar")
		r, err := livekitInit.LiveKitService.GetRoom(ctx, room)
		if err != nil {
			t.Fatal(err)
		}
		watcher := "watcher-" + uuid.NewString()[:8]
		token, err := auth.NewAccessToken(lk.Key, lk.Secret).SetIdentity(watcher).SetValidFor(time.Hour).
			SetVideoGrant(&auth.VideoGrant{RoomJoin: true, Room: room, Hidden: true}).ToJWT()
		if err != nil {
			t.Fatal(err)
		}
		join(t, lk, token)
		eventually(t, "the hidden watcher to be listed in "+room, func() bool {
			p, err := livekitInit.LiveKitService.Participant(ctx, room, watcher)
			return err == nil && p != nil && p.GetPermission().GetHidden()
		})

		// (c) Omar, the call's last person, leaves while the watcher is still
		// connected: the call ends, and its room is deleted.
		gone, err := livekitInit.LiveKitService.Participant(ctx, room, omar.Uid)
		if err != nil || gone == nil {
			t.Fatalf("Omar's connection: %+v, %v", gone, err)
		}
		in["omar"].room.Disconnect()
		eventually(t, "Omar to be gone from "+room, func() bool {
			p, _ := livekitInit.LiveKitService.Participant(ctx, room, omar.Uid)
			return p == nil
		})
		leftAs(t, lk, room, r.Sid, gone)
		if ended, err := callStateBusiness.Get(ctx, s.ID); err != nil || ended == nil || ended.Live {
			t.Errorf("a hidden watcher kept the call going after its last person left: %+v (%v)", ended, err)
		}
		roomDeleted(t, ctx, room)
	})
}
