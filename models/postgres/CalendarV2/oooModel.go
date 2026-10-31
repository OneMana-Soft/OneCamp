package models

import (
	"context"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
)

// What an out-of-office event declines (calendar head 5b, migration 932).
const (
	OOODeclineNone = ""
	OOODeclineNew  = "new"
	OOODeclineAll  = "all"
)

// OOORule is an out-of-office event's decline rule and the message each
// decline carries as its note.
type OOORule struct {
	Decline string
	Message string
}

type oooRow struct {
	id   uuid.UUID
	rule OOORule
}

// OOORulesOf is these events' decline rules, in one read; an event with none
// is missing.
func OOORulesOf(ctx context.Context, events []uuid.UUID) (map[uuid.UUID]OOORule, error) {
	out := map[uuid.UUID]OOORule{}
	if len(events) == 0 {
		return out, nil
	}
	rows, err := pgexec.Query(ctx, `SELECT event_id, decline, message FROM event_ooo_rules WHERE event_id = ANY($1::uuid[])`,
		func(r pgexec.Row) (oooRow, error) {
			var o oooRow
			err := r.Scan(&o.id, &o.rule.Decline, &o.rule.Message)
			return o, err
		}, uuidArray(events))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.OOORulesOf err: %+v", err)
		return nil, err
	}
	for _, o := range rows {
		out[o.id] = o.rule
	}
	return out, nil
}

// SetOOORule keeps an event's decline rule; a rule that declines nothing
// removes it.
func SetOOORule(ctx context.Context, event uuid.UUID, r OOORule) error {
	var err error
	if r.Decline == OOODeclineNone {
		_, err = pgexec.Exec(ctx, `DELETE FROM event_ooo_rules WHERE event_id = $1`, event)
	} else {
		_, err = pgexec.Exec(ctx, `INSERT INTO event_ooo_rules (event_id, decline, message) VALUES ($1, $2, $3)
			ON CONFLICT (event_id) DO UPDATE SET decline = EXCLUDED.decline, message = EXCLUDED.message, updated_at = now()`,
			event, r.Decline, r.Message)
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.SetOOORule err: %+v", err)
	}
	return err
}
