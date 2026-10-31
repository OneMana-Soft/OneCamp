//go:build integration

package integration_test

// One "is this a person" rule (V-89, 29 Oct 2026). A call's dot, on a
// channel's header and in the sidebar, asked LiveKit whether anyone but the
// transcription agent was in the call's room: a recorder or a hidden watcher
// alone lit a call nobody was in, while the rule that ends a call counted
// people only. And a call whose people had all gone to its breakout rooms read
// as over: its own room held nobody, or only its recorder, or was closed.
// Now a call is lit while a person is in any of its rooms, by the rule that
// ends it.
//
// A real call server holds the connections, a recorder and a watcher
// connecting as LiveKit's own do (a recorder's kind, hidden). The dot is read
// as the channel's header reads it and through the sidebar's own route.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"

	callControlBusiness "github.com/akashc777/OneCamp/business/CallControl"
	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	userController "github.com/akashc777/OneCamp/controllers/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/livekitInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// notAPerson connects someone to a room who isn't a person in the call: a
// recorder (a recorder's kind, hidden, as LiveKit's egress joins) or a hidden
// watcher. It answers once LiveKit lists them.
func notAPerson(t *testing.T, ctx context.Context, lk *integration.LiveKitEnv, room, identity string, kind livekit.ParticipantInfo_Kind) *joined {
	t.Helper()
	grant := &auth.VideoGrant{RoomJoin: true, Room: room, Hidden: true, Recorder: kind == livekit.ParticipantInfo_EGRESS}
	token, err := auth.NewAccessToken(lk.Key, lk.Secret).SetIdentity(identity).SetKind(kind).SetValidFor(time.Hour).SetVideoGrant(grant).ToJWT()
	if err != nil {
		t.Fatal(err)
	}
	j := join(t, lk, token)
	t.Cleanup(j.room.Disconnect)
	eventually(t, identity+" to be listed in "+room, func() bool {
		p, err := livekitInit.LiveKitService.Participant(ctx, room, identity)
		return err == nil && p != nil && p.GetPermission().GetHidden() && p.GetKind() == kind
	})
	return j
}

// inSidebar puts a channel in someone's sidebar, as joining it does.
func inSidebar(t *testing.T, dg *integration.DgraphEnv, channel string, who *dgraphStruct.DgraphUser) {
	t.Helper()
	var got struct {
		C []struct {
			UID string `json:"uid"`
		} `json:"c"`
	}
	if err := json.Unmarshal(dg.Query(t, `{ c(func: eq(ch_uuid, "`+channel+`")) { uid } }`), &got); err != nil || len(got.C) != 1 {
		t.Fatalf("the channel %s in the graph: %+v (%v)", channel, got, err)
	}
	dg.Mutate(t, []map[string]any{{"uid": who.Uid, "user_channels": []map[string]any{{"uid": got.C[0].UID}}}})
}

// listingCaughtUp waits until LiveKit's room listing has caught up with who is
// connected to these rooms, as the sidebar reads it: LiveKit refreshes a
// room's count every few seconds, and leaves an egress and an agent out of it.
func listingCaughtUp(t *testing.T, ctx context.Context, rooms ...string) {
	t.Helper()
	for _, room := range rooms {
		eventually(t, "LiveKit's listing of "+room+" to catch up", func() bool {
			people, err := livekitInit.LiveKitService.ConnectedParticipants(ctx, room)
			if err != nil {
				return false
			}
			want := 0
			for _, p := range people {
				if p.Kind != livekit.ParticipantInfo_EGRESS && p.Kind != livekit.ParticipantInfo_AGENT {
					want++
				}
			}
			rs, err := livekitInit.LiveKitService.ListRooms(ctx, room)
			if err != nil {
				return false
			}
			return (len(rs) == 0 && want == 0) || (len(rs) == 1 && int(rs[0].NumParticipants) == want)
		})
	}
}

