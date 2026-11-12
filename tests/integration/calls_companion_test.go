//go:build integration

package integration_test

// meet-04 (calls B1): companion mode, present only and Switch here. A
// companion is another device of someone who may be in a call: it is let in
// through Call Control's companion token, on an identity of its own that reads
// back as its person's, and the call server never lets it publish a
// microphone or a camera. Switch here is the person's own join on the new
// device: one identity holds one connection, so the old device is dropped.
// Run: go test -tags=integration ./tests/integration/ -run 'TestCall.*Companion|TestCallSwitchHere' -v

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"

	callControlBusiness "github.com/akashc777/OneCamp/business/CallControl"
	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	callsController "github.com/akashc777/OneCamp/controllers/Calls"
	channelController "github.com/akashc777/OneCamp/controllers/Channel"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/livekitInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	callStateModel "github.com/akashc777/OneCamp/models/postgres/CallState"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// asSignedIn is ctx as u's signed-in session, as the router's auth leaves it.
func asSignedIn(t *testing.T, ctx context.Context, u *dgraphStruct.DgraphUser) context.Context {
	t.Helper()
	pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, uuid.MustParse(u.Uuid))
	if err != nil || pg == nil {
		t.Fatalf("load %s: %v", u.Uuid, err)
	}
	return context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *u})
}

