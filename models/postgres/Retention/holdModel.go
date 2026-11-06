package Retention

// Holds (migration 569, business/Retention): who is on hold, from when, and by
// whom. Nothing here deletes: releasing a hold and taking a person off one are
// stamps, so the rows stay the record of who was held and when.

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// Every statement here runs through models/pgexec: on the pool, or inside the
// transaction pgexec.InTx put in ctx (a hold and its first people are one
// change).

// ErrNotFound is no hold with that id.
var ErrNotFound = errors.New("retention: no such hold")

// Hold is one hold and the people on it now.
type Hold struct {
	ID         uuid.UUID   `json:"id"`
	Name       string      `json:"name"`
	Note       string      `json:"note"`
	CreatedBy  uuid.UUID   `json:"created_by"`
	CreatedAt  time.Time   `json:"created_at"`
	UpdatedAt  time.Time   `json:"updated_at"`
	ReleasedAt *time.Time  `json:"released_at,omitempty"`
	ReleasedBy *uuid.UUID  `json:"released_by,omitempty"`
	People     []uuid.UUID `json:"people"`
}

const holdColumns = `h.id, h.name, h.note, h.created_by, h.created_at, h.updated_at, h.released_at, h.released_by,
	COALESCE((SELECT array_agg(p.user_id ORDER BY p.added_at, p.user_id) FROM retention_hold_people p
	          WHERE p.hold_id = h.id AND p.removed_at IS NULL), '{}')`

func scanHold(s pgexec.Row) (*Hold, error) {
	var h Hold
	var released sql.NullTime
	var releasedBy uuid.NullUUID
	var people []string
	if err := s.Scan(&h.ID, &h.Name, &h.Note, &h.CreatedBy, &h.CreatedAt, &h.UpdatedAt, &released, &releasedBy, pq.Array(&people)); err != nil {
		return nil, err
	}
	if released.Valid {
		t := released.Time.UTC()
		h.ReleasedAt = &t
	}
	if releasedBy.Valid {
		id := releasedBy.UUID
		h.ReleasedBy = &id
	}
	h.People = make([]uuid.UUID, 0, len(people))
	for _, p := range people {
		if id, err := uuid.Parse(p); err == nil {
			h.People = append(h.People, id)
		}
	}
	return &h, nil
}

// Insert adds a hold with its first people; inside pgexec.InTx, as one change.
func Insert(ctx context.Context, h *Hold, now time.Time) error {
	if _, err := pgexec.Exec(ctx, `INSERT INTO retention_holds (id, name, note, created_by, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $5)`, h.ID, h.Name, h.Note, h.CreatedBy, now); err != nil {
		return err
	}
	_, err := AddPeople(ctx, h.ID, h.People, h.CreatedBy, now)
	return err
}

// List is every hold, live ones first, newest first within each.
func List(ctx context.Context, limit int) ([]*Hold, error) {
	return pgexec.Query(ctx, `SELECT `+holdColumns+` FROM retention_holds h
		ORDER BY (h.released_at IS NULL) DESC, h.created_at DESC, h.id LIMIT $1`, scanHold, limit)
}

// Get is one hold, or ErrNotFound.
func Get(ctx context.Context, id uuid.UUID) (*Hold, error) {
	return get(ctx, id, "")
}

// GetLocked is Get inside pgexec.InTx with the hold's row locked until the
// transaction ends, so changes to one hold run one after another, each
// reading what the last one left.
func GetLocked(ctx context.Context, id uuid.UUID) (*Hold, error) {
	return get(ctx, id, " FOR UPDATE OF h")
}

func get(ctx context.Context, id uuid.UUID, lock string) (*Hold, error) {
	h, found, err := pgexec.QueryOne(ctx, `SELECT `+holdColumns+` FROM retention_holds h WHERE h.id = $1`+lock, scanHold, id)
	if err == nil && !found {
		return nil, ErrNotFound
	}
	return h, err
}

