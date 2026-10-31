//go:build integration

package integration_test

// A guest in an instant meeting opens the board its host put on the stage
// (calls package B, zoom-new-whiteboard): their own call token answers the
// collab token for that board alone while it is on the stage and they are in
// the call, and nothing once it leaves.

import (
	"context"
	"testing"

	"github.com/google/uuid"

	callControlBusiness "github.com/akashc777/OneCamp/business/CallControl"
	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestCallBoardOnAMeetingsStageOpensForItsGuest(t *testing.T) {
	ctx := context.Background()
	pg := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, pg.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	dg := integration.SetupDgraph(t)
	integration.StubMqttClient(t)
	lk := integration.SetupLiveKit(t)
	t.Setenv("JWT_SECRET", "call-board-guest-integration-secret")
	if err := settingsBusiness.SetGuestAccessEnabled(true); err != nil {
		t.Fatal(err)
	}
	people := seedChannel(t, pg, dg, uuid.NewString(), "maya")
	meeting, err := guestBusiness.CreateInstantMeeting(ctx, people["maya"], "")
	if err != nil {
		t.Fatal(err)
	}
	host := join(t, lk, meeting.HostToken)
	t.Cleanup(host.room.Disconnect)
	joinSettled(t, meeting.Room, people["maya"].Uid, host)
	connected(t, ctx, meeting.Room, people["maya"].Uid)
	door, err := guestBusiness.OpenDoor(ctx, meeting.GuestToken)
	if err != nil {
		t.Fatal(err)
	}
	guestToken, _, err := guestBusiness.IssueGuestMeetingToken(ctx, door, "Gia", "")
	if err != nil {
		t.Fatal(err)
	}
	gia := join(t, lk, guestToken)
	t.Cleanup(gia.room.Disconnect)
	identity, _, err := callControlBusiness.GuestOf(guestToken)
	if err != nil {
		t.Fatal(err)
	}
	joinSettled(t, meeting.Room, identity, gia)
	connected(t, ctx, meeting.Room, identity)
	s, err := callStateBusiness.Current(ctx, callStateBusiness.Ref{Kind: callStateBusiness.KindMeeting, ID: meeting.MeetingID})
	if err != nil || s == nil {
		t.Fatalf("no meeting recorded: %v", err)
	}

	_, err = callControlBusiness.GuestBoardToken(ctx, "Bearer "+guestToken)
	refusedWith(t, "Gia's board token before a board is up", err, callControlBusiness.ErrNoBoard)

	made, err := callControlBusiness.Act(ctx, people["maya"], s.ID, callControlBusiness.Action{Action: callControlBusiness.ActNewBoard})
	if err != nil || made.Board == "" {
		t.Fatalf("Maya's new board on the stage: %+v, %v", made, err)
	}
	got, err := callControlBusiness.GuestBoardToken(ctx, "Bearer "+guestToken)
	if err != nil {
		t.Fatalf("Gia's board token with Maya's board on the stage: %v", err)
	}
	if got.Board != made.Board || got.CollabToken == "" || got.DocumentName != "board:"+made.Board {
		t.Fatalf("Gia's board token %+v, want board %s's", got, made.Board)
	}

	if _, err := callControlBusiness.Act(ctx, people["maya"], s.ID, callControlBusiness.Action{Action: callControlBusiness.ActStopBoard}); err != nil {
		t.Fatalf("Maya taking her board down: %v", err)
	}
	_, err = callControlBusiness.GuestBoardToken(ctx, "Bearer "+guestToken)
	refusedWith(t, "Gia's board token after the board left the stage", err, callControlBusiness.ErrNoBoard)
}