// dotIs reads a channel's call dot as who sees it, on the channel's header and
// in their sidebar (its own route), once LiveKit's listing of the call's rooms
// has caught up, and wants both lit, or both out.
func dotIs(t *testing.T, ctx context.Context, channel string, who *dgraphStruct.DgraphUser, want bool, when string, rooms ...string) {
	t.Helper()
	listingCaughtUp(t, ctx, append([]string{channel}, rooms...)...)
	header, err := channelBusiness.GetChannelCallActiveStatus(ctx, channel)
	if err != nil {
		t.Fatalf("%s: the channel's header dot: %v", when, err)
	}
	var info userModels.UserInfo
	info.UserPostgresInfo.Id = uuid.MustParse(who.Uuid)
	info.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: who.Uid, Uuid: who.Uuid}
	rec := httptest.NewRecorder()
	userController.GetDgraphUserInfoByUUIDForSidebarNav(rec, httptest.NewRequest(http.MethodGet, "/sidebarNav", nil).WithContext(context.WithValue(ctx, helpers.UserInfoContextKey, info)))
	var bar struct {
		Data struct {
			Channels []struct {
				UUID       string `json:"ch_uuid"`
				CallActive bool   `json:"ch_call_active"`
			} `json:"user_channels"`
		} `json:"data"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &bar) != nil {
		t.Fatalf("%s: the sidebar answered %d %.300s", when, rec.Code, rec.Body.String())
	}
	side, found := false, false
	for _, c := range bar.Data.Channels {
		if c.UUID == channel {
			side, found = c.CallActive, true
		}
	}
	if !found {
		t.Fatalf("%s: the channel isn't in the sidebar: %.300s", when, rec.Body.String())
	}
	if header != want || side != want {
		t.Errorf("%s: the call's dot is lit %v on the header and %v in the sidebar, want %v", when, header, side, want)
	}
}

func TestACallsDotIsLitByAPersonInAnyOfItsRooms(t *testing.T) {
	ctx, pg, dg, lk := breakoutEnv(t)

	t.Run("a recorder or a hidden watcher alone lights no call", func(t *testing.T) {
		room := uuid.NewString()
		people := seedChannel(t, pg, dg, room, "maya", "outsider")
		inSidebar(t, dg, room, people["maya"])
		if _, err := livekitInit.LiveKitService.CreateRoom(ctx, room); err != nil {
			t.Fatal(err)
		}
		notAPerson(t, ctx, lk, room, "EG_"+uuid.NewString()[:8], livekit.ParticipantInfo_EGRESS)
		notAPerson(t, ctx, lk, room, "watcher-"+uuid.NewString()[:8], livekit.ParticipantInfo_STANDARD)
		dotIs(t, ctx, room, people["maya"], false, "a recorder and a watcher in the room")

		enterCall(t, ctx, lk, room, people, "maya")
		dotIs(t, ctx, room, people["maya"], true, "Maya in the call")
	})

	t.Run("a call whose people are all in its breakout rooms is lit", func(t *testing.T) {
		room := uuid.NewString()
		people := seedChannel(t, pg, dg, room, "sam", "ana", "passerby")
		inSidebar(t, dg, room, people["ana"])
		s, in := enterCall(t, ctx, lk, room, people, "sam", "ana")
		recorder := notAPerson(t, ctx, lk, room, "EG_"+uuid.NewString()[:8], livekit.ParticipantInfo_EGRESS)
		for _, a := range []callControlBusiness.Action{
			{Action: callControlBusiness.ActBreakoutsSet, Rooms: []string{"Design"}},
			{Action: callControlBusiness.ActBreakoutsSpread},
			{Action: callControlBusiness.ActBreakoutsOpen},
		} {
			if _, err := callControlBusiness.Act(ctx, people["sam"], s.ID, a); err != nil {
				t.Fatalf("Sam doing %s: %v", a.Action, err)
			}
		}
		design := callStateBusiness.BreakoutRoom(room, 1)
		for _, name := range []string{"ana", "sam"} {
			rt, err := callControlBusiness.BreakoutToken(ctx, people[name], s.ID, 1, "")
			if err != nil {
				t.Fatalf("%s's token for Design: %v", name, err)
			}
			j := join(t, lk, rt.Token)
			t.Cleanup(j.room.Disconnect)
			joinSettled(t, design, people[name].Uid, j)
			connected(t, ctx, design, people[name].Uid)
			leave(t, ctx, room, people[name].Uid, in[name])
		}
		// (a) The call's own room holds only its recorder.
		dotIs(t, ctx, room, people["ana"], true, "everyone in Design, the recorder alone in the call's own room", design)

		// (b) The recorder stops, and LiveKit closes the empty room.
		recorder.room.Disconnect()
		liveKitBusiness.DeleteRoom(ctx, room)
		roomDeleted(t, ctx, room)
		dotIs(t, ctx, room, people["ana"], true, "everyone in Design, the call's own room closed", design)
	})
}
