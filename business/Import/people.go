package business

// The people an import brought across, as its admin is offered them for an
// invitation.
//
// WHY. An import makes everyone it meets a placeholder so their tasks, comments
// and messages keep their names, and a placeholder cannot sign in. The history
// arrived and the team did not: the only way in was to invite each person by
// hand from the people list, one address at a time, and nobody did. This is the
// list "Invite the N people who came across" is made from; the invitations go
// out through the workspace's own invitation code, so the seat limit, the email
// and the link the admin can share are the ones every invitation has.
//
// Offered: people the import made placeholders for whose address is really
// theirs. Not offered, and counted so the admin is told why: people already in
// the workspace, people already invited, people who had left the source (or
// were deactivated here), bots, and placeholders whose address was made up
// because the source had none. An invitation to a made-up address could never
// be accepted, and accepting one with the real address would not find the
// placeholder, so those people wait for the admin to say who they are.

import (
	"context"
	"strings"

	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	emailService "github.com/akashc777/OneCamp/services/Email"
	"github.com/google/uuid"
)

// InvitablePerson is someone the admin can invite in one step.
type InvitablePerson struct {
	UserID uuid.UUID `json:"user_id"`
	Name   string    `json:"name"`
	Email  string    `json:"email"`
}

// SeatRoom is the free plan's room: how many people the workspace has, how many
// its licence covers, and how many more can join. Left is nil when there is no
// limit.
type SeatRoom struct {
	Used  int  `json:"used"`
	Limit int  `json:"limit"`
	Left  *int `json:"left"`
}

// EmailRoom is whether invitations are emailed at all, and how many more can
// be today. Left is nil when the day has no cap; under one (lent email on
// OneCamp Cloud, EMAIL_DAILY_CAP) it stops short of the cap, keeping a few for
// password resets (services/Email InvitationReserve). Anyone invited past it
// is invited all the same, with a link for the admin to share.
type EmailRoom struct {
	On   bool `json:"on"`
	Left *int `json:"left"`
}

// ImportPeople is the invitation offer for one import.
type ImportPeople struct {
	People         []InvitablePerson `json:"people"`
	AlreadyMembers int               `json:"already_members"`
	AlreadyInvited int               `json:"already_invited"`
	NoEmail        int               `json:"no_email"`
	Left           int               `json:"left"`
	Seats          SeatRoom          `json:"seats"`
	Email          EmailRoom         `json:"email"`
}

// emailRoom is today's room for invitation emails.
func emailRoom() EmailRoom {
	room := EmailRoom{On: emailService.IsEmailEnabled()}
	if left, capped := emailService.InvitationsLeftToday(); capped {
		room.Left = &left
	}
	return room
}

type personKind int

const (
	personInvitable personKind = iota
	personMember
	personInvited
	personNoEmail
	personLeft
	personBot
)

// classifyPerson says what the offer does with one imported person. Pure.
func classifyPerson(p importModels.ImportedPerson) personKind {
	switch {
	case p.IsBot || p.SourceBot:
		return personBot
	case p.Deactivated || p.SourceLeft:
		return personLeft
	case !p.IsExternal:
		return personMember
	case IsPlaceholderEmail(p.Email):
		return personNoEmail
	case p.Invited:
		return personInvited
	}
	return personInvitable
}

// IsPlaceholderEmail reports whether an address on a placeholder was made up
// by an import or an integration rather than being the person's own: the
// imports' @no-reply.local addresses, GitHub's @external.onecamp.local ones,
// and the "+x-…" / "+slack-…" suffixes an import adds when an address was
// already taken. Pure.
func IsPlaceholderEmail(email string) bool {
	email = strings.ToLower(strings.TrimSpace(email))
	at := strings.LastIndex(email, "@")
	if at <= 0 || at == len(email)-1 {
		return true
	}
	local, domain := email[:at], email[at+1:]
	if domain == "no-reply.local" || strings.HasPrefix(domain, "external.onecamp.") {
		return true
	}
	return strings.Contains(local, "+x-") || strings.Contains(local, "+slack-")
}

// seatRoom is what the licence leaves. A count that fails is reported as no
// limit known rather than as full: the offer must not refuse people because a
// query timed out, and the invitation code checks the limit again anyway. Pure
// apart from its inputs.
func seatRoom(used, limit int) SeatRoom {
	room := SeatRoom{Used: used, Limit: limit}
	if limit > 0 {
		left := limit - used
		if left < 0 {
			left = 0
		}
		room.Left = &left
	}
	return room
}

// buildImportPeople turns the imported people into the offer. Pure.
func buildImportPeople(people []importModels.ImportedPerson, used, limit int) *ImportPeople {
	out := &ImportPeople{People: []InvitablePerson{}, Seats: seatRoom(used, limit)}
	seenEmail := make(map[string]bool, len(people))
	for _, p := range people {
		switch classifyPerson(p) {
		case personMember:
			out.AlreadyMembers++
		case personInvited:
			out.AlreadyInvited++
		case personNoEmail:
			out.NoEmail++
		case personLeft:
			out.Left++
		case personInvitable:
			email := strings.ToLower(strings.TrimSpace(p.Email))
			if seenEmail[email] {
				continue
			}
			seenEmail[email] = true
			name := strings.TrimSpace(p.Name)
			if name == "" {
				name = email[:strings.LastIndex(email, "@")]
			}
			out.People = append(out.People, InvitablePerson{UserID: p.UserID, Name: name, Email: email})
		}
	}
	return out
}

// PeopleToInvite is the invitation offer for an import: who came across and
// can be invited now, why the rest can't, and how much room the plan has.
func PeopleToInvite(ctx context.Context, jobId uuid.UUID) (*ImportPeople, error) {
	people, err := importModels.ListImportedPeople(ctx, jobId)
	if err != nil {
		return nil, err
	}
	used, limit, err := userDomain.SeatUsage(ctx)
	if err != nil {
		helpers.LogWarnWithContext(ctx, "Import.PeopleToInvite seat count failed job=%s err=%+v", jobId, err)
		used, limit = 0, 0
	}
	offer := buildImportPeople(people, used, limit)
	offer.Email = emailRoom()
	return offer, nil
}
