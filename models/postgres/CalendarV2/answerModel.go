package models

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// InstanceAnswer is a guest's answer for one instance of a repeating event
// alone (event_instance_answers): it comes before their answer to the series,
// for that instance only.
type InstanceAnswer struct {
	// EventID is the series; RecurrenceID the instance's start in it.
	EventID      uuid.UUID
	RecurrenceID time.Time
	UserID       *uuid.UUID
	Email        string
	Response     string
	Note         string
	RespondedAt  time.Time
	// Proposal is another time they proposed for this instance (nil for none).
	Proposal *Proposal
}

// Key is who answered.
func (a InstanceAnswer) Key() GuestKey {
	if a.UserID != nil {
		id := *a.UserID
		return GuestKey{UserID: &id}
	}
	return GuestKey{Email: strings.ToLower(a.Email)}
}

// InstanceAnswersOf is the answers given for these instances alone: the
// instance of each series[i] that starts at rids[i] in it. One read, whatever
// the number of instances.
func InstanceAnswersOf(ctx context.Context, db DB, series []uuid.UUID, rids []time.Time) ([]InstanceAnswer, error) {
	if len(series) == 0 || len(series) != len(rids) {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT a.event_id, a.recurrence_id, a.user_id, a.email, a.response, a.note, a.responded_at,
		a.proposed_start, a.proposed_end, a.proposed_against_start, a.proposed_against_end
		FROM event_instance_answers a
		JOIN unnest($1::uuid[], $2::timestamptz[]) AS i (event_id, recurrence_id)
		  ON a.event_id = i.event_id AND a.recurrence_id = i.recurrence_id`, uuidArray(series), timesArray(rids))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.InstanceAnswersOf err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var out []InstanceAnswer
	for rows.Next() {
		var a InstanceAnswer
		var user uuid.NullUUID
		var pStart, pEnd, pAgainstStart, pAgainstEnd sql.NullTime
		if err := rows.Scan(&a.EventID, &a.RecurrenceID, &user, &a.Email, &a.Response, &a.Note, &a.RespondedAt,
			&pStart, &pEnd, &pAgainstStart, &pAgainstEnd); err != nil {
			return nil, err
		}
		if user.Valid {
			id := user.UUID
			a.UserID = &id
		}
		a.RecurrenceID, a.RespondedAt = a.RecurrenceID.UTC(), a.RespondedAt.UTC()
		a.Proposal = proposalOf(pStart, pEnd, pAgainstStart, pAgainstEnd)
		out = append(out, a)
	}
	return out, rows.Err()
}

// SetInstanceAnswer writes a guest's answer for one instance alone, with
// their note and proposal, over any they gave for it before.
func SetInstanceAnswer(ctx context.Context, db DB, a InstanceAnswer) error {
	const set = `DO UPDATE SET response = EXCLUDED.response, note = EXCLUDED.note, responded_at = EXCLUDED.responded_at,
			proposed_start = EXCLUDED.proposed_start, proposed_end = EXCLUDED.proposed_end,
			proposed_against_start = EXCLUDED.proposed_against_start, proposed_against_end = EXCLUDED.proposed_against_end`
	v := a.Proposal.values()
	var err error
	if a.UserID != nil {
		_, err = db.ExecContext(ctx, `INSERT INTO event_instance_answers (event_id, recurrence_id, user_id, email, response, note, responded_at,
			proposed_start, proposed_end, proposed_against_start, proposed_against_end)
			VALUES ($1, $2, $3, '', $4, $5, $6, $7, $8, $9, $10)
			ON CONFLICT (event_id, recurrence_id, user_id) WHERE user_id IS NOT NULL `+set,
			a.EventID, a.RecurrenceID, *a.UserID, a.Response, a.Note, a.RespondedAt, v[0], v[1], v[2], v[3])
	} else {
		_, err = db.ExecContext(ctx, `INSERT INTO event_instance_answers (event_id, recurrence_id, user_id, email, response, note, responded_at,
			proposed_start, proposed_end, proposed_against_start, proposed_against_end)
			VALUES ($1, $2, NULL, $3, $4, $5, $6, $7, $8, $9, $10)
			ON CONFLICT (event_id, recurrence_id, lower(email)) WHERE user_id IS NULL `+set,
			a.EventID, a.RecurrenceID, strings.ToLower(strings.TrimSpace(a.Email)), a.Response, a.Note, a.RespondedAt, v[0], v[1], v[2], v[3])
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.SetInstanceAnswer err: %+v", err)
	}
	return err
}

// ClearInstanceAnswers forgets what a guest answered for single instances of
// a series: their answer to all of it stands for every instance again.
func ClearInstanceAnswers(ctx context.Context, db DB, series uuid.UUID, k GuestKey) error {
	cond, who := guestMatch(k, 2)
	_, err := db.ExecContext(ctx, `DELETE FROM event_instance_answers WHERE event_id = $1 AND `+cond, series, who)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.ClearInstanceAnswers err: %+v", err)
	}
	return err
}

// ForgetInstanceAnswers drops the answers of guests no longer on a series:
// the people and addresses taken off it.
func ForgetInstanceAnswers(ctx context.Context, db DB, series uuid.UUID, people []uuid.UUID, emails []string) error {
	if len(people) == 0 && len(emails) == 0 {
		return nil
	}
	_, err := db.ExecContext(ctx, `DELETE FROM event_instance_answers
		WHERE event_id = $1 AND (user_id = ANY($2::uuid[]) OR (user_id IS NULL AND lower(email) = ANY($3::text[])))`,
		series, uuidArray(people), pq.Array(lowerAll(emails)))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.ForgetInstanceAnswers err: %+v", err)
	}
	return err
}

// GuestOn is whether a guest is on an event (a series, for its instances).
func GuestOn(ctx context.Context, db DB, event uuid.UUID, k GuestKey) (bool, error) {
	cond, who := guestMatch(k, 2)
	var on bool
	err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM event_attendees WHERE event_id = $1 AND `+cond+`)`, event, who).Scan(&on)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.GuestOn err: %+v", err)
	}
	return on, err
}
