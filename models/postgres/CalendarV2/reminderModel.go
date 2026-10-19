package models

import (
	"context"
	"encoding/json"
	"time"

	"github.com/akashc777/OneCamp/helpers"
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
func SetReminders(ctx context.Context, db DB, event, person uuid.UUID, minutes []int, at time.Time) error {
	if minutes == nil {
		_, err := db.ExecContext(ctx, `DELETE FROM event_reminders WHERE event_id = $1 AND user_id = $2`, event, person)
		return err
	}
	set := make(pq.Int64Array, 0, len(minutes))
	for _, m := range minutes {
		set = append(set, int64(m))
	}
	_, err := db.ExecContext(ctx, `INSERT INTO event_reminders (event_id, user_id, minutes, updated_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (event_id, user_id) DO UPDATE SET minutes = EXCLUDED.minutes, updated_at = EXCLUDED.updated_at`, event, person, set, at)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.SetReminders err: %+v", err)
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
	rows, err := db.QueryContext(ctx, `SELECT e.id FROM events e
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
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.UnplannedEvents err: %+v", err)
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
