package Calendar

// Who an event's call keeps what it makes for, and where it is filed
// (meet-02): its recording, transcript and notes land on the event, and its
// recap in the one conversation of the event's people (main's ruling 4 (a)).

import (
	"context"

	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// CallPeople are who an event's call keeps what it makes for: its organiser
// first, then each member it invites once, in the order they were added. A
// guest invited by email is never one of them.
type CallPeople struct {
	Title   string
	Members []uuid.UUID
}

// EventCallPeople reads an event's CallPeople, as the server (the recap that
// asks has no reader): ErrEventNotFound for one in the trash or not there.
func EventCallPeople(ctx context.Context, id uuid.UUID) (*CallPeople, error) {
	e, guests, err := stored.load(ctx, id)
	if err != nil {
		return nil, err
	}
	return callPeople(e, guests), nil
}

// callPeople is an event's organiser, then its member guests, each once. Pure.
func callPeople(e *models.Event, guests []models.Attendee) *CallPeople {
	p := &CallPeople{Title: e.Title}
	add := func(id uuid.UUID) {
		for _, m := range p.Members {
			if m == id {
				return
			}
		}
		if id != uuid.Nil {
			p.Members = append(p.Members, id)
		}
	}
	add(e.CreatedBy)
	for _, g := range guests {
		if g.UserID != nil {
			add(*g.UserID)
		}
	}
	return p
}

// CallKeys is what an event's call files what it makes under: the event's own
// uuid and its series' (an instance's call is its series'), and the room of
// its call, whose first holder Call State knows.
type CallKeys struct {
	Events []uuid.UUID
	Room   string
}

// CallArtefactKeys answers an event's CallKeys for someone who may read the
// event (resolve: the calendar's own rule), and ErrEventNotFound for anyone
// else, as for no event at all.
func (s *service) CallArtefactKeys(ctx context.Context, who *model.UserInfo, id string) (CallKeys, error) {
	return s.callArtefactKeys(ctx, who, id, false)
}

// CallArtefactKeysToChange answers an event's CallKeys for someone who may
// change the event (mayChange: its organiser, or a guest the organiser lets
// change it), to delete what its call made or bring it back: the rule that
// gives the event a call and takes it off. A guest who may only read the
// event is refused as for any change; anyone else, as for no event at all.
func (s *service) CallArtefactKeysToChange(ctx context.Context, who *model.UserInfo, id string) (CallKeys, error) {
	return s.callArtefactKeys(ctx, who, id, true)
}

// callArtefactKeys is both: the calendar's read rule, and with change its
// change rule on the event that holds the call (an instance's is its
// series').
func (s *service) callArtefactKeys(ctx context.Context, who *model.UserInfo, id string, change bool) (CallKeys, error) {
	if err := s.ready(ctx); err != nil {
		return CallKeys{}, err
	}
	inst, guests, err := s.resolve(ctx, who, id)
	if err != nil {
		return CallKeys{}, err
	}
	owner := inst.owner()
	if change {
		if err := mayChange(roleOf(owner, guests, personOf(who)), owner); err != nil {
			return CallKeys{}, err
		}
	}
	keys := CallKeys{Events: []uuid.UUID{inst.ID}, Room: owner.CallRoom}
	if owner.ID != inst.ID {
		keys.Events = append(keys.Events, owner.ID)
	}
	return keys, nil
}
