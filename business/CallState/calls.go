package business

// Calls from LiveKit's webhook: someone joined, someone left, the call ended;
// and the reads.

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/CallState"
	collab "github.com/akashc777/OneCamp/services/collab"
	"github.com/google/uuid"
)

// JoinEvent is LiveKit's word that someone connected to, or left, a call.
type JoinEvent struct {
	Room    string
	RoomSID string
	// Identity is who LiveKit says they are, Name the name they show.
	Identity string
	Name     string
	// UserUUID is a member's; nil for a guest.
	UserUUID *uuid.UUID
	// At is when it happened, as LiveKit says.
	At time.Time
}

// RoleRule answers the part someone takes in a call they are joining; starter
// says their join began it. business/CallControl holds the rule.
type RoleRule func(ctx context.Context, ref Ref, e JoinEvent, starter bool) string

// errStillOpening is a call that ended between being made and being read.
var errStillOpening = errors.New("callstate: the room's call ended while it was being opened")

// Joined records a join: it opens the room's call when none is open (the
// first person in starts it), and records the person in it. A repeated or
// stale join changes nothing. Answers the call and the joiner's part in it.
func Joined(ctx context.Context, e JoinEvent, roleOf RoleRule) (*Session, *Person, error) {
	ref, kind, ok := RefOfRoom(e.Room)
	if !ok || strings.TrimSpace(e.Identity) == "" {
		return nil, nil, ErrNotACallRoom
	}
	if e.At.IsZero() {
		e.At = time.Now()
	}
	parent, err := parentOf(ctx, e.Room, kind)
	if err != nil {
		return nil, nil, err
	}
	open, made, err := openFor(ctx, ref, kind, e, parent)
	if err != nil {
		return nil, nil, err
	}
	if made {
		emit(ctx, resourceEventBusiness.CallStarted, open, actorOf(e), nil, map[string]string{"by": e.Name, "started_at": stamp(open.StartedAt)})
	}
	role := roleOf(ctx, ref, e, made)
	first, back, err := model.Joined(ctx, model.Attendee{SessionID: open.ID, Identity: e.Identity, UserUUID: e.UserUUID, Name: e.Name, Role: role}, e.At)
	if err != nil {
		return nil, nil, err
	}
	if first || back {
		emit(ctx, resourceEventBusiness.CallJoined, open, actorOf(e), nil, map[string]string{
			"identity": e.Identity, "role": role, "rejoin": strconv.FormatBool(back),
		})
		Publish(ctx, open.Room)
	}
	s, err := view(ctx, open)
	if err != nil {
		return nil, nil, err
	}
	return s, s.Person(e.Identity), nil
}

// openFor answers the room's open call, making it when there is none. A call
// still open in a room LiveKit has since closed and made again (its end never
// reached us) is over: it is ended, and this join begins a new one.
// parentOf is the live call a breakout room is of (nil for a call's own
// room). A room whose call has ended opens nothing: its people are called
// back with the call.
func parentOf(ctx context.Context, room, kind string) (*uuid.UUID, error) {
	if kind != model.KindBreakout {
		return nil, nil
	}
	parentRoom, _, _ := ParentOfBreakout(room)
	parent, err := model.OpenSessionOf(ctx, parentRoom)
	if err != nil {
		return nil, err
	}
	if parent == nil {
		return nil, ErrNotACallRoom
	}
	return &parent.ID, nil
}

func openFor(ctx context.Context, ref Ref, kind string, e JoinEvent, parent *uuid.UUID) (*model.Session, bool, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		s, made, err := model.OpenSession(ctx, model.Session{
			ID: uuid.New(), Room: e.Room, RoomSID: e.RoomSID, Kind: kind,
			ResourceKind: ref.Kind, ResourceID: ref.ID, StartedBy: e.UserUUID, StartedAt: e.At,
			ParentSessionID: parent,
		})
		if err != nil {
			lastErr = err
			continue
		}
		if !made && staleRoom(s, e.RoomSID) && s.GoesOnInRooms(e.At) {
			// LiveKit closed the call's own room while everyone was in its
			// breakout rooms, and made it again for their way back: the same call.
			if err := model.AdoptRoomSID(ctx, s.ID, e.RoomSID); err != nil {
				return nil, false, err
			}
			s.RoomSID = e.RoomSID
			madeAgain(ctx, s)
			return s, false, nil
		}
		if !made && staleRoom(s, e.RoomSID) {
			if _, _, err := endSession(ctx, s, model.EndedReconciled, resourceEventBusiness.Actor{System: true}, e.At); err != nil {
				return nil, false, err
			}
			lastErr = errStillOpening
			continue
		}
		return s, made, nil
	}
	return nil, false, lastErr
}

