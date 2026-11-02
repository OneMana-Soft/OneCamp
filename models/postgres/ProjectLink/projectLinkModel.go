// Package models (ProjectLink) keeps a project's links (basecamp-16,
// migration 544): the tools its people work in elsewhere, as cards on the
// project's home. business/ProjectLink reads each card from its page; this
// keeps them, each address once among a project's live links, in the
// project's order. A delete is soft (V-78): softDelete.Link.
package models

import (
	"context"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/pgexec"
	softDelete "github.com/akashc777/OneCamp/models/postgres/SoftDelete"
	"github.com/google/uuid"
)

// Link is one of a project's links.
type Link struct {
	ID          uuid.UUID
	ProjectUUID uuid.UUID
	URL         string
	Title       string
	Description string
	SiteName    string
	// Icon and Image are the stored icon's and picture's attachment objects
	// (src_key "projectLink"), or nil.
	Icon      *uuid.UUID
	Image     *uuid.UUID
	Position  int
	CreatedAt time.Time
}

var (
	// ErrFull is a project with as many links as it may hold.
	ErrFull = errors.New("project links: full")
	// ErrDuplicate is an address one of the project's live links has.
	ErrDuplicate = errors.New("project links: already there")
)

const columns = `id, project_uuid, url, title, description, site_name, icon_object, image_object, position, created_at`

func scan(r pgexec.Row) (Link, error) {
	var l Link
	var icon, image uuid.NullUUID
	if err := r.Scan(&l.ID, &l.ProjectUUID, &l.URL, &l.Title, &l.Description, &l.SiteName, &icon, &image, &l.Position, &l.CreatedAt); err != nil {
		return l, err
	}
	l.Icon, l.Image = idOf(icon), idOf(image)
	return l, nil
}

func idOf(n uuid.NullUUID) *uuid.UUID {
	if !n.Valid {
		return nil
	}
	return &n.UUID
}

func nullOf(id *uuid.UUID) uuid.NullUUID {
	if id == nil {
		return uuid.NullUUID{}
	}
	return uuid.NullUUID{UUID: *id, Valid: true}
}

// List is the project's live links, in its order.
func List(ctx context.Context, project uuid.UUID) ([]Link, error) {
	return pgexec.Query(ctx, `SELECT `+columns+` FROM project_links WHERE project_uuid = $1 AND `+softDelete.Live("")+`
		ORDER BY position, created_at, id`, scan, project)
}

// Get is the project's live link with id, or nil.
func Get(ctx context.Context, project, id uuid.UUID) (*Link, error) {
	l, ok, err := pgexec.QueryOne(ctx, `SELECT `+columns+` FROM project_links WHERE id = $1 AND project_uuid = $2 AND `+softDelete.Live(""), scan, id, project)
	if err != nil || !ok {
		return nil, err
	}
	return &l, nil
}

// counts is how many live links the project has, and ErrDuplicate when one
// of them has address ("" asks after none).
func counts(ctx context.Context, project uuid.UUID, address string) (int, error) {
	type got struct {
		n   int
		dup bool
	}
	g, _, err := pgexec.QueryOne(ctx, `SELECT COUNT(*), COALESCE(bool_or(url = $2), false) FROM project_links WHERE project_uuid = $1 AND `+softDelete.Live(""),
		func(r pgexec.Row) (got, error) {
			var g got
			return g, r.Scan(&g.n, &g.dup)
		}, project, address)
	if err == nil && g.dup && address != "" {
		err = ErrDuplicate
	}
	return g.n, err
}

// Room is nil when the project may take a link to address: ErrDuplicate when
// one of its live links has it, ErrFull when it holds most. It is read
// without the project's lock, so a caller asks before slow work and Create
// asks again under the lock.
func Room(ctx context.Context, project uuid.UUID, address string, most int) error {
	n, err := counts(ctx, project, address)
	if err == nil && n >= most {
		err = ErrFull
	}
	return err
}

// lockOf is the transaction lock a project's link count is read and changed
// under: its adds and its links' restores.
func lockOf(project uuid.UUID) string { return "project_links:" + project.String() }

// Bound is a project's most links, for a deleted link's restore: counted
// under the lock its adds take, past most it answers ErrFull, as an add does.
// (An address a live link has since is the store's to refuse: ErrDuplicate.)
func Bound(project uuid.UUID, most int) softDelete.Bound {
	return softDelete.Bound{Lock: lockOf(project), Most: most, Full: ErrFull,
		Count: func(ctx context.Context) (int, error) { return counts(ctx, project, "") }}
}

// Create adds l to its project, last in its order, as by (uuid.Nil: no one
// person), unless the project holds most links (ErrFull) or a live link has
// its address (ErrDuplicate). The count and the write hold the project's
// lock, so two adds at once can't both take the last place or one address.
func Create(ctx context.Context, l Link, by uuid.UUID, most int) (*Link, error) {
	var made Link
	added, err := pgexec.Capped(ctx, lockOf(l.ProjectUUID), most,
		func(ctx context.Context) (int, error) { return counts(ctx, l.ProjectUUID, l.URL) },
		func(ctx context.Context) error {
			var err error
			made, _, err = pgexec.QueryOne(ctx, `INSERT INTO project_links (project_uuid, url, title, description, site_name, icon_object, image_object, created_by, position)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8,
					COALESCE((SELECT MAX(position) FROM project_links WHERE project_uuid = $1 AND `+softDelete.Live("")+`), 0) + 1)
				RETURNING `+columns, scan,
				l.ProjectUUID, l.URL, l.Title, l.Description, l.SiteName, nullOf(l.Icon), nullOf(l.Image), uuid.NullUUID{UUID: by, Valid: by != uuid.Nil})
			return err
		})
	switch {
	case helpers.IsUniqueViolation(err):
		return nil, ErrDuplicate
	case err != nil:
		return nil, err
	case !added:
		return nil, ErrFull
	}
	return &made, nil
}

// SetPictures sets a live link's stored icon and picture (nil: none).
func SetPictures(ctx context.Context, project, id uuid.UUID, icon, image *uuid.UUID) error {
	_, err := pgexec.Exec(ctx, `UPDATE project_links SET icon_object = $3, image_object = $4 WHERE id = $1 AND project_uuid = $2 AND `+softDelete.Live(""),
		id, project, nullOf(icon), nullOf(image))
	return err
}

// Rename sets a live link's title, and answers whether the project has it.
func Rename(ctx context.Context, project, id uuid.UUID, title string) (bool, error) {
	n, err := pgexec.Exec(ctx, `UPDATE project_links SET title = $3 WHERE id = $1 AND project_uuid = $2 AND `+softDelete.Live(""), id, project, title)
	return n > 0, err
}

// Delete deletes the project's live link softly, as by, and answers whether
// the project had it. Its stored pictures stay with it for its restore.
func Delete(ctx context.Context, project, id, by uuid.UUID) (bool, error) {
	return softDelete.Delete(ctx, softDelete.Link, id, by, "project_uuid = $3", project)
}
