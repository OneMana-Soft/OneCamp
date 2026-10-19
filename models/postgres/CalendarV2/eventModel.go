package models

// Postgres access for the calendar's one store (migrations/345_calendar_store):
// calendars, events and event_attendees. business/Calendar owns the rules;
// this package only reads and writes rows, and makes no ids.
//
// Dates (an all-day event's start_date and end_date) travel as "YYYY-MM-DD"
// text both ways, so a date never becomes an instant in Go, where a zone could
// move it a day. Instants are UTC. The timestamptz arrays (exdates, rdates)
// are written as Postgres array text and read back as JSON.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// DB is what a read or a write runs on: the pool, or a transaction.
type DB interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Pool is the server's pool.
func Pool() DB { return postgresInit.DBConn.SqlDB }

// Errors a write answers with.
var (
	// ErrNotFound is no event with that id (a write never touches the trash
	// unless it is moving an event in or out of it).
	ErrNotFound = errors.New("calendar: no such event")
	// ErrStale is a write that named a sequence the event has moved past.
	ErrStale = errors.New("calendar: the event has changed since")
)

// Event is one events row: a single event, a series or an override.
type Event struct {
	ID          uuid.UUID
	CalendarID  uuid.UUID
	Kind        string
	Title       string
	Notes       string
	NotesFormat string
	Location    string
	CallRoom    string
	// A timed event has StartAt and EndAt; an all-day one StartDate and
	// EndDate ("YYYY-MM-DD", the end the day after its last).
	StartAt   *time.Time
	EndAt     *time.Time
	StartDate string
	EndDate   string
	TZ        string
	EndTZ     string
	// Repeating: a series has RRule; an override has SeriesID and
	// RecurrenceID.
	RRule        string
	UntilAt      *time.Time
	SeriesID     *uuid.UUID
	RecurrenceID *time.Time
	Exdates      []time.Time
	Rdates       []time.Time
	Changed      []string

	Transparency       string
	Visibility         string
	Hue                string
	Status             string
	GuestsCanModify    bool
	GuestsCanInvite    bool
	GuestsCanSeeGuests bool
	// EmailOnAnswers is the organiser's "Email me when guests answer".
	EmailOnAnswers bool

	CreatedBy     uuid.UUID
	Sequence      int
	ICalUID       string
	Source        string
	GoogleEventID string

	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time
	DeletedBy *uuid.UUID
}

// AllDay is whether the event is dates rather than instants.
func (e *Event) AllDay() bool { return e.StartDate != "" }

// eventColumns is every column a read returns, in scanEvent's order.
const eventColumns = `e.id, e.calendar_id, e.kind, e.title, e.notes, e.notes_format, e.location, e.call_room,
	e.start_at, e.end_at, to_char(e.start_date, 'YYYY-MM-DD'), to_char(e.end_date, 'YYYY-MM-DD'), e.tz, e.end_tz,
	e.rrule, e.until_at, e.series_id, e.recurrence_id,
	COALESCE(array_to_json(e.exdates), '[]'), COALESCE(array_to_json(e.rdates), '[]'), e.changed,
	e.transparency, e.visibility, e.hue, e.status, e.guests_can_modify, e.guests_can_invite, e.guests_can_see_guests,
	e.created_by, e.sequence, e.ical_uid, e.source, COALESCE(e.google_event_id, ''),
	e.created_at, e.updated_at, e.deleted_at, e.deleted_by, e.email_on_answers`

type scanner interface{ Scan(dest ...any) error }