// Rename sets a live hold's name and note, and stamps it changed; false when
// it's released or gone.
func Rename(ctx context.Context, id uuid.UUID, name, note string, now time.Time) (bool, error) {
	n, err := pgexec.Exec(ctx, `UPDATE retention_holds SET name = $2, note = $3, updated_at = $4
		WHERE id = $1 AND released_at IS NULL`, id, name, note, now)
	return n == 1, err
}

// AddPeople puts people on a hold in ONE statement; it answers the ones who
// weren't on it (someone on it already keeps their row and its added_at).
func AddPeople(ctx context.Context, id uuid.UUID, people []uuid.UUID, by uuid.UUID, now time.Time) ([]uuid.UUID, error) {
	if len(people) == 0 {
		return nil, nil
	}
	rowIDs := make([]uuid.UUID, len(people))
	for i := range rowIDs {
		rowIDs[i] = uuid.New()
	}
	return pgexec.Query(ctx, `INSERT INTO retention_hold_people (id, hold_id, user_id, added_by, added_at)
		SELECT p.id, $1, p.user_id, $4, $5 FROM unnest($2::uuid[], $3::uuid[]) AS p(id, user_id)
		ON CONFLICT (hold_id, user_id) WHERE removed_at IS NULL DO NOTHING
		RETURNING user_id`, scanID, id, pq.Array(strs(rowIDs)), pq.Array(strs(people)), by, now)
}

// RemovePeople takes people off a hold; it answers the ones who were on it.
func RemovePeople(ctx context.Context, id uuid.UUID, people []uuid.UUID, by uuid.UUID, now time.Time) ([]uuid.UUID, error) {
	if len(people) == 0 {
		return nil, nil
	}
	return pgexec.Query(ctx, `UPDATE retention_hold_people SET removed_at = $3, removed_by = $4
		WHERE hold_id = $1 AND user_id = ANY($2::uuid[]) AND removed_at IS NULL RETURNING user_id`,
		scanID, id, pq.Array(strs(people)), now, by)
}

// KnownPeople is which of ids are people in the workspace, gone ones included:
// a hold may name someone who has left, which is often why it's placed.
func KnownPeople(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	out := map[uuid.UUID]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	found, err := pgexec.Query(ctx, `SELECT id FROM users WHERE id = ANY($1::uuid[])`, scanID, pq.Array(strs(ids)))
	for _, id := range found {
		out[id] = true
	}
	return out, err
}

// Release ends a hold; false when it was released already or is gone. Its
// people's rows stay, so the hold still says who it held.
func Release(ctx context.Context, id, by uuid.UUID, now time.Time) (bool, error) {
	n, err := pgexec.Exec(ctx, `UPDATE retention_holds SET released_at = $3, released_by = $2, updated_at = $3
		WHERE id = $1 AND released_at IS NULL`, id, by, now)
	return n == 1, err
}

// HeldPeople is everyone on a live hold, as SQL a kind's purge and count put
// inside their own statement ("created_by IN (" + HeldPeople + ")"), so the
// holds a removal respects are the ones in force when it runs, not a list read
// before it. Every kind uses this one definition of who is held.
const HeldPeople = `SELECT hp.user_id FROM retention_hold_people hp
	JOIN retention_holds hh ON hh.id = hp.hold_id
	WHERE hp.removed_at IS NULL AND hh.released_at IS NULL`

// Held is everyone on a live hold now: whose items no rule removes.
func Held(ctx context.Context) ([]uuid.UUID, error) {
	return pgexec.Query(ctx, `SELECT DISTINCT user_id FROM (`+HeldPeople+`) held ORDER BY user_id`, scanID)
}

// scanID reads a row of one uuid.
func scanID(r pgexec.Row) (id uuid.UUID, err error) {
	err = r.Scan(&id)
	return
}

// strs is ids as text, for a uuid[] parameter.
func strs(ids []uuid.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}
