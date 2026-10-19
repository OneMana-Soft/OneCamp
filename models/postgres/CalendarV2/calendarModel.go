package models

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// PersonalCalendar is the id of owner's own calendar, made with newID and
// zone tz the first time it is needed. Two first times at once make one: the
// second finds the first's.
func PersonalCalendar(ctx context.Context, db DB, owner uuid.UUID, newID uuid.UUID, tz string) (uuid.UUID, error) {
	var id uuid.UUID
	err := db.QueryRowContext(ctx, `SELECT id FROM calendars WHERE owner_id = $1 AND kind = 'personal' AND deleted_at IS NULL`, owner).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.PersonalCalendar read err: %+v", err)
		return uuid.Nil, err
	}
	err = db.QueryRowContext(ctx, `INSERT INTO calendars (id, kind, owner_id, tz) VALUES ($1, 'personal', $2, $3)
		ON CONFLICT (owner_id) WHERE kind = 'personal' AND deleted_at IS NULL DO NOTHING
		RETURNING id`, newID, owner, tz).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		// Someone else's first time won: theirs is the calendar.
		err = db.QueryRowContext(ctx, `SELECT id FROM calendars WHERE owner_id = $1 AND kind = 'personal' AND deleted_at IS NULL`, owner).Scan(&id)
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.PersonalCalendar err: %+v", err)
		return uuid.Nil, err
	}
	return id, nil
}

// ZoneHints are the zones a person has given OneCamp, best first: their quiet
// hours' zone, their newest booking page's, and their newest task repeat's.
// Any may be blank, and none is checked here: the caller takes the first its
// clock can read.
type ZoneHints struct {
	QuietHours, BookingPage, TaskRepeat string
}

