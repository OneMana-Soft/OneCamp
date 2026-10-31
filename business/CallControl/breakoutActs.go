package business

// A call's breakout rooms at work (calls package C, meet-10 and zoom-02):
// those who run the call set them up, put people in them, open them, speak to
// them all and close them; anyone may go back to the main room, and choose a
// room while people may. Moving is the server's word to one person
// ({t:"breakout"}, never a token in a packet) and their own ask for that
// room's token. The rooms' state is mirrored into the room metadata of the
// call's own room and of each of its rooms, which every screen reads.

import (
	"context"
	"errors"
	"slices"
	"time"

	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/livekitInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/CallState"
	"github.com/google/uuid"
)

// roomsGrace is how long the rooms stay up after everyone in them was called
// back, so the word reaches them before their room closes under them.
const roomsGrace = 10 * time.Second

func init() {
	callStateBusiness.OnRoomMadeAgain(restoreRoom)
}

// restoreRoom writes a call's settings into its own room, made again after
// LiveKit closed it while everyone was in the call's breakout rooms: what it
// holds, its lock, spotlight, muting on entry, the board on its stage, its
// rooms and its transcription switch, as the acts that set them write them;
// and it records the call again if its recording ended with the room
// (resumeRecording).
func restoreRoom(ctx context.Context, s *callStateBusiness.Session) {
	meta := map[string]any{}
	if s.Locked != nil {
		meta[metaLocked] = map[string]any{"by": s.Locked.By, "since": s.Locked.At.UnixMilli()}
	}
	if s.Spotlight != "" {
		meta[metaSpotlight] = map[string]any{"identity": s.Spotlight}
	}
	if s.MuteOnEntry {
		meta[metaMuteOnEntry] = true
	}
	if len(s.Held) > 0 {
		meta[metaHeld] = s.Held
	}
	if s.Board != nil {
		meta[metaBoard] = s.Board
	}
	if len(meta) > 0 {
		if _, err := livekitInit.LiveKitService.UpdateRoomMetadata(ctx, s.Room, meta); err != nil {
			helpers.LogErrorWithContext(ctx, "business/CallControl could not write the call's settings into %s made again: %+v", s.Room, err)
		}
	}
	if err := mirrorBreakouts(ctx, s); err != nil {
		helpers.LogErrorWithContext(ctx, "business/CallControl could not write the call's rooms into %s made again: %+v", s.Room, err)
	}
	if err := liveKitBusiness.FollowCall(ctx, s.Room); err != nil {
		helpers.LogErrorWithContext(ctx, "business/CallControl could not give %s made again its call's transcription switch: %+v", s.Room, err)
	}
	resumeRecording(ctx, s)
}

