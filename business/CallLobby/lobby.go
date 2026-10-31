// Package business (CallLobby) is a meeting's lobby (design/calls-vs-zoom
// /PROGRAM.md §14, CALLS-2; the Lobby, #50): someone from outside knocks, waits
// on their page, and whoever runs the call admits or denies them (Call
// Control's admit, deny and admit_all). Members never wait (D-CL-5). Who may
// come in from outside at all stays the guest grant's and the guest-access
// switch's; the lobby only decides when.
//
// IMPORTS. One way: this package imports business/CallState (to tell the
// room's people) and its own model; business/CallControl imports this one, and
// the guest controller calls it before it mints a guest's call token.
package business

import (
	"context"
	"errors"
	"strings"
	"time"

	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	model "github.com/akashc777/OneCamp/models/postgres/CallState"
	"github.com/google/uuid"
)

var (
	// ErrNoKnock is a ticket that isn't a knock for this link.
	ErrNoKnock = errors.New("calllobby: no such knock")
	// ErrDenied is a knock someone running the call turned away.
	ErrDenied = errors.New("calllobby: denied")
)

// waitFor is how long one ask of a knock waits for an answer before saying
// "still waiting": the guest's page asks again at once.
var waitFor = 25 * time.Second

// pollEvery is how often a waiting ask looks.
var pollEvery = time.Second

// Waits answers whether someone from outside waits to be let into a room's
// call: everyone does, unless the meeting's owner chose "anyone with the
// link joins at once", or the call is an event's and they are one of its
// invitees (invited: their own invitation names this room, meet-02).
func Waits(ctx context.Context, room string, invited bool) (bool, error) {
	ref, _, ok := callStateBusiness.RefOfRoom(room)
	if !ok || ref.Kind != callStateBusiness.KindMeeting {
		return true, nil
	}
	m, err := model.MeetingOf(ctx, ref.ID)
	if err != nil {
		return true, err
	}
	if m == nil {
		return true, nil
	}
	switch m.Lobby {
	case model.LobbyNobody:
		return false, nil
	case model.LobbyInvitees:
		return !invited, nil
	}
	return true, nil
}

// ownerName is the name a guest may be shown for a member, never their
// address, "" when there is none to read: a seam for tests.
var ownerName = userDomain.NameForGuests

// Title is what a guest is told they're joining (the guest page names its
// call, as Zoom names its topic): the meeting's own title when it has one,
// else whose it is ("Maya Chen's meeting", or "Maya Chen's room" for a
// personal room), else "Meeting". A conversation's call, opened by its
// invite (HU13), is whoever invited them ("Maya Chen's call"), never the
// conversation's name. A room's is read each time, so it follows its owner's
// name. Never a member's address: guests are outside the workspace.
func Title(ctx context.Context, room string, owner uuid.UUID) string {
	kind := ""
	ref, _, ok := callStateBusiness.RefOfRoom(room)
	if ok && ref.Kind == callStateBusiness.KindMeeting {
		if m, err := model.MeetingOf(ctx, ref.ID); err == nil && m != nil {
			if m.Title != "" {
				return m.Title
			}
			kind = m.Kind
		}
	}
	call := ok && ref.Kind != callStateBusiness.KindMeeting
	if name := strings.TrimSpace(ownerName(ctx, owner)); name != "" {
		switch {
		case call:
			return name + "'s call"
		case kind == model.KindPersonal:
			return name + "'s room"
		}
		return name + "'s meeting"
	}
	if call {
		return "The call"
	}
	return "Meeting"
}

// SaveMeeting records a meeting's home as it starts: an instant meeting,
// owned by who started it, whose guests wait.
func SaveMeeting(ctx context.Context, id string, owner uuid.UUID) error {
	return model.SaveMeeting(ctx, model.Meeting{ID: id, Kind: "instant", Owner: owner, Lobby: model.LobbyGuests, CreatedAt: time.Now()})
}

