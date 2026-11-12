package models

import (
	"context"
	"encoding/json"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// ReminderJobType is the scheduled_jobs job_type of a calendar reminder
// (migration 362): one waiting job per person, event and kind.
const ReminderJobType = "calendar_reminder"

// The kinds of reminder job: the next one a person's reminders give, a
// snooze of one that fired, or a wake-up to plan again when the next
// instance of a series is further off than a plan looks.
const (
	ReminderNext   = "next"
	ReminderSnooze = "snooze"
)

// ReminderPayload is a reminder job's payload: which event and instance, and
// which of the person's reminders, fire when.
type ReminderPayload struct {
	EventID string `json:"event_id"`
	Kind    string `json:"kind"`
	// RecurrenceID is the instance's start in its series, "" for a single
	// event; Start is when the instance starts (an all-day one's first day
	// at 00:00 in the person's zone).
	RecurrenceID string `json:"recurrence_id,omitempty"`
	Start        string `json:"start,omitempty"`
	Minutes      int    `json:"minutes"`
	FireAt       string `json:"fire_at"`
	// Email is a reminder that comes by email rather than as a pop-up
	// (migration 573), as planned.
	Email bool `json:"email,omitempty"`
	// Default is a reminder the person's default reminders gave (they set
	// none on the event), so its email says to change the default to stop it.
	Default bool `json:"default,omitempty"`
	// Replan is a wake-up with nothing to say: plan this person's next
	// reminder for the event again.
	Replan bool `json:"replan,omitempty"`
}

// RemindersOf is each person's own reminders for an event (event_reminders):
// a person with no row has none set (the default applies), and one with an
// empty list chose none.
func RemindersOf(ctx context.Context, db DB, event uuid.UUID, people []uuid.UUID) (map[uuid.UUID][]int, error) {
	out := map[uuid.UUID][]int{}
	if len(people) == 0 {
		return out, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT user_id, minutes FROM event_reminders WHERE event_id = $1 AND user_id = ANY($2::uuid[])`, event, uuidArray(people))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.RemindersOf err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var minutes pq.Int64Array
		if err := rows.Scan(&id, &minutes); err != nil {
			return nil, err
		}
		set := make([]int, 0, len(minutes))
		for _, m := range minutes {
			set = append(set, int(m))
		}
		out[id] = set
	}
	return out, rows.Err()
}

// RemindersForPerson is one person's own reminders for each of these events
// that they set any for (an empty list is none).
func RemindersForPerson(ctx context.Context, db DB, person uuid.UUID, events []uuid.UUID) (map[uuid.UUID][]int, error) {
	out := map[uuid.UUID][]int{}
	if len(events) == 0 {
		return out, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT event_id, minutes FROM event_reminders WHERE user_id = $1 AND event_id = ANY($2::uuid[])`, person, uuidArray(events))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.RemindersForPerson err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var minutes pq.Int64Array
		if err := rows.Scan(&id, &minutes); err != nil {
			return nil, err
		}
		set := make([]int, 0, len(minutes))
		for _, m := range minutes {
			set = append(set, int(m))
		}
		out[id] = set
	}
	return out, rows.Err()
}

// SetReminders is a person's own reminders for an event: nil goes back to
// the default, an empty list is none.
func SetReminders(ctx context.Context, db DB, event, person uuid.UUID, minutes, email []int, at time.Time) error {
	if minutes == nil {
		_, err := db.ExecContext(ctx, `DELETE FROM event_reminders WHERE event_id = $1 AND user_id = $2`, event, person)
		return err
	}
	_, err := db.ExecContext(ctx, `INSERT INTO event_reminders (event_id, user_id, minutes, email_minutes, updated_at) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (event_id, user_id) DO UPDATE SET minutes = EXCLUDED.minutes, email_minutes = EXCLUDED.email_minutes, updated_at = EXCLUDED.updated_at`,
		event, person, int64s(minutes), int64s(email), at)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.SetReminders err: %+v", err)
	}
	return err
}

// int64s is minutes as Postgres takes an integer[]: never NULL. Pure.
func int64s(minutes []int) pq.Int64Array {
	out := make(pq.Int64Array, 0, len(minutes))
	for _, m := range minutes {
		out = append(out, int64(m))
	}
	return out
}

