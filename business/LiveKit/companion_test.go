package business

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/akashc777/OneCamp/initializers/livekitInit"
	"github.com/akashc777/OneCamp/tests/livekitfake"
)

func companionOf(t *testing.T, uid string) string {
	t.Helper()
	c, err := livekitInit.NewCompanionIdentity(uid)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func stillConnected(t *testing.T, room string) []string {
	t.Helper()
	people, err := livekitInit.LiveKitService.ConnectedParticipants(context.Background(), room)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, p := range people {
		out = append(out, p.Identity)
	}
	slices.Sort(out)
	return out
}

// meet-04. Whoever is put out of a call goes on every device: a companion
// never stays in a call its person may no longer be in (a host's removal, a
// revoked right, the stay check), and one alone in it, presenting, is found
// and put out too. Putting out a companion puts out its person: what may not
// stay on one device may not stay on another. Nobody else goes.
func TestPuttingSomeoneOutTakesTheirCompanionsToo(t *testing.T) {
	ctx := context.Background()
	const room, maya, sam = "c0ffee00-0000-4000-8000-0000000000c4", "0x2a", "0x2b"
	mayaPhone, mayaTab, samPhone := companionOf(t, maya), companionOf(t, maya), companionOf(t, sam)
	f := livekitfake.Serve(t, room, "RM_1", maya, mayaPhone, mayaTab, sam, samPhone)

	if err := RemoveFromCall(ctx, room, maya, Remover{}, RemovedNotAMember); err != nil {
		t.Fatal(err)
	}
	if got, want := stillConnected(t, room), []string{sam, samPhone}; !slices.Equal(got, sortedCopy(want)) {
		t.Errorf("after Maya was put out, %v are in the call; want %v", got, sortedCopy(want))
	}

	// Sam presenting from his phone alone.
	f.Leave(room, sam)
	if err := RemoveFromCall(ctx, room, sam, Remover{}, RemovedNotAMember); err != nil {
		t.Fatalf("Sam, in the call only on his phone, wasn't put out: %v", err)
	}
	if got := stillConnected(t, room); len(got) != 0 {
		t.Errorf("after Sam was put out, %v are in the call", got)
	}

	// A companion put out by its own identity takes its person with it.
	f.Room(room, "RM_1", maya, mayaPhone, sam)
	if err := RemoveFromCall(ctx, room, mayaPhone, Remover{}, RemovedNotAMember); err != nil {
		t.Fatal(err)
	}
	if got := stillConnected(t, room); !slices.Equal(got, []string{sam}) {
		t.Errorf("after Maya's phone was put out, %v are in the call; want only Sam", got)
	}

	if err := RemoveFromCall(ctx, room, maya, Remover{}, RemovedNotAMember); !errors.Is(err, ErrNotInCall) {
		t.Errorf("putting out Maya, on no device now: %v, want ErrNotInCall", err)
	}
}

// meet-04, the breakout rooms: a companion stays with the call's own room, so
// when its person goes into a breakout room their companions there leave,
// told why, and their person stays where they are.
func TestACompanionLeavesWhenItsPersonGoesToABreakoutRoom(t *testing.T) {
	ctx := context.Background()
	const room, maya, sam = "c0ffee00-0000-4000-8000-0000000000c5", "0x2a", "0x2b"
	mayaPhone, samPhone := companionOf(t, maya), companionOf(t, sam)
	livekitfake.Serve(t, room, "RM_2", maya, mayaPhone, sam, samPhone)
	if n := RemoveCompanions(ctx, room, maya, RemovedWentToRoom); n != 1 {
		t.Errorf("Maya's companions put out: %d, want 1", n)
	}
	if got, want := stillConnected(t, room), sortedCopy([]string{maya, sam, samPhone}); !slices.Equal(got, want) {
		t.Errorf("after Maya's companions left, %v are in the call; want %v", got, want)
	}
}

// V-860's rule for the in-call AI: only someone connected to the call now may
// ask it. A companion is no stand-in for its person: with only Maya's phone
// connected she isn't in the call, and her phone doesn't make her two people.
func TestACompanionIsNoStandInForItsPersonInTheCall(t *testing.T) {
	ctx := context.Background()
	const room, maya = "c0ffee00-0000-4000-8000-0000000000c6", "0x2a"
	phone := companionOf(t, maya)
	f := livekitfake.Serve(t, room, "RM_3", phone)
	if _, err := ConnectedCall(ctx, room, maya); !errors.Is(err, ErrNotInCall) {
		t.Errorf("Maya, connected only through her phone, reads the call: %v", err)
	}
	f.Room(room, "RM_3", maya, phone)
	if _, err := ConnectedCall(ctx, room, maya); err != nil {
		t.Errorf("Maya, connected: %v", err)
	}
	active, err := livekitInit.LiveKitService.RoomHasHumanParticipants(ctx, room)
	if err != nil || !active {
		t.Fatalf("the call with Maya in it: %v, %v", active, err)
	}
	f.Room(room, "RM_3", phone)
	if active, _ := livekitInit.LiveKitService.RoomHasHumanParticipants(ctx, room); active {
		t.Error("a call with only Maya's phone left in it is still going on")
	}
}

func sortedCopy(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
}
