package business

// Who may come into a call now, beyond being allowed in it at all: not someone
// a host put out of it, and not someone new while it is locked.
//
// Asked twice: when a token is minted (so the green room can say why), and
// when someone connects with a token minted earlier (controllers/LiveKit's
// admit check), since a token outlives the moment it was minted.
//
// Fail closed (21 Oct): a record that doesn't answer keeps people out until it
// does. It used to let them in, so a database that blinked, or a pool that ran
// dry under load, let back in someone a host had just put out.

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	callStateModel "github.com/akashc777/OneCamp/models/postgres/CallState"
)

// CallLockedMsg is what someone kept out of a locked call reads.
const CallLockedMsg = "This call is locked, so nobody new can join it. Ask someone in the call to unlock it."

// ErrCallLocked: the people running the call locked it, and this person
// wasn't in it before.
var ErrCallLocked = &helpers.CallRefusal{Code: "call_locked", Msg: CallLockedMsg}

// RemovedLocked is the reason someone who connected to a locked call with an
// older token is told.
const RemovedLocked = "locked"

// CallUncheckedMsg is what someone reads when whether they may come into a
// call couldn't be checked: they can try again in a moment.
const CallUncheckedMsg = "We couldn't check the call just now. Try again in a moment."

// ErrCallUnchecked: the call's record didn't answer, so no way into it is
// given until it does: a removal or a lock it holds is never skipped. A 503,
// which a page tries again.
var ErrCallUnchecked = &helpers.CallRefusal{Code: "call_unchecked", Msg: CallUncheckedMsg, Status: http.StatusServiceUnavailable}

// RemovedUnconfirmed is the reason someone who has just connected is told when
// whether they may stay couldn't be checked: they can join again once it can.
const RemovedUnconfirmed = "unconfirmed"

// lockedOut reports whether a call's lock keeps someone out: it is locked,
// they weren't in it before it was locked, and they don't run it. a is their
// row in the call, nil when they were never in it. Pure.
func lockedOut(s *callStateModel.Session, a *callStateModel.Attendee) bool {
	if s == nil || s.LockedAt == nil {
		return false
	}
	if a == nil {
		return true
	}
	if a.Role == callStateModel.RoleHost || a.Role == callStateModel.RoleCohost {
		return false
	}
	return !a.JoinedAt.Before(*s.LockedAt)
}

// MayEnter answers whether a member may have a token for the call in a room
// now: nil, or the refusal (helpers.CallRefusal) the green room says before
// they press Join. meetingHost is the instant meeting's own host, who may
// always come back to it. A record that doesn't answer refuses
// (ErrCallUnchecked).
func MayEnter(ctx context.Context, room string, user *dgraphStruct.DgraphUser, meetingHost bool) error {
	s, err := callStateModel.OpenSessionOf(ctx, room)
	if err != nil && !noRecord(err) {
		return unchecked(ctx, room, err)
	}
	if s == nil {
		return nil
	}
	a, err := callStateModel.AttendeeOf(ctx, s.ID, user.Uid)
	if err != nil {
		return unchecked(ctx, room, err)
	}
	if a != nil && a.RemovedBy != "" {
		return ErrRemovedFromCall
	}
	if meetingHost || !lockedOut(s, a) {
		return nil
	}
	if host, err := HostByRule(ctx, room, user, IsWorkspaceAdmin(ctx, user.Uuid)); err == nil && host {
		return nil
	}
	return ErrCallLocked
}

// noRecord is a process with no Call State at all (a tool, a unit test: the
// server itself won't start without its database), where no call has a record
// to keep anyone out of; as against a record that didn't answer.
func noRecord(err error) bool { return errors.Is(err, callStateModel.ErrNoDatabase) }

// unchecked logs why a call couldn't be checked and answers ErrCallUnchecked.
func unchecked(ctx context.Context, room string, err error) error {
	helpers.LogErrorWithContext(ctx, "business/LiveKit could not check who may come into %s: %+v", room, err)
	return ErrCallUnchecked
}

// GuestMayEnter answers whether a guest may have a token for the meeting in a
// room now: not while it is locked, since every guest join is someone new,
// and not while its record doesn't answer.
func GuestMayEnter(ctx context.Context, room string) error {
	s, err := callStateModel.OpenSessionOf(ctx, room)
	if err != nil && !noRecord(err) {
		return unchecked(ctx, room, err)
	}
	if s != nil && s.LockedAt != nil {
		return ErrCallLocked
	}
	return nil
}

// LockedOut reports whether the lock on the call in a room keeps out someone
// who has just connected (with a token minted before it was locked); an
// error when the record didn't answer.
func LockedOut(ctx context.Context, room, identity string) (bool, error) {
	identity = strings.TrimSpace(identity)
	s, err := callStateModel.OpenSessionOf(ctx, room)
	if noRecord(err) {
		return false, nil
	}
	if err != nil || s == nil || s.LockedAt == nil || identity == "" {
		return false, err
	}
	a, err := callStateModel.AttendeeOf(ctx, s.ID, identity)
	if err != nil {
		return false, err
	}
	return lockedOut(s, a), nil
}