// roomMadeAgain writes a call's settings into its own room made again
// (OnRoomMadeAgain).
var roomMadeAgain func(ctx context.Context, s *Session)

// OnRoomMadeAgain sets what runs when a call takes on its own room made
// again: LiveKit closed it while everyone was in the call's breakout rooms,
// and a new room holds none of the metadata every screen reads the call's
// settings from. business/CallControl, which writes them, can't be imported
// here.
func OnRoomMadeAgain(fn func(ctx context.Context, s *Session)) {
	roomMadeAgain = fn
}

// madeAgain runs roomMadeAgain for a call, in the background.
func madeAgain(ctx context.Context, row *model.Session) {
	if roomMadeAgain == nil {
		return
	}
	helpers.GoSafeNamed("calls.room.again", func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		s, err := view(ctx, row)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/CallState could not read the call in %s made again: %+v", row.Room, err)
			return
		}
		roomMadeAgain(ctx, s)
	})
}

// staleRoom is a call that belongs to an earlier LiveKit room of the same
// name. Pure.
func staleRoom(s *model.Session, roomSID string) bool {
	return s.RoomSID != "" && roomSID != "" && s.RoomSID != roomSID
}

// Left records a leave. A leave from an earlier room of the same name, or one
// older than the person's latest join, changes nothing.
func Left(ctx context.Context, e JoinEvent) (*Session, bool, error) {
	open, err := model.OpenSessionOf(ctx, e.Room)
	if err != nil || open == nil || staleRoom(open, e.RoomSID) {
		return nil, false, err
	}
	if e.At.IsZero() {
		e.At = time.Now()
	}
	role := model.RoleParticipant
	if e.UserUUID == nil {
		role = model.RoleGuest
	}
	changed, err := model.Left(ctx, model.Attendee{SessionID: open.ID, Identity: e.Identity, UserUUID: e.UserUUID, Name: e.Name, Role: role}, e.At)
	if err != nil {
		return nil, false, err
	}
	if changed {
		emit(ctx, resourceEventBusiness.CallLeft, open, actorOf(e), nil, map[string]string{"identity": e.Identity})
		Publish(ctx, open.Room)
	}
	s, err := view(ctx, open)
	if changed && s != nil {
		// The call no longer covers them on the board on its stage.
		closeBoardOf(s)
	}
	return s, changed, err
}

// End ends the call going on in a room (the last person left, or LiveKit
// closed the room), answering the call and whether this ended it. An end from
// an earlier room of the same name ends nothing, and nor does one while the
// call goes on in its breakout rooms: it answers the call, still live.
func End(ctx context.Context, room, roomSID, reason string) (*Session, bool, error) {
	open, err := model.OpenSessionOf(ctx, room)
	if err != nil || open == nil || staleRoom(open, roomSID) {
		return nil, false, err
	}
	if open.GoesOnInRooms(time.Now()) {
		s, err := view(ctx, open)
		return s, false, err
	}
	return endSession(ctx, open, reason, resourceEventBusiness.Actor{System: true}, time.Now())
}

// EndLeftInRooms ends the call in a room that goes on in its breakout rooms
// once nobody has been in it, its own room or any of its rooms, for longer
// than the word to come back takes (model.BreakoutReturnGrace). Its rooms end
// with it, so whoever joins later starts a call of their own. It answers
// whether it ended the call.
func EndLeftInRooms(ctx context.Context, room string, now time.Time) (bool, error) {
	open, err := model.OpenSessionOf(ctx, room)
	if err != nil || open == nil || open.Kind == model.KindBreakout || !open.GoesOnInRooms(now) {
		return false, err
	}
	left, err := model.LeftInRooms(ctx, []uuid.UUID{open.ID}, now.Add(-model.BreakoutReturnGrace))
	if err != nil || !left[open.ID] {
		return false, err
	}
	_, ended, err := endSession(ctx, open, model.EndedReconciled, resourceEventBusiness.Actor{System: true}, now)
	return ended, err
}

