package models

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/pgexec"
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

// ImportedCalendar is owner's calendar of this name brought from another
// calendar app (kind 'external', calendar A4), made with zone tz the first
// time: a Google export's other calendars ("Team offsites", "Holidays")
// each come in as one of these.
func ImportedCalendar(ctx context.Context, owner uuid.UUID, name string, newID uuid.UUID, tz string) (uuid.UUID, error) {
	return findOrMakeCalendar(ctx, "ImportedCalendar",
		pgexec.Stmt{SQL: `SELECT id FROM calendars WHERE owner_id = $1 AND kind = 'external' AND name = $2 AND deleted_at IS NULL`, Args: []any{owner, name}},
		pgexec.Stmt{SQL: `INSERT INTO calendars (id, kind, owner_id, name, tz) VALUES ($1, 'external', $2, $3, $4)
		ON CONFLICT (owner_id, name) WHERE kind = 'external' AND deleted_at IS NULL DO NOTHING
		RETURNING id`, Args: []any{newID, owner, name, tz}})
}

// findOrMakeCalendar is the calendar find reads, else the one insert makes.
// The insert does nothing on a conflict, so when another first time won, it
// returns no row and find reads theirs: two first times at once make one.
func findOrMakeCalendar(ctx context.Context, what string, find, insert pgexec.Stmt) (uuid.UUID, error) {
	scanID := func(r pgexec.Row) (uuid.UUID, error) {
		var id uuid.UUID
		err := r.Scan(&id)
		return id, err
	}
	id, found, err := pgexec.QueryOne(ctx, find.SQL, scanID, find.Args...)
	if err == nil && !found {
		if id, found, err = pgexec.QueryOne(ctx, insert.SQL, scanID, insert.Args...); err == nil && !found {
			id, found, err = pgexec.QueryOne(ctx, find.SQL, scanID, find.Args...)
		}
	}
	if err == nil && !found {
		err = sql.ErrNoRows
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.%s err: %+v", what, err)
		return uuid.Nil, err
	}
	return id, nil
}

// ZoneHints are the zones a person has given OneCamp, best first: the zone
// they keep (Person Preferences' `zone`, calendar A3), their quiet hours'
// zone, their newest booking page's, and their newest task repeat's.
// Any may be blank, and none is checked here: the caller takes the first its
// clock can read.
type ZoneHints struct {
	Stored, QuietHours, BookingPage, TaskRepeat string
}

