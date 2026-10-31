package business

import (
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	model "github.com/akashc777/OneCamp/models/postgres/CallState"
)

// Breakout rooms (calls package C, meet-10 and zoom-02; approved by main 26
// Oct 06:5x). Hosts and co-hosts run them (CanBreakouts). A room's media is
// reached only through a token the server mints for a room someone may be
// in (MayTakeRoom): its name is never enough.

// The breakout rooms' acts, on the call's own session (breakoutActs.go): what
// hosts and co-hosts do (CanBreakouts), and choosing your own room while
// people may, or the main room at any time (room 0).
const (
	ActBreakoutsSet       = "breakouts_set"
	ActBreakoutsAssign    = "breakouts_assign"
	ActBreakoutsSpread    = "breakouts_spread"
	ActBreakoutsOpen      = "breakouts_open"
	ActBreakoutsClose     = "breakouts_close"
	ActBreakoutsBroadcast = "breakouts_broadcast"
	ActBreakoutChoose     = "breakout_choose"
)

// The refusals of a call's rooms and their tokens.
var (
	ErrNotYourRoom  = &Refusal{http.StatusForbidden, "not_your_room", "That isn't your room. Whoever runs the call can move you, or you can go back to the main room."}
	ErrNoSuchRoom   = &Refusal{http.StatusNotFound, "no_such_room", "That room isn't there."}
	ErrNoRooms      = &Refusal{http.StatusConflict, "no_rooms", "Set up the rooms first."}
	ErrRoomsOpen    = &Refusal{http.StatusConflict, "rooms_open", "The rooms are open. Close them first."}
	ErrRoomsClosed  = &Refusal{http.StatusConflict, "rooms_closed", "The rooms aren't open."}
	ErrMainRoomOnly = &Refusal{http.StatusConflict, "main_room_only", "That's done for the whole call, from its main room."}
	ErrBadRooms     = &Refusal{http.StatusBadRequest, "bad_rooms", "Name each room in up to 60 characters, on one line, and make 1 to 50 rooms."}
	ErrBadMinutes   = &Refusal{http.StatusBadRequest, "bad_minutes", "Give the rooms up to 240 minutes, or no time limit."}
	ErrBadBroadcast = &Refusal{http.StatusBadRequest, "bad_broadcast", "Write a message of up to 500 characters."}
)

// The limits of a call's rooms: a room's name, how long they may stay open by
// themselves, and a message to every room.
const (
	maxRoomName        = 60
	MaxBreakoutMinutes = 240
	maxBroadcast       = 500
)

// metaBreakouts is the room metadata key a call's rooms live under, in its
// own room and in each of its rooms, which every screen reads (breakoutsMeta).
const metaBreakouts = "breakouts"

// roomNames is a call's rooms' names as they are kept: trimmed, each 1 to 60
// characters on one line, and 1 to MaxBreakouts of them. Pure.
func roomNames(names []string) ([]string, *Refusal) {
	if len(names) < 1 || len(names) > callStateBusiness.MaxBreakouts {
		return nil, ErrBadRooms
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" || utf8.RuneCountInString(n) > maxRoomName || strings.ContainsFunc(n, unicode.IsControl) {
			return nil, ErrBadRooms
		}
		out = append(out, n)
	}
	return out, nil
}

// broadcastText is a message to every room as it is sent: trimmed, 1 to 500
// characters. Pure.
func broadcastText(text string) (string, *Refusal) {
	text = strings.TrimSpace(text)
	if text == "" || utf8.RuneCountInString(text) > maxBroadcast {
		return "", ErrBadBroadcast
	}
	return text, nil
}

// roomAct is whether an action may be done in a breakout room's call: what is
// done to someone in the room, and the room's own board. What belongs to the
// whole call (ending it, locking it, its co-hosts, holds, lobby and rooms) is
// done from its main room, where its record is. Pure.
func roomAct(action string) bool {
	switch action {
	case ActMute, ActMuteAll, ActAskUnmute, ActAskAllUnmute, ActSpotlight, ActEndSpotlight, ActLowerHand, ActLowerAll:
		return true
	}
	return boardAction(action)
}

// breakoutsMeta is what every screen reads of a call's rooms (metaBreakouts):
// whether they are open, whether people choose, when they close by
// themselves and when everyone is called back (Unix milliseconds), their
// names, and who is in which by call identity. In room n's own metadata it
// also says which room it is (n 0: the call's own room). Pure.
func breakoutsMeta(s *model.Session, rooms []model.Breakout, assigned map[string]int, n int) map[string]any {
	named := make([]map[string]any, 0, len(rooms))
	for _, r := range rooms {
		named = append(named, map[string]any{"n": r.Position, "name": r.Name})
	}
	if assigned == nil {
		assigned = map[string]int{}
	}
	out := map[string]any{"open": s.BreakoutsOpenAt != nil, "rooms": named, "assigned": assigned}
	if s.BreakoutsOpenAt != nil {
		out["choose"] = s.BreakoutsChoose
		if s.BreakoutsEndsAt != nil {
			out["endsAt"] = s.BreakoutsEndsAt.UnixMilli()
		}
		if s.BreakoutsClosingAt != nil {
			out["closingAt"] = s.BreakoutsClosingAt.UnixMilli()
		}
	}
	if n > 0 {
		out["room"] = n
	}
	return out
}

// Spread assigns people round rooms 1 to n in the order they are listed, so
// no two rooms differ by more than one. Nobody, or no rooms, spreads nobody.
// Pure.
func Spread(people []string, rooms int) map[string]int {
	out := make(map[string]int, len(people))
	if rooms < 1 {
		return out
	}
	for i, p := range people {
		out[p] = i%rooms + 1
	}
	return out
}

// MayTakeRoom answers whether someone with a part in the call, assigned to
// room assigned (0: none yet), may have a token for room want of its rooms:
// their own room; any room while people may choose (choose); any room, to
// visit, for whoever runs the call. Never a room that isn't there. Pure.
func MayTakeRoom(role string, assigned, want, rooms int, choose bool) *Refusal {
	if want < 1 || want > rooms {
		return ErrNoSuchRoom
	}
	if model.RunsCall(role) || choose || assigned == want {
		return nil
	}
	return ErrNotYourRoom
}
