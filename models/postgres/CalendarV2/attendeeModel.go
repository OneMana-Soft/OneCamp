package models

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// Attendee is one event_attendees row: a workspace member (UserID) or someone
// invited by email (A2).
type Attendee struct {
	EventID uuid.UUID
	UserID  *uuid.UUID
	Email   string
	// Name is how a guest invited by email was named ("Alex Doe"); a
	// member's name is their profile's.
	Name        string
	Role        string
	Response    string
	Note        string
	AddedAt     time.Time
	RespondedAt *time.Time
	// InviteState says whether a guest by email was emailed their invitation:
	// InviteSent, InviteNotSent (this server sends no such email) or
	// InviteFailed; "" for a member, and before anything was tried.
	InviteState string
	// LinkHash and LinkSealed are a guest by email's answer link: the hash it
	// is found by, and the link itself sealed with the server's key
	// (helpers.EncryptSecret), so the organiser can copy it or have it sent
	// again. Neither is ever sent anywhere.
	LinkHash   []byte
	LinkSealed string
	// NudgedAt is when they were last asked again for an answer.
	NudgedAt *time.Time
}

// The invite states of a guest by email.
const (
	InviteSent    = "sent"
	InviteNotSent = "not_sent"
	InviteFailed  = "failed"
)

// Key is who the attendee is: their member id, or their address in lower case.
func (a Attendee) Key() GuestKey {
	if a.UserID != nil {
		id := *a.UserID
		return GuestKey{UserID: &id}
	}
	return GuestKey{Email: strings.ToLower(strings.TrimSpace(a.Email))}
}

// CopyTo is the attendee on another event (a series' later part, an instance
// made an event of its own), with their answer and note. A guest by email's
// link is the old event's, and an invitation names one event, so the copy has
// neither: its own link is made, and its invitation sent, for the new event.
func (a Attendee) CopyTo(event uuid.UUID) Attendee {
	c := a
	c.EventID = event
	c.LinkHash, c.LinkSealed, c.InviteState, c.NudgedAt = nil, "", "", nil
	return c
}

// GuestKey names one guest of an event: a member by id, or someone by email
// (in lower case).
type GuestKey struct {
	UserID *uuid.UUID
	Email  string
}

// Is is whether the key names a.
func (k GuestKey) Is(a Attendee) bool {
	if k.UserID != nil {
		return a.UserID != nil && *a.UserID == *k.UserID
	}
	return a.UserID == nil && k.Email != "" && strings.EqualFold(strings.TrimSpace(a.Email), k.Email)
}

// attendeeColumns is every column of a guest, in scanAttendee's order.
const attendeeColumns = `event_id, user_id, email, name, role, response, note, added_at, responded_at,
	invite_state, rsvp_token_hash, rsvp_token_sealed, nudged_at`

func scanAttendee(s scanner) (Attendee, error) {
	var a Attendee
	var user uuid.NullUUID
	var responded, nudged sql.NullTime
	if err := s.Scan(&a.EventID, &user, &a.Email, &a.Name, &a.Role, &a.Response, &a.Note, &a.AddedAt, &responded,
		&a.InviteState, &a.LinkHash, &a.LinkSealed, &nudged); err != nil {
		return a, err
	}
	if user.Valid {
		id := user.UUID
		a.UserID = &id
	}
	a.AddedAt = a.AddedAt.UTC()
	a.RespondedAt, a.NudgedAt = utcOrNil(responded), utcOrNil(nudged)
	return a, nil
}