func scanEvent(s scanner) (*Event, error) {
	var (
		e                          Event
		startAt, endAt, until, rid sql.NullTime
		startDate, endDate         sql.NullString
		series, createdBy          uuid.NullUUID
		exdates, rdates            []byte
		deletedAt                  sql.NullTime
		deletedBy                  uuid.NullUUID
	)
	err := s.Scan(&e.ID, &e.CalendarID, &e.Kind, &e.Title, &e.Notes, &e.NotesFormat, &e.Location, &e.CallRoom,
		&startAt, &endAt, &startDate, &endDate, &e.TZ, &e.EndTZ,
		&e.RRule, &until, &series, &rid,
		&exdates, &rdates, pq.Array(&e.Changed),
		&e.Transparency, &e.Visibility, &e.Hue, &e.Status, &e.GuestsCanModify, &e.GuestsCanInvite, &e.GuestsCanSeeGuests,
		&createdBy, &e.Sequence, &e.ICalUID, &e.Source, &e.GoogleEventID,
		&e.CreatedAt, &e.UpdatedAt, &deletedAt, &deletedBy, &e.EmailOnAnswers)
	if err != nil {
		return nil, err
	}
	e.StartAt, e.EndAt, e.UntilAt, e.RecurrenceID = utcOrNil(startAt), utcOrNil(endAt), utcOrNil(until), utcOrNil(rid)
	e.StartDate, e.EndDate = startDate.String, endDate.String
	if series.Valid {
		id := series.UUID
		e.SeriesID = &id
	}
	if createdBy.Valid {
		e.CreatedBy = createdBy.UUID
	}
	if e.Exdates, err = timesOf(exdates); err != nil {
		return nil, err
	}
	if e.Rdates, err = timesOf(rdates); err != nil {
		return nil, err
	}
	e.CreatedAt, e.UpdatedAt = e.CreatedAt.UTC(), e.UpdatedAt.UTC()
	e.DeletedAt = utcOrNil(deletedAt)
	if deletedBy.Valid {
		id := deletedBy.UUID
		e.DeletedBy = &id
	}
	if e.Changed == nil {
		e.Changed = []string{}
	}
	return &e, nil
}

func utcOrNil(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	u := t.Time.UTC()
	return &u
}