func endSession(ctx context.Context, open *model.Session, reason string, actor resourceEventBusiness.Actor, at time.Time) (*Session, bool, error) {
	ended, err := model.EndSession(ctx, open.ID, reason, at)
	if err != nil {
		return nil, false, err
	}
	if !ended {
		s, err := sessionByID(ctx, open.ID)
		return s, false, err
	}
	s, err := sessionByID(ctx, open.ID)
	if err != nil || s == nil {
		return s, true, err
	}
	duration, endedAt := 0, ""
	if s.EndedAt != nil {
		duration, endedAt = int(s.EndedAt.Sub(s.StartedAt).Seconds()), stamp(*s.EndedAt)
	}
	emit(ctx, resourceEventBusiness.CallEnded, open, actor, nil, map[string]string{
		"reason": reason, "duration_s": strconv.Itoa(duration), "people": strconv.Itoa(len(s.People)),
		"started_at": stamp(s.StartedAt), "ended_at": endedAt,
	})
	Publish(ctx, open.Room)
	// An ended call covers nobody on the board that was on its stage.
	closeBoardOf(s)
	if open.Kind != model.KindBreakout {
		// Its breakout rooms end with it.
		EndRoomsOf(ctx, open.ID, model.EndedRoomsClosed)
	}
	return s, true, nil
}

// EndRoomsOf ends the calls of a call's open breakout rooms and closes their
// LiveKit rooms, best-effort: the sweep ends any it missed, as a room of a
// call that no longer goes on in its rooms (business/CallControl).
func EndRoomsOf(ctx context.Context, parent uuid.UUID, reason string) {
	rooms, err := model.OpenRoomsOf(ctx, parent)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/CallState could not read the breakout rooms of %s to end them: %+v", parent, err)
		return
	}
	for i := range rooms {
		if _, _, err := endSession(ctx, &rooms[i], reason, resourceEventBusiness.Actor{System: true}, time.Now()); err != nil {
			helpers.LogErrorWithContext(ctx, "business/CallState could not end the breakout room %s: %+v", rooms[i].Room, err)
			continue
		}
		liveKitBusiness.DeleteRoom(ctx, rooms[i].Room)
	}
}

// closeBoardOf closes the live sockets on the board on a call's stage, so
// everyone on it is asked again what they may do: a leave and an end, by
// whichever path (the webhooks, an end for everyone, the sweep), go through
// Left and endSession, so the grant ends at once on each.
func closeBoardOf(s *Session) {
	if s.Board != nil {
		collab.ReauthBoardLater(s.Board.UUID, nil)
	}
}

// Current answers the call going on in a conversation or meeting, or nil.
func Current(ctx context.Context, ref Ref) (*Session, error) {
	open, err := model.CurrentSession(ctx, ref.Kind, ref.ID)
	if err != nil || open == nil {
		return nil, err
	}
	return view(ctx, open)
}

// Get answers one call by its id, or nil.
func Get(ctx context.Context, id string) (*Session, error) {
	sid, err := uuid.Parse(strings.TrimSpace(id))
	if err != nil {
		return nil, nil
	}
	return sessionByID(ctx, sid)
}

