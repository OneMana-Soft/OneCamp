package business

// What the people running a call change about it (business/CallControl's
// actions): someone's part, the lock, the spotlight, people joining muted,
// the call's end. Each is recorded here, told to Resource Events where others
// listen for it, and published to the conversation's people.

import (
	"context"
	"errors"
	"slices"
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

// IsLivePost is whether post is a call's post while the call goes on (calls
// CALLS-3b): the call's thread hangs off it until the call ends, so it isn't
// deleted before then.
func IsLivePost(ctx context.Context, post uuid.UUID) (bool, error) {
	live, err := model.LiveCallsOfPosts(ctx, []uuid.UUID{post})
	return len(live) > 0, err
}

// AttachPost gives the call its post in its conversation, once (calls
// CALLS-3b, business/Huddle): only where it has none yet, so instances that
// raced to write it attach the same one. Answers whether this made it the
// call's post; the people in the call are told.
func AttachPost(ctx context.Context, s *Session, post uuid.UUID) (bool, error) {
	row, err := rowOf(ctx, s)
	if err != nil {
		return false, err
	}
	won, err := model.AttachPost(ctx, row.ID, post)
	if err != nil || !won {
		return false, err
	}
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

// SetHeld holds what (one of model.Holds) for everyone in the call who
// doesn't run it, or with on false lets them again, answering what the call
// holds now.
func SetHeld(ctx context.Context, s *Session, what string, on bool) ([]string, error) {
	row, err := rowOf(ctx, s)
	if err != nil {
		return nil, err
	}
	held, live, err := model.SetHeld(ctx, row.ID, what, on)
	if err != nil {
		return nil, err
	}
	if !live {
		return nil, ErrCallOver
	}
	// Its conversation hears of a chat held, so an open reply box asks again.
	Publish(ctx, row.Room)
	return held, nil
}

// SetAllowed lets someone in the call do what (one of model.Holds) although
// the call holds it, or with on false takes that back, answering what they're
// allowed now. ErrNotInThisCall when the call has no record of them.
func SetAllowed(ctx context.Context, s *Session, identity, what string, on bool) ([]string, error) {
	row, err := rowOf(ctx, s)
	if err != nil {
		return nil, err
	}
	allowed, found, err := model.SetAllowed(ctx, row.ID, identity, what, on)
	if err == nil && !found {
		err = ErrNotInThisCall
	}
	return allowed, err
}

// SetMuteOnEntry turns on or off people joining the call muted.
func SetMuteOnEntry(ctx context.Context, s *Session, on bool) error {
	row, err := rowOf(ctx, s)
	if err != nil {
		return err
	}
	return model.SetMuteOnEntry(ctx, row.ID, on)
}

// SetQA sets the call's Q&A; ErrCallOver once it has ended.
func SetQA(ctx context.Context, s *Session, qa QA) error {
	row, err := rowOf(ctx, s)
	if err != nil {
		return err
	}
	live, err := model.SetQA(ctx, row.ID, qa.Open, qa.Shown, qa.Anonymous)
	if err == nil && !live {
		err = ErrCallOver
	}
	return err
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

// SetHand raises or lowers someone's hand, answering whether it changed and,
// for a raised hand, when it went up. A hand raised again keeps its place in
// the queue (the time it first went up).
func SetHand(ctx context.Context, s *Session, identity string, raised bool) (bool, *time.Time, error) {
	row, err := rowOf(ctx, s)
	if err != nil {
		return false, nil, err
	}
	var at *time.Time
	if raised {
		now := time.Now().UTC().Truncate(time.Millisecond)
		at = &now
	}
	changed, err := model.SetHand(ctx, row.ID, identity, at)
	if err != nil {
		return false, nil, err
	}
	if changed {
		Publish(ctx, row.Room)
	} else if raised {
		if a, err := model.AttendeeOf(ctx, row.ID, identity); err == nil && a != nil {
			at = a.HandRaisedAt
		}
	}
	return changed, at, nil
}

// LowerAllHands lowers every raised hand in a call, answering whose.
func LowerAllHands(ctx context.Context, s *Session) ([]string, error) {
	row, err := rowOf(ctx, s)
	if err != nil {
		return nil, err
	}
	lowered, err := model.LowerAllHands(ctx, row.ID)
	if err == nil && len(lowered) > 0 {
		Publish(ctx, row.Room)
	}
	return lowered, err
}

// ErrNoSuchFeedback is feedback the call doesn't know.
var ErrNoSuchFeedback = errors.New("callstate: no such feedback")

// SetFeedback sets ("" clears) what stays beside someone's name, answering
// whether it changed.
func SetFeedback(ctx context.Context, s *Session, identity, feedback string) (bool, error) {
	if feedback != "" && !slices.Contains(model.FeedbackKinds, feedback) {
		return false, ErrNoSuchFeedback
	}
	row, err := rowOf(ctx, s)
	if err != nil {
		return false, err
	}
	changed, err := model.SetFeedback(ctx, row.ID, identity, feedback)
	if err == nil && changed {
		Publish(ctx, row.Room)
	}
	return changed, err
}