// timesOf reads a timestamptz[] sent as JSON.
func timesOf(b []byte) ([]time.Time, error) {
	var out []time.Time
	if len(b) == 0 {
		return []time.Time{}, nil
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	for i := range out {
		out[i] = out[i].UTC()
	}
	if out == nil {
		out = []time.Time{}
	}
	return out, nil
}

// timesArray writes instants as a Postgres timestamptz[] literal.
func timesArray(ts []time.Time) string {
	parts := make([]string, len(ts))
	for i, t := range ts {
		parts[i] = `"` + t.UTC().Format(time.RFC3339Nano) + `"`
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// uuidArray writes ids as a Postgres uuid[] literal: the cheap way through the
// driver for a long list (a list handed to pq.Array is formatted twice).
func uuidArray(ids []uuid.UUID) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = id.String()
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func nullDate(d string) sql.NullString { return sql.NullString{String: d, Valid: d != ""} }

func nullUUID(id *uuid.UUID) uuid.NullUUID {
	if id == nil {
		return uuid.NullUUID{}
	}
	return uuid.NullUUID{UUID: *id, Valid: true}
}

func nullText(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

// insertColumns are the columns an insert writes, in insertArgs' order.
const insertColumns = `id, calendar_id, kind, title, notes, notes_format, location, call_room,
	start_at, end_at, start_date, end_date, tz, end_tz,
	rrule, until_at, series_id, recurrence_id, exdates, rdates, changed,
	transparency, visibility, hue, status, guests_can_modify, guests_can_invite, guests_can_see_guests,
	created_by, sequence, ical_uid, source, google_event_id, created_at, updated_at, deleted_at, deleted_by,
	email_on_answers`

// insertColumnCount is how many values one row of insertColumns takes.
const insertColumnCount = 38

// insertPlaceholders is one row's placeholders from $first, with the casts
// the text-sent values need.
func insertPlaceholders(first int) string {
	var b strings.Builder
	b.WriteByte('(')
	for i := 0; i < insertColumnCount; i++ {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString("$")
		b.WriteString(strconv.Itoa(first + i))
		switch i {
		case 10, 11:
			b.WriteString("::date")
		case 18, 19:
			b.WriteString("::timestamptz[]")
		}
	}
	b.WriteByte(')')
	return b.String()
}

func insertArgs(e *Event) []any {
	return []any{e.ID, e.CalendarID, e.Kind, e.Title, e.Notes, e.NotesFormat, e.Location, e.CallRoom,
		e.StartAt, e.EndAt, nullDate(e.StartDate), nullDate(e.EndDate), e.TZ, e.EndTZ,
		e.RRule, e.UntilAt, nullUUID(e.SeriesID), e.RecurrenceID, timesArray(e.Exdates), timesArray(e.Rdates), pq.Array(nonNil(e.Changed)),
		e.Transparency, e.Visibility, e.Hue, e.Status, e.GuestsCanModify, e.GuestsCanInvite, e.GuestsCanSeeGuests,
		e.CreatedBy, e.Sequence, e.ICalUID, e.Source, nullText(e.GoogleEventID), e.CreatedAt, e.UpdatedAt, e.DeletedAt, nullUUID(e.DeletedBy),
		e.EmailOnAnswers}
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// Insert writes a new event.
func Insert(ctx context.Context, db DB, e *Event) error {
	_, err := db.ExecContext(ctx, `INSERT INTO events (`+insertColumns+`) VALUES `+insertPlaceholders(1), insertArgs(e)...)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.Insert err: %+v", err)
	}
	return err
}

// InsertIfAbsent writes the events not stored yet, in one statement, and
// answers the ids it wrote: an id already there keeps its row.
func InsertIfAbsent(ctx context.Context, db DB, events []*Event) (map[uuid.UUID]bool, error) {
	wrote := map[uuid.UUID]bool{}
	if len(events) == 0 {
		return wrote, nil
	}
	var values []string
	args := make([]any, 0, len(events)*insertColumnCount)
	for i, e := range events {
		values = append(values, insertPlaceholders(i*insertColumnCount+1))
		args = append(args, insertArgs(e)...)
	}
	rows, err := db.QueryContext(ctx, `INSERT INTO events (`+insertColumns+`) VALUES `+strings.Join(values, ", ")+`
		ON CONFLICT (id) DO NOTHING RETURNING id`, args...)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.InsertIfAbsent err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		wrote[id] = true
	}
	return wrote, rows.Err()
}

// Update writes every changeable field of e, its series, the instance it
// replaces among them and its iCalendar uid (an override moved to a series'
// new part, or to an instance's new start, or made an event of its own), and
// adds one to its sequence, when it is still at
// expect (any sequence when expect is nil). It answers the new sequence,
// ErrStale when the event has moved on, and ErrNotFound when there is no such
// event. A write never touches the trash state.
func Update(ctx context.Context, db DB, e *Event, expect *int) (int, error) {
	var seq int
	err := db.QueryRowContext(ctx, `UPDATE events SET
			kind = $2, title = $3, notes = $4, notes_format = $5, location = $6, call_room = $7,
			start_at = $8, end_at = $9, start_date = $10::date, end_date = $11::date, tz = $12, end_tz = $13,
			rrule = $14, until_at = $15, exdates = $16::timestamptz[], rdates = $17::timestamptz[], changed = $18,
			transparency = $19, visibility = $20, hue = $21, status = $22,
			guests_can_modify = $23, guests_can_invite = $24, guests_can_see_guests = $25,
			google_event_id = $26, updated_at = $27, series_id = $29, recurrence_id = $30, ical_uid = $31,
			email_on_answers = $32, sequence = sequence + 1
		WHERE id = $1 AND ($28::int IS NULL OR sequence = $28)
		RETURNING sequence`,
		e.ID, e.Kind, e.Title, e.Notes, e.NotesFormat, e.Location, e.CallRoom,
		e.StartAt, e.EndAt, nullDate(e.StartDate), nullDate(e.EndDate), e.TZ, e.EndTZ,
		e.RRule, e.UntilAt, timesArray(e.Exdates), timesArray(e.Rdates), pq.Array(nonNil(e.Changed)),
		e.Transparency, e.Visibility, e.Hue, e.Status,
		e.GuestsCanModify, e.GuestsCanInvite, e.GuestsCanSeeGuests,
		nullText(e.GoogleEventID), e.UpdatedAt, expect, nullUUID(e.SeriesID), e.RecurrenceID, e.ICalUID, e.EmailOnAnswers).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, missingOrStale(ctx, db, e.ID)
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.Update err: %+v", err)
		return 0, err
	}
	return seq, nil
}

// LockSeries holds a series' row for the rest of the transaction, and answers
// its sequence and how many overrides it has, trashed ones too; ErrNotFound
// when there is no such row. A change to a series is written only while both
// are as it read them, so a change to one instance meanwhile is never lost.
func LockSeries(ctx context.Context, db DB, id uuid.UUID) (int, int, error) {
	var seq, overrides int
	err := db.QueryRowContext(ctx, `SELECT sequence FROM events WHERE id = $1 FOR UPDATE`, id).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, ErrNotFound
	}
	if err != nil {
		return 0, 0, err
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE series_id = $1`, id).Scan(&overrides); err != nil {
		return 0, 0, err
	}
	return seq, overrides, nil
}

// RemoveFoldedOverrides removes overrides whose changes a write has moved onto
// their series' own row (a series that stops repeating is its first
// instance): only overrides, never a single event or a series.
func RemoveFoldedOverrides(ctx context.Context, db DB, ids []uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := db.ExecContext(ctx, `DELETE FROM events WHERE id = ANY($1::uuid[]) AND series_id IS NOT NULL`, uuidArray(ids))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.RemoveFoldedOverrides err: %+v", err)
	}
	return err
}

// missingOrStale says why a write that named an event touched no row.
func missingOrStale(ctx context.Context, db DB, id uuid.UUID) error {
	var there bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM events WHERE id = $1)`, id).Scan(&there); err != nil {
		return err
	}
	if there {
		return ErrStale
	}
	return ErrNotFound
}

// SetDeleted moves an event into the trash (at, by) or back out of it (nil,
// nil), adding one to its sequence, when it is still at expect.
func SetDeleted(ctx context.Context, db DB, id uuid.UUID, at *time.Time, by *uuid.UUID, now time.Time, expect *int) (int, error) {
	var seq int
	err := db.QueryRowContext(ctx, `UPDATE events SET deleted_at = $2, deleted_by = $3, updated_at = $4, sequence = sequence + 1
		WHERE id = $1 AND ($5::int IS NULL OR sequence = $5) RETURNING sequence`,
		id, at, nullUUID(by), now, expect).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, missingOrStale(ctx, db, id)
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.SetDeleted err: %+v", err)
		return 0, err
	}
	return seq, nil
}