// companionToken asks POST /calls/{session_id}/companion-token as u (through
// an API token when through): the answer's status, its code and the token.
func companionToken(t *testing.T, ctx context.Context, u *dgraphStruct.DgraphUser, sessionID string, through bool) (int, string, callControlBusiness.RoomToken) {
	t.Helper()
	as := asSignedIn(t, ctx, u)
	if through {
		as = helpers.WithActsThroughToken(as)
	}
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("session_id", sessionID)
	req := httptest.NewRequest(http.MethodPost, "/calls/"+sessionID+"/companion-token", bytes.NewReader([]byte(`{"language":"en"}`)))
	rec := httptest.NewRecorder()
	callsController.CallCompanionToken(rec, req.WithContext(context.WithValue(as, chi.RouteCtxKey, rctx)))
	var answer struct {
		Code string                        `json:"code"`
		Data callControlBusiness.RoomToken `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &answer)
	return rec.Code, answer.Code, answer.Data
}

// switchHere is u's own join of a channel's call from another device: the
// channel's POST /ch/getCallToken, the token it answers.
func switchHere(t *testing.T, ctx context.Context, u *dgraphStruct.DgraphUser, channel string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"channel_uuid": channel})
	rec := httptest.NewRecorder()
	channelController.MakeVideoChannelCall(rec, httptest.NewRequest(http.MethodPost, "/ch/getCallToken", bytes.NewReader(body)).WithContext(asSignedIn(t, ctx, u)))
	var answer struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); rec.Code != http.StatusOK || err != nil || answer.Data.Token == "" {
		t.Fatalf("%s's join from another device: %d %s", u.Uuid, rec.Code, rec.Body.String())
	}
	return answer.Data.Token
}

// microphones is how many microphone tracks the call server has published in
// a room for a person, on all their devices.
func microphones(ctx context.Context, room, person string) (int, error) {
	people, err := livekitInit.LiveKitService.ConnectedParticipants(ctx, room)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, p := range people {
		if livekitInit.PersonOf(p.Identity) != person {
			continue
		}
		for _, track := range p.Tracks {
			if track.Source == livekit.TrackSource_MICROPHONE {
				n++
			}
		}
	}
	return n, nil
}

// published is whether someone in a room has published a source now.
func published(ctx context.Context, room, identity string, source livekit.TrackSource) bool {
	p, err := livekitInit.LiveKitService.Participant(ctx, room, identity)
	if err != nil || p == nil {
		return false
	}
	for _, track := range p.Tracks {
		if track.Source == source {
			return true
		}
	}
	return false
}

// THE ECHO TEST (meet-04's risk line). Sam talks on his laptop, joins on his
// phone as a companion, then moves the call to his tablet with Switch here,
// and keeps the laptop as a second companion. Watched the whole time, Sam
// never has more than one microphone in the call, and has exactly one at the
// end: a companion's microphone and camera are refused by the call server,
// even after it was told to let the companion publish everything, and the
// laptop is dropped (told it's a duplicate) the moment the tablet joins. The
// companions may share a screen until the host holds sharing.
func TestCallSwitchHereAndACompanionLeaveOneMicrophone(t *testing.T) {
	ctx, pg, dg, lk := breakoutEnv(t)
	room := uuid.NewString()
	people := seedChannel(t, pg, dg, room, "maya", "sam", "outsider")
	sam := people["sam"].Uid
	s, in := enterCall(t, ctx, lk, room, people, "maya", "sam")
	laptop := in["sam"]
	if err := publishTrack(laptop.room, livekit.TrackSource_MICROPHONE); err != nil {
		t.Fatalf("Sam's microphone on his laptop: %v", err)
	}
	eventually(t, "Sam's microphone", func() bool { n, _ := microphones(ctx, room, sam); return n == 1 })

	var most atomic.Int32
	stop := make(chan struct{})
	var watching sync.WaitGroup
	watching.Add(1)
	go func() {
		defer watching.Done()
		for {
			if n, err := microphones(ctx, room, sam); err == nil && int32(n) > most.Load() {
				most.Store(int32(n))
			}
			select {
			case <-stop:
				return
			case <-time.After(25 * time.Millisecond):
			}
		}
	}()

	// His phone, as a companion.
	code, refusal, phoneToken := companionToken(t, ctx, people["sam"], s.ID, false)
	if code != http.StatusOK {
		t.Fatalf("Sam's companion token: %d %s", code, refusal)
	}
	phoneRoom, phoneID := tokenRoom(t, phoneToken.Token)
	if person, ok := livekitInit.CompanionOf(phoneID); !ok || person != sam || phoneRoom != room || phoneToken.Room != room || phoneToken.Session != s.ID {
		t.Fatalf("Sam's companion token lets %q into %s (answer %+v)", phoneID, phoneRoom, phoneToken)
	}
	if got := grantOf(t, phoneToken.Token); len(got) != len(livekitInit.CompanionSources) {
		t.Errorf("Sam's companion may publish %v", got)
	}
	phone := join(t, lk, phoneToken.Token)
	joinSettled(t, room, phoneID, phone)
	connected(t, ctx, room, phoneID)
	if err := publishTrack(phone.room, livekit.TrackSource_MICROPHONE); err == nil {
		t.Error("the call server took a microphone from Sam's companion")
	}
	if err := publishTrack(phone.room, livekit.TrackSource_CAMERA); err == nil {
		t.Error("the call server took a camera from Sam's companion")
	}
	// Told later to let it publish everything (a hold let go, a part given):
	// still no microphone.
	if err := livekitInit.LiveKitService.SetPublishSources(ctx, room, phoneID, livekitInit.CallPublishSources); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if err := publishTrack(phone.room, livekit.TrackSource_MICROPHONE); err == nil {
		t.Error("the call server took a microphone from Sam's companion after it was given every source")
	}
	if err := publishTrack(phone.room, livekit.TrackSource_SCREEN_SHARE); err != nil {
		t.Errorf("Sam presenting from his companion: %v", err)
	}
	eventually(t, "Sam's companion's screen", func() bool { return published(ctx, room, phoneID, livekit.TrackSource_SCREEN_SHARE) })

	// A companion is no second Sam in the call.
	cs, err := callStateBusiness.Get(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p := cs.Person(phoneID); p != nil {
		t.Errorf("Sam's companion is an attendee of its own: %+v", p)
	}

	// Switch here: Sam's tablet joins as Sam.
	tablet := join(t, lk, switchHere(t, ctx, people["sam"], room))
	joinSettled(t, room, sam, tablet)
	eventually(t, "the laptop to be dropped", func() bool {
		r, _ := laptop.disconnected.Load().(lksdk.DisconnectionReason)
		return r == lksdk.DuplicateIdentity
	})
	if err := publishTrack(tablet.room, livekit.TrackSource_MICROPHONE); err != nil {
		t.Fatalf("Sam's microphone on his tablet: %v", err)
	}
	eventually(t, "Sam's microphone on his tablet", func() bool {
		n, _ := microphones(ctx, room, sam)
		return n == 1 && published(ctx, room, sam, livekit.TrackSource_MICROPHONE)
	})

	// The laptop stays in as a muted companion.
	code, refusal, laptopToken := companionToken(t, ctx, people["sam"], s.ID, false)
	if code != http.StatusOK {
		t.Fatalf("the laptop's companion token: %d %s", code, refusal)
	}
	_, laptopID := tokenRoom(t, laptopToken.Token)
	if laptopID == phoneID {
		t.Fatal("two companions of Sam share an identity: one would drop the other")
	}
	second := join(t, lk, laptopToken.Token)
	joinSettled(t, room, laptopID, second)
	if err := publishTrack(second.room, livekit.TrackSource_MICROPHONE); err == nil {
		t.Error("the call server took a microphone from Sam's laptop, now a companion")
	}

	// The host holds sharing: the companion's screen goes like anyone's.
	if _, err := callControlBusiness.Act(ctx, people["maya"], s.ID, callControlBusiness.Action{Action: callControlBusiness.ActHold, What: callStateModel.HoldShare, On: true}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the companion's screen to go with sharing held", func() bool {
		return !published(ctx, room, phoneID, livekit.TrackSource_SCREEN_SHARE)
	})

	time.Sleep(500 * time.Millisecond)
	close(stop)
	watching.Wait()
	if n, err := microphones(ctx, room, sam); err != nil || n != 1 {
		t.Errorf("Sam has %d microphones in the call at the end (%v), want exactly 1", n, err)
	}
	if m := most.Load(); m != 1 {
		t.Errorf("Sam had up to %d microphones in the call at once, want never more than 1", m)
	}
}

// Who may have a companion: someone who may be in the call while it goes on,
// signed in themselves. Not through an API token, not from outside the
// channel, not into a call that has ended; and a companion goes with its
// person: the host puts Sam out, and his phone goes too and can't come back,
// nor can a companion token he was given before. Nobody left in the call's
// own room: no companion, told to join instead. Every refusal is answered in
// its own words and status, never as a server error.
func TestCallACompanionGoesWithItsPerson(t *testing.T) {
	ctx, pg, dg, lk := breakoutEnv(t)
	room := uuid.NewString()
	people := seedChannel(t, pg, dg, room, "maya", "sam", "ana", "outsider")
	sam := people["sam"].Uid
	s, in := enterCall(t, ctx, lk, room, people, "maya", "sam")

	if code, refusal, _ := companionToken(t, ctx, people["sam"], s.ID, true); code != http.StatusForbidden || refusal != callControlBusiness.ErrThroughToken.Code {
		t.Errorf("Sam's companion through his API token: %d %s", code, refusal)
	}
	if code, refusal, tok := companionToken(t, ctx, people["outsider"], s.ID, false); code != http.StatusForbidden || refusal != "not_in_call" || tok.Token != "" {
		t.Errorf("someone outside the channel asking for a companion: %d %q, want %d not_in_call", code, refusal, http.StatusForbidden)
	}
	if code, _, _ := companionToken(t, ctx, people["sam"], uuid.NewString(), false); code == http.StatusOK {
		t.Errorf("a companion for a call that isn't there: %d", code)
	}
	// Ana is in the channel but not in the call: she may present to it.
	if code, refusal, _ := companionToken(t, ctx, people["ana"], s.ID, false); code != http.StatusOK {
		t.Errorf("Ana, in the channel, presenting to its call: %d %s", code, refusal)
	}

	code, refusal, tok := companionToken(t, ctx, people["sam"], s.ID, false)
	if code != http.StatusOK {
		t.Fatalf("Sam's companion: %d %s", code, refusal)
	}
	_, phoneID := tokenRoom(t, tok.Token)
	phone := join(t, lk, tok.Token)
	joinSettled(t, room, phoneID, phone)
	connected(t, ctx, room, phoneID)
	// A companion token Sam holds, not used yet.
	code, refusal, staleTok := companionToken(t, ctx, people["sam"], s.ID, false)
	if code != http.StatusOK {
		t.Fatalf("Sam's second companion: %d %s", code, refusal)
	}

	if _, err := callControlBusiness.Act(ctx, people["maya"], s.ID, callControlBusiness.Action{Action: callControlBusiness.ActRemove, Identity: sam}); err != nil {
		t.Fatalf("Maya putting Sam out: %v", err)
	}
	eventually(t, "Sam's phone to be put out with him", func() bool {
		p, _ := livekitInit.LiveKitService.Participant(ctx, room, phoneID)
		return p == nil
	})
	if code, refusal, _ := companionToken(t, ctx, people["sam"], s.ID, false); code == http.StatusOK {
		t.Errorf("Sam, put out by the host, got a companion back in: %d %s", code, refusal)
	}

	// The token he was given before is thrown out when it connects: his
	// companion may stay only while he may (judged as Sam, not by its own
	// identity), told why.
	_, staleID := tokenRoom(t, staleTok.Token)
	stale := join(t, lk, staleTok.Token)
	joinSettled(t, room, staleID, stale)
	connected(t, ctx, room, staleID)
	eventually(t, "the companion token Sam held from before to be thrown out", func() bool {
		p, _ := livekitInit.LiveKitService.Participant(ctx, room, staleID)
		return p == nil
	})
	if why := toldRemoved(stale); why != "host" {
		t.Errorf("Sam's companion from before was told %q, want host", why)
	}

	// Maya leaves the call's own room, so nobody is in it: Ana, who may be in
	// the call, is told to join it instead.
	in["maya"].room.Disconnect()
	eventually(t, "nobody in the call's own room", func() bool {
		active, err := livekitInit.LiveKitService.RoomHasHumanParticipants(ctx, room)
		return err == nil && !active
	})
	if code, refusal, tok := companionToken(t, ctx, people["ana"], s.ID, false); code != http.StatusConflict || refusal != "call_not_started" || tok.Token != "" {
		t.Errorf("Ana's companion with nobody in the call's own room: %d %q, want %d call_not_started", code, refusal, http.StatusConflict)
	}
}

// joinCompanion joins a companion of u to the call s, through the companion
// token, as LiveKit and its webhook would: its identity and its connection.
func joinCompanion(t *testing.T, ctx context.Context, lk *integration.LiveKitEnv, s *callStateBusiness.Session, u *dgraphStruct.DgraphUser) (string, *joined) {
	t.Helper()
	code, refusal, tok := companionToken(t, ctx, u, s.ID, false)
	if code != http.StatusOK {
		t.Fatalf("%s's companion: %d %s", u.UserName, code, refusal)
	}
	return joinToken(t, ctx, lk, s.Room, tok.Token)
}

// joinToken joins someone to a room with a token, as LiveKit and its webhook
// would: who it lets in, and their connection.
func joinToken(t *testing.T, ctx context.Context, lk *integration.LiveKitEnv, room, token string) (string, *joined) {
	t.Helper()
	_, id := tokenRoom(t, token)
	j := join(t, lk, token)
	t.Cleanup(j.room.Disconnect)
	joinSettled(t, room, id, j)
	connected(t, ctx, room, id)
	return id, j
}

// mayShare is whether the call server lets someone in a room publish a
// screen now.
func mayShare(ctx context.Context, room, identity string) bool {
	p, err := livekitInit.LiveKitService.Participant(ctx, room, identity)
	if err != nil || p == nil || p.Permission == nil || !p.Permission.CanPublish {
		return false
	}
	return slices.Contains(p.Permission.CanPublishSources, livekit.TrackSource_SCREEN_SHARE)
}

// screenMuted is whether someone's screen in a room is published and muted.
func screenMuted(ctx context.Context, room, identity string) bool {
	p, err := livekitInit.LiveKitService.Participant(ctx, room, identity)
	if err != nil || p == nil {
		return false
	}
	for _, track := range p.Tracks {
		if track.Source == livekit.TrackSource_SCREEN_SHARE {
			return track.Muted
		}
	}
	return false
}

// meet-04, every device: what someone may share under the call's holds,
// their companions may share, and what is done to their sharing reaches
// their companions' screens. The host's phone goes on presenting when she
// holds sharing (it shares as she may). Sam, let through the hold, may
// present again from his phone, in the call already, and from a tablet that
// joins later on a token minted while he was held; the host muting Sam's
// screen mutes it on his companions.
func TestCallACompanionSharesAsItsPersonMay(t *testing.T) {
	ctx, pg, dg, lk := breakoutEnv(t)
	room := uuid.NewString()
	people := seedChannel(t, pg, dg, room, "maya", "sam", "outsider")
	sam := people["sam"].Uid
	s, _ := enterCall(t, ctx, lk, room, people, "maya", "sam")
	act := func(a callControlBusiness.Action) {
		t.Helper()
		if _, err := callControlBusiness.Act(ctx, people["maya"], s.ID, a); err != nil {
			t.Fatalf("Maya doing %s: %v", a.Action, err)
		}
	}
	present := func(id string, j *joined) {
		t.Helper()
		eventually(t, id+" to be let share", func() bool { return mayShare(ctx, room, id) })
		if err := publishTrack(j.room, livekit.TrackSource_SCREEN_SHARE); err != nil {
			t.Fatalf("%s presenting: %v", id, err)
		}
		eventually(t, id+"'s screen", func() bool { return published(ctx, room, id, livekit.TrackSource_SCREEN_SHARE) })
	}

	mayaPhoneID, mayaPhone := joinCompanion(t, ctx, lk, s, people["maya"])
	samPhoneID, samPhone := joinCompanion(t, ctx, lk, s, people["sam"])
	present(mayaPhoneID, mayaPhone)
	present(samPhoneID, samPhone)

	// Sharing held: Sam's phone stops presenting; the host's goes on.
	act(callControlBusiness.Action{Action: callControlBusiness.ActHold, What: callStateModel.HoldShare, On: true})
	eventually(t, "Sam's phone's screen to go with sharing held", func() bool {
		return !published(ctx, room, samPhoneID, livekit.TrackSource_SCREEN_SHARE)
	})
	if !mayShare(ctx, room, mayaPhoneID) || !published(ctx, room, mayaPhoneID, livekit.TrackSource_SCREEN_SHARE) {
		t.Error("the host's phone stopped presenting when she held sharing")
	}

	// Sam's tablet's token, minted while he is held: no screen.
	code, refusal, tabletTok := companionToken(t, ctx, people["sam"], s.ID, false)
	if code != http.StatusOK {
		t.Fatalf("Sam's tablet's companion token: %d %s", code, refusal)
	}
	if got := grantOf(t, tabletTok.Token); len(got) != 0 {
		t.Fatalf("Sam's tablet's token, minted with sharing held, grants %v", got)
	}

	// Let through: his phone, in the call already, may present again, and
	// his tablet, joining now, is seated as Sam may share.
	act(callControlBusiness.Action{Action: callControlBusiness.ActAllow, Identity: sam, What: callStateModel.HoldShare, On: true})
	present(samPhoneID, samPhone)
	tabletID, tablet := joinToken(t, ctx, lk, room, tabletTok.Token)
	present(tabletID, tablet)

	// The host mutes Sam's screen: on every device of his.
	act(callControlBusiness.Action{Action: callControlBusiness.ActMute, Identity: sam, Source: "screen_share"})
	eventually(t, "Sam's screens on his companions to be muted", func() bool {
		return screenMuted(ctx, room, samPhoneID) && screenMuted(ctx, room, tabletID)
	})
	if screenMuted(ctx, room, mayaPhoneID) {
		t.Error("muting Sam's screen muted the host's phone")
	}
}

// meet-04, the breakout rooms: a companion stays with the call's own room.
// Sam going into a breakout room puts his phone out of the call, told he went
// to a room; Ana's phone stays. A breakout room's call has no companions.
func TestCallACompanionStaysWithTheCallsOwnRoom(t *testing.T) {
	ctx, pg, dg, lk := breakoutEnv(t)
	room := uuid.NewString()
	people := seedChannel(t, pg, dg, room, "maya", "sam", "ana", "outsider")
	sam := people["sam"].Uid
	s, _ := enterCall(t, ctx, lk, room, people, "maya", "sam", "ana")
	act := func(a callControlBusiness.Action) {
		t.Helper()
		if _, err := callControlBusiness.Act(ctx, people["maya"], s.ID, a); err != nil {
			t.Fatalf("Maya doing %s: %v", a.Action, err)
		}
	}
	samPhoneID, samPhone := joinCompanion(t, ctx, lk, s, people["sam"])
	anaPhoneID, _ := joinCompanion(t, ctx, lk, s, people["ana"])

	act(callControlBusiness.Action{Action: callControlBusiness.ActBreakoutsSet, Rooms: []string{"Design"}})
	act(callControlBusiness.Action{Action: callControlBusiness.ActBreakoutsAssign, Identity: sam, Room: 1})
	act(callControlBusiness.Action{Action: callControlBusiness.ActBreakoutsOpen})
	design, err := callControlBusiness.BreakoutToken(ctx, people["sam"], s.ID, 1, "")
	if err != nil {
		t.Fatalf("Sam's token for his room: %v", err)
	}
	joinToken(t, ctx, lk, design.Room, design.Token)

	eventually(t, "Sam's phone to leave the call as he went to a room", func() bool {
		p, _ := livekitInit.LiveKitService.Participant(ctx, room, samPhoneID)
		return p == nil
	})
	if why := toldRemoved(samPhone); why != "went_to_room" {
		t.Errorf("Sam's phone was told %q, want went_to_room", why)
	}
	if p, _ := livekitInit.LiveKitService.Participant(ctx, room, anaPhoneID); p == nil {
		t.Error("Ana's phone left the call when Sam went to a room")
	}

	_, err = callControlBusiness.CompanionToken(ctx, people["sam"], design.Session, "")
	refusedWith(t, "a companion of Sam in his breakout room's call", err, callControlBusiness.ErrNotInCall)
}
