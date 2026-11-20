package Calendar

// Each person's own choice to keep when they're busy to themselves (gcal-14,
// Google Calendar's "hide free/busy"): their value of the workspace's
// calendar.freebusy.members policy, kept in the Policy Registry's person
// scope, which only they set. It narrows the workspace's, never widens it:
// with the workspace's off nobody sees anyone's busy times, whatever they
// chose. Every reader of someone else's busy times asks busySeenOf.

import (
	"context"
	"encoding/json"

	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// FreeBusyChoice is a person's own choice as their settings show it.
type FreeBusyChoice struct {
	// Hidden is whether they keep when they're busy to themselves.
	Hidden bool `json:"hidden"`
	// Workspace is whether the workspace lets members see when others are
	// busy at all: off, nobody's shows, and the choice changes nothing.
	Workspace bool `json:"workspace"`
}

// The Policy Registry's reads and writes of a person's value; tests swap them.
var (
	personFreeBusySet = func(ctx context.Context, person string) (hidden bool, err error) {
		info, err := policyBusiness.Admin.InfoIn(ctx, policyBusiness.CalendarFreeBusy.Key, personScope(person))
		if err != nil {
			return false, err
		}
		shown, _ := info.Value.(bool)
		return info.IsSet && !shown, nil
	}
	setPersonFreeBusy = func(ctx context.Context, person string, hidden bool, by policyBusiness.Changer) error {
		var err error
		if hidden {
			_, err = policyBusiness.Admin.SetIn(ctx, policyBusiness.CalendarFreeBusy.Key, personScope(person), json.RawMessage(`false`), by)
		} else {
			_, err = policyBusiness.Admin.ClearIn(ctx, policyBusiness.CalendarFreeBusy.Key, personScope(person), by)
		}
		return err
	}
)

func personScope(person string) policyBusiness.Scope {
	return policyBusiness.Scope{Kind: policyBusiness.ScopePerson, ID: person}
}

// MyFreeBusy is who's own choice.
func MyFreeBusy(ctx context.Context, who *model.UserInfo) (FreeBusyChoice, error) {
	me := personOf(who)
	if me == uuid.Nil {
		return FreeBusyChoice{}, ErrEventNotFound
	}
	hidden, err := personFreeBusySet(ctx, me.String())
	if err != nil {
		return FreeBusyChoice{}, err
	}
	return FreeBusyChoice{Hidden: hidden, Workspace: freeBusyPolicy(ctx)}, nil
}

// SetMyFreeBusy keeps when who is busy to themselves (hidden), or shows it
// to members again as the workspace does; the change is recorded as a
// policy's is. Only the person sets their own.
func SetMyFreeBusy(ctx context.Context, who *model.UserInfo, hidden bool) (FreeBusyChoice, error) {
	me := personOf(who)
	if me == uuid.Nil {
		return FreeBusyChoice{}, ErrEventNotFound
	}
	by := policyBusiness.Changer{UserID: me, Email: who.UserPostgresInfo.EmailID}
	if err := setPersonFreeBusy(ctx, me.String(), hidden, by); err != nil {
		return FreeBusyChoice{}, err
	}
	return MyFreeBusy(ctx, who)
}
