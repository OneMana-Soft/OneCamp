package business

import (
	"context"
	"errors"
	"testing"

	callStateModel "github.com/akashc777/OneCamp/models/postgres/CallState"
	"github.com/google/uuid"
)

// A member let in through a link they opened as themselves may be in that
// link's call while the link lives: never with guest access off, a room's
// address only while rooms are on, and one read for every room (HU13).
func TestAMemberLetInThroughALinkMayBeInItsCall(t *testing.T) {
	oldRead, oldRooms, oldCap, oldOn := liveAdmissionsOf, roomsOn, linkCapDays, guestOn
	t.Cleanup(func() { liveAdmissionsOf, roomsOn, linkCapDays, guestOn = oldRead, oldRooms, oldCap, oldOn })
	maya := uuid.New()
	reads := 0
	liveAdmissionsOf = func(_ context.Context, user uuid.UUID, capDays int) ([]callStateModel.LiveAdmission, error) {
		reads++
		if user != maya || capDays != 30 {
			t.Errorf("read for %v under a cap of %d", user, capDays)
		}
		return []callStateModel.LiveAdmission{
			{Room: "channel-uuid", GrantID: uuid.New()},
			{Room: "meet-m1", GrantID: uuid.New(), Standing: true},
		}, nil
	}
	linkCapDays = func(context.Context) int { return 30 }
	rooms, guests := true, true
	roomsOn = func() bool { return rooms }
	guestOn = func() bool { return guests }

	got, err := roomsAdmittedTo(context.Background(), maya)
	if err != nil || !got["channel-uuid"] || !got["meet-m1"] || len(got) != 2 || reads != 1 {
		t.Fatalf("with guest access and rooms on: %v, %v (%d reads)", got, err, reads)
	}
	rooms = false
	if got, _ := roomsAdmittedTo(context.Background(), maya); !got["channel-uuid"] || got["meet-m1"] {
		t.Errorf("rooms off still opens a room's address: %v", got)
	}
	guests = false
	reads = 0
	if got, _ := roomsAdmittedTo(context.Background(), maya); len(got) != 0 || reads != 0 {
		t.Errorf("guest access off: %v (%d reads)", got, reads)
	}
	if got, _ := roomsAdmittedTo(context.Background(), uuid.Nil); len(got) != 0 {
		t.Errorf("nobody: %v", got)
	}

	// A read that fails is an error, never a yes.
	guests = true
	liveAdmissionsOf = func(context.Context, uuid.UUID, int) ([]callStateModel.LiveAdmission, error) {
		return nil, errors.New("down")
	}
	if got, err := roomsAdmittedTo(context.Background(), maya); err == nil || len(got) != 0 {
		t.Errorf("a failed read: %v, %v", got, err)
	}
}
