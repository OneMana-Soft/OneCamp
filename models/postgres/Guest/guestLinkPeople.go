package models

// The workspace's links, counted and moved for an admin's decisions (docs
// C6-2): how many a shorter cap would stop, and the live links of people who
// have left, found by who made them and who published them. Every statement
// runs through models/pgexec.

import (
	"context"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
)

// CountStoppedByCap is how many links live now would stop if the cap were
// days: made days ago or more, and not standing. Nothing for days of 0.
func CountStoppedByCap(ctx context.Context, days int) (int, error) {
	if days <= 0 {
		return 0, nil
	}
	q := `SELECT count(*) FROM guest_grants WHERE ` + liveSQL(1) + ` AND NOT ` + StandingSQL +
		` AND created_at <= NOW() - make_interval(days => $2::int)`
	n, err := pgexec.Scalar[int](ctx, q, LinkCapDays(ctx), days)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Guest/CountStoppedByCap err: %+v", err)
	}
	return n, err
}

// CountLiveByPeople is how many live links each person made or published, in
// one read: a link one made and another published counts for both, and one
// made and published by the same person once. A person with none is left out.
func CountLiveByPeople(ctx context.Context, who []uuid.UUID) (map[uuid.UUID]int, error) {
	if len(who) == 0 {
		return map[uuid.UUID]int{}, nil
	}
	q := `SELECT person, count(*) FROM (
		SELECT created_by AS person FROM guest_grants WHERE created_by = ANY($2) AND ` + liveSQL(1) + `
		UNION ALL
		SELECT published_by FROM guest_grants WHERE published_by = ANY($2)
			AND published_by IS DISTINCT FROM created_by AND ` + liveSQL(1) + `
	) links GROUP BY person`
	n, err := pgexec.Pairs[uuid.UUID, int](ctx, q, LinkCapDays(ctx), who)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Guest/CountLiveByPeople err: %+v", err)
	}
	return n, err
}

// HandOverLinks puts a person's live links under someone else's name, made
// and published both, and closes their personal rooms, all in one
// transaction: a room is its owner's, at their name, and ends when they leave
// (D-CL-35), so it is never handed on. It answers how many moved and how many
// rooms closed. What moved keeps working.
func HandOverLinks(ctx context.Context, from, to uuid.UUID) (moved, closed int64, err error) {
	err = pgexec.InTx(ctx, func(ctx context.Context) error {
		capDays := LinkCapDays(ctx)
		rooms, err := pgexec.Exec(ctx, `UPDATE guest_grants SET revoked_at = NOW()
			WHERE (created_by = $2 OR published_by = $2) AND `+StandingSQL+` AND `+liveSQL(1), capDays, from)
		if err != nil {
			return err
		}
		made, err := pgexec.Exec(ctx, `UPDATE guest_grants SET created_by = $3 WHERE created_by = $2 AND `+liveSQL(1), capDays, from, to)
		if err != nil {
			return err
		}
		published, err := pgexec.Exec(ctx, `UPDATE guest_grants SET published_by = $3 WHERE published_by = $2 AND `+liveSQL(1), capDays, from, to)
		moved, closed = made+published, rooms
		return err
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Guest/HandOverLinks err: %+v", err)
		return 0, 0, err
	}
	return moved, closed, nil
}

// RevokeLiveByPerson turns off every live link a person made or published,
// pages and rooms with them, and answers which, so their guests' live views
// can be closed too.
func RevokeLiveByPerson(ctx context.Context, who uuid.UUID) ([]uuid.UUID, error) {
	q := `UPDATE guest_grants SET revoked_at = NOW() WHERE (created_by = $2 OR published_by = $2) AND ` + liveSQL(1) +
		` RETURNING ` + grantColumns
	grants, err := pgexec.Query(ctx, q, grantRow, LinkCapDays(ctx), who)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Guest/RevokeLiveByPerson err: %+v", err)
		return nil, err
	}
	ids := make([]uuid.UUID, 0, len(grants))
	for _, g := range grants {
		ids = append(ids, g.Id)
	}
	return ids, nil
}
