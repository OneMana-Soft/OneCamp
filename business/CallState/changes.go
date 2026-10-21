package business

// What the people running a call change about it (business/CallControl's
// actions): someone's part, the lock, the spotlight, people joining muted,
// the call's end. Each is recorded here, told to Resource Events where others
// listen for it, and published to the conversation's people.

import (
	"context"
	"time"

	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	model "github.com/akashc777/OneCamp/models/postgres/CallState"
	"github.com/google/uuid"
)

// rowOf is the call's row, read again: the action applies to the call as it is now.
func rowOf(ctx context.Context, s *Session) (*model.Session, error) {
	id, err := uuid.Parse(s.ID)
	if err != nil {
		return nil, err
	}
	row, err := model.SessionByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if row == nil || !row.Open() {
		return nil, ErrCallOver
	}
	return row, nil
}

// SetRole changes someone's part in a call (co-host and back, or host when the
// call's hosts have all gone), answering whether they were in it.
func SetRole(ctx context.Context, s *Session, identity, role string, actor resourceEventBusiness.Actor) (bool, error) {
	row, err := rowOf(ctx, s)
	if err != nil {
		return false, err
	}
	changed, err := model.SetRole(ctx, row.ID, identity, role)
	if err != nil || !changed {
		return false, err
	}
	subjects := []string(nil)
	if p := s.Person(identity); p != nil && p.UserUUID != "" {
		subjects = []string{p.UserUUID}
	}
	emit(ctx, resourceEventBusiness.CallRoleChanged, row, actor, subjects, map[string]string{"identity": identity, "role": role})
	Publish(ctx, row.Room)
	return true, nil
}

// NoteRemoved tells others that someone running the call put a person out of
// it. The removal itself, and the mark that keeps them out until the call
// ends, are business/LiveKit's RemoveFromCall.
func NoteRemoved(ctx context.Context, s *Session, identity string, actor resourceEventBusiness.Actor) {
	row, err := rowOf(ctx, s)
	if err != nil {
		return
	}
	subjects := []string(nil)
	if p := s.Person(identity); p != nil && p.UserUUID != "" {
		subjects = []string{p.UserUUID}
	}
	emit(ctx, resourceEventBusiness.CallParticipantRemoved, row, actor, subjects, map[string]string{"identity": identity})
	Publish(ctx, row.Room)
}

// SetLock locks a call (nobody new can join it) on behalf of by, or unlocks it.
func SetLock(ctx context.Context, s *Session, locked bool, by string, actor resourceEventBusiness.Actor) error {
	row, err := rowOf(ctx, s)
	if err != nil {
		return err
	}
	var at *time.Time
	kind := resourceEventBusiness.CallUnlocked
	if locked {
		now := time.Now()
		at = &now
		kind = resourceEventBusiness.CallLocked
	}
	if err := model.SetLock(ctx, row.ID, by, at); err != nil {
		return err
	}
	emit(ctx, kind, row, actor, nil, nil)
	Publish(ctx, row.Room)
	return nil
}

// SetSpotlight puts one person large on everyone's stage, or nobody with "".
func SetSpotlight(ctx context.Context, s *Session, identity string) error {
	row, err := rowOf(ctx, s)
	if err != nil {
		return err
	}
	return model.SetSpotlight(ctx, row.ID, identity)
}

// SetMuteOnEntry turns on or off people joining the call muted.
func SetMuteOnEntry(ctx context.Context, s *Session, on bool) error {
	row, err := rowOf(ctx, s)
	if err != nil {
		return err
	}
	return model.SetMuteOnEntry(ctx, row.ID, on)
}

// EndForEveryone ends a call on behalf of someone running it.
func EndForEveryone(ctx context.Context, s *Session, actor resourceEventBusiness.Actor) (*Session, error) {
	row, err := rowOf(ctx, s)
	if err != nil {
		return nil, err
	}
	ended, _, err := endSession(ctx, row, model.EndedByHost, actor, time.Now())
	return ended, err
}
