//go:build integration

package integration_test

// A host's removal holds even when the call's record can't answer (21 Oct).
// In the integrator's gate, under load, a member a host had just put out
// minted a token back into the call: every check that keeps someone out let
// them in when its read failed, and the record that keeps them out was
// best-effort. These tests take the record away for the length of a step,
// so the window opens every time rather than under load.

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/livekitInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/tests/integration"
)

// attendanceAway takes the call's attendance out of reach (renamed), as a
// database that doesn't answer it, and answers how to put it back; it is put
// back when the test ends whatever happens.
func attendanceAway(t *testing.T, db *sql.DB) func() {
	t.Helper()
	if _, err := db.Exec(`ALTER TABLE call_attendance RENAME TO call_attendance_away`); err != nil {
		t.Fatal(err)
	}
	back := false
	restore := func() {
		if back {
			return
		}
		back = true
		if _, err := db.Exec(`ALTER TABLE call_attendance_away RENAME TO call_attendance`); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(restore)
	return restore
}

// One member in a channel's call, as LiveKit and the server know it.
func oneInACall(t *testing.T, name string) (context.Context, *integration.Env, *integration.LiveKitEnv, string, *dgraphStruct.DgraphUser, string) {
	t.Helper()
	ctx := context.Background()
	pg := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, pg.DSN); err != nil {
		t.Fatal(err)
	}
	env := integration.SetupLiveKit(t)
	integration.SetupRedis(t)
	dg := integration.SetupDgraph(t)
	integration.StubMqttClient(t)
	id := uuid.New()
	room := uuid.NewString() // a channel's call
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:p", "dgraph.type": "User", "user_uuid": id.String(), "user_name": name},
		{"uid": "_:c", "dgraph.type": "Channel", "ch_uuid": room, "ch_name": "launch", "ch_members": []map[string]any{{"uid": "_:p"}}},
	})
	if _, err := livekitInit.LiveKitService.CreateRoom(ctx, room); err != nil {
		t.Fatal(err)
	}
	token, err := livekitInit.LiveKitService.GenerateToken(room, uids["p"], title(name), false, "")
	if err != nil {
		t.Fatal(err)
	}
	return ctx, pg, env, room, &dgraphStruct.DgraphUser{Uid: uids["p"], Uuid: id.String(), UserName: name}, token
}

// A removal is never made without the record that keeps them out, and no
// token is given while that record can't be read: before, the host's removal
// put Sam out with nothing to keep him out, and his next token was minted.
func TestCallAPutOutHoldsWhenTheRecordCantAnswer(t *testing.T) {
	ctx, pg, env, room, sam, token := oneInACall(t, "sam")
	in := join(t, env, token)
	joinSettled(t, room, sam.Uid, in)
	connected(t, ctx, room, sam.Uid)
	host := liveKitBusiness.Remover{Identity: "0xa1", Name: "Maya Chen"}

	back := attendanceAway(t, pg.PG)
	if err := liveKitBusiness.RemoveFromCall(ctx, room, sam.Uid, host, liveKitBusiness.RemovedByHost); !errors.Is(err, liveKitBusiness.ErrCallUnchecked) {
		t.Errorf("a removal the record couldn't keep: %v", err)
	}
	if people, _ := livekitInit.LiveKitService.ConnectedParticipants(ctx, room); len(people) != 1 {
		t.Errorf("put out with nothing to keep him out: %v", people)
	}
	if _, _, err := liveKitBusiness.CreateRoomAndGetToken(ctx, room, sam, false, ""); !errors.Is(err, liveKitBusiness.ErrCallUnchecked) {
		t.Errorf("a token minted while the call couldn't be checked: %v", err)
	}
	back()

	// Once the record answers, the removal holds.
	if err := liveKitBusiness.RemoveFromCall(ctx, room, sam.Uid, host, liveKitBusiness.RemovedByHost); err != nil {
		t.Fatal(err)
	}
	eventually(t, "Sam to be put out", func() bool { return in.disconnected.Load() != nil })
	if _, _, err := liveKitBusiness.CreateRoomAndGetToken(ctx, room, sam, false, ""); !errors.Is(err, liveKitBusiness.ErrRemovedFromCall) {
		t.Fatalf("a removed member minted a token back into the call: %v", err)
	}
}

// Someone who connects while whether they may stay can't be checked is put
// out, told it was unconfirmed, and comes back once it can be: before, a
// check that couldn't answer let them stay, so a token minted before a
// removal or a lock walked back in while the database blinked.
func TestCallSomeoneWhoseStandingCantBeCheckedIsPutOut(t *testing.T) {
	ctx, pg, env, room, ana, token := oneInACall(t, "ana")
	back := attendanceAway(t, pg.PG)
	in := join(t, env, token)
	joinSettled(t, room, ana.Uid, in)
	connected(t, ctx, room, ana.Uid) // the server's check runs, and can't answer
	eventually(t, "Ana to be put out", func() bool { return in.disconnected.Load() != nil })
	if reason := toldRemoved(in); reason != liveKitBusiness.RemovedUnconfirmed {
		t.Errorf("Ana was told %q", reason)
	}
	back()

	// It answers again: a new token, and she stays.
	token, _, err := liveKitBusiness.CreateRoomAndGetToken(ctx, room, ana, false, "")
	if err != nil {
		t.Fatal(err)
	}
	again := join(t, env, token)
	joinSettled(t, room, ana.Uid, again)
	connected(t, ctx, room, ana.Uid)
	// The server's check has run, and let her stay.
	if busy := helpers.SettleBackground(15 * time.Second); len(busy) > 0 {
		t.Fatalf("the check was still running: %v", busy)
	}
	if again.disconnected.Load() != nil {
		t.Error("put out once the check answered")
	}
}
