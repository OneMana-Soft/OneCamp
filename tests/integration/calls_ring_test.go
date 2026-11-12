//go:build integration

package integration_test

// Ring in DMs (slack-06 H2): a DM's or a small group's call rings its people
// on their own activity topic (MESSAGE_CALL_RING) when it starts, with who
// calls, where, and until when; never the caller, a bot or an external guest,
// and quietly for someone who muted the conversation. Joining stops the ring
// for whoever joined, and the end for everyone rung. A member who says no
// (POST /calls/{session_id}/decline) stops their own ring, tells the starter,
// and is named on the ended call's card; nobody outside the conversation,
// nobody in the call and nobody after the call ends may.
//
// Calls are started and ended as LiveKit's webhooks do (Call State's Joined
// and End); what the server publishes is read from the one recorder
// (integration.RecordMqtt).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	chatBusiness "github.com/akashc777/OneCamp/business/Chat"
	callsController "github.com/akashc777/OneCamp/controllers/Calls"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	callStateModel "github.com/akashc777/OneCamp/models/postgres/CallState"
	"github.com/akashc777/OneCamp/tests/integration"
)

// ringFace and ringMsg are MESSAGE_CALL_RING as the web app reads it.
type ringFace struct {
	UUID    string `json:"uuid"`
	Name    string `json:"name"`
	Profile string `json:"profile"`
}

type ringMsg struct {
	State    string    `json:"state"`
	Session  string    `json:"session"`
	Caller   *ringFace `json:"caller"`
	Resource *struct {
		Kind string `json:"kind"`
		UUID string `json:"uuid"`
	} `json:"resource"`
	RingUntil *time.Time `json:"ring_until"`
	Quiet     bool       `json:"quiet"`
	By        *ringFace  `json:"by"`
	People    []struct {
		ringFace
		Quiet bool `json:"quiet"`
	} `json:"people"`
}

// ringMember is someone a test seeds: a bot, an external guest, someone who
// muted the conversation, or someone outside it.
type ringMember struct {
	name                    string
	bot, guest, muted, away bool
}

func ringEnv(t *testing.T) (context.Context, *integration.Env, *integration.DgraphEnv, *integration.MqttRecorder) {
	t.Helper()
	ctx := context.Background()
	pg := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, pg.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	dg := integration.SetupDgraph(t)
	return ctx, pg, dg, integration.RecordMqtt(t)
}

