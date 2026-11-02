// Package models (ProjectRole) keeps people's roles in projects: admin,
// member, commenter or viewer (migration 320). The graph's member and admin
// edges say who is in a project; a row says how much they may do there
// (business/WorkAccess decides, business/Project writes).
//
// A role taken away is kept (V-184 head 3, migration 549): the row is marked
// deleted_at and deleted_by (whoever took it; NULL for the parity sweep), every
// read says softDelete.Live, and giving the role again brings the same row
// back, live.
package models

import (
	"context"
	"database/sql"
	"errors"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/models/pgexec"
	softDelete "github.com/akashc777/OneCamp/models/postgres/SoftDelete"
	"github.com/google/uuid"
)

// Roles, as rows spell them.
const (
	Admin     = "admin"
	Member    = "member"
	Commenter = "commenter"
	Viewer    = "viewer"
)

// ErrNoDatabase is a read or write with no database.
var ErrNoDatabase = errors.New("project roles: not connected")

func connected() bool {
	return postgresInit.DBConn != nil && postgresInit.DBConn.SqlDB != nil
}

// RolesIn is someone's role in each of the projects that has a row for them,
// by project, in one read. With no database at all (a test, a tool run
// without one) nobody has a row, so the graph's answer stands; a server always
// has one, since signing in reads it.
func RolesIn(ctx context.Context, userID uuid.UUID, projectIDs []uuid.UUID) (map[uuid.UUID]string, error) {
	out := map[uuid.UUID]string{}
	if len(projectIDs) == 0 || !connected() {
		return out, nil
	}
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(c,
		`SELECT project_id, role FROM project_roles WHERE user_id = $1 AND project_id = ANY($2) AND `+softDelete.Live(""), userID, projectIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p uuid.UUID
		var role string
		if err := rows.Scan(&p, &role); err != nil {
			return nil, err
		}
		out[p] = role
	}
	return out, rows.Err()
}

// Roles is everyone's role in a project, by person.
func Roles(ctx context.Context, projectID uuid.UUID) (map[uuid.UUID]string, error) {
	if !connected() {
		return nil, ErrNoDatabase
	}
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(c,
		`SELECT user_id, role FROM project_roles WHERE project_id = $1 AND `+softDelete.Live(""), projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]string{}
	for rows.Next() {
		var u uuid.UUID
		var role string
		if err := rows.Scan(&u, &role); err != nil {
			return nil, err
		}
		out[u] = role
	}
	return out, rows.Err()
}

// RolesOfMany is everyone's role in each of projects, by project then person,
// in one statement (the parity sweep reads a page at a time); a project with
// no rows is missing.
func RolesOfMany(ctx context.Context, projects []uuid.UUID) (map[uuid.UUID]map[uuid.UUID]string, error) {
	if !connected() {
		return nil, ErrNoDatabase
	}
	out := map[uuid.UUID]map[uuid.UUID]string{}
	if len(projects) == 0 {
		return out, nil
	}
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(c,
		`SELECT project_id, user_id, role FROM project_roles WHERE project_id = ANY($1) AND `+softDelete.Live(""), projects)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p, u uuid.UUID
		var role string
		if err := rows.Scan(&p, &u, &role); err != nil {
			return nil, err
		}
		if out[p] == nil {
			out[p] = map[uuid.UUID]string{}
		}
		out[p][u] = role
	}
	return out, rows.Err()
}

// Set gives someone a role in a project, by whom (uuid.Nil for nobody: the
// parity sweep). A project with no row of its own (one the graph has and
// Postgres doesn't) takes no role: the graph's answer stands for it. A role
// taken away earlier comes back on its own row, live, even when it is the
// same role.
func Set(ctx context.Context, projectID, userID uuid.UUID, role string, by uuid.UUID) error {
	if !connected() {
		return ErrNoDatabase
	}
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	var grantedBy any
	if by != uuid.Nil {
		grantedBy = by
	}
	_, err := postgresInit.DBConn.SqlDB.ExecContext(c, `
		INSERT INTO project_roles (project_id, user_id, role, granted_by, granted_at)
		SELECT $1, $2, $3, $4, NOW() WHERE EXISTS (SELECT 1 FROM projects WHERE id = $1)
		ON CONFLICT (project_id, user_id) DO UPDATE SET role = EXCLUDED.role, granted_by = EXCLUDED.granted_by, granted_at = NOW(),
		 deleted_at = NULL, deleted_by = NULL
		 WHERE project_roles.role <> EXCLUDED.role OR project_roles.deleted_at IS NOT NULL`,
		projectID, userID, role, grantedBy)
	return err
}