// Touch adds one to an event's sequence and stamps it: a change kept in
// another table (its guest list).
func Touch(ctx context.Context, db DB, id uuid.UUID, now time.Time) (int, error) {
	var seq int
	err := db.QueryRowContext(ctx, `UPDATE events SET updated_at = $2, sequence = sequence + 1 WHERE id = $1 RETURNING sequence`, id, now).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return seq, err
}

// SetGoogleID records the event's copy in Google Calendar ("" forgets it),
// and nothing else: the copy's id says nothing about the event's own fields.
func SetGoogleID(ctx context.Context, db DB, id uuid.UUID, googleID string) error {
	_, err := db.ExecContext(ctx, `UPDATE events SET google_event_id = $2 WHERE id = $1`, id, nullText(googleID))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.SetGoogleID err: %+v", err)
	}
	return err
}

// Get is one event, in the trash or not; nil when there is none.
func Get(ctx context.Context, db DB, id uuid.UUID) (*Event, error) {
	e, err := scanEvent(db.QueryRowContext(ctx, `SELECT `+eventColumns+` FROM events e WHERE e.id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.Get err: %+v", err)
		return nil, err
	}
	return e, nil
}

// GetMany is the events with these ids that exist, in the trash or not.
func GetMany(ctx context.Context, db DB, ids []uuid.UUID) ([]*Event, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	return queryEvents(ctx, db, `SELECT `+eventColumns+` FROM events e WHERE e.id = ANY($1::uuid[])`, uuidArray(ids))
}

func queryEvents(ctx context.Context, db DB, q string, args ...any) ([]*Event, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2 query err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var out []*Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Window is a stretch of time a read is about: instants for timed events,
// and the dates that could fall in it anywhere for all-day ones.
type Window struct {
	From, To         time.Time
	FromDate, ToDate string // "YYYY-MM-DD": the first date that could touch From, the date after the last that could touch To
}

// RangeOf is what person is on (as organiser or guest) that could touch w,
// not in the trash: single events overlapping it, and series that reach it
// or have an instance moved by hand into it (their instances are worked out
// by the caller). At most limit rows, by start.
func RangeOf(ctx context.Context, db DB, person uuid.UUID, w Window, limit int) ([]*Event, error) {
	return queryEvents(ctx, db, `SELECT `+eventColumns+` FROM events e
		WHERE e.deleted_at IS NULL AND e.series_id IS NULL
		  AND (e.created_by = $1 OR EXISTS (SELECT 1 FROM event_attendees a WHERE a.event_id = e.id AND a.user_id = $1))
		  AND (`+overlaps+` OR `+movedInto+`)
		ORDER BY COALESCE(e.start_at, (e.start_date::timestamp AT TIME ZONE 'UTC')), e.id
		LIMIT $6`, person, w.From, w.To, w.FromDate, w.ToDate, limit)
}

// movedInto is a series with a live instance moved by hand into the window
// over the same parameters as overlaps: past the series' end or before its
// start, where the series itself doesn't reach.
const movedInto = `(e.rrule <> '' AND EXISTS (SELECT 1 FROM events o
	WHERE o.series_id = e.id AND o.deleted_at IS NULL AND o.status <> 'cancelled'
	  AND ((o.start_at <= $3 AND o.end_at >= $2) OR (o.start_date <= $5::date AND o.end_date >= $4::date))))`

// overlaps is the window test of RangeOf and AwayOf over $2 (from), $3 (to),
// $4 (from date) and $5 (to date), with the bounds counted in, as the graph
// read it replaces counted them (le(start, to) AND ge(end, from)): a moment's
// read (from = to) finds what is on at that moment. A series reaches the
// window when it starts by its end (an all-day one by its last date) and
// either never ends or its last start is no earlier than the window's start
// less its longest instance and a day.
const overlaps = `(
	(e.rrule = '' AND ((e.start_at <= $3 AND e.end_at >= $2) OR (e.start_date <= $5::date AND e.end_date >= $4::date)))
	OR (e.rrule <> '' AND (e.start_at <= $3 OR e.start_date <= $5::date)
	    AND (e.until_at IS NULL OR e.until_at >= $2 - (COALESCE(e.end_at - e.start_at, (e.end_date - e.start_date) * interval '1 day') + interval '1 day')))
)`

// TrashOf is person's events in the trash, newest first, deleted before
// before (nil: from the newest), at most limit.
func TrashOf(ctx context.Context, db DB, person uuid.UUID, before *time.Time, limit int) ([]*Event, error) {
	return queryEvents(ctx, db, `SELECT `+eventColumns+` FROM events e
		WHERE e.created_by = $1 AND e.deleted_at IS NOT NULL AND ($2::timestamptz IS NULL OR e.deleted_at < $2)
		ORDER BY e.deleted_at DESC, e.id
		LIMIT $3`, person, before, limit)
}

// AwayOf is the out-of-office events of these people that could touch w, not
// in the trash nor cancelled: single events and series (expanded by the
// caller).
func AwayOf(ctx context.Context, db DB, people []uuid.UUID, w Window, limit int) ([]*Event, error) {
	if len(people) == 0 {
		return nil, nil
	}
	return queryEvents(ctx, db, `SELECT `+eventColumns+` FROM events e
		WHERE e.created_by = ANY($1::uuid[]) AND e.kind = 'out_of_office'
		  AND e.deleted_at IS NULL AND e.status <> 'cancelled' AND e.series_id IS NULL
		  AND `+overlaps+`
		ORDER BY COALESCE(e.start_at, (e.start_date::timestamp AT TIME ZONE 'UTC')), e.id
		LIMIT $6`, uuidArray(people), w.From, w.To, w.FromDate, w.ToDate, limit)
}

// OverridesOf is the overrides of these series that could touch w: by the
// instance they replace or by their own times, trashed ones too (a deleted
// instance is an override in the trash, and stays deleted).
func OverridesOf(ctx context.Context, db DB, series []uuid.UUID, w Window) ([]*Event, error) {
	if len(series) == 0 {
		return nil, nil
	}
	return queryEvents(ctx, db, `SELECT `+eventColumns+` FROM events e
		WHERE e.series_id = ANY($1::uuid[])
		  AND ((e.recurrence_id >= $2::timestamptz - interval '2 days' AND e.recurrence_id <= $3::timestamptz + interval '2 days')
		       OR (e.start_at <= $3 AND e.end_at >= $2)
		       OR (e.start_date <= $5::date AND e.end_date >= $4::date))`,
		uuidArray(series), w.From, w.To, w.FromDate, w.ToDate)
}

// OverridesOfSeries is every override of one series, trashed ones too, by the
// instance each replaces: what a change to the whole series carries along.
// At most limit.
func OverridesOfSeries(ctx context.Context, db DB, series uuid.UUID, limit int) ([]*Event, error) {
	return queryEvents(ctx, db, `SELECT `+eventColumns+` FROM events e WHERE e.series_id = $1 ORDER BY e.recurrence_id LIMIT $2`, series, limit)
}

// OverrideAt is the override of one instance of a series, trashed or not;
// nil when the instance has none.
func OverrideAt(ctx context.Context, db DB, series uuid.UUID, recurrence time.Time) (*Event, error) {
	e, err := scanEvent(db.QueryRowContext(ctx, `SELECT `+eventColumns+` FROM events e WHERE e.series_id = $1 AND e.recurrence_id = $2`, series, recurrence))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return e, err
}
