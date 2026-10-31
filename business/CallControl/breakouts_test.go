package business

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	model "github.com/akashc777/OneCamp/models/postgres/CallState"
)

// Calls package C, breakout rooms (meet-10, zoom-02), approved by main 26 Oct
// 06:5x: running the rooms is for hosts and co-hosts alone; a room's media
// only through a token the server mints for the room someone may be in.

func TestOnlyWhoRunsTheCallRunsItsRooms(t *testing.T) {
	for _, c := range []struct {
		kind, role string
		may        bool
	}{
		{"channel", model.RoleHost, true},
		{"channel", model.RoleCohost, true},
		{"meeting", model.RoleHost, true},
		{"meeting", model.RoleCohost, true},
		{"channel", model.RoleParticipant, false},
		{"channel", model.RoleInvitee, false},
		{"meeting", model.RoleGuest, false},
		// A DM's or a group's call has no rooms, and nobody runs it.
		{"dm", model.RoleParticipant, false},
		{"group", model.RoleParticipant, false},
		{"group", model.RoleHost, false},
	} {
		if got := slices.Contains(AbilitiesOf(c.kind, c.role), CanBreakouts); got != c.may {
			t.Errorf("a %s in a %s call may run its rooms: %v, want %v", c.role, c.kind, got, c.may)
		}
	}
}

// Assigning evenly: in the order people are listed, round the rooms, so no
// two rooms differ by more than one.
func TestEveryoneIsSpreadEvenly(t *testing.T) {
	people := []string{"a", "b", "c", "d", "e", "f", "g"}
	got := Spread(people, 3)
	if len(got) != len(people) {
		t.Fatalf("spread %d people, want %d", len(got), len(people))
	}
	sizes := map[int]int{}
	for _, p := range people {
		n := got[p]
		if n < 1 || n > 3 {
			t.Fatalf("%s went to room %d of 3", p, n)
		}
		sizes[n]++
	}
	if sizes[1] != 3 || sizes[2] != 2 || sizes[3] != 2 {
		t.Errorf("room sizes %v, want 3, 2 and 2", sizes)
	}
	if got["a"] != 1 || got["b"] != 2 || got["c"] != 3 || got["d"] != 1 {
		t.Errorf("not round the rooms in order: %v", got)
	}
	if len(Spread(people, 0)) != 0 || len(Spread(nil, 3)) != 0 {
		t.Error("no rooms, or nobody, spreads nobody")
	}
}

// Which room someone's token may be for: their own; any, while people may
// choose; any, to visit, for whoever runs the call; never one that isn't
// there, and never for someone with no room while choosing is off.
func TestARoomTokenIsForTheRoomYouMayBeIn(t *testing.T) {
	for _, c := range []struct {
		name           string
		role           string
		assigned, want int
		choose         bool
		ok             bool
		code           string
	}{
		{"your own room", model.RoleParticipant, 2, 2, false, true, ""},
		{"a guest's own room", model.RoleGuest, 1, 1, false, true, ""},
		{"someone else's room", model.RoleParticipant, 2, 3, false, false, "not_your_room"},
		{"a guest in someone else's room", model.RoleGuest, 1, 2, false, false, "not_your_room"},
		{"no room yet", model.RoleParticipant, 0, 1, false, false, "not_your_room"},
		{"any room while people choose", model.RoleParticipant, 0, 3, true, true, ""},
		{"a guest choosing", model.RoleGuest, 2, 1, true, true, ""},
		{"a host visiting", model.RoleHost, 0, 3, false, true, ""},
		{"a co-host visiting", model.RoleCohost, 1, 2, false, true, ""},
		{"a room that isn't there", model.RoleHost, 0, 4, false, false, "no_such_room"},
		{"room nought", model.RoleParticipant, 0, 0, true, false, "no_such_room"},
	} {
		err := MayTakeRoom(c.role, c.assigned, c.want, 3, c.choose)
		if (err == nil) != c.ok {
			t.Errorf("%s: refused %v, want ok %v", c.name, err, c.ok)
			continue
		}
		if err != nil && err.Code != c.code {
			t.Errorf("%s: refused with %q, want %q", c.name, err.Code, c.code)
		}
	}
}

