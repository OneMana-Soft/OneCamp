//go:build integration
// +build integration

package integration_test

// zoom-15: a call's beginning and end reach the workspace's outgoing webhooks
// (business/CallState webhooks.go), against real Postgres 12, Dgraph and Redis:
// the call's record, the events' bus, each webhook from its table, and each
// owner's reach by the app's own rules (a channel's readers; a meeting's
// host, by the meeting's link, and nobody else). What would be sent is
// observed in place of sending it (business/Webhook ObserveDeliveriesForTest).
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestACallsBeginningAndEnd' -v

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	callStateModel "github.com/akashc777/OneCamp/models/postgres/CallState"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestACallsBeginningAndEndReachOnlyWebhooksWhoseOwnersMayReadIt(t *testing.T) {
	ctx := context.Background()
	pg := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, pg.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	dg := integration.SetupDgraph(t)
	integration.StubMqttClient(t)
	t.Cleanup(firebaseInit.ObservePushesForTest(func(map[string]string, []string) {}))

	type delivery struct {
		webhook uuid.UUID
		kind    string
		data    map[string]any
	}
	var mu sync.Mutex
	var sent []delivery
	t.Cleanup(webhookBusiness.ObserveDeliveriesForTest(func(id uuid.UUID, kind string, payload []byte) {
		var body struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(payload, &body); err != nil {
			t.Errorf("a delivery's body doesn't read: %v", err)
		}
		mu.Lock()
		sent = append(sent, delivery{id, kind, body.Data})
		mu.Unlock()
	}))
	// What webhook was sent of a call: its deliveries of that session, by kind.
	of := func(webhook uuid.UUID, session string) map[string]map[string]any {
		mu.Lock()
		defer mu.Unlock()
		out := map[string]map[string]any{}
		for _, d := range sent {
			if d.webhook == webhook && d.data["session_id"] == session {
				out[d.kind] = d.data
			}
		}
		return out
	}

	channel := uuid.NewString()
	people := seedChannel(t, pg, dg, channel, "maya", "outsider")
	maya, outsider := uuid.MustParse(people["maya"].Uuid), uuid.MustParse(people["outsider"].Uuid)
	hook := func(name string, owner uuid.UUID) uuid.UUID {
		t.Helper()
		id := uuid.New()
		if _, err := pg.PG.Exec(`INSERT INTO webhooks (id, name, type, token_hash, target_url, events, created_by, scope_type) VALUES ($1, $2, 'outgoing', $3, $4, '["call.started","call.ended"]', $5, 'org')`,
			id, name, "hash-"+id.String(), "https://hooks.example.com/"+name, owner); err != nil {
			t.Fatal(err)
		}
		return id
	}
	mayas, outsiders := hook("mayas", maya), hook("outsiders", outsider)

	role := func(_ context.Context, _ callStateBusiness.Ref, e callStateBusiness.JoinEvent, starter bool) string {
		if starter {
			return callStateModel.RoleHost
		}
		return callStateModel.RoleParticipant
	}
	// A call someone begins and, minutes later, leaves last, which ends it as
	// LiveKit's participant_left does when the room is left empty
	// (business/CallControl OnLeft, then OnEnded): the call as its record has
	// it at its end.
	call := func(room string, who uuid.UUID, identity string, minutes int) *callStateBusiness.Session {
		t.Helper()
		at := time.Now().Add(-time.Duration(minutes) * time.Minute).Truncate(time.Second)
		sid := "RM_" + identity
		if _, _, err := callStateBusiness.Joined(ctx, callStateBusiness.JoinEvent{Room: room, RoomSID: sid, Identity: identity, Name: "Maya Chen", UserUUID: &who, At: at}, role); err != nil {
			t.Fatalf("a call in %s: %v", room, err)
		}
		if _, _, err := callStateBusiness.Left(ctx, callStateBusiness.JoinEvent{Room: room, RoomSID: sid, Identity: identity, UserUUID: &who, At: at.Add(time.Duration(minutes) * time.Minute)}); err != nil {
			t.Fatalf("the leave from %s: %v", room, err)
		}
		s, ended, err := callStateBusiness.End(ctx, room, sid, callStateModel.EndedLastLeft)
		if err != nil || !ended || s == nil || s.EndedAt == nil {
			t.Fatalf("the end of the call in %s: ended %v, %v", room, ended, err)
		}
		return s
	}
	// How long a call went on, as its record says.
	lasted := func(s *callStateBusiness.Session) float64 { return float64(int(s.EndedAt.Sub(s.StartedAt).Seconds())) }
	ends := func(webhook uuid.UUID, session string) func() bool {
		return func() bool { return of(webhook, session)["call.ended"] != nil }
	}
	// Every event emitted so far has been through every sink.
	settled := func() {
		t.Helper()
		if !resourceEventBusiness.Sinks.Settle(15 * time.Second) {
			t.Fatal("the events' sinks didn't settle")
		}
	}
	nothingSaid := func(what string, d map[string]any) {
		t.Helper()
		b, _ := json.Marshal(d)
		for _, k := range []string{"by", "room", "name", "title"} {
			if _, ok := d[k]; ok {
				t.Errorf("%s says %s: %s", what, k, b)
			}
		}
	}

	t.Run("a channel's call: its member's workspace webhook is sent its beginning and its end, whole", func(t *testing.T) {
		s := call(channel, maya, people["maya"].Uid, 25)
		eventually(t, "the channel call's end at Maya's webhook", ends(mayas, s.ID))
		got := of(mayas, s.ID)
		start, end := got["call.started"], got["call.ended"]
		if start == nil || start["channel_id"] != channel || start["actor_uuid"] != maya.String() || start["started_at"] == nil {
			t.Fatalf("the beginning: %v", start)
		}
		if end["channel_id"] != channel || end["duration_s"] != lasted(s) || lasted(s) < 1500 || end["people"] != float64(1) ||
			end["reason"] != callStateModel.EndedLastLeft || end["started_at"] != start["started_at"] || end["ended_at"] == nil {
			t.Errorf("the end: %v (began %v, lasted %v)", end, start["started_at"], lasted(s))
		}
		nothingSaid("the beginning", start)
		nothingSaid("the end", end)
	})

	t.Run("a meeting's call: its host's webhook is sent it, a stranger's nothing", func(t *testing.T) {
		meeting := uuid.NewString()
		room := "meet-" + meeting
		expires := time.Now().Add(time.Hour)
		if _, err := guestModel.CreateGrant(ctx, []byte("walk-"+meeting), guestModel.ResourceMeeting, room, guestModel.CapabilityJoin, maya, &expires); err != nil {
			t.Fatal(err)
		}
		s := call(room, maya, people["maya"].Uid, 10)
		eventually(t, "the meeting's end at its host's webhook", ends(mayas, s.ID))
		end := of(mayas, s.ID)["call.ended"]
		if end["meeting_id"] != meeting || end["duration_s"] != lasted(s) || lasted(s) < 600 || end["channel_id"] != nil {
			t.Errorf("the meeting's end: %v (lasted %v)", end, lasted(s))
		}
		settled()
		if got := of(outsiders, s.ID); len(got) != 0 {
			t.Errorf("a stranger's webhook was sent a meeting's call: %v", got)
		}
	})

	t.Run("a DM's call reaches no webhook", func(t *testing.T) {
		room := helpers.GetGroupingId(maya.String(), outsider.String())
		s := call(room, maya, people["maya"].Uid, 5)
		settled()
		if got, other := of(mayas, s.ID), of(outsiders, s.ID); len(got)+len(other) != 0 {
			t.Errorf("a DM's call was sent: %v %v", got, other)
		}
	})
}