// seedRingConversation makes a DM (group false: its first two members) or a
// group of the members not away, each a person in Postgres and the graph
// named as their name title-cased, with the conversation's setting: "all", or
// "mention" for one who muted it. It answers the conversation's grouping id
// and everyone by name.
func seedRingConversation(t *testing.T, pg *integration.Env, dg *integration.DgraphEnv, group bool, members ...ringMember) (string, map[string]*dgraphStruct.DgraphUser) {
	t.Helper()
	ids := map[string]uuid.UUID{}
	var nodes, in []map[string]any
	var inIDs []string
	for _, m := range members {
		id := uuid.New()
		ids[m.name] = id
		if _, err := pg.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, is_bot, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, NOW(), NOW())`,
			id, m.name+"-"+id.String()[:8]+"@example.test", m.name+"-"+id.String()[:8], title(m.name), m.bot); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + m.name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": title(m.name),
			"is_bot": m.bot, "is_external": m.bot || m.guest})
		if !m.away {
			in = append(in, map[string]any{"uid": "_:" + m.name})
			inIDs = append(inIDs, id.String())
		}
	}
	grp := strings.ReplaceAll(uuid.NewString(), "-", "")
	if !group {
		if len(inIDs) != 2 {
			t.Fatalf("a DM is two people, not %v", inIDs)
		}
		grp = helpers.GetGroupingId(inIDs[0], inIDs[1])
	}
	nodes = append(nodes, map[string]any{"uid": "_:grp", "dgraph.type": "Dm", "dm_grouping_id": grp, "dm_participants": in})
	uids := dg.Mutate(t, nodes)
	out := map[string]*dgraphStruct.DgraphUser{}
	for _, m := range members {
		out[m.name] = &dgraphStruct.DgraphUser{Uid: uids[m.name], Uuid: ids[m.name].String(), UserName: title(m.name), IsBot: m.bot}
		if m.away || m.bot {
			continue
		}
		level := "all"
		if m.muted {
			level = "mention"
		}
		if _, err := pg.PG.Exec(`INSERT INTO users_chat_notification (user_id, grp_id, notification_type) VALUES ($1, $2, $3) ON CONFLICT (user_id, grp_id) DO NOTHING`, ids[m.name], grp, level); err != nil {
			t.Fatal(err)
		}
	}
	return grp, out
}

// ringRole is the part a member takes in a call: its starter hosts it.
func ringRole(_ context.Context, _ callStateBusiness.Ref, _ callStateBusiness.JoinEvent, starter bool) string {
	if starter {
		return callStateModel.RoleHost
	}
	return callStateModel.RoleParticipant
}

// ringJoin has who join the call in the conversation grp, as LiveKit's
// webhook tells it, starting it when none is going on.
func ringJoin(t *testing.T, ctx context.Context, grp string, who *dgraphStruct.DgraphUser) *callStateBusiness.Session {
	t.Helper()
	id := uuid.MustParse(who.Uuid)
	s, _, err := callStateBusiness.Joined(ctx, callStateBusiness.JoinEvent{Room: grp, Identity: who.Uid, Name: who.UserName, UserUUID: &id, At: time.Now()}, ringRole)
	if err != nil || s == nil {
		t.Fatalf("%s joining the call in %s: %+v, %v", who.UserName, grp, s, err)
	}
	return s
}

// ringsTo is every MESSAGE_CALL_RING told to who so far, in order.
func ringsTo(t *testing.T, rec *integration.MqttRecorder, who *dgraphStruct.DgraphUser) []ringMsg {
	t.Helper()
	var out []ringMsg
	for _, raw := range rec.Messages(helpers.GetMqttTopicForUserActivity(who.Uuid), mqttStruct.MESSAGE_CALL_RING) {
		var m ringMsg
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("a ring to %s that doesn't read: %s (%v)", who.UserName, raw, err)
		}
		out = append(out, m)
	}
	return out
}

// statesOf is the states of these messages about one call, in order.
func statesOf(msgs []ringMsg, session string) string {
	var out []string
	for _, m := range msgs {
		if m.Session == session {
			out = append(out, m.State)
		}
	}
	return strings.Join(out, ",")
}

// decline asks POST /calls/{session_id}/decline as who.
func decline(t *testing.T, who *dgraphStruct.DgraphUser, session string) (int, string) {
	t.Helper()
	code, body := callRead(t, callsController.DeclineCall, http.MethodPost, "/calls/"+session+"/decline", who, "", session)
	return code, string(body["msg"]) + string(body["code"])
}

func TestADMOrGroupCallRingsItsPeople(t *testing.T) {
	ctx, pg, dg, rec := ringEnv(t)

	t.Run("a DM rings the other person, saying who calls, where, and until when", func(t *testing.T) {
		grp, p := seedRingConversation(t, pg, dg, false, ringMember{name: "maya"}, ringMember{name: "jonas"})
		s := ringJoin(t, ctx, grp, p["maya"])
		settle(t)
		got := ringsTo(t, rec, p["jonas"])
		if len(got) != 1 {
			t.Fatalf("Jonas was told %d times (%+v), want one ring", len(got), got)
		}
		r := got[0]
		if r.State != "ring" || r.Session != s.ID || r.Quiet {
			t.Errorf("Jonas's ring: %+v, want a loud ring for call %s", r, s.ID)
		}
		if r.Caller == nil || r.Caller.UUID != p["maya"].Uuid || r.Caller.Name != "Maya" {
			t.Errorf("the caller: %+v, want Maya", r.Caller)
		}
		if r.Resource == nil || r.Resource.Kind != "dm" || r.Resource.UUID != grp {
			t.Errorf("where: %+v, want the DM %q", r.Resource, grp)
		}
		if r.RingUntil == nil || r.RingUntil.Sub(s.StartedAt) < 30*time.Second-time.Millisecond || r.RingUntil.Sub(s.StartedAt) > 30*time.Second+time.Millisecond {
			t.Errorf("rings until %v, want 30 s after the call started (%v)", r.RingUntil, s.StartedAt)
		}
		told := ringsTo(t, rec, p["maya"])
		if len(told) != 1 || told[0].State != "ringing" || told[0].Session != s.ID || len(told[0].People) != 1 ||
			told[0].People[0].UUID != p["jonas"].Uuid || told[0].People[0].Name != "Jonas" || told[0].People[0].Quiet ||
			told[0].RingUntil == nil || !told[0].RingUntil.Equal(*r.RingUntil) {
			t.Errorf("the caller was told %+v, want that the call rings Jonas until %v", told, r.RingUntil)
		}
	})

	t.Run("a group rings its people but the caller, a bot and a guest; quietly whoever muted it", func(t *testing.T) {
		grp, p := seedRingConversation(t, pg, dg, true, ringMember{name: "maya"}, ringMember{name: "jonas"}, ringMember{name: "ana", muted: true},
			ringMember{name: "atlas", bot: true}, ringMember{name: "gus", guest: true}, ringMember{name: "olga", away: true})
		s := ringJoin(t, ctx, grp, p["maya"])
		settle(t)
		for name, quiet := range map[string]bool{"jonas": false, "ana": true} {
			got := ringsTo(t, rec, p[name])
			if len(got) != 1 || got[0].State != "ring" || got[0].Session != s.ID || got[0].Quiet != quiet ||
				got[0].Resource == nil || got[0].Resource.Kind != "group" || got[0].Resource.UUID != grp || got[0].Caller == nil || got[0].Caller.UUID != p["maya"].Uuid {
				t.Errorf("%s was told %+v, want one ring for the group's call, quiet %v", name, got, quiet)
			}
		}
		told := ringsTo(t, rec, p["maya"])
		if len(told) != 1 || told[0].State != "ringing" || len(told[0].People) != 2 || told[0].People[0].Quiet == told[0].People[1].Quiet {
			t.Errorf("Maya, the caller, was told %+v, want that it rings Jonas and Ana (quietly)", told)
		}
		for _, f := range append(told, ringMsg{})[0].People {
			if f.UUID != p["jonas"].Uuid && f.UUID != p["ana"].Uuid || f.Quiet != (f.UUID == p["ana"].Uuid) {
				t.Errorf("Maya was told it rings %+v", f)
			}
		}
		for _, name := range []string{"atlas", "gus", "olga"} {
			if st := statesOf(ringsTo(t, rec, p[name]), s.ID); st != "" {
				t.Errorf("%s was told %q, want nothing", name, st)
			}
		}
	})
}

func TestARingStopsWhenTheyJoinOrTheCallEnds(t *testing.T) {
	ctx, pg, dg, rec := ringEnv(t)
	grp, p := seedRingConversation(t, pg, dg, true, ringMember{name: "maya"}, ringMember{name: "jonas"}, ringMember{name: "ana"})
	s := ringJoin(t, ctx, grp, p["maya"])
	settle(t)
	ringJoin(t, ctx, grp, p["jonas"])
	settle(t)
	if st := statesOf(ringsTo(t, rec, p["jonas"]), s.ID); st != "ring,stop" {
		t.Errorf("Jonas, joining: told %q, want the ring then stop (his other devices)", st)
	}
	if st := statesOf(ringsTo(t, rec, p["ana"]), s.ID); st != "ring" {
		t.Errorf("Ana, while Jonas joins: told %q, want the ring alone", st)
	}
	if _, did, err := callStateBusiness.End(ctx, grp, "", callStateModel.EndedLastLeft); err != nil || !did {
		t.Fatalf("the end: %v %v", did, err)
	}
	settle(t)
	for name, want := range map[string]string{"jonas": "ring,stop,stop", "ana": "ring,stop", "maya": "ringing,stop"} {
		if st := statesOf(ringsTo(t, rec, p[name]), s.ID); st != want {
			t.Errorf("%s, after the end: told %q, want %q", name, st, want)
		}
	}
}

// A ring again rings the call's starter when they have left it, and that
// ring stops as any other: when they join again, or at the end. Their first
// join, which started the call, stops nothing (nothing rang them).
func TestARingAgainToItsStarterStops(t *testing.T) {
	ctx, pg, dg, rec := ringEnv(t)
	for _, then := range []string{"rejoins", "the call ends"} {
		t.Run("the starter is rung again, then "+then, func(t *testing.T) {
			grp, p := seedRingConversation(t, pg, dg, true, ringMember{name: "maya"}, ringMember{name: "jonas"}, ringMember{name: "ana"})
			// A call Maya started a minute ago, so its first ring is over.
			session := uuid.New()
			if _, err := pg.PG.Exec(`INSERT INTO call_sessions (id, room, kind, resource_kind, resource_id, started_by, started_at)
				VALUES ($1, $2, 'huddle', 'group', $2, $3, NOW() - interval '1 minute')`, session, grp, p["maya"].Uuid); err != nil {
				t.Fatal(err)
			}
			id := session.String()
			ringJoin(t, ctx, grp, p["maya"])
			ringJoin(t, ctx, grp, p["jonas"])
			mayaID := uuid.MustParse(p["maya"].Uuid)
			if _, left, err := callStateBusiness.Left(ctx, callStateBusiness.JoinEvent{Room: grp, Identity: p["maya"].Uid, Name: p["maya"].UserName, UserUUID: &mayaID, At: time.Now()}); err != nil || !left {
				t.Fatalf("Maya leaving: %v %v", left, err)
			}
			settle(t)
			if st := statesOf(ringsTo(t, rec, p["maya"]), id); st != "" {
				t.Errorf("Maya, the starter, before the ring again: told %q, want nothing (her first join stops nothing)", st)
			}
			if code, msg, rung := ringAgain(t, p["jonas"], id); code != http.StatusOK || rung != 2 {
				t.Fatalf("Jonas rings again: %d %s, rang %d, want 200 with Maya and Ana rung", code, msg, rung)
			}
			settle(t)
			if st := statesOf(ringsTo(t, rec, p["maya"]), id); st != "ring" {
				t.Fatalf("Maya, out of the call: told %q, want the ring again", st)
			}
			if then == "rejoins" {
				ringJoin(t, ctx, grp, p["maya"])
			} else if _, did, err := callStateBusiness.End(ctx, grp, "", callStateModel.EndedLastLeft); err != nil || !did {
				t.Fatalf("the end: %v %v", did, err)
			}
			settle(t)
			if st := statesOf(ringsTo(t, rec, p["maya"]), id); st != "ring,stop" {
				t.Errorf("Maya, rung again, then %s: told %q, want the ring again, then stop on her other devices", then, st)
			}
		})
	}
}

func TestADeclineStopsTheRingAndTellsTheStarter(t *testing.T) {
	ctx, pg, dg, rec := ringEnv(t)
	grp, p := seedRingConversation(t, pg, dg, true, ringMember{name: "maya"}, ringMember{name: "jonas"}, ringMember{name: "ana"}, ringMember{name: "olga", away: true})
	s := ringJoin(t, ctx, grp, p["maya"])
	settle(t)
	declines := func(who string) int {
		var n int
		if err := pg.PG.QueryRow(`SELECT count(*) FROM call_declines WHERE session_id = $1 AND user_uuid = $2`, s.ID, p[who].Uuid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	if code, msg := decline(t, p["jonas"], s.ID); code != http.StatusOK {
		t.Fatalf("Jonas declines: %d %s", code, msg)
	}
	settle(t)
	if st := statesOf(ringsTo(t, rec, p["jonas"]), s.ID); st != "ring,stop" {
		t.Errorf("Jonas, declining: told %q, want the ring then stop", st)
	}
	told := ringsTo(t, rec, p["maya"])
	if statesOf(told, s.ID) != "ringing,declined" || told[len(told)-1].By == nil || told[len(told)-1].By.UUID != p["jonas"].Uuid || told[len(told)-1].By.Name != "Jonas" {
		t.Errorf("Maya, the starter, was told %+v, want that Jonas declined", told)
	}
	if st := statesOf(ringsTo(t, rec, p["ana"]), s.ID); st != "ring" {
		t.Errorf("Ana was told %q of Jonas's decline", st)
	}

	// A repeat answers the same and changes nothing.
	if code, msg := decline(t, p["jonas"], s.ID); code != http.StatusOK {
		t.Errorf("Jonas declines again: %d %s, want 200", code, msg)
	}
	settle(t)
	if n := declines("jonas"); n != 1 {
		t.Errorf("Jonas's declines kept: %d, want one", n)
	}
	if st := statesOf(ringsTo(t, rec, p["maya"]), s.ID); st != "ringing,declined" {
		t.Errorf("Maya after the repeat: told %q, want one decline", st)
	}
	if st := statesOf(ringsTo(t, rec, p["jonas"]), s.ID); st != "ring,stop" {
		t.Errorf("Jonas after the repeat: told %q, want one stop", st)
	}

	// Nobody outside the conversation, and no call that isn't a DM's or a
	// group's going on, has anything to decline; someone in the call is
	// refused.
	if code, msg := decline(t, p["olga"], s.ID); code != http.StatusNotFound {
		t.Errorf("Olga, outside the group: %d %s, want 404", code, msg)
	}
	if code, msg := decline(t, p["maya"], s.ID); code != http.StatusConflict {
		t.Errorf("Maya, in the call: %d %s, want 409", code, msg)
	}
	for what, id := range map[string]string{"no such call": uuid.NewString(), "not a call's id": "not-a-call"} {
		if code, msg := decline(t, p["jonas"], id); code != http.StatusNotFound {
			t.Errorf("%s: %d %s, want 404", what, code, msg)
		}
	}
	channelCall, channel := uuid.New(), uuid.NewString()
	if _, err := pg.PG.Exec(`INSERT INTO call_sessions (id, room, kind, resource_kind, resource_id, started_at) VALUES ($1, $2, 'huddle', 'channel', $2, NOW())`, channelCall, channel); err != nil {
		t.Fatal(err)
	}
	if code, msg := decline(t, p["jonas"], channelCall.String()); code != http.StatusNotFound {
		t.Errorf("a channel's call: %d %s, want 404", code, msg)
	}
	if _, did, err := callStateBusiness.End(ctx, grp, "", callStateModel.EndedLastLeft); err != nil || !did {
		t.Fatalf("the end: %v %v", did, err)
	}
	settle(t)
	if code, msg := decline(t, p["ana"], s.ID); code != http.StatusNotFound {
		t.Errorf("Ana, after the call ended: %d %s, want 404", code, msg)
	}
	if n := declines("ana") + declines("olga") + declines("maya"); n != 0 {
		t.Errorf("%d refused declines were kept", n)
	}
}

// The ended call's card names who declined it and who started it, written
// once with its other facts at the end.
func TestTheEndedCallsCardSaysWhoDeclined(t *testing.T) {
	ctx, pg, dg, _ := ringEnv(t)
	grp, p := seedRingConversation(t, pg, dg, true, ringMember{name: "maya"}, ringMember{name: "jonas"}, ringMember{name: "ana"})
	session, post := uuid.New(), uuid.NewString()
	dg.Mutate(t, []map[string]any{{"uid": "_:c", "dgraph.type": "Chat", "chat_uuid": post, "chat_kind": "huddle", "chat_system": fmt.Sprintf(`{"session":%q}`, session)}})
	if _, err := pg.PG.Exec(`INSERT INTO call_sessions (id, room, kind, resource_kind, resource_id, started_by, started_at, post_uuid)
		VALUES ($1, $2, 'huddle', 'group', $2, $3, NOW() - interval '1 minute', $4)`, session, grp, p["maya"].Uuid, post); err != nil {
		t.Fatal(err)
	}
	if code, msg := decline(t, p["jonas"], session.String()); code != http.StatusOK {
		t.Fatalf("Jonas declines: %d %s", code, msg)
	}
	if _, did, err := callStateBusiness.End(ctx, grp, "", callStateModel.EndedLastLeft); err != nil || !did {
		t.Fatalf("the end: %v %v", did, err)
	}
	settle(t)
	var got struct {
		C []struct {
			System string `json:"chat_system"`
		} `json:"c"`
	}
	if err := json.Unmarshal(dg.Query(t, fmt.Sprintf(`{ c(func: eq(chat_uuid, %q)) { chat_system } }`, post)), &got); err != nil || len(got.C) != 1 {
		t.Fatalf("the card: %+v, %v", got, err)
	}
	var facts struct {
		Ended    *time.Time `json:"ended"`
		By       string     `json:"by"`
		Declined []struct {
			UUID string `json:"uuid"`
			Name string `json:"name"`
		} `json:"declined"`
	}
	if err := json.Unmarshal([]byte(got.C[0].System), &facts); err != nil || facts.Ended == nil {
		t.Fatalf("the card's facts: %q, %v", got.C[0].System, err)
	}
	if len(facts.Declined) != 1 || facts.Declined[0].UUID != p["jonas"].Uuid || facts.Declined[0].Name != "Jonas" {
		t.Errorf("the card's declines: %+v, want Jonas", facts.Declined)
	}
	// Who started it, so the card offers its starter "Call again", never
	// "Call back".
	if facts.By != p["maya"].Uuid {
		t.Errorf("the card's starter: %q, want Maya's %q", facts.By, p["maya"].Uuid)
	}
}

// call_declines (576) goes down and up again on Postgres 12, and keeps one
// row a person a call.
func TestCallDeclinesMigrationUpDownUp(t *testing.T) {
	env := integration.SetupEnv(t)
	exists := func() bool {
		var n int
		if err := env.PG.QueryRow(`SELECT count(*) FROM information_schema.tables WHERE table_name = 'call_declines'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	if _, err := env.PG.Exec(migrationSQL(t, "call_declines", "down")); err != nil || exists() {
		t.Fatalf("down: %v, the table still there %v", err, exists())
	}
	if _, err := env.PG.Exec(migrationSQL(t, "call_declines", "up")); err != nil || !exists() {
		t.Fatalf("up again: %v, the table there %v", err, exists())
	}
	session, user := uuid.New(), uuid.New()
	insert := `INSERT INTO call_declines (session_id, user_uuid, declined_at) VALUES ($1, $2, NOW())`
	if _, err := env.PG.Exec(insert, session, user); err != nil {
		t.Fatal(err)
	}
	if _, err := env.PG.Exec(insert, session, user); err == nil {
		t.Error("a second decline by one person of one call was kept")
	}
	if _, err := env.PG.Exec(insert, session, uuid.New()); err != nil {
		t.Errorf("another person's decline of the call: %v", err)
	}
}

