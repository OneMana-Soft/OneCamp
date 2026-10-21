package business

import (
	"fmt"
	"testing"
	"time"

	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	model "github.com/akashc777/OneCamp/models/postgres/CallState"
	"github.com/google/uuid"
)

// A room's name says what its call belongs to, and what it belongs to names
// its room again: the one rule every reader of a call uses.
func TestARoomNamesWhatItsCallBelongsTo(t *testing.T) {
	channel := "4f0c2a1e-3b7d-4c55-9a51-0d8b1f6a2c33"
	dm := "4f0c2a1e-3b7d-4c55-9a51-0d8b1f6a2c33 5a1d3b2f-4c8e-4d66-8b62-1e9c2a7b3d44"
	group := "0123456789abcdef0123456789abcdef"
	meeting := "meet-6f1c0e2a-1111-4222-8333-944455556666"
	for room, want := range map[string]struct {
		kind, id, call string
	}{
		channel: {"channel", channel, model.KindHuddle},
		dm:      {"dm", dm, model.KindHuddle},
		group:   {"group", group, model.KindHuddle},
		meeting: {"meeting", "6f1c0e2a-1111-4222-8333-944455556666", model.KindMeeting},
	} {
		ref, kind, ok := RefOfRoom(room)
		if !ok || ref.Kind != want.kind || ref.ID != want.id || kind != want.call {
			t.Errorf("%q is %+v %q %v, want %+v", room, ref, kind, ok, want)
		}
		if RoomOf(ref) != room {
			t.Errorf("%+v names room %q, want %q", ref, RoomOf(ref), room)
		}
	}
	for _, room := range []string{"", "meet-", "not a uuid-ish-name", "general"} {
		if _, _, ok := RefOfRoom(room); ok {
			t.Errorf("%q was taken for a call's room", room)
		}
	}
}

// A call recorded for an earlier room of the same name (LiveKit closed it and
// made it again, and its end never reached us) is over; one with no room id
// either side is not judged stale.
func TestACallFromAnEarlierRoomIsStale(t *testing.T) {
	s := &model.Session{RoomSID: "RM_old"}
	if !staleRoom(s, "RM_new") {
		t.Error("a call from an earlier room wasn't stale")
	}
	if staleRoom(s, "RM_old") || staleRoom(s, "") || staleRoom(&model.Session{}, "RM_new") {
		t.Error("a call was judged stale without two room ids that differ")
	}
}

func row(at time.Time) *model.Session {
	return &model.Session{ID: uuid.New(), Room: "r", RoomSID: "RM_1", Kind: model.KindHuddle, ResourceKind: "channel", ResourceID: "c", StartedAt: at}
}

// A call reads as its people see it: who is in it now and who was, guests
// marked, the removed marked, the lock by the name of whoever set it.
func TestACallReadsAsItsPeopleSeeIt(t *testing.T) {
	at := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	maya := uuid.New()
	s := row(at)
	locked := at.Add(5 * time.Minute)
	s.LockedAt, s.LockedBy = &locked, "0x1"
	left := at.Add(time.Minute)
	got := viewOf(s, []model.Attendee{
		{Identity: "0x1", UserUUID: &maya, Name: "Maya", Role: model.RoleHost, JoinedAt: at},
		{Identity: "guest-a-b", Name: "Guest: Priya", Role: model.RoleGuest, JoinedAt: at, LeftAt: &left, RemovedBy: "0x1"},
	}, []model.Recording{{EgressID: "EG_1", StartedAt: at, Status: "recording"}})
	if !got.Live || got.Locked == nil || got.Locked.By != "Maya" || len(got.Recordings) != 1 {
		t.Fatalf("read %+v", got)
	}
	in := got.InCall()
	if len(in) != 1 || in[0].UserUUID != maya.String() || in[0].Guest {
		t.Errorf("in the call: %+v", in)
	}
	priya := got.Person("guest-a-b")
	if priya == nil || !priya.Guest || priya.In || !priya.Removed {
		t.Errorf("the removed guest reads %+v", priya)
	}

	s.LockedBy = "0x99"
	if v := viewOf(s, nil, nil); v.Locked == nil || v.Locked.By != "Someone" {
		t.Errorf("a lock by someone gone reads %+v", v.Locked)
	}
}

// The conversation hears of a call with at most twelve faces, and all of them
// counted; what is switched on only while it is live.
func TestTheConversationHearsTwelveFacesAndTheCount(t *testing.T) {
	at := time.Now()
	var people []model.Attendee
	for i := 0; i < 20; i++ {
		id := uuid.New()
		people = append(people, model.Attendee{Identity: fmt.Sprintf("0x%d", i+1), UserUUID: &id, Name: fmt.Sprintf("P%d", i), Role: model.RoleParticipant, JoinedAt: at})
	}
	s := viewOf(row(at), people, nil)
	switches := liveKitBusiness.CallView{
		Recording:     liveKitBusiness.RecordingView{On: true, By: "Maya"},
		Transcription: liveKitBusiness.TranscriptionSwitch{On: true, By: "Jonas", Where: "server"},
	}
	p := statePayload(s, switches)
	if len(p.People) != 12 || p.Count != 20 || !p.Live || !p.Recording.On || p.Recording.ByName != "Maya" || p.Transcription.Where != "server" {
		t.Fatalf("payload %+v", p)
	}
	ended := at.Add(time.Hour)
	over := row(at)
	over.EndedAt = &ended
	q := statePayload(viewOf(over, nil, nil), switches)
	if q.Live || q.Recording.On || q.Transcription.On || q.EndedAt == nil {
		t.Errorf("an ended call says %+v", q)
	}
}
