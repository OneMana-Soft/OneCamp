package business

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	callStateModel "github.com/akashc777/OneCamp/models/postgres/CallState"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/google/uuid"
)

// doors is a meeting's link, a call's invite and a doc's link, by raw token,
// with the call the invite opens going on in channel-1.
type doors struct {
	grants  map[string]*guestModel.GuestGrant
	session uuid.UUID
	call    *callStateModel.Session
	readErr error
}

func withDoors(t *testing.T) *doors {
	t.Helper()
	oldHash, oldID, oldSession, oldOn, oldLive, oldRooms := grantByHash, grantByID, sessionByID, guestOn, grantLive, roomsOn
	t.Cleanup(func() {
		grantByHash, grantByID, sessionByID, guestOn, grantLive, roomsOn = oldHash, oldID, oldSession, oldOn, oldLive, oldRooms
	})
	d := &doors{session: uuid.New()}
	d.call = &callStateModel.Session{ID: d.session, Room: "channel-1"}
	d.grants = map[string]*guestModel.GuestGrant{
		"meeting": {Id: uuid.New(), ResourceType: guestModel.ResourceMeeting, ResourceID: "meet-m1", Capability: guestModel.CapabilityJoin},
		"call":    {Id: uuid.New(), ResourceType: guestModel.ResourceCall, ResourceID: d.session.String(), Capability: guestModel.CapabilityJoin},
		"doc":     {Id: uuid.New(), ResourceType: guestModel.ResourceDoc, ResourceID: "d1", Capability: guestModel.CapabilityView},
	}
	grantByHash = func(_ context.Context, hash []byte) (*guestModel.GuestGrant, error) {
		for token, g := range d.grants {
			if bytes.Equal(hash, hashToken(token)) {
				return g, nil
			}
		}
		return nil, nil
	}
	grantByID = func(_ context.Context, id uuid.UUID) (*guestModel.GuestGrant, error) {
		for _, g := range d.grants {
			if g.Id == id {
				return g, nil
			}
		}
		return nil, nil
	}
	sessionByID = func(_ context.Context, id uuid.UUID) (*callStateModel.Session, error) {
		if d.readErr != nil {
			return nil, d.readErr
		}
		if id == d.session {
			return d.call, nil
		}
		return nil, nil
	}
	guestOn = func() bool { return true }
	roomsOn = func() bool { return true }
	grantLive = func(context.Context, *guestModel.GuestGrant) error { return nil }
	return d
}

// A link a guest opens comes to a call: a meeting's own room, or for a call's
// invite, its call's room while the call goes on (HU13). Anything else opens
// nothing, as does an invite whose call is over.
func TestALinkOpensItsCall(t *testing.T) {
	ctx := context.Background()
	d := withDoors(t)

	if door, err := OpenDoor(ctx, "meeting"); err != nil || door.Room != "meet-m1" || door.Grant != d.grants["meeting"] {
		t.Errorf("a meeting's link: %+v, %v", door, err)
	}
	if door, err := OpenDoor(ctx, "call"); err != nil || door.Room != "channel-1" || door.Grant != d.grants["call"] {
		t.Errorf("a call's invite: %+v, %v", door, err)
	}
	for _, token := range []string{"doc", "nothing"} {
		if _, err := OpenDoor(ctx, token); !errors.Is(err, ErrInvalidGrant) {
			t.Errorf("%s: %v", token, err)
		}
	}

	ended := time.Now()
	d.call = &callStateModel.Session{ID: d.session, Room: "channel-1", EndedAt: &ended}
	if _, err := OpenDoor(ctx, "call"); !errors.Is(err, ErrInvalidGrant) {
		t.Errorf("an invite whose call is over: %v", err)
	}

	// A read that fails is the server's trouble (a 503 the page retries),
	// never "this link is no longer available".
	d.readErr = errors.New("down")
	if _, err := OpenDoor(ctx, "call"); err == nil || IsUnavailable(err) {
		t.Errorf("a failed read: %v", err)
	}

	guestOn = func() bool { return false }
	if _, err := OpenDoor(ctx, "meeting"); !errors.Is(err, ErrGuestDisabled) {
		t.Errorf("guest access off: %v", err)
	}
}

// A guest who connected through a call's invite may stay in that call while
// it goes on and the invite lives: never another call, never once the call is
// over, the invite is revoked or guest access is off (HU13).
func TestACallsGuestMayStayOnlyInItsCall(t *testing.T) {
	ctx := context.Background()
	d := withDoors(t)
	call := d.grants["call"]
	guest := guestIdentityPrefix(call.Id.String()) + "abcdefgh"

	if ok, err := GuestMayJoin(ctx, "channel-1", guest); err != nil || !ok {
		t.Errorf("in its call: %v, %v", ok, err)
	}
	if ok, _ := GuestMayJoin(ctx, "channel-2", guest); ok {
		t.Error("in another conversation's call")
	}
	if ok, _ := GuestMayJoin(ctx, "meet-m1", guest); ok {
		t.Error("in a meeting")
	}
	meetingGuest := guestIdentityPrefix(d.grants["meeting"].Id.String()) + "abcdefgh"
	if ok, err := GuestMayJoin(ctx, "meet-m1", meetingGuest); err != nil || !ok {
		t.Errorf("a meeting's guest in its meeting: %v, %v", ok, err)
	}
	if ok, _ := GuestMayJoin(ctx, "channel-1", meetingGuest); ok {
		t.Error("a meeting's guest in a conversation's call")
	}

	ended := time.Now()
	d.call = &callStateModel.Session{ID: d.session, Room: "channel-1", EndedAt: &ended}
	if ok, _ := GuestMayJoin(ctx, "channel-1", guest); ok {
		t.Error("once the call is over")
	}
	d.call = &callStateModel.Session{ID: d.session, Room: "channel-1"}
	revoked := time.Now()
	call.RevokedAt = &revoked
	if ok, _ := GuestMayJoin(ctx, "channel-1", guest); ok {
		t.Error("with its invite revoked")
	}
	call.RevokedAt = nil
	guestOn = func() bool { return false }
	if ok, _ := GuestMayJoin(ctx, "channel-1", guest); ok {
		t.Error("with guest access off")
	}

	// A read that fails is an error, never a yes.
	guestOn = func() bool { return true }
	d.readErr = errors.New("down")
	if ok, err := GuestMayJoin(ctx, "channel-1", guest); ok || err == nil {
		t.Errorf("a failed read: %v, %v", ok, err)
	}
}