// ringAgain asks POST /calls/{session_id}/ring as who, and answers its
// status, its words and code, and how many it rang (-1 for no answer).
func ringAgain(t *testing.T, who *dgraphStruct.DgraphUser, session string) (int, string, int) {
	t.Helper()
	code, body := callRead(t, callsController.RingCall, http.MethodPost, "/calls/"+session+"/ring", who, "", session)
	data := struct {
		Rung *int `json:"rung"`
	}{}
	_ = json.Unmarshal(body["data"], &data)
	rung := -1
	if data.Rung != nil {
		rung = *data.Rung
	}
	return code, string(body["msg"]) + string(body["code"]), rung
}

// Someone in a DM's or group's call rings its people again: whoever isn't in
// it, a decliner too (ringing again is the caller's own ask, and the decline
// stays recorded), each for 30 s from now, and the asker is told who. Never
// while a ring still rings, at most RingAgainMax times, only from in the
// call, and only by a member.
func TestSomeoneInTheCallRingsItsPeopleAgain(t *testing.T) {
	ctx, pg, dg, rec := ringEnv(t)
	grp, p := seedRingConversation(t, pg, dg, true, ringMember{name: "maya"}, ringMember{name: "jonas"}, ringMember{name: "ana"}, ringMember{name: "olga", away: true})
	// A call that started a minute ago, so its first ring is over; Maya is in it.
	session := uuid.New()
	if _, err := pg.PG.Exec(`INSERT INTO call_sessions (id, room, kind, resource_kind, resource_id, started_by, started_at)
		VALUES ($1, $2, 'huddle', 'group', $2, $3, NOW() - interval '1 minute')`, session, grp, p["maya"].Uuid); err != nil {
		t.Fatal(err)
	}
	if s := ringJoin(t, ctx, grp, p["maya"]); s.ID != session.String() {
		t.Fatalf("Maya joined call %s, want %s", s.ID, session)
	}
	id := session.String()
	if code, msg := decline(t, p["jonas"], id); code != http.StatusOK {
		t.Fatalf("Jonas declines: %d %s", code, msg)
	}
	settle(t)

	before := time.Now()
	if code, msg, rung := ringAgain(t, p["maya"], id); code != http.StatusOK || rung != 2 {
		t.Fatalf("Maya rings again: %d %s, rang %d, want 200 with two rung", code, msg, rung)
	}
	settle(t)
	for _, name := range []string{"jonas", "ana"} {
		got := append([]ringMsg{{}}, ringsTo(t, rec, p[name])...)
		last := got[len(got)-1]
		if last.State != "ring" || last.Session != id || last.Caller == nil || last.Caller.UUID != p["maya"].Uuid ||
			last.RingUntil == nil || last.RingUntil.Before(before.Add(30*time.Second-time.Second)) {
			t.Errorf("%s was told %+v, want a ring from Maya for 30 s from now", name, last)
		}
	}
	if st := statesOf(ringsTo(t, rec, p["jonas"]), id); st != "stop,ring" {
		t.Errorf("Jonas, the decliner: told %q, want his decline's stop, then the ring again", st)
	}
	told := ringsTo(t, rec, p["maya"])
	if st := statesOf(told, id); st != "declined,ringing" || len(told[len(told)-1].People) != 2 {
		t.Errorf("Maya: told %q (%+v), want Jonas's decline, then that it rings Jonas and Ana", st, told)
	}
	if st := statesOf(ringsTo(t, rec, p["olga"]), id); st != "" {
		t.Errorf("Olga, outside the group, was told %q", st)
	}

	// Jonas declines the ring again: his devices stop, and Maya hears it again.
	if code, msg := decline(t, p["jonas"], id); code != http.StatusOK {
		t.Fatalf("Jonas declines again: %d %s", code, msg)
	}
	settle(t)
	if st := statesOf(ringsTo(t, rec, p["jonas"]), id); st != "stop,ring,stop" {
		t.Errorf("Jonas, declining the ring again: told %q", st)
	}
	if st := statesOf(ringsTo(t, rec, p["maya"]), id); st != "declined,ringing,declined" {
		t.Errorf("Maya, as Jonas declines the ring again: told %q", st)
	}

	// While it rings, no ring again.
	if code, msg, _ := ringAgain(t, p["maya"], id); code != http.StatusTooManyRequests || !strings.Contains(msg, "still_ringing") {
		t.Errorf("Maya rings again while it rings: %d %s, want 429 still_ringing", code, msg)
	}
	// Twice more once each ring is over, then never again.
	call := chatBusiness.RingingCall{Session: id, Kind: "group", Resource: grp, InCall: []string{p["maya"].Uuid}}
	for i, later := range []time.Duration{31 * time.Second, 62 * time.Second} {
		if n, err := chatBusiness.RingAgain(ctx, call, p["maya"].Uuid, time.Now().Add(later)); err != nil || n != 2 {
			t.Errorf("ring again %d: %d %v, want 2 rung", i+2, n, err)
		}
	}
	if _, err := chatBusiness.RingAgain(ctx, call, p["maya"].Uuid, time.Now().Add(5*time.Minute)); !errors.Is(err, chatBusiness.ErrRingLimit) {
		t.Errorf("a fourth ring again: %v, want the limit", err)
	}
	if code, msg, _ := ringAgain(t, p["maya"], id); code != http.StatusTooManyRequests || !strings.Contains(msg, "ring_limit") {
		t.Errorf("Maya rings again past the limit: %d %s, want 429 ring_limit", code, msg)
	}

	// Only someone in the call, and only a member, may; and only a DM's or a
	// group's call going on.
	if code, msg, _ := ringAgain(t, p["ana"], id); code != http.StatusConflict || !strings.Contains(msg, "not_in_call") {
		t.Errorf("Ana, not in the call: %d %s, want 409 not_in_call", code, msg)
	}
	if code, msg, _ := ringAgain(t, p["olga"], id); code != http.StatusNotFound {
		t.Errorf("Olga, outside the group: %d %s, want 404", code, msg)
	}
	if code, msg, _ := ringAgain(t, p["maya"], uuid.NewString()); code != http.StatusNotFound {
		t.Errorf("no such call: %d %s, want 404", code, msg)
	}
	if _, did, err := callStateBusiness.End(ctx, grp, "", callStateModel.EndedLastLeft); err != nil || !did {
		t.Fatalf("the end: %v %v", did, err)
	}
	settle(t)
	if code, msg, _ := ringAgain(t, p["maya"], id); code != http.StatusNotFound {
		t.Errorf("after the end: %d %s, want 404", code, msg)
	}
}