// actOnBreakouts does what someone running a call asked of its rooms (an
// action whose ability is CanBreakouts), on the call's own session. Who runs
// the rooms is often in one of them, so they act from any room of the call.
func actOnBreakouts(ctx context.Context, actor *dgraphStruct.DgraphUser, sessionID string, a Action) (Result, error) {
	s, me, err := inCallOrItsRooms(ctx, sessionID, actor.Uid)
	if err != nil {
		return Result{}, err
	}
	if !slices.Contains(AbilitiesOf(s.Resource.Kind, me.Role), CanBreakouts) {
		return Result{}, ErrNotAllowed
	}
	parent, err := uuid.Parse(s.ID)
	if err != nil {
		return Result{}, ErrNotInCall
	}
	roomLocks.Acquire(s.Room, 0)
	defer roomLocks.Release(s.Room)
	now := time.Now()
	switch a.Action {
	case ActBreakoutsSet:
		names, bad := roomNames(a.Rooms)
		if bad != nil {
			return Result{}, bad
		}
		set, err := model.SetBreakouts(ctx, parent, names)
		if err != nil {
			return Result{}, err
		}
		if !set {
			return Result{}, ErrRoomsOpen
		}
		return Result{Affected: len(names)}, mirrorBreakouts(ctx, s)
	case ActBreakoutsAssign:
		if a.Identity == "" {
			return Result{}, ErrNoTarget
		}
		if p := s.Person(a.Identity); p == nil || p.Removed {
			return Result{}, ErrTargetGone
		}
		return assign(ctx, s, map[string]int{a.Identity: a.Room}, actor.Uid)
	case ActBreakoutsSpread:
		rooms, err := model.Breakouts(ctx, parent)
		if err != nil {
			return Result{}, err
		}
		if len(rooms) == 0 {
			return Result{}, ErrNoRooms
		}
		people, err := toSpread(ctx, s)
		if err != nil {
			return Result{}, err
		}
		return assign(ctx, s, Spread(people, len(rooms)), actor.Uid)
	case ActBreakoutsOpen:
		if a.Minutes < 0 || a.Minutes > MaxBreakoutMinutes {
			return Result{}, ErrBadMinutes
		}
		var endsAt *time.Time
		if a.Minutes > 0 {
			t := now.Add(time.Duration(a.Minutes) * time.Minute)
			endsAt = &t
		}
		opened, err := model.OpenBreakouts(ctx, parent, a.On, endsAt, now)
		if err != nil {
			return Result{}, err
		}
		if !opened {
			rooms, err := model.Breakouts(ctx, parent)
			if err != nil {
				return Result{}, err
			}
			if len(rooms) == 0 {
				return Result{}, ErrNoRooms
			}
			return Result{}, ErrRoomsOpen
		}
		if err := mirrorBreakouts(ctx, s); err != nil {
			return Result{}, err
		}
		assigned, err := model.Assignments(ctx, parent)
		if err != nil {
			return Result{}, err
		}
		n := sendToRooms(ctx, s, assigned)
		if endsAt != nil {
			later(parent, *endsAt)
		}
		return Result{Affected: n}, nil
	case ActBreakoutsClose:
		back, closing, err := model.CloseBreakouts(ctx, parent, now)
		if err != nil {
			return Result{}, err
		}
		if !closing {
			return Result{}, ErrRoomsClosed
		}
		if err := mirrorBreakouts(ctx, s); err != nil {
			return Result{}, err
		}
		tellAcross(ctx, s, liveKitBusiness.CallPacket{T: "breakout_closing", By: me.Name, At: back.UTC().Format(time.RFC3339)})
		later(parent, back)
		return Result{Affected: 1}, nil
	case ActBreakoutsBroadcast:
		text, bad := broadcastText(a.Text)
		if bad != nil {
			return Result{}, bad
		}
		if s.Breakouts == nil || s.Breakouts.OpenAt == nil {
			return Result{}, ErrRoomsClosed
		}
		rooms := tellAcross(ctx, s, liveKitBusiness.CallPacket{T: "breakout_broadcast", By: me.Name, Text: text})
		return Result{Affected: rooms}, nil
	}
	return Result{}, ErrUnknownAction
}

// assign puts people in a call's rooms (room 0: back in its main room), and
// while the rooms are open sends each to theirs.
func assign(ctx context.Context, s *callStateBusiness.Session, rooms map[string]int, by string) (Result, error) {
	parent := uuid.MustParse(s.ID)
	into := map[string]int{}
	var back []string
	for identity, n := range rooms {
		if n == 0 {
			back = append(back, identity)
			continue
		}
		into[identity] = n
	}
	if len(back) > 0 {
		if err := model.Unassign(ctx, parent, back); err != nil {
			return Result{}, err
		}
	}
	if len(into) > 0 {
		done, err := model.Assign(ctx, parent, into, by)
		if err != nil {
			return Result{}, err
		}
		if !done {
			return Result{}, ErrNoSuchRoom
		}
	}
	if err := mirrorBreakouts(ctx, s); err != nil {
		return Result{}, err
	}
	if s.Breakouts != nil && s.Breakouts.OpenAt != nil && s.Breakouts.ClosingAt == nil {
		sendToRooms(ctx, s, rooms)
	}
	return Result{Affected: len(rooms)}, nil
}

