package business

// The reconcile sweep (calls.session.reconcile, every minute, CL11): LiveKit
// is the truth about who is in a room now. A webhook lost on its way (a
// restart, a network blink, a webhook address set wrong) must never leave a
// call open that has ended, someone in a call they left, or a call going on
// that nobody recorded, and so nobody can run.

import (
	"context"
	"time"

	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/livekitInit"
	model "github.com/akashc777/OneCamp/models/postgres/CallState"
	"github.com/google/uuid"
	"github.com/livekit/protocol/livekit"
)

func init() {
	sweepBusiness.Jobs.Register(sweepBusiness.Job{Name: "calls.session.reconcile", Every: time.Minute, Run: Reconcile})
}

// reconcileLimit is how many open calls one pass looks at.
const reconcileLimit = 500

// emptyGrace is how long a call may sit with nobody in it before the sweep
// ends it: its people may still be connecting.
const emptyGrace = 30 * time.Second

// Reconcile is one pass of the sweep: every call recorded as going on, and
// every room of this workspace's LiveKit has people in.
func Reconcile(ctx context.Context) error {
	if !livekitInit.LiveKitService.Ready() {
		return nil
	}
	rooms, err := livekitInit.LiveKitService.ListAllRooms(ctx)
	if err != nil {
		return err
	}
	live := make(map[string]*livekit.Room, len(rooms))
	for _, r := range rooms {
		if r != nil {
			live[r.Name] = r
		}
	}
	open, err := callStateBusiness.OpenCalls(ctx, reconcileLimit)
	if err != nil {
		return err
	}
	dueBreakouts(ctx, open)
	seen := make(map[string]bool, len(open))
	for _, s := range open {
		seen[s.Room] = true
		reconcileRoom(ctx, s.Room, live[s.Room], s)
	}
	endLeftInRooms(ctx, open)
	// A room nobody recorded a call in, with people in it by LiveKit's count.
	// LiveKit refreshes the count every 5 s, so a call that began in the last
	// seconds is the next pass's; reading the count first spares a shared
	// server's other rooms a lookup each.
	for name, r := range live {
		if seen[name] || r.NumParticipants == 0 || !callStateBusiness.BelongsHere(ctx, name) {
			continue
		}
		reconcileRoom(ctx, name, r, nil)
	}
	return nil
}

// endLeftInRooms ends each call that goes on in its breakout rooms with
// nobody in it, its own room or any of its rooms, for longer than the word to
// come back takes. Its rooms end with it, and whoever joins later starts a
// call of their own, which nothing of the old one, its recording included,
// carries on into.
func endLeftInRooms(ctx context.Context, open []*callStateBusiness.Session) {
	now := time.Now()
	rooms := map[uuid.UUID]string{}
	var calls []uuid.UUID
	for _, s := range open {
		if s.Parent == "" && s.GoesOnInRooms(now) {
			id := uuid.MustParse(s.ID)
			calls = append(calls, id)
			rooms[id] = s.Room
		}
	}
	left, err := model.LeftInRooms(ctx, calls, now.Add(-model.BreakoutReturnGrace))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/CallControl could not read which calls were left in their rooms: %+v", err)
		return
	}
	// Each end is the call's own: its events, its rooms. Under the room's lock
	// it is checked again, in case someone came back since the read.
	for id := range left {
		room := rooms[id]
		roomLocks.Acquire(room, 0)
		ended, err := callStateBusiness.EndLeftInRooms(ctx, room, now)
		roomLocks.Release(room)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/CallControl could not end the call left in its rooms in %s: %+v", room, err)
		} else if ended {
			helpers.LogInfoWithContext(ctx, "calls.session.reconcile ended the call in %s: nobody in it or its rooms since the grace to come back", room)
		}
	}
}

// reconcileRoom makes a room's call record what LiveKit says is going on in
// it: the call ends when the room has gone or emptied, anyone LiveKit has and
// the record lacks is recorded, and anyone the record has in it that LiveKit
// doesn't has left. s is the room's open call, nil when none is recorded.
func reconcileRoom(ctx context.Context, room string, lk *livekit.Room, s *callStateBusiness.Session) {
	roomLocks.Acquire(room, 0)
	defer roomLocks.Release(room)
	if lk == nil {
		if s != nil {
			endReconciled(ctx, room, "")
		}
		return
	}
	people, err := livekitInit.LiveKitService.ConnectedParticipants(ctx, room)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/CallControl could not list the people in %s: %+v", room, err)
		return
	}
	var humans []*livekit.ParticipantInfo
	for _, p := range people {
		if livekitInit.IsPerson(p) {
			humans = append(humans, p)
		}
	}
	if s != nil && s.RoomSID != "" && lk.Sid != "" && s.RoomSID != lk.Sid {
		// The room was closed and made again: the recorded call is an old one.
		endReconciled(ctx, room, s.RoomSID)
		s = nil
	}
	// Ends a call only when nobody is in it, asked as the leave webhook asks.
	if anyone, _ := stillHere(people, nil); !anyone {
		if s != nil && time.Since(s.StartedAt) > emptyGrace {
			endReconciled(ctx, room, lk.Sid)
		}
		return
	}
	now := time.Now()
	present := make(map[string]bool, len(humans))
	for _, p := range humans {
		present[p.Identity] = true
		if s != nil {
			if q := s.Person(p.Identity); q != nil && q.In {
				continue
			}
		}
		got, person, err := callStateBusiness.Joined(ctx, eventOf(ctx, room, lk.Sid, p, now), roleRule)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/CallControl could not record %s in %s: %+v", p.Identity, room, err)
			continue
		}
		if person != nil {
			seat(got, p, person)
		}
	}
	if s == nil {
		return
	}
	var after *callStateBusiness.Session
	for _, q := range s.InCall() {
		if present[q.Identity] {
			continue
		}
		e := callStateBusiness.JoinEvent{Room: room, RoomSID: lk.Sid, Identity: q.Identity, Name: q.Name, At: now}
		if id, err := uuid.Parse(q.UserUUID); err == nil {
			e.UserUUID = &id
		}
		left, changed, err := callStateBusiness.Left(ctx, e)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/CallControl could not record %s leaving %s: %+v", q.Identity, room, err)
			continue
		}
		if changed && left != nil {
			after = left
		}
	}
	// A presenter the sweep found gone takes their board off the stage, as
	// their leave's webhook would have. after is read once the leaves (and
	// this pass's joins) are written, never the call as the pass began.
	if after == nil {
		return
	}
	if err := settleStage(ctx, after); err != nil {
		helpers.LogErrorWithContext(ctx, "business/CallControl could not settle the board on %s's stage: %+v", room, err)
	}
}

func endReconciled(ctx context.Context, room, roomSID string) {
	if _, _, err := callStateBusiness.End(ctx, room, roomSID, model.EndedReconciled); err != nil {
		helpers.LogErrorWithContext(ctx, "business/CallControl could not end the call in %s: %+v", room, err)
	}
}