// A call's rooms are checked before anything is stored: one to fifty rooms,
// each named in 1 to 60 characters once trimmed, on one line.
func TestRoomNamesAreCheckedBeforeAnythingIsStored(t *testing.T) {
	if got, err := roomNames([]string{" Design ", "Build"}); err != nil || !slices.Equal(got, []string{"Design", "Build"}) {
		t.Errorf("two good names: %q, %v", got, err)
	}
	if got, err := roomNames([]string{strings.Repeat("é", 60)}); err != nil || len(got) != 1 {
		t.Errorf("a name of 60 characters (120 bytes) is refused: %v", err)
	}
	for name, names := range map[string][]string{
		"no rooms":            nil,
		"a blank name":        {"Design", "  "},
		"a name over 60":      {strings.Repeat("é", 61)},
		"51 rooms":            slices.Repeat([]string{"Room"}, 51),
		"a name on two lines": {"Room\n1"},
	} {
		if _, err := roomNames(names); err != ErrBadRooms {
			t.Errorf("%s: %v, want bad_rooms", name, err)
		}
	}
}

// In a breakout room, only what is done to a person in it and the room's own
// board can be done; what belongs to the whole call is done from its main
// room, where its record is.
func TestARoomRunsOnlyWhatIsItsOwn(t *testing.T) {
	for _, a := range []string{ActMute, ActMuteAll, ActAskUnmute, ActAskAllUnmute, ActSpotlight, ActEndSpotlight,
		ActLowerHand, ActLowerAll, ActPresentBoard, ActStopBoard, ActBoardDraw, ActBoardGuests, ActUndoGuests} {
		if !roomAct(a) {
			t.Errorf("%s is refused in a room", a)
		}
	}
	for _, a := range []string{ActEnd, ActLock, ActUnlock, ActMakeCohost, ActRemoveCohost, ActRemove, ActMuteOnEntry,
		ActHold, ActAllow, ActAdmit, ActDeny, ActAdmitAll, ActBreakoutsSet, ActBreakoutsAssign, ActBreakoutsSpread,
		ActBreakoutsOpen, ActBreakoutsClose, ActBreakoutsBroadcast} {
		if roomAct(a) {
			t.Errorf("%s is done in a room", a)
		}
	}
}

// Every screen in the call, and in each of its rooms, reads the rooms from the
// room's metadata: whether they are open, whether people choose, when they
// close by themselves, when everyone is called back, their names, and who is
// in which. A room's own metadata also says which room it is.
func TestEveryScreenReadsTheRoomsFromItsRoom(t *testing.T) {
	open := time.UnixMilli(1_700_000_000_000)
	ends := open.Add(10 * time.Minute)
	s := model.Session{BreakoutsOpenAt: &open, BreakoutsChoose: true, BreakoutsEndsAt: &ends}
	rooms := []model.Breakout{{Position: 1, Name: "Design"}, {Position: 2, Name: "Build"}}
	got := breakoutsMeta(&s, rooms, map[string]int{"0xa": 1, "guest-g": 2}, 0)
	want := map[string]any{
		"open": true, "choose": true, "endsAt": ends.UnixMilli(),
		"rooms":    []map[string]any{{"n": 1, "name": "Design"}, {"n": 2, "name": "Build"}},
		"assigned": map[string]int{"0xa": 1, "guest-g": 2},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the main room reads %v, want %v", got, want)
	}
	if inRoom := breakoutsMeta(&s, rooms, nil, 2); inRoom["room"] != 2 {
		t.Errorf("room 2 reads it is room %v", inRoom["room"])
	}
	back := open.Add(time.Minute)
	closed := breakoutsMeta(&model.Session{BreakoutsClosingAt: &back}, rooms, nil, 0)
	if closed["open"] != false || closed["closingAt"] != nil || closed["endsAt"] != nil {
		t.Errorf("rooms everyone was called back from read %v", closed)
	}
	counting := breakoutsMeta(&model.Session{BreakoutsOpenAt: &open, BreakoutsClosingAt: &back}, rooms, nil, 0)
	if counting["open"] != true || counting["closingAt"] != back.UnixMilli() {
		t.Errorf("rooms counting down read %v", counting)
	}
}
