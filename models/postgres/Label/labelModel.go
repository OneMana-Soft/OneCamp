package Label

// Sensitivity labels (migration 570, business/Label): the labels a workspace
// admin names, and which label each board or doc carries. Nothing here
// deletes: removing a label and taking one off an item are stamps, so the rows
// stay the record of what was labelled, when and by whom.

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

// Every statement here runs through models/pgexec: on the pool, or inside the
// transaction pgexec.InTx put in ctx (an item's old label unset and its new
// one set are one change).

// ErrNotFound is no live label with that id.
var ErrNotFound = errors.New("label: no such label")

// ErrNameTaken is a name another live label has, in any case.
var ErrNameTaken = errors.New("label: another label has that name")

// Label is one live label.
type Label struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Colour    string    `json:"colour"`
	Rank      int       `json:"rank"`
	Rules     []string  `json:"rules"`
	CreatedBy uuid.UUID `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

const labelColumns = `l.id, l.name, l.colour, l.rank, l.rules, l.created_by, l.created_at, l.updated_at`

func scanLabel(s pgexec.Row) (*Label, error) {
	var l Label
	if err := s.Scan(&l.ID, &l.Name, &l.Colour, &l.Rank, pq.Array(&l.Rules), &l.CreatedBy, &l.CreatedAt, &l.UpdatedAt); err != nil {
		return nil, err
	}
	if l.Rules == nil {
		l.Rules = []string{}
	}
	l.CreatedAt, l.UpdatedAt = l.CreatedAt.UTC(), l.UpdatedAt.UTC()
	return &l, nil
}

// nameTaken is the live-name index refusing a second label of one name, in
// either driver's error (pgexec's pool answers pgx's, not lib/pq's).
func nameTaken(err error) bool {
	return helpers.IsUniqueViolationOn(err, "sensitivity_labels_live_name_idx")
}

// List is every live label, highest rank first, then by name.
func List(ctx context.Context) ([]*Label, error) {
	return pgexec.Query(ctx, `SELECT `+labelColumns+` FROM sensitivity_labels l
		WHERE l.deleted_at IS NULL ORDER BY l.rank DESC, lower(l.name), l.id`, scanLabel)
}

// Get is one live label.
func Get(ctx context.Context, id uuid.UUID) (*Label, error) {
	l, ok, err := pgexec.QueryOne(ctx, `SELECT `+labelColumns+` FROM sensitivity_labels l
		WHERE l.id = $1 AND l.deleted_at IS NULL`, scanLabel, id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotFound
	}
	return l, nil
}

// Insert adds a label.
func Insert(ctx context.Context, l *Label, now time.Time) error {
	_, err := pgexec.Exec(ctx, `INSERT INTO sensitivity_labels (id, name, colour, rank, rules, created_by, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $7)`, l.ID, l.Name, l.Colour, l.Rank, pq.Array(l.Rules), l.CreatedBy, now)
	if nameTaken(err) {
		return ErrNameTaken
	}
	return err
}

// Update writes a live label's name, colour, rank and rules.
func Update(ctx context.Context, l *Label, now time.Time) error {
	n, err := pgexec.Exec(ctx, `UPDATE sensitivity_labels SET name = $2, colour = $3, rank = $4, rules = $5, updated_at = $6
		WHERE id = $1 AND deleted_at IS NULL`, l.ID, l.Name, l.Colour, l.Rank, pq.Array(l.Rules), now)
	if nameTaken(err) {
		return ErrNameTaken
	}
	if err == nil && n == 0 {
		return ErrNotFound
	}
	return err
}

// Remove stamps a live label removed: its rules stop holding on every item.
func Remove(ctx context.Context, id, by uuid.UUID, now time.Time) error {
	n, err := pgexec.Exec(ctx, `UPDATE sensitivity_labels SET deleted_at = $2, deleted_by = $3, updated_at = $2
		WHERE id = $1 AND deleted_at IS NULL`, id, now, by)
	if err == nil && n == 0 {
		return ErrNotFound
	}
	return err
}

// OfItem is the live label an item carries, nil for none.
func OfItem(ctx context.Context, kind string, item uuid.UUID) (*Label, error) {
	l, ok, err := pgexec.QueryOne(ctx, `SELECT `+labelColumns+` FROM item_labels i
		JOIN sensitivity_labels l ON l.id = i.label_id AND l.deleted_at IS NULL
		WHERE i.kind = $1 AND i.item_uuid = $2 AND i.unset_at IS NULL`, scanLabel, kind, item)
	if err != nil || !ok {
		return nil, err
	}
	return l, nil
}

// OfItems is the live label of each item that carries one, by item.
func OfItems(ctx context.Context, kind string, items []uuid.UUID) (map[uuid.UUID]*Label, error) {
	out := map[uuid.UUID]*Label{}
	if len(items) == 0 {
		return out, nil
	}
	type pair struct {
		item  uuid.UUID
		label *Label
	}
	rows, err := pgexec.Query(ctx, `SELECT i.item_uuid, `+labelColumns+` FROM item_labels i
		JOIN sensitivity_labels l ON l.id = i.label_id AND l.deleted_at IS NULL
		WHERE i.kind = $1 AND i.item_uuid = ANY($2) AND i.unset_at IS NULL`, func(s pgexec.Row) (pair, error) {
		var p pair
		var l Label
		if err := s.Scan(&p.item, &l.ID, &l.Name, &l.Colour, &l.Rank, pq.Array(&l.Rules), &l.CreatedBy, &l.CreatedAt, &l.UpdatedAt); err != nil {
			return p, err
		}
		if l.Rules == nil {
			l.Rules = []string{}
		}
		p.label = &l
		return p, nil
	}, kind, pq.Array(items))
	if err != nil {
		return nil, err
	}
	for _, p := range rows {
		out[p.item] = p.label
	}
	return out, nil
}

// Set puts a label on an item in place of the one it carries (unset with the
// same stamp); inside pgexec.InTx, as one change.
func Set(ctx context.Context, kind string, item, label, by uuid.UUID, now time.Time) error {
	if err := Unset(ctx, kind, item, by, now); err != nil {
		return err
	}
	_, err := pgexec.Exec(ctx, `INSERT INTO item_labels (id, kind, item_uuid, label_id, set_by, set_at)
		VALUES ($1, $2, $3, $4, $5, $6)`, uuid.New(), kind, item, label, by, now)
	return err
}

// Unset takes the label an item carries off it; nothing when it carries none.
// A row whose label was removed is unset too, so it never comes back.
func Unset(ctx context.Context, kind string, item, by uuid.UUID, now time.Time) error {
	_, err := pgexec.Exec(ctx, `UPDATE item_labels SET unset_at = $3, unset_by = $4
		WHERE kind = $1 AND item_uuid = $2 AND unset_at IS NULL`, kind, item, now, by)
	return err
}

// Items is every item a live label is on now, by kind.
func Items(ctx context.Context, label uuid.UUID) (map[string][]uuid.UUID, error) {
	type pair struct {
		kind string
		item uuid.UUID
	}
	rows, err := pgexec.Query(ctx, `SELECT kind, item_uuid FROM item_labels
		WHERE label_id = $1 AND unset_at IS NULL ORDER BY kind, set_at, item_uuid`, func(s pgexec.Row) (pair, error) {
		var p pair
		err := s.Scan(&p.kind, &p.item)
		return p, err
	}, label)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	out := map[string][]uuid.UUID{}
	for _, p := range rows {
		out[p.kind] = append(out[p.kind], p.item)
	}
	return out, nil
}
