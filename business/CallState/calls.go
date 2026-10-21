package business

// Calls from LiveKit's webhook: someone joined, someone left, the call ended;
// and the reads.

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	model "github.com/akashc777/OneCamp/models/postgres/CallState"
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
	open, made, err := openFor(ctx, ref, kind, e)
	if err != nil {
		return nil, nil, err
	}
	if made {
		emit(ctx, resourceEventBusiness.CallStarted, open, actorOf(e), nil, map[string]string{"by": e.Name})
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
func openFor(ctx context.Context, ref Ref, kind string, e JoinEvent) (*model.Session, bool, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		s, made, err := model.OpenSession(ctx, model.Session{
			ID: uuid.New(), Room: e.Room, RoomSID: e.RoomSID, Kind: kind,
			ResourceKind: ref.Kind, ResourceID: ref.ID, StartedBy: e.UserUUID, StartedAt: e.At,
		})
		if err != nil {
			lastErr = err
			continue
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
	return s, changed, err
}

// End ends the call going on in a room (the last person left, or LiveKit
// closed the room), answering the call and whether this ended it. An end from
// an earlier room of the same name ends nothing.
func End(ctx context.Context, room, roomSID, reason string) (*Session, bool, error) {
	open, err := model.OpenSessionOf(ctx, room)
	if err != nil || open == nil || staleRoom(open, roomSID) {
		return nil, false, err
	}
	return endSession(ctx, open, reason, resourceEventBusiness.Actor{System: true}, time.Now())
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
	duration := 0
	if s.EndedAt != nil {
		duration = int(s.EndedAt.Sub(s.StartedAt).Seconds())
	}
	emit(ctx, resourceEventBusiness.CallEnded, open, actor, nil, map[string]string{
		"reason": reason, "duration_s": strconv.Itoa(duration), "people": strconv.Itoa(len(s.People)),
	})
	Publish(ctx, open.Room)
	return s, true, nil
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
	out := make([]*Session, 0, len(rows))
	for i := range rows {
		s, err := view(ctx, &rows[i])
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
	attendees, err := model.Attendees(ctx, s.ID)
	if err != nil {
		return nil, err
	}
	recordings, err := model.Recordings(ctx, s.ID)
	if err != nil {
		return nil, err
	}
	return viewOf(s, attendees, recordings), nil
}

// viewOf is a call as it is read, from its rows. Pure.
func viewOf(s *model.Session, attendees []model.Attendee, recordings []model.Recording) *Session {
	out := &Session{
		ID: s.ID.String(), Kind: s.Kind, Resource: Ref{Kind: s.ResourceKind, ID: s.ResourceID},
		Room: s.Room, RoomSID: s.RoomSID, Live: s.Open(), StartedAt: s.StartedAt, EndedAt: s.EndedAt,
		EndedReason: s.EndedReason, Topic: s.Topic, Spotlight: s.Spotlight, MuteOnEntry: s.MuteOnEntry,
		People: make([]Person, 0, len(attendees)), Recordings: make([]Recording, 0, len(recordings)),
	}
	if s.StartedBy != nil {
		out.StartedBy = s.StartedBy.String()
	}
	if s.PostUUID != nil {
		out.PostUUID = s.PostUUID.String()
	}
	for _, a := range attendees {
		p := Person{
			Identity: a.Identity, Name: a.Name, Role: a.Role, Guest: a.UserUUID == nil, In: a.In(),
			JoinedAt: a.JoinedAt, LeftAt: a.LeftAt, SecondsIn: a.SecondsIn, Removed: a.RemovedBy != "",
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

// emit tells Resource Events about a change to a call. Data always says which
// call (session) and room, and what kind of call it is.
func emit(ctx context.Context, kind resourceEventBusiness.Kind, s *model.Session, actor resourceEventBusiness.Actor, subjects []string, data map[string]string) {
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