// ZoneHintsOf reads the zone hints of these people, in one query.
func ZoneHintsOf(ctx context.Context, db DB, people []uuid.UUID) (map[uuid.UUID]ZoneHints, error) {
	out := map[uuid.UUID]ZoneHints{}
	if len(people) == 0 {
		return out, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT u.id, COALESCE(p.tz, ''), COALESCE(b.tz, ''), COALESCE(r.tz, '')
		FROM unnest($1::uuid[]) AS u(id)
		LEFT JOIN LATERAL (SELECT quiet_hours_tz AS tz FROM users_notification_preferences WHERE user_id = u.id LIMIT 1) p ON true
		LEFT JOIN LATERAL (SELECT hours->>'tz' AS tz FROM booking_pages WHERE user_id = u.id ORDER BY updated_at DESC LIMIT 1) b ON true
		LEFT JOIN LATERAL (SELECT time_zone AS tz FROM task_recurrences WHERE created_by = u.id AND time_zone <> '' ORDER BY updated_at DESC LIMIT 1) r ON true`,
		uuidArray(people))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.ZoneHintsOf err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var h ZoneHints
		if err := rows.Scan(&id, &h.QuietHours, &h.BookingPage, &h.TaskRepeat); err != nil {
			return nil, err
		}
		out[id] = h
	}
	return out, rows.Err()
}

// PeopleIn is which of these ids are people in users: an event's organiser
// and guests must be, or the row can't be written.
func PeopleIn(ctx context.Context, db DB, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	out := map[uuid.UUID]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT id FROM users WHERE id = ANY($1::uuid[])`, uuidArray(ids))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.PeopleIn err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// PeopleByEmail is the accounts these addresses belong to, by address in
// lower case: someone invited by an address that is a member's is invited as
// that member. Accounts made for people outside the workspace (a GitHub
// stub) are no one's here.
func PeopleByEmail(ctx context.Context, db DB, emails []string) (map[string]uuid.UUID, error) {
	out := map[string]uuid.UUID{}
	if len(emails) == 0 {
		return out, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT ON (lower(email_id)) lower(email_id), id FROM users
		WHERE lower(email_id) = ANY($1::text[]) AND deleted_at IS NULL AND is_external IS NOT TRUE
		ORDER BY lower(email_id), created_at`, pq.Array(lowerAll(emails)))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.PeopleByEmail err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var email string
		var id uuid.UUID
		if err := rows.Scan(&email, &id); err != nil {
			return nil, err
		}
		out[email] = id
	}
	return out, rows.Err()
}

// OldEvent is one calendar_events row, the store the move reads from.
type OldEvent struct {
	ID            uuid.UUID
	Title         string
	Description   string
	StartTime     time.Time
	EndTime       time.Time
	CreatedBy     *uuid.UUID
	GoogleEventID string
	IsFocus       bool
	CreatedAt     time.Time
	UpdatedAt     time.Time
	DeletedAt     *time.Time
}

const oldEventColumns = `id, title, COALESCE(description, ''), start_time, end_time, created_by, COALESCE(google_calendar_event_id, ''),
	is_focus, COALESCE(created_at, NOW()), COALESCE(updated_at, NOW()), deleted_at`

func scanOldEvent(s scanner) (*OldEvent, error) {
	var e OldEvent
	var by uuid.NullUUID
	var deleted sql.NullTime
	if err := s.Scan(&e.ID, &e.Title, &e.Description, &e.StartTime, &e.EndTime, &by, &e.GoogleEventID,
		&e.IsFocus, &e.CreatedAt, &e.UpdatedAt, &deleted); err != nil {
		return nil, err
	}
	if by.Valid {
		id := by.UUID
		e.CreatedBy = &id
	}
	e.StartTime, e.EndTime, e.CreatedAt, e.UpdatedAt = e.StartTime.UTC(), e.EndTime.UTC(), e.CreatedAt.UTC(), e.UpdatedAt.UTC()
	e.DeletedAt = utcOrNil(deleted)
	return &e, nil
}

func queryOldEvents(ctx context.Context, db DB, q string, args ...any) ([]*OldEvent, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2 old events err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var out []*OldEvent
	for rows.Next() {
		e, err := scanOldEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// OldEventsByID is the calendar_events rows with these ids.
func OldEventsByID(ctx context.Context, db DB, ids []uuid.UUID) ([]*OldEvent, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	return queryOldEvents(ctx, db, `SELECT `+oldEventColumns+` FROM calendar_events WHERE id = ANY($1::uuid[])`, uuidArray(ids))
}

// OldEventsAfter is the next limit calendar_events rows by id after after.
func OldEventsAfter(ctx context.Context, db DB, after uuid.UUID, limit int) ([]*OldEvent, error) {
	return queryOldEvents(ctx, db, `SELECT `+oldEventColumns+` FROM calendar_events WHERE id > $1 ORDER BY id LIMIT $2`, after, limit)
}

// MovedUnchangedAfter is the next limit ids, after after, of events the move
// made and nobody has changed since, not in the trash: the ones a later move
// may still bring up to date.
func MovedUnchangedAfter(ctx context.Context, db DB, after uuid.UUID, limit int) ([]uuid.UUID, error) {
	rows, err := db.QueryContext(ctx, `SELECT id FROM events
		WHERE source = 'moved' AND sequence = 0 AND deleted_at IS NULL AND id > $1
		ORDER BY id LIMIT $2`, after, limit)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.MovedUnchangedAfter err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// RefreshMoved writes e over the row the move made for it, when nobody has
// changed that row since (source 'moved', sequence 0), and says whether it
// did. Its sequence stays 0: it is still the move's.
func RefreshMoved(ctx context.Context, db DB, e *Event) (bool, error) {
	res, err := db.ExecContext(ctx, `UPDATE events SET
			kind = $2, title = $3, notes = $4, notes_format = $5,
			start_at = $6, end_at = $7, start_date = $8::date, end_date = $9::date, tz = $10,
			google_event_id = $11, updated_at = $12, deleted_at = $13, deleted_by = NULL
		WHERE id = $1 AND source = 'moved' AND sequence = 0`,
		e.ID, e.Kind, e.Title, e.Notes, e.NotesFormat,
		e.StartAt, e.EndAt, nullDate(e.StartDate), nullDate(e.EndDate), e.TZ,
		nullText(e.GoogleEventID), e.UpdatedAt, e.DeletedAt)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.RefreshMoved err: %+v", err)
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ReplaceMovedAttendees sets the guest list of an event the move made and
// nobody has changed since to guests.
func ReplaceMovedAttendees(ctx context.Context, db DB, event uuid.UUID, guests []Attendee) error {
	var mine bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM events WHERE id = $1 AND source = 'moved' AND sequence = 0)`, event).Scan(&mine); err != nil || !mine {
		return err
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM event_attendees WHERE event_id = $1`, event); err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.ReplaceMovedAttendees err: %+v", err)
		return err
	}
	return AddAttendees(ctx, db, guests)
}

// TrashMoved puts in the trash, at at, the events of these ids that the move
// made and nobody has changed since: events gone from the old stores after an
// earlier move brought them across.
func TrashMoved(ctx context.Context, db DB, ids []uuid.UUID, at time.Time) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	res, err := db.ExecContext(ctx, `UPDATE events SET deleted_at = $2, updated_at = $2
		WHERE id = ANY($1::uuid[]) AND source = 'moved' AND sequence = 0 AND deleted_at IS NULL`, uuidArray(ids), at)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.TrashMoved err: %+v", err)
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}
