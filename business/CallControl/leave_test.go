package business

import (
	"context"
	"errors"
	"testing"
	"time"

	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	"github.com/livekit/protocol/livekit"
)

// V-85 (29 Oct 2026): someone whose connection drops (a laptop sleeps, a tab
// crashes, the line goes) and who comes back, or who is in on a second
// device, has two connections under one identity. LiveKit tells of the old
// one's leave late, after its departure window, when the new one is already
// in. A connection is told apart by its SID, never by who it is: that late
// leave must neither record them gone nor end the call under them, as it did
// to an organiser waiting on their lobby (the guests knocked into a call that
// was gone).

func conn(identity, sid string) *livekit.ParticipantInfo {
	return &livekit.ParticipantInfo{Identity: identity, Sid: sid, State: livekit.ParticipantInfo_ACTIVE, Kind: livekit.ParticipantInfo_STANDARD}
}

// seamLeave stands in for LiveKit's listing of the room and for Call State's
// record of a leave, and counts the leaves recorded.
func seamLeave(t *testing.T, people []*livekit.ParticipantInfo, listErr error) *int {
	t.Helper()
	wasPeople, wasLeave := connectedPeople, leaveOf
	t.Cleanup(func() { connectedPeople, leaveOf = wasPeople, wasLeave })
	recorded := 0
	connectedPeople = func(context.Context, string) ([]*livekit.ParticipantInfo, error) { return people, listErr }
	leaveOf = func(context.Context, callStateBusiness.JoinEvent) (*callStateBusiness.Session, bool, error) {
		recorded++
		return nil, false, nil
	}
	return &recorded
}

func TestStillHereTellsConnectionsApartByTheirSID(t *testing.T) {
	maya, mayaAgain, sam := conn("maya", "PA_old"), conn("maya", "PA_new"), conn("sam", "PA_sam")
	recorder := conn("EG_recorder", "PA_egress")
	recorder.Kind = livekit.ParticipantInfo_EGRESS
	watcher := conn("watcher", "PA_hidden")
	watcher.Permission = &livekit.ParticipantPermission{Hidden: true}
	agent := conn("transcriber-bot", "PA_agent")
	for _, c := range []struct {
		name         string
		people       []*livekit.ParticipantInfo
		gone         *livekit.ParticipantInfo
		anyone, them bool
	}{
		{"the same person on another connection is still here", []*livekit.ParticipantInfo{mayaAgain}, maya, true, true},
		{"the connection that left, still listed, is no one", []*livekit.ParticipantInfo{maya}, maya, false, false},
		{"someone else is still here", []*livekit.ParticipantInfo{maya, sam}, maya, true, false},
		{"a recorder, a hidden watcher and the agent are no one", []*livekit.ParticipantInfo{recorder, watcher, agent}, maya, false, false},
		{"a leave with no SID tells no connection apart, so whoever is listed counts", []*livekit.ParticipantInfo{mayaAgain}, conn("maya", ""), true, true},
		{"with nobody gone it asks whether anyone is here", []*livekit.ParticipantInfo{sam}, nil, true, false},
		{"an empty room has nobody", nil, maya, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			anyone, them := stillHere(c.people, c.gone)
			if anyone != c.anyone || them != c.them {
				t.Fatalf("stillHere = (anyone %v, them %v), want (%v, %v)", anyone, them, c.anyone, c.them)
			}
		})
	}
}

// The late-webhook order: Maya is back on a new connection when her old
// one's leave comes. Nothing is recorded and the call goes on.
func TestALateLeaveFromAReplacedConnectionChangesNothing(t *testing.T) {
	recorded := seamLeave(t, []*livekit.ParticipantInfo{conn("maya", "PA_new")}, nil)
	if OnLeft(context.Background(), "room-v85", "RM_1", conn("maya", "PA_old"), time.Now()) {
		t.Fatal("the call was ended under a person who is still in it")
	}
	if *recorded != 0 {
		t.Fatalf("the late leave of a replaced connection was recorded %d time(s): it marks a person who is in the call as gone", *recorded)
	}
}

func TestTheLastConnectionLeavingEmptiesTheCall(t *testing.T) {
	// LiveKit may still list the connection that left, briefly.
	recorded := seamLeave(t, []*livekit.ParticipantInfo{conn("maya", "PA_old"), conn("transcriber-bot", "PA_agent")}, nil)
	if !OnLeft(context.Background(), "room-v85", "RM_1", conn("maya", "PA_old"), time.Now()) {
		t.Fatal("the last person left and the call was not empty")
	}
	if *recorded != 1 {
		t.Fatalf("the leave was recorded %d time(s), want 1", *recorded)
	}
}

func TestALeaveWithOthersStillInRecordsItAndEndsNothing(t *testing.T) {
	recorded := seamLeave(t, []*livekit.ParticipantInfo{conn("sam", "PA_sam")}, nil)
	if OnLeft(context.Background(), "room-v85", "RM_1", conn("maya", "PA_old"), time.Now()) {
		t.Fatal("the call ended while Sam is still in it")
	}
	if *recorded != 1 {
		t.Fatalf("the leave was recorded %d time(s), want 1", *recorded)
	}
}

func TestALeaveWhoseListingFailsIsRecordedAndEndsNothing(t *testing.T) {
	recorded := seamLeave(t, nil, errors.New("livekit unreachable"))
	if OnLeft(context.Background(), "room-v85", "RM_1", conn("maya", "PA_old"), time.Now()) {
		t.Fatal("a call was ended on a listing that failed")
	}
	if *recorded != 1 {
		t.Fatalf("the leave was recorded %d time(s), want 1", *recorded)
	}
}
