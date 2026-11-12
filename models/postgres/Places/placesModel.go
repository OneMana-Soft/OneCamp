// Package models (Places) stores a person's places: the things they starred or
// pinned, of any kind, in the order they put them (migration 206). Every query
// is scoped to the person; nobody reads or changes another's places. Recents
// are read from resource_views, the log of who opened which doc or board.
package models

import (
	"context"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/models/pgexec"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/google/uuid"
)

// Place is one row: a person's star or pin on one thing.
type Place struct {
	ID           uuid.UUID `json:"id"`
	UserUUID     uuid.UUID `json:"-"`
	Kind         string    `json:"kind"`
	ResourceKind string    `json:"resource_kind"`
	ResourceUUID string    `json:"resource_uuid"`
	Position     string    `json:"position"`
	CreatedAt    time.Time `json:"created_at"`
}

const columns = `id, user_uuid, kind, resource_kind, resource_uuid, position, created_at`

func scan(row interface{ Scan(...any) error }) (*Place, error) {
	var p Place
	if err := row.Scan(&p.ID, &p.UserUUID, &p.Kind, &p.ResourceKind, &p.ResourceUUID, &p.Position, &p.CreatedAt); err != nil {
		return nil, err
	}
	return &p, nil
}

// List is the person's places of one kind, in their order; resourceKind ""
// lists every kind of thing together, as the Starred list mixes them.
func List(ctx context.Context, userUUID uuid.UUID, kind, resourceKind string) ([]*Place, error) {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	q := `SELECT ` + columns + ` FROM user_places WHERE user_uuid = $1 AND kind = $2`
	args := []any{userUUID, kind}
	if resourceKind != "" {
		q += ` AND resource_kind = $3`
		args = append(args, resourceKind)
	}
	// position is COLLATE "C", so this is byte order, the order keys are made in.
	q += ` ORDER BY position, created_at`
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(c, q, args...)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Places List err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var out []*Place
	for rows.Next() {
		p, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Put places a thing at position, or moves it there if the person already
// has it; either way it answers the row as stored.
func Put(ctx context.Context, p *Place) (*Place, error) {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	out, err := scan(postgresInit.DBConn.SqlDB.QueryRowContext(c, `
		INSERT INTO user_places (id, user_uuid, kind, resource_kind, resource_uuid, position)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (user_uuid, kind, resource_uuid) DO UPDATE SET position = EXCLUDED.position
		RETURNING `+columns,
		uuid.New(), p.UserUUID, p.Kind, p.ResourceKind, p.ResourceUUID, p.Position))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Places Put err: %+v", err)
	}
	return out, err
}

// Remove takes one of the person's places away. Removing one they don't have
// is not an error: the place is gone either way.
func Remove(ctx context.Context, userUUID uuid.UUID, kind, resourceUUID string) error {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(c,
		`DELETE FROM user_places WHERE user_uuid = $1 AND kind = $2 AND resource_uuid = $3`,
		userUUID, kind, resourceUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Places Remove err: %+v", err)
	}
	return err
}

// RemoveEverywhere takes away everyone's places on things that are gone for
// good (a purged doc).
func RemoveEverywhere(ctx context.Context, resourceKind string, resourceUUIDs []string) error {
	if len(resourceUUIDs) == 0 {
		return nil
	}
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(c,
		`DELETE FROM user_places WHERE resource_kind = $1 AND resource_uuid = ANY($2)`,
		resourceKind, resourceUUIDs)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Places RemoveEverywhere err: %+v", err)
	}
	return err
}

// ForgetResourceRows removes what people's own rows hold of resources of one
// kind ("doc", "board") gone for good: who viewed them, their saved-for-later
// entries, and their guest links (the guests' one forget,
// guestModel.ForgetResources; the guests' comments cascade), so no link
// outlives what it opened. Each is tried on its own, and the first failure is
// answered once every one was tried.
func ForgetResourceRows(ctx context.Context, kind string, resourceUUIDs []string) error {
	if len(resourceUUIDs) == 0 || postgresInit.DBConn == nil {
		return nil
	}
	var first error
	for _, q := range []string{
		`DELETE FROM resource_views WHERE resource_type = $1 AND resource_uuid = ANY($2)`,
		`DELETE FROM saved_items WHERE item_type = $1 AND item_id = ANY($2)`,
	} {
		if _, err := pgexec.Exec(ctx, q, kind, resourceUUIDs); err != nil {
			helpers.LogErrorWithContext(ctx, "models/Places ForgetResourceRows %s: %v", strings.Fields(q)[2], err)
			if first == nil {
				first = err
			}
		}
	}
	if _, err := forgetGuestGrants(ctx, kind, resourceUUIDs); err != nil {
		helpers.LogErrorWithContext(ctx, "models/Places ForgetResourceRows guest_grants: %v", err)
		if first == nil {
			first = err
		}
	}
	return first
}

// forgetGuestGrants is the guests' forget of links to resources gone for good.
var forgetGuestGrants = guestModel.ForgetResources

// View is one thing a person opened, and when they last did.
type View struct {
	ResourceUUID string    `json:"resource_uuid"`
	LastViewedAt time.Time `json:"last_viewed_at"`
}

// RecentViews is what the person opened of one kind (resource_views' "doc"
// or "board") since a moment, newest first, at most limit: after a view
// read before (nil: from the newest), as the next page. The page is a
// keyset on (last_viewed_at, resource_uuid), never an offset: a view made
// between two pages moves to the top and neither skips nor repeats one. The
// user's recent index (migration 206's resource_views_user_recent_idx)
// orders by last_viewed_at alone; resource_uuid only breaks ties between
// views made at the same moment, so the order and the keyset are total.
func RecentViews(ctx context.Context, userUUID, resourceType string, since time.Time, after *View, limit int) ([]View, error) {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	args := []any{userUUID, resourceType, since, limit}
	keyset := ""
	if after != nil {
		keyset = `AND (last_viewed_at, resource_uuid) < ($5, $6)`
		args = append(args, after.LastViewedAt, after.ResourceUUID)
	}
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(c, `
		SELECT resource_uuid, last_viewed_at FROM resource_views
		 WHERE user_uuid = $1 AND resource_type = $2 AND last_viewed_at >= $3 `+keyset+`
		 ORDER BY last_viewed_at DESC, resource_uuid DESC
		 LIMIT $4`, args...)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Places RecentViews err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var out []View
	for rows.Next() {
		var v View
		if err := rows.Scan(&v.ResourceUUID, &v.LastViewedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
