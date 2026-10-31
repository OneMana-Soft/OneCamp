//go:build integration

package integration_test

// What the people running a call hold for everyone else (calls meet-12,
// zoom-14), on a real call server, graph and database: a hold on media takes
// the source from whoever doesn't run the call, and the call server
// unpublishes what was live; a new token is minted without it; a co-host
// and someone let through keep it; a chat hold takes the replies of a member,
// an invitee and a guest, wherever they write from, and never the host's;
// each change is written to the audit log; and a call that ends ends its
// holds.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	lka "github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"

	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	callControlBusiness "github.com/akashc777/OneCamp/business/CallControl"
	callInviteBusiness "github.com/akashc777/OneCamp/business/CallInvite"
	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	callThreadBusiness "github.com/akashc777/OneCamp/business/CallThread"
	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	_ "github.com/akashc777/OneCamp/business/Huddle" // the call's post, linked as the server links it
	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	callsController "github.com/akashc777/OneCamp/controllers/Calls"
	postController "github.com/akashc777/OneCamp/controllers/Post"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/livekitInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// The holds migration goes down and up again on Postgres 12, and its checks
// hold: only the five holds are kept.
func TestCallHoldsMigrationUpDownUp(t *testing.T) {
	env := integration.SetupEnv(t)
	column := func(table, name string) bool {
		var n int
		if err := env.PG.QueryRow(`SELECT count(*) FROM information_schema.columns WHERE table_name = $1 AND column_name = $2`, table, name).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	if _, err := env.PG.Exec(migrationSQL(t, "call_holds", "down")); err != nil {
		t.Fatalf("down: %v", err)
	}
	if column("call_sessions", "held") || column("call_attendance", "allowed") {
		t.Fatal("the holds columns are still there after down")
	}
	if _, err := env.PG.Exec(migrationSQL(t, "call_holds", "up")); err != nil {
		t.Fatalf("up again: %v", err)
	}
	session := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO call_sessions (id, room, kind, resource_kind, resource_id, started_at, held) VALUES ($1, 'room-h', 'huddle', 'channel', 'room-h', NOW(), '{chat,share}')`, session); err != nil {
		t.Fatalf("chat and sharing held: %v", err)
	}
	if _, err := env.PG.Exec(`UPDATE call_sessions SET held = '{shout}' WHERE id = $1`, session); err == nil {
		t.Error("a hold the call doesn't know was stored")
	}
	if _, err := env.PG.Exec(`INSERT INTO call_attendance (session_id, identity, name, role, joined_at, last_joined_at, allowed) VALUES ($1, 'a', 'A', 'participant', NOW(), NOW(), '{everything}')`, session); err == nil {
		t.Error("an allowance the call doesn't know was stored")
	}
}

// grantOf is what a call token lets its holder publish, as LiveKit reads it.
func grantOf(t *testing.T, token string) []string {
	t.Helper()
	v, err := lka.ParseAPIToken(token)
	if err != nil {
		t.Fatal(err)
	}
	c, err := v.Verify(livekitInit.LiveKitService.Config.ApiSecret)
	if err != nil || c.Video == nil {
		t.Fatalf("the token's grant: %v", err)
	}
	if !c.Video.GetCanPublish() {
		return []string{}
	}
	return c.Video.CanPublishSources
}

// A host holds microphones: Sam's live microphone is unpublished by the call
// server and he can't publish one again, a token minted for him now lacks it,
// a co-host keeps hers, Sam let through gets his back, and asking him to
// unmute lets him through. The host's hold and let are in the audit log.
func TestCallAHostHoldsMicrophonesForEveryoneElse(t *testing.T) {
	ctx := context.Background()
	pg := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, pg.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	dg := integration.SetupDgraph(t)
	integration.StubMqttClient(t)
	lk := integration.SetupLiveKit(t)

	room := uuid.NewString()
	people := seedChannel(t, pg, dg, room, "maya", "sam", "ana", "outsider")
	if _, err := livekitInit.LiveKitService.CreateRoom(ctx, room); err != nil {
		t.Fatal(err)
	}
	in := map[string]*joined{}
	for _, name := range []string{"maya", "sam", "ana"} { // Maya starts it, so hosts it
		token, err := livekitInit.LiveKitService.GenerateToken(room, people[name].Uid, title(name), false, "", livekitInit.CallPublishSources)
		if err != nil {
			t.Fatal(err)
		}
		in[name] = join(t, lk, token)
		joinSettled(t, room, people[name].Uid, in[name])
		connected(t, ctx, room, people[name].Uid)
	}
	if err := publishTrack(in["sam"].room, livekit.TrackSource_MICROPHONE); err != nil {
		t.Fatal(err)
	}
	live := func(name string, source livekit.TrackSource) bool {
		p, _ := livekitInit.LiveKitService.Participant(ctx, room, people[name].Uid)
		if p == nil {
			return false
		}
		for _, tr := range p.Tracks {
			if tr.Source == source {
				return true
			}
		}
		return false
	}
	may := func(name string, source livekit.TrackSource) bool {
		p, _ := livekitInit.LiveKitService.Participant(ctx, room, people[name].Uid)
		return p != nil && p.Permission != nil && p.Permission.CanPublish && slices.Contains(p.Permission.CanPublishSources, source)
	}
	eventually(t, "Sam's microphone to be live", func() bool { return live("sam", livekit.TrackSource_MICROPHONE) })
	s, err := callStateBusiness.Current(ctx, callStateBusiness.Ref{Kind: "channel", ID: room})
	if err != nil || s == nil {
		t.Fatalf("no call recorded: %v", err)
	}
	act := func(name string, a callControlBusiness.Action) (callControlBusiness.Result, error) {
		return callControlBusiness.Act(ctx, people[name], s.ID, a)
	}
	if _, err := act("ana", callControlBusiness.Action{Action: callControlBusiness.ActHold, What: "microphone", On: true}); !errors.Is(err, callControlBusiness.ErrNotAllowed) {
		t.Errorf("a participant held microphones: %v", err)
	}
	if _, err := act("maya", callControlBusiness.Action{Action: callControlBusiness.ActHold, What: "everything", On: true}); !errors.Is(err, callControlBusiness.ErrNoSuchHold) {
		t.Errorf("a hold the call doesn't know: %v", err)
	}

	// Through the API, so the hold is audited as the host's.
	code, body := callRead(t, callsController.ActInCall, "POST", "/calls/"+s.ID+"/act", people["maya"], `{"action":"hold","what":"microphone","on":true}`, s.ID)
	if code != 200 {
		t.Fatalf("Maya holds microphones: %d %s", code, body["msg"])
	}
	eventually(t, "Sam's microphone to be taken", func() bool {
		return !live("sam", livekit.TrackSource_MICROPHONE) && !may("sam", livekit.TrackSource_MICROPHONE)
	})
	if !may("sam", livekit.TrackSource_CAMERA) || !may("maya", livekit.TrackSource_MICROPHONE) || may("ana", livekit.TrackSource_MICROPHONE) {
		t.Error("the hold took the wrong sources")
	}
	// Nor can he publish one again: the call server refuses it.
	if err := publishTrack(in["sam"].room, livekit.TrackSource_MICROPHONE); err == nil || live("sam", livekit.TrackSource_MICROPHONE) {
		t.Errorf("Sam published a held microphone: %v", err)
	}
	r, err := livekitInit.LiveKitService.GetRoom(ctx, room)
	if err != nil || !strings.Contains(r.Metadata, `"held":["microphone"]`) {
		t.Errorf("every screen reads the hold: %v %v", r, err)
	}
	samToken, _, err := liveKitBusiness.CreateRoomAndGetToken(ctx, room, people["sam"], false, "")
	if err != nil || slices.Contains(grantOf(t, samToken), "microphone") || !slices.Contains(grantOf(t, samToken), "camera") {
		t.Errorf("a token minted while microphones are held: %v %v", grantOf(t, samToken), err)
	}
	if _, err := act("maya", callControlBusiness.Action{Action: callControlBusiness.ActAskAllUnmute}); !errors.Is(err, callControlBusiness.ErrMicsHeld) {
		t.Errorf("asking everyone to unmute while microphones are held: %v", err)
	}

	// Ana made co-host runs the call: hers come back.
	if _, err := act("maya", callControlBusiness.Action{Action: callControlBusiness.ActMakeCohost, Identity: people["ana"].Uid}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "Ana's microphone to come back with her part", func() bool { return may("ana", livekit.TrackSource_MICROPHONE) })

	// Sam let through, through the API: his comes back, and the room is told.
	code, body = callRead(t, callsController.ActInCall, "POST", "/calls/"+s.ID+"/act", people["maya"], `{"action":"allow","identity":"`+people["sam"].Uid+`","what":"microphone","on":true}`, s.ID)
	if code != 200 {
		t.Fatalf("Maya lets Sam talk: %d %s", code, body["msg"])
	}
	eventually(t, "Sam let through", func() bool {
		p, _ := livekitInit.LiveKitService.Participant(ctx, room, people["sam"].Uid)
		return may("sam", livekit.TrackSource_MICROPHONE) && p != nil && p.Attributes[callControlBusiness.AttrAllowed] == "microphone"
	})
	if _, err := act("maya", callControlBusiness.Action{Action: callControlBusiness.ActAllow, Identity: people["sam"].Uid, What: "microphone"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "Sam's let taken back", func() bool { return !may("sam", livekit.TrackSource_MICROPHONE) })

	// Asked to unmute while held, he's let through first.
	if _, err := act("maya", callControlBusiness.Action{Action: callControlBusiness.ActAskUnmute, Identity: people["sam"].Uid}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "Sam let through by the ask", func() bool { return may("sam", livekit.TrackSource_MICROPHONE) })

	// Let go: everyone may again, and the screens read nothing held.
	if _, err := act("maya", callControlBusiness.Action{Action: callControlBusiness.ActHold, What: "microphone"}); err != nil {
		t.Fatal(err)
	}
	if r, err := livekitInit.LiveKitService.GetRoom(ctx, room); err != nil || strings.Contains(r.Metadata, `"held"`) {
		t.Errorf("nothing held is no key: %v %v", r, err)
	}
	eventually(t, "the audit log to have the hold and the let", func() bool {
		var n int
		err := pg.PG.QueryRow(`SELECT count(*) FROM admin_audit_log WHERE action IN ('call.hold', 'call.allow') AND metadata->>'what' = 'microphone'`).Scan(&n)
		return err == nil && n == 2
	})
}

// A host holds chat: a member, someone let in by the invite and a guest can't
// reply in the call's thread, the member not from the channel's thread view
// or its reply route either, while the host can; a member let through can;
// and the call's end ends it.
func TestCallAHostHoldsTheCallsChat(t *testing.T) {
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
	room := uuid.NewString()
	people := seedChannel(t, pg, dg, room, "maya", "ben", "jonas")
	maya, ben, jonas := people["maya"], people["ben"], people["jonas"]
	info := func(u *dgraphStruct.DgraphUser) *userModels.UserInfo {
		i := &userModels.UserInfo{UserDgraphInfo: *u}
		i.UserPostgresInfo.Id = uuid.MustParse(u.Uuid)
		return i
	}
	enter := func(u *dgraphStruct.DgraphUser, token string) {
		t.Helper()
		joinSettled(t, room, u.Uid, join(t, lk, token))
		connected(t, ctx, room, u.Uid)
	}
	mayaToken, _, err := liveKitBusiness.CreateRoomAndGetToken(ctx, room, maya, false, "")
	if err != nil {
		t.Fatal(err)
	}
	enter(maya, mayaToken)
	var s *callStateBusiness.Session
	eventually(t, "the call to take its post", func() bool {
		s, err = callStateBusiness.Current(ctx, callStateBusiness.Ref{Kind: "channel", ID: room})
		return err == nil && s != nil && s.PostUUID != ""
	})
	benToken, _, err := liveKitBusiness.CreateRoomAndGetToken(ctx, room, ben, false, "")
	if err != nil {
		t.Fatal(err)
	}
	enter(ben, benToken)
	link, err := callInviteBusiness.Invite(ctx, room, maya)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := callInviteBusiness.Enter(ctx, callInviteBusiness.Door{Token: link.GuestToken}, jonas, "")
	if err != nil {
		t.Fatal(err)
	}
	enter(jonas, entry.Token)
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
			if strings.HasPrefix(id, "guest-") {
				gia = id
				return true
			}
		}
		return false
	})
	connected(t, ctx, room, gia)
	giaAuth := "Bearer " + guestToken
	thread := conversationBusiness.Ref{Kind: conversationBusiness.KindThread, ID: s.PostUUID}
	act := func(a callControlBusiness.Action) {
		t.Helper()
		if _, err := callControlBusiness.Act(ctx, maya, s.ID, a); err != nil {
			t.Fatal(err)
		}
	}
	// One of the channel's post routes as u, as its thread view calls it: the
	// answer's status, its code, and the reply it made, if any.
	asMember := func(u *dgraphStruct.DgraphUser, route http.HandlerFunc, body any) (int, string, string) {
		t.Helper()
		pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, uuid.MustParse(u.Uuid))
		if err != nil || pg == nil {
			t.Fatalf("load %s: %v", u.UserFullName, err)
		}
		as := context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *u})
		raw, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		route(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(as))
		var answer struct {
			Code string `json:"code"`
			Data struct {
				Reply string `json:"comment_id"`
			} `json:"data"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &answer)
		return rec.Code, answer.Code, answer.Data.Reply
	}
	// A reply to the call's message through the channel's own reply route.
	channelReply := func(u *dgraphStruct.DgraphUser, text string) (int, string) {
		t.Helper()
		code, why, _ := asMember(u, postController.CreateCommentInPost, postAdapter.InputCreateOrUpdateCommentToPost{PostUuid: s.PostUUID, HTMLText: "<p>" + text + "</p>"})
		return code, why
	}
	// Ben's reply from before the hold, to edit and react to while it holds.
	code, _, benReply := asMember(ben, postController.CreateCommentInPost, postAdapter.InputCreateOrUpdateCommentToPost{PostUuid: s.PostUUID, HTMLText: "<p>Before the hold</p>"})
	if code != http.StatusOK || benReply == "" {
		t.Fatalf("Ben's reply before the hold: %d %q", code, benReply)
	}
	edit := func(u *dgraphStruct.DgraphUser, text string) (int, string) {
		t.Helper()
		code, why, _ := asMember(u, postController.UpdateCommentInPost, postAdapter.InputCreateOrUpdateCommentToPost{Uuid: benReply, HTMLText: "<p>" + text + "</p>"})
		return code, why
	}
	react := func(u *dgraphStruct.DgraphUser, emoji string) (int, string) {
		t.Helper()
		code, why, _ := asMember(u, postController.CreateOrUpdateReactionOnCommentPost, postAdapter.InputUpdateReactionForCommentInPost{Uuid: benReply, EmojiUuid: emoji})
		return code, why
	}

	act(callControlBusiness.Action{Action: callControlBusiness.ActHold, What: "chat", On: true})
	// An empty identity is nobody's: a row kept under one, running the call,
	// never lets a member through the hold (CR-27's SF1).
	if _, err := pg.PG.ExecContext(ctx, `INSERT INTO call_attendance (session_id, identity, name, role, joined_at, last_joined_at)
		VALUES ($1, '', 'Nobody', 'host', now(), now())`, s.ID); err != nil {
		t.Fatal(err)
	}
	if err := callThreadBusiness.ReplyAsMember(ctx, info(ben), s.ID, "hi"); !errors.Is(err, callThreadBusiness.ErrChatHeld) {
		t.Errorf("Ben's reply while chat is held: %v", err)
	}
	if d, err := conversationBusiness.Resolve(ctx, info(ben), thread); err != nil || d.Level != conversationBusiness.LevelRead || !slices.Contains(d.Reasons, conversationBusiness.ReasonCallChatHeld) {
		t.Errorf("Ben in the channel's thread view: %+v %v", d, err)
	}
	if code, why := channelReply(ben, "hi"); code != http.StatusForbidden || why != "chat_held" {
		t.Errorf("Ben's reply through the channel's reply route: %d %q", code, why)
	}
	if code, why := edit(ben, "Rewritten while held"); code != http.StatusForbidden || why != "chat_held" {
		t.Errorf("Ben's edit while held: %d %q", code, why)
	}
	if code, why := react(ben, "+1"); code != http.StatusForbidden || why != "chat_held" {
		t.Errorf("Ben's reaction while held: %d %q", code, why)
	}
	if err := callThreadBusiness.ReplyAsMember(ctx, info(jonas), s.ID, "hi"); !errors.Is(err, callThreadBusiness.ErrChatHeld) {
		t.Errorf("Jonas's reply while chat is held: %v", err)
	}
	if err := callThreadBusiness.ReplyAsGuest(ctx, giaAuth, "hi"); !errors.Is(err, callThreadBusiness.ErrChatHeld) {
		t.Errorf("Gia's reply while chat is held: %v", err)
	}
	if err := callThreadBusiness.ReplyAsMember(ctx, info(maya), s.ID, "One at a time, please"); err != nil {
		t.Errorf("the host's reply: %v", err)
	}
	if code, why := channelReply(maya, "Questions at the end"); code != http.StatusOK {
		t.Errorf("the host's reply through the channel's reply route: %d %q", code, why)
	}
	if code, why := react(maya, "eyes"); code != http.StatusOK {
		t.Errorf("the host's reaction while chat is held: %d %q", code, why)
	}

	act(callControlBusiness.Action{Action: callControlBusiness.ActAllow, Identity: ben.Uid, What: "chat", On: true})
	if err := callThreadBusiness.ReplyAsMember(ctx, info(ben), s.ID, "Thanks"); err != nil {
		t.Errorf("Ben let through: %v", err)
	}
	if code, why := edit(ben, "Before the hold, and after"); code != http.StatusOK {
		t.Errorf("Ben's edit once let through: %d %q", code, why)
	}

	// The call ends, and so does its hold.
	callControlBusiness.OnEnded(ctx, room, s.RoomSID, "last_left")
	if d, err := conversationBusiness.Resolve(ctx, info(jonas), thread); err == nil && slices.Contains(d.Reasons, conversationBusiness.ReasonCallChatHeld) {
		t.Errorf("a held chat outlived its call: %+v", d)
	}
	if d, err := conversationBusiness.Resolve(ctx, info(ben), thread); err != nil || d.Level < conversationBusiness.LevelReply {
		t.Errorf("Ben in the thread after the call: %+v %v", d, err)
	}
}