// ZoneHintsOf reads the zone hints of these people, in one query.
func ZoneHintsOf(ctx context.Context, db DB, people []uuid.UUID) (map[uuid.UUID]ZoneHints, error) {
	out := map[uuid.UUID]ZoneHints{}
	if len(people) == 0 {
		return out, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT u.id, COALESCE(z.tz, ''), COALESCE(p.tz, ''), COALESCE(b.tz, ''), COALESCE(r.tz, '')
		FROM unnest($1::uuid[]) AS u(id)
		LEFT JOIN LATERAL (SELECT value #>> '{}' AS tz FROM person_preferences WHERE user_id = u.id AND key = 'zone' AND jsonb_typeof(value) = 'string') z ON true
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
		if err := rows.Scan(&id, &h.Stored, &h.QuietHours, &h.BookingPage, &h.TaskRepeat); err != nil {
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

// MembersIn is which of these ids are members now: their account isn't
// deactivated, an outside guest's or a bot's (User.IsMember's rule, on an
// active account). Only they are fed a calendar, shared one or have theirs
// laid over another's, and only they see when others are busy.
func MembersIn(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	if len(ids) == 0 {
		return map[uuid.UUID]bool{}, nil
	}
	out, err := pgexec.Pairs[uuid.UUID, bool](ctx, `SELECT id, true FROM users WHERE id = ANY($1::uuid[])
		AND deleted_at IS NULL AND is_external IS NOT TRUE AND is_bot IS NOT TRUE`, uuidArray(ids))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.MembersIn err: %+v", err)
		return nil, err
	}
	return out, nil
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
	return queryIDs(ctx, db, "MovedUnchangedAfter", `SELECT id FROM events
		WHERE source = 'moved' AND sequence = 0 AND deleted_at IS NULL AND id > $1
		ORDER BY id LIMIT $2`, after, limit)
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

// ContainerCalendar is the calendar that belongs to a container (a project's,
// kind 'project'), made with newID and zone tz the first time: one per
// container, and two first times at once make one.
func ContainerCalendar(ctx context.Context, containerKind, containerID string, newID uuid.UUID, tz string) (uuid.UUID, error) {
	return findOrMakeCalendar(ctx, "ContainerCalendar",
		pgexec.Stmt{SQL: `SELECT id FROM calendars WHERE container_kind = $1 AND container_id = $2 AND deleted_at IS NULL`, Args: []any{containerKind, containerID}},
		pgexec.Stmt{SQL: `INSERT INTO calendars (id, kind, container_kind, container_id, tz) VALUES ($1, $2, $2, $3, $4)
		ON CONFLICT (container_kind, container_id) WHERE container_kind <> '' AND deleted_at IS NULL DO NOTHING
		RETURNING id`, Args: []any{newID, containerKind, containerID, tz}})
}

// Calendar is a calendar as the calendar's rules read it.
type Calendar struct {
	ID            uuid.UUID
	Kind          string
	OwnerID       *uuid.UUID
	Name          string
	Hue           string
	TZ            string
	ContainerKind string
	ContainerID   string
	Deleted       bool
}

// CalendarsByID are these calendars, in the trash or not.
func CalendarsByID(ctx context.Context, ids []uuid.UUID) ([]Calendar, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	return queryCalendars(ctx, `SELECT id, kind, owner_id, name, hue, tz, container_kind, container_id, deleted_at IS NOT NULL
		FROM calendars WHERE id = ANY($1::uuid[])`, uuidArray(ids))
}

// NextOf is each calendar's next live events from now on, in one read:
// single events and series that reach past now (their instances worked out
// by the caller), at most limit rows a calendar, each calendar's by start.
func NextOf(ctx context.Context, db DB, calendars []uuid.UUID, now time.Time, limit int) ([]*Event, error) {
	if len(calendars) == 0 {
		return nil, nil
	}
	return queryEvents(ctx, db, `SELECT `+eventColumns+` FROM unnest($1::uuid[]) AS c(id)
		CROSS JOIN LATERAL (SELECT * FROM events e
			WHERE e.calendar_id = c.id AND e.deleted_at IS NULL AND e.series_id IS NULL AND e.status <> 'cancelled'
			  AND ((e.rrule = '' AND (e.end_at >= $2 OR e.end_date > $2::date)) OR (e.rrule <> '' AND (e.until_at IS NULL OR e.until_at >= $2)))
			ORDER BY COALESCE(e.start_at, (e.start_date::timestamp AT TIME ZONE 'UTC')), e.id
			LIMIT $3) e
		ORDER BY e.calendar_id, COALESCE(e.start_at, (e.start_date::timestamp AT TIME ZONE 'UTC')), e.id`, uuidArray(calendars), now, limit)
}

// OwnCalendars are owner's own calendars (theirs, and those imports made
// for them), not in the trash.
func OwnCalendars(ctx context.Context, owner uuid.UUID) ([]Calendar, error) {
	return queryCalendars(ctx, `SELECT id, kind, owner_id, name, hue, tz, container_kind, container_id, deleted_at IS NOT NULL
		FROM calendars WHERE owner_id = $1 AND container_kind = '' AND deleted_at IS NULL ORDER BY kind <> 'personal', name, id`, owner)
}

// ContainerCalendars are the calendars of a kind of container, not in the
// trash, at most limit, oldest first.
func ContainerCalendars(ctx context.Context, kind string, limit int) ([]Calendar, error) {
	return queryCalendars(ctx, `SELECT id, kind, owner_id, name, hue, tz, container_kind, container_id, deleted_at IS NOT NULL
		FROM calendars WHERE container_kind = $1 AND deleted_at IS NULL ORDER BY created_at, id LIMIT $2`, kind, limit)
}

func queryCalendars(ctx context.Context, q string, args ...any) ([]Calendar, error) {
	out, err := pgexec.Query(ctx, q, scanCalendar, args...)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2 calendars err: %+v", err)
	}
	return out, err
}

func scanCalendar(r pgexec.Row) (Calendar, error) {
	var c Calendar
	var owner uuid.NullUUID
	if err := r.Scan(&c.ID, &c.Kind, &owner, &c.Name, &c.Hue, &c.TZ, &c.ContainerKind, &c.ContainerID, &c.Deleted); err != nil {
		return c, err
	}
	if owner.Valid {
		o := owner.UUID
		c.OwnerID = &o
	}
	return c, nil
}

// The levels a calendar is shared at.
const (
	ShareDetails = "details"
	ShareEdit    = "edit"
	ShareManage  = "manage"
)

// CalendarShare is a calendar shared with someone at a level.
type CalendarShare struct {
	CalendarID uuid.UUID
	PersonID   uuid.UUID
	Level      string
}

// SharesFor are the shares these calendars give person, by calendar.
func SharesFor(ctx context.Context, person uuid.UUID, calendars []uuid.UUID) (map[uuid.UUID]string, error) {
	if len(calendars) == 0 {
		return map[uuid.UUID]string{}, nil
	}
	out, err := pgexec.Pairs[uuid.UUID, string](ctx, `SELECT calendar_id, level FROM calendar_shares WHERE person_id = $1 AND calendar_id = ANY($2::uuid[])`, person, uuidArray(calendars))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.SharesFor err: %+v", err)
		return nil, err
	}
	return out, nil
}

// SharesOf are who a calendar is shared with.
func SharesOf(ctx context.Context, calendar uuid.UUID) ([]CalendarShare, error) {
	out, err := pgexec.Query(ctx, `SELECT calendar_id, person_id, level FROM calendar_shares WHERE calendar_id = $1 ORDER BY created_at, person_id`,
		func(r pgexec.Row) (CalendarShare, error) {
			var s CalendarShare
			err := r.Scan(&s.CalendarID, &s.PersonID, &s.Level)
			return s, err
		}, calendar)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.SharesOf err: %+v", err)
	}
	return out, err
}

// SetShare shares a calendar with someone at a level, or ("" level) stops.
func SetShare(ctx context.Context, calendar, person, by uuid.UUID, level string) error {
	var err error
	if level == "" {
		_, err = pgexec.Exec(ctx, `DELETE FROM calendar_shares WHERE calendar_id = $1 AND person_id = $2`, calendar, person)
	} else {
		_, err = pgexec.Exec(ctx, `INSERT INTO calendar_shares (calendar_id, person_id, level, granted_by) VALUES ($1, $2, $3, $4)
			ON CONFLICT (calendar_id, person_id) DO UPDATE SET level = EXCLUDED.level, granted_by = EXCLUDED.granted_by, updated_at = NOW()`, calendar, person, level, by)
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.SetShare err: %+v", err)
	}
	return err
}

// PersonalCalendarsOf are these people's own calendars, by owner.
func PersonalCalendarsOf(ctx context.Context, owners []uuid.UUID) ([]Calendar, error) {
	if len(owners) == 0 {
		return nil, nil
	}
	return queryCalendars(ctx, `SELECT id, kind, owner_id, name, hue, tz, container_kind, container_id, deleted_at IS NOT NULL
		FROM calendars WHERE kind = 'personal' AND deleted_at IS NULL AND owner_id = ANY($1::uuid[])`, uuidArray(owners))
}

// SharedWith are the calendars shared with person, not in the trash.
func SharedWith(ctx context.Context, person uuid.UUID) ([]Calendar, error) {
	return queryCalendars(ctx, `SELECT c.id, c.kind, c.owner_id, c.name, c.hue, c.tz, c.container_kind, c.container_id, c.deleted_at IS NOT NULL
		FROM calendars c JOIN calendar_shares s ON s.calendar_id = c.id WHERE s.person_id = $1 AND c.deleted_at IS NULL ORDER BY s.created_at, c.id`, person)
}

// UpdateCalendar changes a calendar's name and colour (nil leaves one).
func UpdateCalendar(ctx context.Context, id uuid.UUID, name, hue *string) error {
	_, err := pgexec.Exec(ctx, `UPDATE calendars SET name = COALESCE($2, name), hue = COALESCE($3, hue), updated_at = NOW() WHERE id = $1`, id, name, hue)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.UpdateCalendar err: %+v", err)
	}
	return err
}