// SetLobby changes who waits in a meeting's lobby: guests (true) or nobody.
// An event call's guests wait as it was made: all but its invitees.
func SetLobby(ctx context.Context, meetingID string, guestsWait bool) (bool, error) {
	lobby := model.LobbyNobody
	if guestsWait {
		lobby = model.LobbyGuests
		m, err := model.MeetingOf(ctx, meetingID)
		if err != nil {
			return false, err
		}
		if m != nil && m.Kind == model.KindEvent {
			lobby = model.LobbyInvitees
		}
	}
	return model.SetLobby(ctx, meetingID, lobby)
}

// Knock records someone from outside asking to come into a room's call
// through a grant, and tells the people running it. It answers their ticket.
func Knock(ctx context.Context, room string, grantID uuid.UUID, name string) (uuid.UUID, error) {
	k := model.Knock{ID: uuid.New(), Room: room, GrantID: grantID, Name: strings.TrimSpace(name), KnockedAt: time.Now()}
	if err := model.AddKnock(ctx, k); err != nil {
		return uuid.Nil, err
	}
	callStateBusiness.Publish(ctx, room)
	return k.ID, nil
}

// Answer waits up to waitFor for what comes of a knock made through grantID:
// admitted (true, with the name they knocked with), still waiting (false), or
// ErrDenied. A ticket made through another grant is ErrNoKnock, so a ticket
// alone opens nothing.
func Answer(ctx context.Context, ticket, grantID uuid.UUID) (bool, string, error) {
	deadline := time.Now().Add(waitFor)
	for {
		k, err := model.KnockByID(ctx, ticket)
		if err != nil {
			return false, "", err
		}
		if k == nil || k.GrantID != grantID {
			return false, "", ErrNoKnock
		}
		switch k.Decision {
		case model.KnockAdmitted:
			return true, k.Name, nil
		case model.KnockDenied, model.KnockGone:
			return false, "", ErrDenied
		}
		if time.Now().After(deadline) {
			return false, "", nil
		}
		select {
		case <-ctx.Done():
			return false, "", nil
		case <-time.After(pollEvery):
		}
	}
}

// Leave records that someone waiting gave up (closed their page).
func Leave(ctx context.Context, ticket, grantID uuid.UUID) error {
	k, err := model.KnockByID(ctx, ticket)
	if err != nil || k == nil || k.GrantID != grantID {
		return err
	}
	if changed, err := model.Decide(ctx, ticket, model.KnockGone, "", "", time.Now()); err != nil || !changed {
		return err
	}
	callStateBusiness.Publish(ctx, k.Room)
	return nil
}

// Waiting is someone in a room's lobby, as the people running the call see them.
type Waiting struct {
	Ticket    string    `json:"ticket"`
	Name      string    `json:"name"`
	KnockedAt time.Time `json:"knocked_at"`
}

// WaitingIn answers the people waiting in a room's lobby, oldest first.
func WaitingIn(ctx context.Context, room string) ([]Waiting, error) {
	rows, err := model.Waiting(ctx, room)
	if err != nil {
		return nil, err
	}
	out := make([]Waiting, 0, len(rows))
	for _, k := range rows {
		out = append(out, Waiting{Ticket: k.ID.String(), Name: k.Name, KnockedAt: k.KnockedAt})
	}
	return out, nil
}

// Decide admits or denies one person waiting in a room, answering whether
// they were still waiting there. by is who decided (their identity).
func Decide(ctx context.Context, room, ticket string, admit bool, by string) (bool, error) {
	id, err := uuid.Parse(ticket)
	if err != nil {
		return false, ErrNoKnock
	}
	k, err := model.KnockByID(ctx, id)
	if err != nil {
		return false, err
	}
	if k == nil || k.Room != room {
		return false, ErrNoKnock
	}
	decision := model.KnockDenied
	if admit {
		decision = model.KnockAdmitted
	}
	changed, err := model.Decide(ctx, id, decision, by, "", time.Now())
	if err == nil && changed {
		callStateBusiness.Publish(ctx, room)
	}
	return changed, err
}

// AdmitAll admits everyone waiting in a room, answering how many.
func AdmitAll(ctx context.Context, room, by string) (int, error) {
	n, err := model.DecideAll(ctx, room, model.KnockAdmitted, by, time.Now())
	if err != nil {
		return 0, err
	}
	if n > 0 {
		callStateBusiness.Publish(ctx, room)
	}
	return int(n), nil
}