// EmailRemindersOf is which of each person's own reminders for an event come
// by email (migration 573): only people with their own reminders appear.
func EmailRemindersOf(ctx context.Context, event uuid.UUID, people []uuid.UUID) (map[uuid.UUID][]int, error) {
	if len(people) == 0 {
		return map[uuid.UUID][]int{}, nil
	}
	return minutesByID(ctx, "EmailRemindersOf",
		`SELECT user_id, email_minutes FROM event_reminders WHERE event_id = $1 AND user_id = ANY($2::uuid[])`, event, uuidArray(people))
}

// EmailRemindersForPerson is which of one person's own reminders for each of
// these events come by email: only events they set their own for appear.
func EmailRemindersForPerson(ctx context.Context, person uuid.UUID, events []uuid.UUID) (map[uuid.UUID][]int, error) {
	if len(events) == 0 {
		return map[uuid.UUID][]int{}, nil
	}
	return minutesByID(ctx, "EmailRemindersForPerson",
		`SELECT event_id, email_minutes FROM event_reminders WHERE user_id = $1 AND event_id = ANY($2::uuid[])`, person, uuidArray(events))
}

// minutesByID reads rows of (an id, an integer[] of minutes) into a map by
// the id: a person's, or an event's.
func minutesByID(ctx context.Context, what, query string, args ...any) (map[uuid.UUID][]int, error) {
	rows, err := pgexec.Query(ctx, query, scanIDMinutes, args...)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.%s err: %+v", what, err)
		return nil, err
	}
	out := make(map[uuid.UUID][]int, len(rows))
	for _, r := range rows {
		out[r.id] = r.minutes
	}
	return out, nil
}

// idMinutes is a row of an id and a set of minutes.
type idMinutes struct {
	id      uuid.UUID
	minutes []int
}

// scanIDMinutes reads (an id, an integer[] of minutes).
func scanIDMinutes(r pgexec.Row) (idMinutes, error) {
	var row idMinutes
	var set pq.Int64Array
	if err := r.Scan(&row.id, &set); err != nil {
		return row, err
	}
	row.minutes = make([]int, len(set))
	for i, m := range set {
		row.minutes[i] = int(m)
	}
	return row, nil
}

// ReplanPersonJobs has every event person is on (they organise it, or are a
// member guest) that may remind them between from and to planned again for
// them, now: one wake-up each, in place of any next reminder waiting, so a
// change of their defaults reaches events planned under the old ones. At
// most limit, soonest first. One statement, whatever the count.
func ReplanPersonJobs(ctx context.Context, person uuid.UUID, from, to time.Time, limit int) error {
	_, err := pgexec.Exec(ctx, `INSERT INTO scheduled_jobs (id, job_type, user_uuid, payload, run_at, max_attempts)
		SELECT gen_random_uuid(), $4::text, $1::uuid,
		       jsonb_build_object('event_id', e.id::text, 'kind', $5::text, 'minutes', 0, 'fire_at', to_char($2::timestamptz AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), 'replan', true),
		       $2::timestamptz, 3
		FROM events e
		WHERE e.deleted_at IS NULL AND e.series_id IS NULL AND e.status <> 'cancelled'
		  AND (e.created_by = $1::uuid OR EXISTS (SELECT 1 FROM event_attendees a WHERE a.event_id = e.id AND a.user_id = $1))
		  AND (
		    (e.rrule = '' AND (e.start_at BETWEEN $2 AND $3 OR e.start_date BETWEEN ($2::timestamptz)::date AND ($3::timestamptz)::date))
		    OR (e.rrule <> '' AND (e.until_at IS NULL OR e.until_at >= $2) AND coalesce(e.start_at, e.start_date::timestamptz) <= $3)
		  )
		ORDER BY coalesce(e.start_at, e.start_date::timestamptz)
		LIMIT $6
		ON CONFLICT (user_uuid, (payload->>'event_id'), (payload->>'kind'))
		  WHERE job_type = 'calendar_reminder' AND status = 'pending' AND deleted_at IS NULL
		DO UPDATE SET payload = EXCLUDED.payload, run_at = EXCLUDED.run_at, attempts = 0, last_error = NULL, updated_at = NOW()`,
		person, from, to, ReminderJobType, ReminderNext, limit)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.ReplanPersonJobs err: %+v", err)
	}
	return err
}