// SubscribedCalendar makes owner a calendar read from an address (kind
// 'subscribed', calendar B8), named and coloured, in zone tz.
func SubscribedCalendar(ctx context.Context, id, owner uuid.UUID, name, hue, tz string) error {
	_, err := pgexec.Exec(ctx, `INSERT INTO calendars (id, kind, owner_id, name, hue, tz) VALUES ($1, 'subscribed', $2, $3, $4, $5)`, id, owner, name, hue, tz)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.SubscribedCalendar err: %+v", err)
	}
	return err
}

// DeleteSubscribedCalendar deletes owner's calendar read from an address,
// and with it everything on it: its events are the address's, never anyone's
// own. It says whether there was one.
func DeleteSubscribedCalendar(ctx context.Context, id, owner uuid.UUID) (bool, error) {
	n, err := pgexec.Exec(ctx, `DELETE FROM calendars WHERE id = $1 AND owner_id = $2 AND kind = 'subscribed'`, id, owner)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.DeleteSubscribedCalendar err: %+v", err)
		return false, err
	}
	return n == 1, nil
}

// DeleteSubscribedEvents deletes events that left the file a subscribed
// calendar reads, with their changed instances: only events an address
// brought (source 'subscribed'), and only on that calendar.
func DeleteSubscribedEvents(ctx context.Context, calendar uuid.UUID, ids []uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := pgexec.Exec(ctx, `DELETE FROM events WHERE calendar_id = $1 AND source = 'subscribed' AND id = ANY($2::uuid[])`, calendar, uuidArray(ids))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.DeleteSubscribedEvents err: %+v", err)
	}
	return err
}