// toSpread is everyone in a call's own room now who doesn't run it, in the
// order they first joined: whom "spread everyone" puts in the rooms.
func toSpread(ctx context.Context, s *callStateBusiness.Session) ([]string, error) {
	here, err := livekitInit.LiveKitService.ConnectedParticipants(ctx, s.Room)
	if err != nil {
		return nil, err
	}
	present := map[string]bool{}
	for _, p := range here {
		if livekitInit.IsPerson(p) {
			present[p.Identity] = true
		}
	}
	var out []string
	for _, p := range s.People {
		if present[p.Identity] && !p.Removed && !model.RunsCall(p.Role) {
			out = append(out, p.Identity)
		}
	}
	return out, nil
}

// sendToRooms tells each person where they now belong ({t:"breakout"}, room 0
// for the main room), in whichever of the call's rooms they are: one packet
// per room, to everyone it is for. Answers how many were told.
func sendToRooms(ctx context.Context, s *callStateBusiness.Session, rooms map[string]int) int {
	names, err := model.Breakouts(ctx, uuid.MustParse(s.ID))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/CallControl could not read the rooms of %s to send people to them: %+v", s.Room, err)
		return 0
	}
	to := map[int][]string{}
	for identity, n := range rooms {
		to[n] = append(to[n], identity)
	}
	for n, identities := range to {
		p := liveKitBusiness.CallPacket{T: "breakout", Room: n}
		if n > 0 && n <= len(names) {
			p.Name = names[n-1].Name
		}
		tellAcross(ctx, s, p, identities...)
	}
	return len(rooms)
}

// chooseRoom puts someone in a room of the call they chose: any room while
// people choose (their own at any time), or with room 0 back in its main
// room, which anyone may at any time.
func chooseRoom(ctx context.Context, identity, sessionID string, n int) (Result, error) {
	s, me, err := inCallOrItsRooms(ctx, sessionID, identity)
	if err != nil {
		return Result{}, err
	}
	parent := uuid.MustParse(s.ID)
	if n != 0 {
		if s.Breakouts == nil || s.Breakouts.OpenAt == nil || s.Breakouts.ClosingAt != nil {
			return Result{}, ErrRoomsClosed
		}
		rooms, err := model.Breakouts(ctx, parent)
		if err != nil {
			return Result{}, err
		}
		assigned, err := model.Assignments(ctx, parent)
		if err != nil {
			return Result{}, err
		}
		if refused := MayTakeRoom(me.Role, assigned[identity], n, len(rooms), s.Breakouts.Choose); refused != nil {
			return Result{}, refused
		}
	}
	roomLocks.Acquire(s.Room, 0)
	defer roomLocks.Release(s.Room)
	if n == 0 {
		if err := model.Unassign(ctx, parent, []string{identity}); err != nil {
			return Result{}, err
		}
	} else if done, err := model.Assign(ctx, parent, map[string]int{identity: n}, identity); err != nil {
		return Result{}, err
	} else if !done {
		return Result{}, ErrNoSuchRoom
	}
	return Result{Affected: 1}, mirrorBreakouts(ctx, s)
}

// inCallOrItsRooms is a live call (never one of its breakout rooms' calls) and
// someone in it now: in its own room or in one of its open rooms, as LiveKit
// has them, and never put out of it. What acts on its rooms start from, as
// present does for the others.
func inCallOrItsRooms(ctx context.Context, sessionID, identity string) (*callStateBusiness.Session, *callStateBusiness.Person, error) {
	s, err := liveCall(ctx, sessionID)
	if err != nil {
		return nil, nil, err
	}
	if s.Parent != "" {
		return nil, nil, ErrMainRoomOnly
	}
	me := s.Person(identity)
	if me == nil || me.Removed {
		return nil, nil, ErrNotInCall
	}
	here, err := inCallFamily(ctx, s, identity)
	if err != nil {
		return nil, nil, err
	}
	if !here {
		return nil, nil, ErrNotInCall
	}
	return s, me, nil
}