// AttendeesOf is the guests of each of these events, in the order they were
// added: one read for a whole page of events.
func AttendeesOf(ctx context.Context, db DB, events []uuid.UUID) (map[uuid.UUID][]Attendee, error) {
	out := map[uuid.UUID][]Attendee{}
	if len(events) == 0 {
		return out, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT `+attendeeColumns+`
		FROM event_attendees WHERE event_id = ANY($1::uuid[])
		ORDER BY added_at, user_id, lower(email)`, uuidArray(events))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.AttendeesOf err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		a, err := scanAttendee(rows)
		if err != nil {
			return nil, err
		}
		out[a.EventID] = append(out[a.EventID], a)
	}
	return out, rows.Err()
}

// AttendeeByLink is the guest whose answer link hashes to hash, or nil.
func AttendeeByLink(ctx context.Context, db DB, hash []byte) (*Attendee, error) {
	if len(hash) == 0 {
		return nil, nil
	}
	a, err := scanAttendee(db.QueryRowContext(ctx, `SELECT `+attendeeColumns+`
		FROM event_attendees WHERE rsvp_token_hash = $1`, hash))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.AttendeeByLink err: %+v", err)
		return nil, err
	}
	return &a, nil
}

// AddAttendees writes guests not on their event yet; one already there keeps
// their row, answer and note. Written attendeeBatch at a time, so no statement
// passes Postgres's limit on values.
func AddAttendees(ctx context.Context, db DB, guests []Attendee) error {
	for len(guests) > 0 {
		n := min(len(guests), attendeeBatch)
		if err := addAttendees(ctx, db, guests[:n]); err != nil {
			return err
		}
		guests = guests[n:]
	}
	return nil
}

// attendeeValues is how many values one guest's row takes.
const attendeeValues = 13

// attendeeBatch is how many guests one statement writes: attendeeValues each,
// under Postgres's 65,535.
const attendeeBatch = 4000

func addAttendees(ctx context.Context, db DB, guests []Attendee) error {
	var q strings.Builder
	q.WriteString(`INSERT INTO event_attendees (` + attendeeColumns + `) VALUES `)
	args := make([]any, 0, len(guests)*attendeeValues)
	for i, a := range guests {
		if i > 0 {
			q.WriteString(", ")
		}
		q.WriteString("(")
		for k := 1; k <= attendeeValues; k++ {
			if k > 1 {
				q.WriteString(", ")
			}
			q.WriteString("$" + strconv.Itoa(i*attendeeValues+k))
		}
		q.WriteString(")")
		var hash any
		if len(a.LinkHash) > 0 {
			hash = a.LinkHash
		}
		args = append(args, a.EventID, nullUUID(a.UserID), a.Email, a.Name, a.Role, a.Response, a.Note, a.AddedAt, a.RespondedAt,
			a.InviteState, hash, a.LinkSealed, a.NudgedAt)
	}
	q.WriteString(` ON CONFLICT DO NOTHING`)
	if _, err := db.ExecContext(ctx, q.String(), args...); err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.AddAttendees err: %+v", err)
		return err
	}
	return nil
}

// RemoveAttendees takes people off an event, and answers how many were on it.
func RemoveAttendees(ctx context.Context, db DB, event uuid.UUID, people []uuid.UUID) (int, error) {
	if len(people) == 0 {
		return 0, nil
	}
	res, err := db.ExecContext(ctx, `DELETE FROM event_attendees WHERE event_id = $1 AND user_id = ANY($2::uuid[])`, event, uuidArray(people))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.RemoveAttendees err: %+v", err)
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// lowerAll is addresses in lower case, as guests by email are matched. Pure.
func lowerAll(emails []string) []string {
	out := make([]string, len(emails))
	for i, e := range emails {
		out[i] = strings.ToLower(strings.TrimSpace(e))
	}
	return out
}

// RemoveEmailGuests takes guests invited by email off an event, by address in
// any case; their links stop working with their rows.
func RemoveEmailGuests(ctx context.Context, db DB, event uuid.UUID, emails []string) (int, error) {
	if len(emails) == 0 {
		return 0, nil
	}
	res, err := db.ExecContext(ctx, `DELETE FROM event_attendees WHERE event_id = $1 AND user_id IS NULL AND lower(email) = ANY($2::text[])`,
		event, pq.Array(lowerAll(emails)))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.RemoveEmailGuests err: %+v", err)
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// guestMatch is the condition naming one guest, with its value as $at.
func guestMatch(k GuestKey, at int) (string, any) {
	if k.UserID != nil {
		return "user_id = $" + strconv.Itoa(at), *k.UserID
	}
	return "user_id IS NULL AND lower(email) = $" + strconv.Itoa(at), strings.ToLower(k.Email)
}

// SetRoles puts guests already on their events in the role each names
// (required or optional), answers and links untouched. A guest no longer on
// their event is passed over.
func SetRoles(ctx context.Context, db DB, guests []Attendee) error {
	for _, g := range guests {
		cond, who := guestMatch(g.Key(), 3)
		if _, err := db.ExecContext(ctx, `UPDATE event_attendees SET role = $2 WHERE event_id = $1 AND `+cond, g.EventID, g.Role, who); err != nil {
			helpers.LogErrorWithContext(ctx, "models/CalendarV2.SetRoles err: %+v", err)
			return err
		}
	}
	return nil
}

// SetAnswer is a guest's answer to an event, with their note, and whether
// they were on it. An answer is the guest's, not a change to the event: the
// event's sequence stays, so nobody's open edit is made stale by it.
func SetAnswer(ctx context.Context, db DB, event uuid.UUID, k GuestKey, response, note string, at time.Time) (bool, error) {
	cond, who := guestMatch(k, 5)
	res, err := db.ExecContext(ctx, `UPDATE event_attendees SET response = $2, note = $3, responded_at = $4
		WHERE event_id = $1 AND `+cond, event, response, note, at, who)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.SetAnswer err: %+v", err)
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// SetInviteState records whether a guest by email was emailed their invitation.
func SetInviteState(ctx context.Context, db DB, event uuid.UUID, email, state string) error {
	_, err := db.ExecContext(ctx, `UPDATE event_attendees SET invite_state = $3
		WHERE event_id = $1 AND user_id IS NULL AND lower(email) = $2`, event, strings.ToLower(strings.TrimSpace(email)), state)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.SetInviteState err: %+v", err)
	}
	return err
}

// SetLink gives a guest by email a new answer link: its hash and its sealed
// form. The old one stops working.
func SetLink(ctx context.Context, db DB, event uuid.UUID, email string, hash []byte, sealed string) error {
	_, err := db.ExecContext(ctx, `UPDATE event_attendees SET rsvp_token_hash = $3, rsvp_token_sealed = $4
		WHERE event_id = $1 AND user_id IS NULL AND lower(email) = $2`, event, strings.ToLower(strings.TrimSpace(email)), hash, sealed)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.SetLink err: %+v", err)
	}
	return err
}

// SetNudged stamps when these guests were last asked again for an answer.
func SetNudged(ctx context.Context, db DB, event uuid.UUID, people []uuid.UUID, emails []string, at time.Time) error {
	if len(people) == 0 && len(emails) == 0 {
		return nil
	}
	_, err := db.ExecContext(ctx, `UPDATE event_attendees SET nudged_at = $4
		WHERE event_id = $1 AND (user_id = ANY($2::uuid[]) OR (user_id IS NULL AND lower(email) = ANY($3::text[])))`,
		event, uuidArray(people), pq.Array(lowerAll(emails)), at)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.SetNudged err: %+v", err)
	}
	return err
}

// String names the guest in a key: their id, or "mailto:" and their address.
func (k GuestKey) String() string {
	if k.UserID != nil {
		return k.UserID.String()
	}
	return "mailto:" + k.Email
}