// OpenCalls answers the calls going on, oldest first, at most limit of them.
func OpenCalls(ctx context.Context, limit int) ([]*Session, error) {
	rows, err := model.OpenSessions(ctx, limit)
	if err != nil {
		return nil, err
	}
	// Time in each call's breakout rooms, read once for every call listed
	// (CR-67's SF on package C), not once a call.
	parents := make([]uuid.UUID, 0, len(rows))
	for i := range rows {
		if rows[i].Kind != model.KindBreakout {
			parents = append(parents, rows[i].ID)
		}
	}
	inRooms, err := model.SecondsInRoomsOf(ctx, parents, time.Now())
	if err != nil {
		return nil, err
	}
	out := make([]*Session, 0, len(rows))
	for i := range rows {
		s, err := viewWith(ctx, &rows[i], inRooms[rows[i].ID])
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func sessionByID(ctx context.Context, id uuid.UUID) (*Session, error) {
	s, err := model.SessionByID(ctx, id)
	if err != nil || s == nil {
		return nil, err
	}
	return view(ctx, s)
}

// view is a call's row read with its people and recordings.
func view(ctx context.Context, s *model.Session) (*Session, error) {
	var inRooms map[string]int
	if s.Kind != model.KindBreakout {
		var err error
		if inRooms, err = model.SecondsInRooms(ctx, s.ID, time.Now()); err != nil {
			return nil, err
		}
	}
	return viewWith(ctx, s, inRooms)
}

// viewWith is view with the time each person has spent in the call's
// breakout rooms already read (nil for a room's own call, or none).
func viewWith(ctx context.Context, s *model.Session, inRooms map[string]int) (*Session, error) {
	attendees, err := model.Attendees(ctx, s.ID)
	if err != nil {
		return nil, err
	}
	recordings, err := model.Recordings(ctx, s.ID)
	if err != nil {
		return nil, err
	}
	out := viewOf(s, attendees, recordings)
	// Time in a call's breakout rooms is time in the call (CR-62's SF3 on
	// package C): a room's own call keeps the visits, and the call adds them.
	for i := range out.People {
		out.People[i].SecondsIn += inRooms[out.People[i].Identity]
	}
	return out, nil
}

// viewOf is a call as it is read, from its rows. Pure.
func viewOf(s *model.Session, attendees []model.Attendee, recordings []model.Recording) *Session {
	out := &Session{
		ID: s.ID.String(), Kind: s.Kind, Resource: Ref{Kind: s.ResourceKind, ID: s.ResourceID},
		Room: s.Room, RoomSID: s.RoomSID, Live: s.Open(), StartedAt: s.StartedAt, EndedAt: s.EndedAt,
		EndedReason: s.EndedReason, Topic: s.Topic, Spotlight: s.Spotlight, MuteOnEntry: s.MuteOnEntry, Held: s.Held,
		QA:     QA{Open: s.QAOpen, Shown: s.QAShown, Anonymous: s.QAAnonymous},
		Board:  boardOf(s),
		People: make([]Person, 0, len(attendees)), Recordings: make([]Recording, 0, len(recordings)),
	}
	if s.StartedBy != nil {
		out.StartedBy = s.StartedBy.String()
	}
	if s.PostUUID != nil {
		out.PostUUID = s.PostUUID.String()
	}
	if s.ParentSessionID != nil {
		out.Parent = s.ParentSessionID.String()
	}
	if s.BreakoutsOpenAt != nil || s.BreakoutsClosingAt != nil {
		out.Breakouts = &Breakouts{OpenAt: s.BreakoutsOpenAt, Choose: s.BreakoutsChoose, EndsAt: s.BreakoutsEndsAt, ClosingAt: s.BreakoutsClosingAt}
	}
	for _, a := range attendees {
		p := Person{
			Identity: a.Identity, Name: a.Name, Role: a.Role, Guest: a.UserUUID == nil, In: a.In(),
			JoinedAt: a.JoinedAt, LeftAt: a.LeftAt, SecondsIn: a.SecondsIn, Removed: a.RemovedBy != "",
			HandRaisedAt: a.HandRaisedAt, Feedback: a.Feedback, Allowed: a.Allowed,
		}
		if a.UserUUID != nil {
			p.UserUUID = a.UserUUID.String()
		}
		out.People = append(out.People, p)
	}
	if s.LockedAt != nil {
		by := "Someone"
		if p := out.Person(s.LockedBy); p != nil && p.Name != "" {
			by = p.Name
		}
		out.Locked = &Lock{By: by, At: *s.LockedAt}
	}
	for _, r := range recordings {
		rec := Recording{EgressID: r.EgressID, StartedAt: r.StartedAt, EndedAt: r.EndedAt, Status: r.Status}
		if r.StartedBy != nil {
			rec.StartedBy = r.StartedBy.String()
		}
		out.Recordings = append(out.Recordings, rec)
	}
	return out
}

// actorOf is who a join or leave was by: the member, or nobody for a guest.
func actorOf(e JoinEvent) resourceEventBusiness.Actor {
	if e.UserUUID == nil {
		return resourceEventBusiness.Actor{}
	}
	return resourceEventBusiness.Actor{UserUUID: e.UserUUID.String()}
}

// stamp is a call's time as its events carry it: UTC, to the nanosecond. Pure.
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// emit tells Resource Events about a change to a call. Data always says which
// call (session) and room, and what kind of call it is. A breakout room's
// call says nothing: it is part of its parent call, which is what people,
// pushes, webhooks and workflows hear of.
func emit(ctx context.Context, kind resourceEventBusiness.Kind, s *model.Session, actor resourceEventBusiness.Actor, subjects []string, data map[string]string) {
	if s.Kind == model.KindBreakout {
		return
	}
	if data == nil {
		data = map[string]string{}
	}
	data["session"] = s.ID.String()
	data["room"] = s.Room
	data["kind"] = s.Kind
	resourceEventBusiness.Bus.Emit(ctx, resourceEventBusiness.Event{
		Kind: kind, ResourceKind: s.ResourceKind, ResourceUUID: s.ResourceID,
		Actor: actor, At: time.Now(), Subjects: subjects, Data: data,
	})
}