// inCallFamily is whether someone is in a call's own room now, or in the open
// breakout room of it Call State has them in, as LiveKit has them there.
func inCallFamily(ctx context.Context, s *callStateBusiness.Session, identity string) (bool, error) {
	here, err := livekitInit.LiveKitService.RoomHasParticipant(ctx, s.Room, identity)
	if err != nil || here || !s.GoesOnInRooms(time.Now()) {
		return here, err
	}
	in, err := model.InRooms(ctx, uuid.MustParse(s.ID))
	if err != nil {
		return false, err
	}
	room, ok := in[identity]
	if !ok {
		return false, nil
	}
	return livekitInit.LiveKitService.RoomHasParticipant(ctx, room, identity)
}

// RoomToken is a token for one of a call's breakout rooms: the LiveKit room it
// lets its holder into, that room's call, its number and its name.
type RoomToken struct {
	Token   string `json:"token"`
	Room    string `json:"room"`
	Session string `json:"session"`
	Number  int    `json:"number"`
	Name    string `json:"name"`
}

// BreakoutToken mints a member's token for room n of the call whose own
// session is sessionID: never through an API token.
func BreakoutToken(ctx context.Context, actor *dgraphStruct.DgraphUser, sessionID string, n int, language string) (RoomToken, error) {
	if helpers.ActsThroughToken(ctx) {
		return RoomToken{}, ErrThroughToken
	}
	if actor == nil || actor.Uid == "" {
		return RoomToken{}, ErrNotInCall
	}
	return roomToken(ctx, sessionID, actor.Uid, false, n, language)
}

// GuestBreakoutToken mints a guest's token for room n of their call, proved
// by their own call token, for the call's own room or one of its rooms.
func GuestBreakoutToken(ctx context.Context, callToken string, n int, language string) (RoomToken, error) {
	identity, room, err := GuestOf(callToken)
	if err != nil {
		return RoomToken{}, ErrNotInCall
	}
	if parent, _, ok := callStateBusiness.ParentOfBreakout(room); ok {
		room = parent
	}
	s, err := guestSession(ctx, room)
	if err != nil {
		return RoomToken{}, err
	}
	return roomToken(ctx, s.ID, identity, true, n, language)
}

// roomToken mints someone's token for room n of a call while its rooms are
// open: someone in the call now, for a room they may be in (MayTakeRoom), by
// the name, part and holds they have in the call. The room's LiveKit room and
// its call are made on first use.
func roomToken(ctx context.Context, sessionID, identity string, guest bool, n int, language string) (RoomToken, error) {
	s, me, err := inCallOrItsRooms(ctx, sessionID, identity)
	if err != nil {
		return RoomToken{}, err
	}
	if s.Breakouts == nil || s.Breakouts.OpenAt == nil {
		return RoomToken{}, ErrRoomsClosed
	}
	parent := uuid.MustParse(s.ID)
	rooms, err := model.Breakouts(ctx, parent)
	if err != nil {
		return RoomToken{}, err
	}
	assigned, err := model.Assignments(ctx, parent)
	if err != nil {
		return RoomToken{}, err
	}
	if refused := MayTakeRoom(me.Role, assigned[identity], n, len(rooms), s.Breakouts.Choose); refused != nil {
		return RoomToken{}, refused
	}
	room := callStateBusiness.BreakoutRoom(s.Room, n)
	if _, err := livekitInit.LiveKitService.CreateRoom(ctx, room); err != nil {
		return RoomToken{}, err
	}
	call, err := callStateBusiness.OpenRoom(ctx, s, n)
	if err != nil {
		return RoomToken{}, err
	}
	// The room captions while the call does, and only then. A room that
	// couldn't take the switch starts off, the safe side; anyone in it may
	// turn the call's on again.
	if err := liveKitBusiness.FollowCall(ctx, room); err != nil {
		helpers.LogErrorWithContext(ctx, "business/CallControl could not give room %s its call's transcription switch: %+v", room, err)
	}
	sources := liveKitBusiness.PublishSources(s.Held, me.Allowed, model.RunsCall(me.Role))
	var token string
	if guest {
		token, err = livekitInit.LiveKitService.GenerateGuestToken(room, identity, me.Name, language, sources)
	} else {
		token, err = liveKitBusiness.GenerateToken(ctx, room, identity, me.Name, false, language, sources)
	}
	if err != nil {
		return RoomToken{}, err
	}
	if err := mirrorBreakouts(ctx, s); err != nil {
		helpers.LogErrorWithContext(ctx, "business/CallControl could not tell room %s what its call's rooms are: %+v", room, err)
	}
	return RoomToken{Token: token, Room: room, Session: call.ID, Number: n, Name: rooms[n-1].Name}, nil
}

