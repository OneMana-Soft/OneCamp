package business

// Inviting (docs programme B6; the coordinator's decision on A5's gap): anyone
// who may edit a doc may invite people and teams to it, at their own level or
// below, as Notion lets an editor share. An invite only adds:
//   - never general access, subpages, removals, guest links or a replacement;
//   - never a person or team the doc already names, nor its owner, nor the
//     inviter, so an invite can't lower anyone's access, put an existing grant
//     on a clock (an "until" ends the grant it is on), or keep the inviter's
//     own access past a team or space they lose.
//
// Everything else stays full access's (Apply). A hard lock holds an editor at
// comment, and a commenter can't invite, so a hard-locked doc takes invites
// from full access alone.

import (
	"context"
	"errors"
	"strings"

	docBusiness "github.com/akashc777/OneCamp/business/Doc"
)

var (
	// ErrAboveYourLevel is an invite at a level above the inviter's own.
	ErrAboveYourLevel = errors.New("access: an invite can't give more access than you have")
	// ErrNotAnInvite is a change an invite can't make: anything but adding
	// people and teams.
	ErrNotAnInvite = errors.New("access: an invite only adds people and teams")
	// ErrAlreadyShared is an invite naming someone the doc already names.
	ErrAlreadyShared = errors.New("access: already has access")
)

// MayInvite reports whether a decision lets its person invite people and
// teams to the resource (Invite), each at their own level or below: edit or
// more, which a hard lock holds at comment, and nothing on a deleted
// resource. Everything else about who has access is full access's (Apply).
// Every offer to share that only adds people asks this, so the share
// dialog's Invite and sharing with someone just mentioned agree.
func MayInvite(d Decision) bool { return d.Level >= LevelEdit }

// inviteRefusal says why someone with decision d may not make change c as an
// invite, or nil when they may: each grant at their own level or below. Pure.
func inviteRefusal(d Decision, c Change) error {
	switch {
	case !MayInvite(d):
		return ErrForbidden
	case len(c.Add) == 0, len(c.Remove) > 0, c.General != nil, c.Private != nil, c.Replace, c.ApplyToSubpages:
		return ErrNotAnInvite
	}
	for _, g := range c.Add {
		switch {
		case g.Level <= LevelNone:
			return ErrBadLevel
		case g.Level > d.Level:
			return ErrAboveYourLevel
		}
	}
	return nil
}

// namedAlready says whether a grant names someone the doc names already: a
// person or a team it is shared with, its owner, or the inviter. Pure.
func namedAlready(before *docBusiness.Sharing, who Subject, g Grant) bool {
	id := strings.ToLower(g.UUID)
	if g.Kind == PrincipalTeam {
		return before.Teams[id] != nil
	}
	if _, ok := before.People[id]; ok {
		return true
	}
	if before.Owner != nil && strings.EqualFold(before.Owner.Uuid, id) {
		return true
	}
	return strings.EqualFold(who.UserUUID, id)
}

// Invite adds people and teams to a doc as who may (see above). It takes the
// one path every change to sharing takes (apply), with the invite's own rule:
// inviteRefusal before the sharing is read, and namedAlready against it once
// it is. What follows any change (the end of grants given until a date, the
// indexes, the events with invite=true, the live sockets) is apply's.
func Invite(ctx context.Context, who Subject, c Change) (Explanation, error) {
	return apply(ctx, who, c, applyOpts{
		allow: func(d Decision, c Change) error { return inviteRefusal(d, c) },
		against: func(c Change, before *docBusiness.Sharing) error {
			for _, g := range c.Add {
				if namedAlready(before, who, g) {
					return ErrAlreadyShared
				}
			}
			return nil
		},
		tags: map[string]string{"invite": "true"},
	})
}