// PlanReminderJob is a person's waiting reminder job of a kind for an event,
// made or moved to fire at runAt. One per person, event and kind.
func PlanReminderJob(ctx context.Context, db DB, person uuid.UUID, p ReminderPayload, runAt time.Time) error {
	payload, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO scheduled_jobs (id, job_type, user_uuid, payload, run_at, max_attempts)
		VALUES ($1, $2, $3, $4, $5, 3)
		ON CONFLICT (user_uuid, (payload->>'event_id'), (payload->>'kind'))
		  WHERE job_type = 'calendar_reminder' AND status = 'pending' AND deleted_at IS NULL
		DO UPDATE SET payload = EXCLUDED.payload, run_at = EXCLUDED.run_at, attempts = 0, last_error = NULL, updated_at = NOW()`,
		uuid.New(), ReminderJobType, person, string(payload), runAt)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.PlanReminderJob err: %+v", err)
	}
	return err
}

// CancelReminderJobs cancels the waiting reminder jobs of an event: of these
// people (nil: everyone's), of a kind ("": every kind).
func CancelReminderJobs(ctx context.Context, db DB, event uuid.UUID, people []uuid.UUID, kind string) error {
	var users any
	if people != nil {
		users = uuidArray(people)
	}
	_, err := db.ExecContext(ctx, `UPDATE scheduled_jobs SET status = 'cancelled', updated_at = NOW()
		WHERE job_type = 'calendar_reminder' AND status = 'pending' AND deleted_at IS NULL
		  AND payload->>'event_id' = $1
		  AND ($2::uuid[] IS NULL OR user_uuid = ANY($2::uuid[]))
		  AND ($3 = '' OR payload->>'kind' = $3)`, event.String(), users, kind)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.CancelReminderJobs err: %+v", err)
	}
	return err
}

// CancelReminderJobsExcept cancels the waiting reminder jobs of an event of
// everyone but these people: those no longer on it.
func CancelReminderJobsExcept(ctx context.Context, db DB, event uuid.UUID, keep []uuid.UUID) error {
	_, err := db.ExecContext(ctx, `UPDATE scheduled_jobs SET status = 'cancelled', updated_at = NOW()
		WHERE job_type = 'calendar_reminder' AND status = 'pending' AND deleted_at IS NULL
		  AND payload->>'event_id' = $1 AND NOT (user_uuid = ANY($2::uuid[]))`, event.String(), uuidArray(keep))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.CancelReminderJobsExcept err: %+v", err)
	}
	return err
}

// UnplannedEvents is up to limit live events and series, each with its
// organiser, that start (or, for a series, still run) between from and to
// and have no reminder waiting for anyone: those a write made with nobody
// told (an import, the move, the demo's seeding), for the planner to reach.
func UnplannedEvents(ctx context.Context, db DB, from, to time.Time, limit int) ([]uuid.UUID, error) {
	return queryIDs(ctx, db, "UnplannedEvents", `SELECT e.id FROM events e
		WHERE e.deleted_at IS NULL AND e.series_id IS NULL AND e.status <> 'cancelled'
		  AND (
		    (e.rrule = '' AND (e.start_at BETWEEN $1 AND $2 OR e.start_date BETWEEN ($1::timestamptz)::date AND ($2::timestamptz)::date))
		    OR (e.rrule <> '' AND (e.until_at IS NULL OR e.until_at >= $1) AND coalesce(e.start_at, e.start_date::timestamptz) <= $2)
		  )
		  AND NOT EXISTS (SELECT 1 FROM scheduled_jobs j
		    WHERE j.job_type = 'calendar_reminder' AND j.status IN ('pending', 'running') AND j.deleted_at IS NULL
		      AND j.payload->>'event_id' = e.id::text)
		ORDER BY coalesce(e.start_at, e.start_date::timestamptz)
		LIMIT $3`, from, to, limit)
}