// mirrorBreakouts writes a call's rooms as they are now into the metadata of
// its own room and of each of its open rooms (breakoutsMeta).
func mirrorBreakouts(ctx context.Context, s *callStateBusiness.Session) error {
	parent := uuid.MustParse(s.ID)
	row, err := model.SessionByID(ctx, parent)
	if err != nil {
		return err
	}
	if row == nil {
		return ErrCallOver
	}
	rooms, err := model.Breakouts(ctx, parent)
	if err != nil {
		return err
	}
	assigned, err := model.Assignments(ctx, parent)
	if err != nil {
		return err
	}
	// The call's own room may be gone while everyone is in its rooms:
	// restoreRoom writes this into it when it is made again.
	if err := setMeta(ctx, s.Room, metaBreakouts, breakoutsMeta(row, rooms, assigned, 0)); err != nil && !errors.Is(err, livekitInit.ErrRoomNotFound) {
		return err
	}
	open, err := model.OpenRoomsOf(ctx, parent)
	if err != nil {
		return err
	}
	for _, r := range open {
		_, n, _ := callStateBusiness.ParentOfBreakout(r.Room)
		if err := setMeta(ctx, r.Room, metaBreakouts, breakoutsMeta(row, rooms, assigned, n)); err != nil {
			helpers.LogErrorWithContext(ctx, "business/CallControl could not tell %s what its call's rooms are: %+v", r.Room, err)
		}
	}
	return nil
}

// tellAcross sends a packet to people (or everyone) in a call's own room and
// in each of its open rooms, wherever they are; answers how many rooms of the
// call there were to tell.
func tellAcross(ctx context.Context, s *callStateBusiness.Session, p liveKitBusiness.CallPacket, identities ...string) int {
	tell(ctx, s.Room, p, identities...)
	rooms, err := model.OpenRoomsOf(ctx, uuid.MustParse(s.ID))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/CallControl could not read the rooms of %s to tell them: %+v", s.Room, err)
		return 0
	}
	for _, r := range rooms {
		tell(ctx, r.Room, p, identities...)
	}
	return len(rooms)
}

// later settles a call's rooms at when (they close by themselves, or their
// countdown ends), in this process; the sweep does it after a restart.
func later(parent uuid.UUID, when time.Time) {
	time.AfterFunc(time.Until(when), func() {
		helpers.GoSafeNamed("calls.breakouts", func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			settleBreakouts(ctx, parent, time.Now())
		})
	})
}

