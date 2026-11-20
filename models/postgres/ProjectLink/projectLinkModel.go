// Package models (ProjectLink) keeps a project's links (basecamp-16,
// migration 544): the tools its people work in elsewhere, as cards on the
// project's home. business/ProjectLink reads each card from its page; this
// keeps them, each address once among a project's live links, in the
// project's order. A delete is soft (V-78): softDelete.Link.
package models

import (
	"context"
	"errors"
	"sort"
	"time"

	linkDomain "github.com/akashc777/OneCamp/domain/ProjectLink"
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

const columns = linkDomain.Columns

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
// of them has one of addresses (none asks after none).
func counts(ctx context.Context, project uuid.UUID, addresses []string) (int, error) {
	type got struct {
		n   int
		dup bool
	}
	g, _, err := pgexec.QueryOne(ctx, linkDomain.Counts,
		func(r pgexec.Row) (got, error) {
			var g got
			return g, r.Scan(&g.n, &g.dup)
		}, project, addresses)
	if err == nil && g.dup && len(addresses) > 0 {
		err = ErrDuplicate
	}
	return g.n, err
}

// Room is nil when the project may take a link to address: ErrDuplicate when
// one of its live links has it, ErrFull when it holds most. It is read
// without the project's lock, so a caller asks before slow work and
// CreateMany asks again under the lock.
func Room(ctx context.Context, project uuid.UUID, address string, most int) error {
	n, err := counts(ctx, project, []string{address})
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
		Count: func(ctx context.Context) (int, error) { return counts(ctx, project, nil) }}
}

// CreateMany adds links to the project, last in its order in the order given,
// as by (uuid.Nil: no one person), in one write, and answers them in that
// order: all of them, or none when the project would hold more than most
// (ErrFull) or a live link, or another of them, has one of their addresses
// (ErrDuplicate). The count and the write hold the project's lock, so two
// adds at once can't both take the last places or one address.
func CreateMany(ctx context.Context, project uuid.UUID, links []Link, by uuid.UUID, most int) ([]Link, error) {
	if len(links) == 0 {
		return nil, nil
	}
	n := len(links)
	urls, titles, descriptions, sites := make([]string, n), make([]string, n), make([]string, n), make([]string, n)
	icons, images := make([]string, n), make([]string, n)
	for i, l := range links {
		urls[i], titles[i], descriptions[i], sites[i] = l.URL, l.Title, l.Description, l.SiteName
		icons[i], images[i] = textOf(l.Icon), textOf(l.Image)
	}
	var made []Link
	// Capped adds while the count is under its limit: n more fit when the
	// count is at most most-n.
	added, err := pgexec.Capped(ctx, lockOf(project), most-n+1,
		func(ctx context.Context) (int, error) { return counts(ctx, project, urls) },
		func(ctx context.Context) error {
			var err error
			made, err = pgexec.Query(ctx, linkDomain.Insert, scan,
				project, urls, titles, descriptions, sites, icons, images, uuid.NullUUID{UUID: by, Valid: by != uuid.Nil})
			if err == nil && len(made) != n {
				err = errors.New("project links: the write didn't answer every link")
			}
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
	sort.Slice(made, func(i, j int) bool { return made[i].Position < made[j].Position })
	return made, nil
}

// textOf is an object's id as Insert's arrays hold it: "" for none.
func textOf(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
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