// Delete takes someone's role in a project away, by whom (uuid.Nil for
// nobody). The row stays, marked: no read sees it, and Set brings it back.
func Delete(ctx context.Context, projectID, userID, by uuid.UUID) error {
	if !connected() {
		return ErrNoDatabase
	}
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(c,
		`UPDATE project_roles SET deleted_at = NOW(), deleted_by = $3 WHERE project_id = $1 AND user_id = $2 AND `+softDelete.Live(""),
		projectID, userID, softDelete.By(by))
	return err
}

// DeleteIn takes someone's roles in all of the projects away, by whom: they
// left the projects' team. The rows stay, marked, as Delete's.
func DeleteIn(ctx context.Context, userID uuid.UUID, projectIDs []uuid.UUID, by uuid.UUID) error {
	if len(projectIDs) == 0 {
		return nil
	}
	if !connected() {
		return ErrNoDatabase
	}
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(c,
		`UPDATE project_roles SET deleted_at = NOW(), deleted_by = $3 WHERE user_id = $1 AND project_id = ANY($2) AND `+softDelete.Live(""),
		userID, projectIDs, softDelete.By(by))
	return err
}

// Keep makes a project's live rows say exactly roles (by person): it adds,
// changes and brings back rows, and takes away those of people who aren't in
// roles (marked, as Delete's), in one transaction. For the parity sweep; by
// is nobody, so a row it brings back has no granter and one it takes away no
// taker.
func Keep(ctx context.Context, projectID uuid.UUID, roles map[uuid.UUID]string) (changed int, err error) {
	if !connected() {
		return 0, ErrNoDatabase
	}
	err = postgresInit.InTx(ctx, postgresInit.DBConn.DBTimeout, func(ctx context.Context, tx *sql.Tx) error {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM projects WHERE id = $1)`, projectID).Scan(&exists); err != nil || !exists {
			return err
		}
		people := make([]uuid.UUID, 0, len(roles))
		for person, role := range roles {
			people = append(people, person)
			res, err := tx.ExecContext(ctx, `
				INSERT INTO project_roles (project_id, user_id, role) VALUES ($1, $2, $3)
				ON CONFLICT (project_id, user_id) DO UPDATE SET role = EXCLUDED.role, granted_at = NOW(),
				 granted_by = CASE WHEN project_roles.deleted_at IS NULL THEN project_roles.granted_by END,
				 deleted_at = NULL, deleted_by = NULL
				 WHERE project_roles.role <> EXCLUDED.role OR project_roles.deleted_at IS NOT NULL`, projectID, person, role)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n > 0 {
				changed++
			}
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE project_roles SET deleted_at = NOW(), deleted_by = NULL WHERE project_id = $1 AND NOT (user_id = ANY($2)) AND `+softDelete.Live(""),
			projectID, people)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			changed += int(n)
		}
		return nil
	})
	return changed, err
}

// RolesAmong is RolesIn for many people at once: each one's role rows in
// the projects, by person and then project. One read.
func RolesAmong(ctx context.Context, userIDs, projectIDs []uuid.UUID) (map[uuid.UUID]map[uuid.UUID]string, error) {
	out := map[uuid.UUID]map[uuid.UUID]string{}
	if len(userIDs) == 0 || len(projectIDs) == 0 || !connected() {
		return out, nil
	}
	type row struct {
		user, project uuid.UUID
		role          string
	}
	rows, err := pgexec.Query(ctx,
		`SELECT user_id, project_id, role FROM project_roles WHERE user_id = ANY($1) AND project_id = ANY($2) AND `+softDelete.Live(""),
		func(r pgexec.Row) (row, error) {
			var x row
			err := r.Scan(&x.user, &x.project, &x.role)
			return x, err
		}, userIDs, projectIDs)
	if err != nil {
		return nil, err
	}
	for _, x := range rows {
		if out[x.user] == nil {
			out[x.user] = map[uuid.UUID]string{}
		}
		out[x.user][x.project] = x.role
	}
	return out, nil
}