// settleBreakouts does what is due for a call's rooms at now: rooms whose
// time is up start their countdown, and once it has run everyone is called
// back, by whoever claims it first (FinishBreakouts).
func settleBreakouts(ctx context.Context, parent uuid.UUID, now time.Time) {
	row, err := model.SessionByID(ctx, parent)
	if err != nil || row == nil || !row.Open() || row.BreakoutsOpenAt == nil {
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/CallControl could not read %s to settle its rooms: %+v", parent, err)
		}
		return
	}
	s, err := callStateBusiness.Get(ctx, parent.String())
	if err != nil || s == nil {
		return
	}
	if row.BreakoutsClosingAt == nil {
		if row.BreakoutsEndsAt == nil || now.Before(*row.BreakoutsEndsAt) {
			return
		}
		back, closing, err := model.CloseBreakouts(ctx, parent, now)
		if err != nil || !closing {
			return
		}
		if err := mirrorBreakouts(ctx, s); err != nil {
			helpers.LogErrorWithContext(ctx, "business/CallControl could not show %s's rooms closing: %+v", s.Room, err)
		}
		tellAcross(ctx, s, liveKitBusiness.CallPacket{T: "breakout_closing", At: back.UTC().Format(time.RFC3339)})
		later(parent, back)
		return
	}
	finished, err := model.FinishBreakouts(ctx, parent, now)
	if err != nil || !finished {
		return
	}
	if err := mirrorBreakouts(ctx, s); err != nil {
		helpers.LogErrorWithContext(ctx, "business/CallControl could not show %s's rooms closed: %+v", s.Room, err)
	}
	tellAcross(ctx, s, liveKitBusiness.CallPacket{T: "breakout"})
	// The rooms close a little later, so the word reaches everyone in them
	// first; anyone still there then is put out by the close.
	time.AfterFunc(roomsGrace, func() {
		helpers.GoSafeNamed("calls.breakouts.close", func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			callStateBusiness.EndRoomsOf(ctx, parent, model.EndedRoomsClosed)
		})
	})
}

// dueBreakouts settles every call whose rooms have something due (the sweep,
// after a restart lost the timers), and ends the rooms of calls that no
// longer go on in them.
func dueBreakouts(ctx context.Context, open []*callStateBusiness.Session) {
	now := time.Now()
	due, err := model.DueBreakouts(ctx, now, reconcileLimit)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/CallControl could not read the calls whose rooms are due: %+v", err)
	}
	for _, id := range due {
		settleBreakouts(ctx, id, now)
	}
	parents := map[string]*callStateBusiness.Session{}
	for _, s := range open {
		if s.Parent == "" {
			parents[s.ID] = s
		}
	}
	strays := map[string]bool{}
	for _, s := range open {
		if s.Parent == "" || strays[s.Parent] {
			continue
		}
		// A room's call outlasts its parent's rooms only by the time the word
		// to come back takes, and never its parent.
		if p := parents[s.Parent]; p == nil || !p.GoesOnInRooms(now.Add(-roomsGrace)) {
			strays[s.Parent] = true
			callStateBusiness.EndRoomsOf(ctx, uuid.MustParse(s.Parent), model.EndedRoomsClosed)
		}
	}
}

// roomStayCheck is whether someone who connected to room n of the call in
// parentRoom may be in it: the rooms are open and it is a room they may be in
// (MayTakeRoom), as their part in the call has it. A token for a room is no
// right to it: the rooms may have closed since, or they were moved.
func roomStayCheck(ctx context.Context, parentRoom string, n int, identity string) (bool, string, error) {
	parent, err := model.OpenSessionOf(ctx, parentRoom)
	if err != nil {
		return false, "", err
	}
	if parent == nil || parent.BreakoutsOpenAt == nil {
		return false, liveKitBusiness.RemovedRoomClosed, nil
	}
	a, err := model.AttendeeOf(ctx, parent.ID, identity)
	if err != nil {
		return false, "", err
	}
	if a == nil || a.RemovedBy != "" {
		return false, liveKitBusiness.RemovedNotYourRoom, nil
	}
	rooms, err := model.Breakouts(ctx, parent.ID)
	if err != nil {
		return false, "", err
	}
	assigned, err := model.Assignments(ctx, parent.ID)
	if err != nil {
		return false, "", err
	}
	if MayTakeRoom(a.Role, assigned[identity], n, len(rooms), parent.BreakoutsChoose) != nil {
		return false, liveKitBusiness.RemovedNotYourRoom, nil
	}
	return true, "", nil
}