// Everyone already in: nothing rings, and the ring again isn't counted.
func TestRingingAgainWithEveryoneInRingsNobody(t *testing.T) {
	ctx, pg, dg, rec := ringEnv(t)
	grp, p := seedRingConversation(t, pg, dg, false, ringMember{name: "maya"}, ringMember{name: "jonas"})
	session := uuid.New()
	if _, err := pg.PG.Exec(`INSERT INTO call_sessions (id, room, kind, resource_kind, resource_id, started_by, started_at)
		VALUES ($1, $2, 'huddle', 'dm', $2, $3, NOW() - interval '1 minute')`, session, grp, p["maya"].Uuid); err != nil {
		t.Fatal(err)
	}
	ringJoin(t, ctx, grp, p["maya"])
	ringJoin(t, ctx, grp, p["jonas"])
	settle(t)
	for i := 0; i < RingAgainTries; i++ {
		if code, msg, rung := ringAgain(t, p["maya"], session.String()); code != http.StatusOK || rung != 0 {
			t.Fatalf("ring again %d with everyone in: %d %s, rang %d, want 200 with nobody rung", i+1, code, msg, rung)
		}
	}
	settle(t)
	if st := statesOf(ringsTo(t, rec, p["jonas"]), session.String()); st != "stop" {
		t.Errorf("Jonas, in the call, was told %q, want his join's stop alone", st)
	}
}

// RingAgainTries is more ring agains than the limit, none of which counts.
const RingAgainTries = chatBusiness.RingAgainMax + 1
