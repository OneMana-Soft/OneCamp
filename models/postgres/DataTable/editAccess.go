package models

// Who edits a table's rows (V-170, migration data_table_edit_access): the table's general level
// for its members, and the people granted edit beyond it. A removed grant is a
// soft delete: every read here sees only live grants (deleted_at IS NULL), and
// the removed row stays as it was, with who removed it and when.

import (
	"context"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
)

// MaxEditors is how many people a table's owner may grant edit: the people a
// Share dialog lists and an audit row names, never a whole workspace (that is
// GeneralEdit).
const MaxEditors = 250

// ErrTooManyEditors is a change that would grant more than MaxEditors people.
var ErrTooManyEditors = errors.New("a table can name at most 250 people who edit it")

// ErrTableGone is a table that isn't there (deleted, or never was) when who
// edits it is read or changed.
var ErrTableGone = errors.New("table not found")

// Editor is a person granted edit on a table, as its managers see them.
type Editor struct {
	UserID    uuid.UUID `json:"user_id"`
	Name      string    `json:"name"`
	Email     string    `json:"email"`
	GrantedAt time.Time `json:"granted_at"`
}

// EditAccess is who edits a table's rows: its general level, and the people
// granted edit, the earliest granted first.
type EditAccess struct {
	General string   `json:"general"`
	Editors []Editor `json:"people"`
}

// EditorIDs is the people granted edit, by id.
func (a EditAccess) EditorIDs() []uuid.UUID {
	ids := make([]uuid.UUID, len(a.Editors))
	for i, e := range a.Editors {
		ids[i] = e.UserID
	}
	return ids
}

func scanOne(r pgexec.Row) (int, error) {
	var one int
	return one, r.Scan(&one)
}

// EditGranted is whether user holds a live edit grant on table: one read of
// the live grants' index.
func EditGranted(ctx context.Context, table, user uuid.UUID) (bool, error) {
	if user == uuid.Nil {
		return false, nil
	}
	_, found, err := pgexec.QueryOne(ctx, `SELECT 1 FROM data_table_grants
		WHERE table_id = $1 AND user_id = $2 AND deleted_at IS NULL`, scanOne, table, user)
	return found, err
}

// EditGrantedAmong is the tables among tables that user holds a live edit
// grant on: one read for all of them, however many.
func EditGrantedAmong(ctx context.Context, user uuid.UUID, tables []uuid.UUID) (map[uuid.UUID]bool, error) {
	out := make(map[uuid.UUID]bool)
	if user == uuid.Nil || len(tables) == 0 {
		return out, nil
	}
	ids, err := pgexec.Query(ctx, `SELECT table_id FROM data_table_grants
		WHERE user_id = $1 AND table_id = ANY($2) AND deleted_at IS NULL`, scanID, user, tables)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}

// liveEditors is table's live grants with each person's name and email.
func liveEditors(ctx context.Context, table uuid.UUID) ([]Editor, error) {
	return pgexec.Query(ctx, `SELECT g.user_id, COALESCE(NULLIF(u.display_name, ''), u.username, ''), COALESCE(u.email_id, ''), g.granted_at
		FROM data_table_grants g JOIN users u ON u.id = g.user_id
		WHERE g.table_id = $1 AND g.deleted_at IS NULL
		ORDER BY g.granted_at, g.id`, func(r pgexec.Row) (Editor, error) {
		var e Editor
		return e, r.Scan(&e.UserID, &e.Name, &e.Email, &e.GrantedAt)
	}, table)
}

// EditAccessOf is who edits table: its general level and its live grants.
func EditAccessOf(ctx context.Context, table uuid.UUID) (EditAccess, error) {
	general, found, err := pgexec.QueryOne(ctx, `SELECT general_level FROM data_tables WHERE id = $1 AND deleted_at IS NULL`, pgexec.Text, table)
	if err != nil {
		return EditAccess{}, err
	}
	if !found {
		return EditAccess{}, ErrTableGone
	}
	editors, err := liveEditors(ctx, table)
	return EditAccess{General: general, Editors: editors}, err
}

// EditChange is a change to who edits a table: its general level when set,
// the people to grant edit and the people whose grants are removed.
type EditChange struct {
	General *string
	Add     []uuid.UUID
	Remove  []uuid.UUID
}

// Changed is whether after differs from a: its general level, or who is
// granted edit (in any order).
func (a EditAccess) Changed(after EditAccess) bool {
	if a.General != after.General || len(a.Editors) != len(after.Editors) {
		return true
	}
	had := make(map[uuid.UUID]bool, len(a.Editors))
	for _, e := range a.Editors {
		had[e.UserID] = true
	}
	for _, e := range after.Editors {
		if !had[e.UserID] {
			return true
		}
	}
	return false
}

// SetEditAccess makes change to who edits table, by by, in one transaction
// that locks the table's row FOR NO KEY UPDATE: two changes to one table run
// one after the other, each reading what the other left, and rows pointing at
// the table are still written meanwhile. A granted person's grant isn't
// renewed; a removed grant is soft-deleted. It returns who edited the table
// before and after, or rolls back with ErrTooManyEditors past MaxEditors.
//
// record, when the change changed something, writes it down before the
// transaction commits: its error rolls the change back, so nothing changes
// unrecorded.
func SetEditAccess(ctx context.Context, table, by uuid.UUID, change EditChange, record func(before, after EditAccess) error) (before, after EditAccess, err error) {
	err = pgexec.InTx(ctx, func(ctx context.Context) error {
		general, found, err := pgexec.QueryOne(ctx, `SELECT general_level FROM data_tables WHERE id = $1 AND deleted_at IS NULL FOR NO KEY UPDATE`, pgexec.Text, table)
		if err != nil {
			return err
		}
		if !found {
			return ErrTableGone
		}
		if before.Editors, err = liveEditors(ctx, table); err != nil {
			return err
		}
		before.General, after.General = general, general
		if change.General != nil && *change.General != general {
			if _, err := pgexec.Exec(ctx, `UPDATE data_tables SET general_level = $2 WHERE id = $1`, table, *change.General); err != nil {
				return err
			}
			after.General = *change.General
		}
		if len(change.Remove) > 0 {
			if _, err := pgexec.Exec(ctx, `UPDATE data_table_grants SET deleted_at = NOW(), deleted_by = $3
				WHERE table_id = $1 AND user_id = ANY($2) AND deleted_at IS NULL`, table, change.Remove, by); err != nil {
				return err
			}
		}
		if len(change.Add) > 0 {
			if _, err := pgexec.Exec(ctx, `INSERT INTO data_table_grants (table_id, user_id, granted_by)
				SELECT $1, person, $3 FROM unnest($2::uuid[]) AS person
				ON CONFLICT (table_id, user_id) WHERE deleted_at IS NULL DO NOTHING`, table, change.Add, by); err != nil {
				return err
			}
		}
		if after.Editors, err = liveEditors(ctx, table); err != nil {
			return err
		}
		if len(after.Editors) > MaxEditors {
			return ErrTooManyEditors
		}
		if record != nil && before.Changed(after) {
			return record(before, after)
		}
		return nil
	})
	if err != nil {
		return EditAccess{}, EditAccess{}, err
	}
	return before, after, nil
}
