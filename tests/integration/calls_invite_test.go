//go:build integration

package integration_test

// A call's invite and members at a link as themselves (calls HU13): an invite
// opens its call alone, while it goes on; a guest knocks, a member comes in as
// themselves without the conversation (agreed with CHAT-LEAD, 21 Oct); a link
// that ends puts out whom it let in.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	callControlBusiness "github.com/akashc777/OneCamp/business/CallControl"
	callInviteBusiness "github.com/akashc777/OneCamp/business/CallInvite"
	callRoomBusiness "github.com/akashc777/OneCamp/business/CallRoom"
	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	callStateModel "github.com/akashc777/OneCamp/models/postgres/CallState"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func callAdmissionsMigration(t *testing.T, dir string) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(here), "..", "..", "migrations", "*_call_admissions."+dir+".sql"))
	if len(matches) != 1 {
		t.Fatalf("want one call_admissions %s migration, found %v", dir, matches)
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The admissions' migration goes down and up again on Postgres 12: an
// invitee's part is kept going up, and becomes a participant's going down.
func TestCallAdmissionsMigrationUpDownUp(t *testing.T) {
	env := integration.SetupEnv(t)
	session := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO call_sessions (id, room, kind, resource_kind, resource_id, started_at) VALUES ($1, 'r', 'huddle', 'channel', 'c', NOW())`, session); err != nil {
		t.Fatal(err)
	}
	attend := func(identity, role string) error {
		_, err := env.PG.Exec(`INSERT INTO call_attendance (session_id, identity, role, joined_at, last_joined_at) VALUES ($1, $2, $3, NOW(), NOW())`, session, identity, role)
		return err
	}
	if err := attend("0x1", "invitee"); err != nil {
		t.Fatalf("an invitee's part: %v", err)
	}
	if _, err := env.PG.Exec(callAdmissionsMigration(t, "down")); err != nil {
		t.Fatalf("down: %v", err)
	}
	var role string
	if err := env.PG.QueryRow(`SELECT role FROM call_attendance WHERE identity = '0x1'`).Scan(&role); err != nil || role != "participant" {
		t.Errorf("an invitee going down: %q, %v", role, err)
	}
	if err := attend("0x2", "invitee"); err == nil {
		t.Error("an invitee's part with the migration down")
	}
	if _, err := env.PG.Exec(callAdmissionsMigration(t, "up")); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if err := attend("0x3", "invitee"); err != nil {
		t.Errorf("an invitee's part up again: %v", err)
	}
}

// The subquery call_admissions reads live grants by (LiveGrantsSQL) and a
// link's one read by its token (GetActiveByHash) agree on every grant:
// live, revoked, expired, past the workspace's link cap, a room's standing
// address past it, and a call's invite. Both are liveSQL; this keeps them so
// (DOCS-LEAD-2, 21 Oct).
func TestGuestLiveGrantsAgreeWithTheLinksOwnRead(t *testing.T) {
	ctx := context.Background()
	pg := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, pg.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	dg := integration.SetupDgraph(t)
	integration.StubMqttClient(t)
	// The policy's change is recorded against whoever made it: a member.
	owner := uuid.MustParse(seedChannel(t, pg, dg, uuid.NewString(), "maya", "outsider")["maya"].Uuid)
	if _, err := policyBusiness.Admin.Set(ctx, policyBusiness.GuestLinkMaxDays.Key, json.RawMessage("30"), policyBusiness.Changer{UserID: owner, Email: "maya@example.test"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = policyBusiness.Admin.Set(context.Background(), policyBusiness.GuestLinkMaxDays.Key, json.RawMessage("0"), policyBusiness.Changer{UserID: owner})
	})
	later, earlier := time.Now().Add(time.Hour), time.Now().Add(-time.Hour)
	type kind struct {
		name, resource, then string
		expires              *time.Time
		live                 bool
	}
	for i, k := range []kind{
		{"a live meeting link", "meeting", "", &later, true},
		{"a revoked link", "meeting", "UPDATE guest_grants SET revoked_at = NOW() WHERE id = $1", &later, false},
		{"an expired link", "meeting", "", &earlier, false},
		{"a link past the cap", "doc", "UPDATE guest_grants SET created_at = NOW() - interval '31 days' WHERE id = $1", nil, false},
		{"a room's address past the cap", "meeting", "UPDATE guest_grants SET slug = 'agree-room', created_at = NOW() - interval '31 days' WHERE id = $1", nil, true},
		{"a call's invite", "call", "", &later, true},
	} {
		hash := []byte("agree-" + strconv.Itoa(i))
		id, err := guestModel.CreateGrant(ctx, hash, k.resource, uuid.NewString(), guestModel.CapabilityJoin, owner, k.expires)
		if err != nil {
			t.Fatal(err)
		}
		if k.then != "" {
			if _, err := pg.PG.Exec(k.then, id); err != nil {
				t.Fatal(err)
			}
		}
		byHash, err := guestModel.GetActiveByHash(ctx, hash)
		if err != nil {
			t.Fatal(err)
		}
		var bySubquery bool
		if err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `SELECT $1::uuid IN (`+guestModel.LiveGrantsSQL(2)+`)`, id, guestModel.LinkCapDays(ctx)).Scan(&bySubquery); err != nil {
			t.Fatal(err)
		}
		if (byHash != nil) != k.live || bySubquery != k.live {
			t.Errorf("%s: by its token %v, by the subquery %v, want %v", k.name, byHash != nil, bySubquery, k.live)
		}
	}
}

// An invite opens its call alone, while it goes on. A guest who opens it is
// let into that call; a member who opens it comes in as themselves, runs
// nothing, replies only in the call's thread and never reaches the
// conversation; someone already in the conversation just joins it. Revoked,
// the invite puts out whom it let in; once the call is over it opens nothing,
// not even the next call in the same conversation.
func TestCallAnInviteOpensItsCallAlone(t *testing.T) {
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
	maya, ben, jonas := people["maya"], people["ben"], people["jonas"]
	info := func(u *dgraphStruct.DgraphUser) *userModels.UserInfo {
		i := &userModels.UserInfo{UserDgraphInfo: *u}
		i.UserPostgresInfo.Id = uuid.MustParse(u.Uuid)
		return i
	}
	enterCall := func(u *dgraphStruct.DgraphUser, token string) *joined {
		t.Helper()
		j := join(t, lk, token)
		joinSettled(t, room, u.Uid, j)
		connected(t, ctx, room, u.Uid)
		return j
	}

	// Maya starts the channel's call; Ben, in it, may not invite (a channel's participant).
	mayaToken, _, err := liveKitBusiness.CreateRoomAndGetToken(ctx, room, maya, false, "")
	if err != nil {
		t.Fatal(err)
	}
	enterCall(maya, mayaToken)
	benToken, _, err := liveKitBusiness.CreateRoomAndGetToken(ctx, room, ben, false, "")
	if err != nil {
		t.Fatal(err)
	}
	enterCall(ben, benToken)
	if _, err := callInviteBusiness.Invite(ctx, room, ben); !errors.Is(err, callInviteBusiness.ErrNotAllowed) {
		t.Errorf("a channel's participant's invite: %v", err)
	}
	if _, err := callInviteBusiness.Invite(ctx, room, jonas); !errors.Is(err, callInviteBusiness.ErrNotInCall) {
		t.Errorf("an invite from someone not in the call: %v", err)
	}
	link, err := callInviteBusiness.Invite(ctx, room, maya)
	if err != nil {
		t.Fatalf("the host's invite: %v", err)
	}

	// A guest opens it: the call, named for whoever invited them.
	door, err := guestBusiness.OpenDoor(ctx, link.GuestToken)
	if err != nil || door.Room != room {
		t.Fatalf("the invite's door: %+v, %v", door, err)
	}
	guestToken, _, err := guestBusiness.IssueGuestMeetingToken(ctx, door, "Gia", "")
	if err != nil {
		t.Fatal(err)
	}
	gia := join(t, lk, guestToken)
	giaIdentity := ""
	eventually(t, "Gia to be in the call", func() bool {
		for _, id := range stillIn(ctx, room) {
			if len(id) > 6 && id[:6] == "guest-" {
				giaIdentity = id
				return true
			}
		}
		return false
	})
	if ok, err := guestBusiness.GuestMayJoin(ctx, room, giaIdentity); err != nil || !ok {
		t.Errorf("Gia in the invite's call: %v, %v", ok, err)
	}

	// Jonas, not in the channel, opens it signed in: in as himself, an invitee.
	entry, err := callInviteBusiness.Enter(ctx, callInviteBusiness.Door{Token: link.GuestToken}, jonas, "")
	if err != nil || entry.Token == "" || entry.Room != room || entry.Title != "Maya's call" || entry.Conversation != nil {
		t.Fatalf("Jonas at the invite: %+v, %v", entry, err)
	}
	jonasIn := enterCall(jonas, entry.Token)
	s, err := callStateBusiness.Current(ctx, callStateBusiness.Ref{Kind: "channel", ID: room})
	if err != nil || s == nil || s.Person(jonas.Uid) == nil || s.Person(jonas.Uid).Role != callStateModel.RoleInvitee {
		t.Fatalf("Jonas's part: %+v, %v", s, err)
	}
	if can := callControlBusiness.AbilitiesOf("channel", callStateModel.RoleInvitee); len(can) != 0 {
		t.Errorf("an invitee may %v", can)
	}
	if _, err := callInviteBusiness.Invite(ctx, room, jonas); !errors.Is(err, callInviteBusiness.ErrNotAllowed) {
		t.Errorf("an invitee's invite: %v", err)
	}
	// The connect check lets him stay.
	time.Sleep(1500 * time.Millisecond)
	if jonasIn.disconnected.Load() != nil {
		t.Fatalf("Jonas was put out: %v", jonasIn.disconnected.Load())
	}
	if ok, err := liveKitBusiness.MemberMayBeInCall(ctx, room, jonas.Uid); err != nil || !ok {
		t.Errorf("Jonas may be in the call: %v, %v", ok, err)
	}
	// He raises his own hand through the members' route, as anyone in a call
	// may, and lowers it; nobody else's (DESIGN-BUILD-4, 22 Oct).
	hand := func(a callControlBusiness.Action) error {
		_, err := callControlBusiness.Act(ctx, jonas, s.ID, a)
		return err
	}
	if err := hand(callControlBusiness.Action{Action: callControlBusiness.ActRaiseHand}); err != nil {
		t.Errorf("Jonas raises his hand: %v", err)
	}
	if now, err := callStateBusiness.Get(ctx, s.ID); err != nil || now.Person(jonas.Uid) == nil || now.Person(jonas.Uid).HandRaisedAt == nil {
		t.Errorf("Jonas's hand after raising it: %+v, %v", now, err)
	}
	if err := hand(callControlBusiness.Action{Action: callControlBusiness.ActLowerHand, Identity: maya.Uid}); !errors.Is(err, callControlBusiness.ErrNotAllowed) {
		t.Errorf("Jonas lowers Maya's hand: %v", err)
	}
	if err := hand(callControlBusiness.Action{Action: callControlBusiness.ActLowerHand}); err != nil {
		t.Errorf("Jonas lowers his hand: %v", err)
	}

	// The call's thread, at reply; the conversation and another of its threads, not found.
	post, other := uuid.New(), uuid.New()
	if _, err := pg.PG.Exec(`UPDATE call_sessions SET post_uuid = $1 WHERE room = $2 AND ended_at IS NULL`, post, room); err != nil {
		t.Fatal(err)
	}
	resolve := func(u *dgraphStruct.DgraphUser, kind, id string) (conversationBusiness.Ref, error) {
		ref := conversationBusiness.Ref{Kind: kind, ID: id}
		_, err := conversationBusiness.Resolve(ctx, info(u), ref)
		return ref, err
	}
	// Only through the call's thread surface (calls CALLS-3b, CHAT-LEAD-2):
	// asked by any other route, the thread isn't there for him.
	if d, err := conversationBusiness.Resolve(conversationBusiness.ForCallThread(ctx), info(jonas), conversationBusiness.Ref{Kind: conversationBusiness.KindThread, ID: post.String()}); err != nil ||
		d.Level != conversationBusiness.LevelReply || len(d.Reasons) != 1 || d.Reasons[0] != conversationBusiness.ReasonCallInvite {
		t.Errorf("the call's thread, on its surface: %+v, %v", d, err)
	}
	if _, err := resolve(jonas, conversationBusiness.KindThread, post.String()); !errors.Is(err, conversationBusiness.ErrNotFound) {
		t.Errorf("the call's thread through a member route: %v", err)
	}
	if d, err := conversationBusiness.Resolve(ctx, info(jonas), conversationBusiness.Ref{Kind: conversationBusiness.KindCall, ID: room}); err != nil || d.Level != conversationBusiness.LevelPost {
		t.Errorf("the call: %+v, %v", d, err)
	}
	for _, c := range []struct{ kind, id string }{{conversationBusiness.KindChannel, room}, {conversationBusiness.KindThread, other.String()}} {
		if _, err := resolve(jonas, c.kind, c.id); !errors.Is(err, conversationBusiness.ErrNotFound) {
			t.Errorf("Jonas on the conversation's %s %s: %v", c.kind, c.id, err)
		}
	}

	// A guest let in by the invite reaches nothing of the conversation either:
	// not its messages, not a thread (the call's own, until the call's post
	// is written, CALLS-3b), not a post of their own (CHAT-LEAD (c)).
	if _, err := guestBusiness.GetGuestChannel(ctx, door.Grant, time.Time{}); !errors.Is(err, guestBusiness.ErrForbidden) {
		t.Errorf("Gia on the channel: %v", err)
	}
	for _, id := range []uuid.UUID{post, other} {
		if _, err := guestBusiness.GetGuestThread(ctx, door.Grant, id.String()); !errors.Is(err, guestBusiness.ErrForbidden) {
			t.Errorf("Gia on a thread: %v", err)
		}
	}
	if err := guestBusiness.PostAsGuest(ctx, door.Grant, "Gia", "hello", ""); !errors.Is(err, guestBusiness.ErrForbidden) {
		t.Errorf("Gia posting in the channel: %v", err)
	}

	// Ben, in the conversation, opening the invite just joins its call.
	if entry, err := callInviteBusiness.Enter(ctx, callInviteBusiness.Door{Token: link.GuestToken}, ben, ""); err != nil || entry.Conversation == nil || entry.Token != "" {
		t.Errorf("Ben at the invite: %+v, %v", entry, err)
	}

	// Revoked: Gia and Jonas are put out; Maya and Ben stay; the thread closes.
	grantID := uuid.MustParse(link.GrantID)
	if err := guestBusiness.RevokeGrant(ctx, grantID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "Gia and Jonas to be put out", func() bool {
		return gia.disconnected.Load() != nil && jonasIn.disconnected.Load() != nil
	})
	if in := stillIn(ctx, room); len(in) != 2 {
		t.Errorf("still in after the revoke: %v", in)
	}
	if _, err := conversationBusiness.Resolve(conversationBusiness.ForCallThread(ctx), info(jonas), conversationBusiness.Ref{Kind: conversationBusiness.KindThread, ID: post.String()}); !errors.Is(err, conversationBusiness.ErrNotFound) {
		t.Errorf("the thread after the revoke: %v", err)
	}
	if ok, _ := liveKitBusiness.MemberMayBeInCall(ctx, room, jonas.Uid); ok {
		t.Error("Jonas may be in the call after the revoke")
	}

	// A second invite, then the call ends: it opens nothing, not the next call either.
	second, err := callInviteBusiness.Invite(ctx, room, maya)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := callInviteBusiness.Enter(ctx, callInviteBusiness.Door{Token: second.GuestToken}, jonas, ""); err != nil {
		t.Fatal(err)
	}
	if err := settingsBusiness.SetGuestAccessEnabled(false); err != nil {
		t.Fatal(err)
	}
	if ok, _ := liveKitBusiness.MemberMayBeInCall(ctx, room, jonas.Uid); ok {
		t.Error("Jonas may be in the call with guest access off")
	}
	if err := settingsBusiness.SetGuestAccessEnabled(true); err != nil {
		t.Fatal(err)
	}
	s, _ = callStateBusiness.Current(ctx, callStateBusiness.Ref{Kind: "channel", ID: room})
	callControlBusiness.OnEnded(ctx, room, s.RoomSID, callStateModel.EndedByHost)
	if _, err := guestBusiness.OpenDoor(ctx, second.GuestToken); !errors.Is(err, guestBusiness.ErrInvalidGrant) {
		t.Errorf("an invite after its call: %v", err)
	}
	if _, err := callInviteBusiness.Enter(ctx, callInviteBusiness.Door{Token: second.GuestToken}, jonas, ""); !guestBusiness.IsUnavailable(err) {
		t.Errorf("Jonas at an invite after its call: %v", err)
	}
	if rooms, err := liveKitBusiness.RoomsAdmittedTo(ctx, uuid.MustParse(jonas.Uuid)); err != nil || len(rooms) != 0 {
		t.Errorf("Jonas's calls after the call: %v, %v", rooms, err)
	}
	// The next call in the channel: Maya starts it; the old invite opens nothing.
	mayaToken, _, err = liveKitBusiness.CreateRoomAndGetToken(ctx, room, maya, false, "")
	if err != nil {
		t.Fatal(err)
	}
	connectedAgain := join(t, lk, mayaToken)
	joinSettled(t, room, maya.Uid, connectedAgain)
	connected(t, ctx, room, maya.Uid)
	if _, err := guestBusiness.OpenDoor(ctx, second.GuestToken); !errors.Is(err, guestBusiness.ErrInvalidGrant) {
		t.Errorf("the old invite on the next call: %v", err)
	}
	if _, err := resolve(jonas, conversationBusiness.KindCall, room); !errors.Is(err, conversationBusiness.ErrNotFound) {
		t.Errorf("Jonas on the next call: %v", err)
	}
}

// A colleague at a room's address comes in as themselves, skipping the lobby,
// for as long as the address answers: with rooms turned off they may not stay.
// A room with a passcode asks it of them too.
func TestCallAMemberAtARoomsAddressComesInAsThemselves(t *testing.T) {
	ctx := context.Background()
	t.Setenv("JWT_SECRET", "call-invite-integration-secret")
	pg := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, pg.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	dg := integration.SetupDgraph(t)
	integration.StubMqttClient(t)
	integration.SetupLiveKit(t)
	if err := settingsBusiness.SetGuestAccessEnabled(true); err != nil {
		t.Fatal(err)
	}
	if err := settingsBusiness.SetGuestRoomsEnabled(true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = settingsBusiness.SetGuestRoomsEnabled(true)
		_ = settingsBusiness.SetGuestAccessEnabled(false)
	})
	people := seedChannel(t, pg, dg, uuid.NewString(), "maya", "ben", "outsider")
	maya, ben := people["maya"], people["ben"]

	room, err := callRoomBusiness.Open(ctx, uuid.MustParse(maya.Uuid))
	if err != nil {
		t.Fatal(err)
	}
	entry, err := callInviteBusiness.Enter(ctx, callInviteBusiness.Door{Slug: room.Slug}, ben, "")
	if err != nil || entry.Token == "" || entry.Room != room.Room || entry.Title != "Maya's room" {
		t.Fatalf("Ben at Maya's room: %+v, %v", entry, err)
	}
	if ok, err := liveKitBusiness.MemberMayBeInCall(ctx, room.Room, ben.Uid); err != nil || !ok {
		t.Errorf("Ben may be in Maya's room: %v, %v", ok, err)
	}
	if err := settingsBusiness.SetGuestRoomsEnabled(false); err != nil {
		t.Fatal(err)
	}
	if ok, _ := liveKitBusiness.MemberMayBeInCall(ctx, room.Room, ben.Uid); ok {
		t.Error("Ben may be in Maya's room with rooms off")
	}
	if err := settingsBusiness.SetGuestRoomsEnabled(true); err != nil {
		t.Fatal(err)
	}

	// A passcode: asked of members too.
	if _, err := callRoomBusiness.Change(ctx, uuid.MustParse(maya.Uuid), callRoomBusiness.Changes{Passcode: ptrTo("432198")}); err != nil {
		t.Fatal(err)
	}
	if _, err := callInviteBusiness.Enter(ctx, callInviteBusiness.Door{Slug: room.Slug}, ben, ""); !errors.Is(err, callRoomBusiness.ErrLocked) {
		t.Errorf("Ben at a locked room: %v", err)
	}
}

func ptrTo[T any](v T) *T { return &v }
