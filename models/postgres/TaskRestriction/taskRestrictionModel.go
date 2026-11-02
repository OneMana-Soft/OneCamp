// Package models (TaskRestriction) keeps restricted tasks and the people let
// into them (migration 472; design/projects-vs-asana-jira/WA2_PLAN.md).
// business/WorkAccess decides by these rows who may read a restricted task.
//
// Nothing here is deleted (V-179, the owner's rule: every delete is soft).
// Lifting a restriction, taking a person off and a person's leaving the
// project mark the rows (deleted_at, deleted_by) and keep them; every read
// takes live rows only (SoftDelete.Live). A task restricted again, or a
// person let in again, brings its own row back.
package models

import (
	"context"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/models/pgexec"
	softDelete "github.com/akashc777/OneCamp/models/postgres/SoftDelete"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// ErrNoDatabase is a call before Postgres is connected.
var ErrNoDatabase = errors.New("task restrictions: not connected")

func connected() bool {
	return postgresInit.DBConn != nil && postgresInit.DBConn.SqlDB != nil
}

// Candidate is a restricted task, as its rows stand to someone: LetIn when
// they are let in by its rows alone (granted on it, or its creator, or, for
// one restricted with another (Via), granted on that one or its creator).
// For one they aren't, whether they're its assignee, or that one's, or run
// its project is the graph's to say
// (business/WorkAccess).
type Candidate struct {
	TaskID    uuid.UUID
	ProjectID uuid.UUID
	// Via is the task it is restricted with (a task above it), whose people
	// are its people; uuid.Nil for one restricted on its own.
	Via uuid.UUID
	// LetIn is whether its rows let the reader in; never for nobody.
	LetIn bool
}

// Candidates are every restricted task, each saying whether its rows let
// reader in (LetIn): granted on it or its creator, themselves or through the
// task it is restricted with. For uuid.Nil (nobody), none lets them in. One
// read: the decision hides those that don't, and the ones that do carry the
// lock (WA2b). Restricted tasks are few. Before Postgres is connected (unit
// tests) there are none, as with every read of its rows.
func Candidates(ctx context.Context, reader uuid.UUID) ([]Candidate, error) {
	if !connected() {
		return nil, nil
	}
	return pgexec.Query(ctx, `
		SELECT r.task_id, r.project_id, r.via_task_id,
		       NOT ($1::uuid = '00000000-0000-0000-0000-000000000000'::uuid OR (`+keptOut("$1::uuid")+`)) AS let_in
		FROM task_restrictions r
		JOIN tasks t ON t.id = r.task_id
		LEFT JOIN tasks v ON v.id = r.via_task_id
		WHERE `+softDelete.Live("r"),
		func(r pgexec.Row) (Candidate, error) {
			var (
				x   Candidate
				via uuid.NullUUID
			)
			err := r.Scan(&x.TaskID, &x.ProjectID, &via, &x.LetIn)
			x.Via = via.UUID
			return x, err
		}, reader)
}

// keptOut is whether a restricted task's rows leave who out, over r (its
// row), t (the task) and v (the task it is restricted with): neither made by
// them, and no grant to them on it or on v.
func keptOut(who string) string {
	return `t.created_by IS DISTINCT FROM ` + who + `
		AND (r.via_task_id IS NULL OR v.created_by IS DISTINCT FROM ` + who + `)
		AND NOT EXISTS (SELECT 1 FROM task_restriction_grants g
		                WHERE g.task_id = COALESCE(r.via_task_id, r.task_id) AND g.user_id = ` + who + `
		                AND ` + softDelete.Live("g") + `)`
}

// CandidatesAmong is Candidates for each of readers (people; nobody has none
// let in, as Candidates says), by reader, in one read: every restricted task
// once per reader. Restricted tasks are few.
func CandidatesAmong(ctx context.Context, readers []uuid.UUID) (map[uuid.UUID][]Candidate, error) {
	out := map[uuid.UUID][]Candidate{}
	if len(readers) == 0 || !connected() {
		return out, nil
	}
	ids := make([]string, len(readers))
	for i, id := range readers {
		ids[i] = id.String()
	}
	type row struct {
		reader uuid.UUID
		c      Candidate
	}
	rows, err := pgexec.Query(ctx, `
		SELECT rd.id, r.task_id, r.project_id, r.via_task_id,
		       NOT (rd.id = '00000000-0000-0000-0000-000000000000'::uuid OR (`+keptOut("rd.id")+`)) AS let_in
		FROM unnest($1::uuid[]) AS rd(id)
		CROSS JOIN task_restrictions r
		JOIN tasks t ON t.id = r.task_id
		LEFT JOIN tasks v ON v.id = r.via_task_id
		WHERE `+softDelete.Live("r"),
		func(r pgexec.Row) (row, error) {
			var (
				x   row
				via uuid.NullUUID
			)
			err := r.Scan(&x.reader, &x.c.TaskID, &x.c.ProjectID, &via, &x.c.LetIn)
			x.c.Via = via.UUID
			return x, err
		}, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	for _, x := range rows {
		out[x.reader] = append(out[x.reader], x.c)
	}
	return out, nil
}

// Restriction is one task's: whether it is restricted, and who is granted.
type Restriction struct {
	Restricted   bool
	ProjectID    uuid.UUID
	RestrictedBy uuid.UUID
	RestrictedAt time.Time
	// Via is the task it is restricted with, a task above it; uuid.Nil when
	// it is restricted on its own.
	Via uuid.UUID
	// Grants are the people let in by name: its own, or Via's when it is
	// restricted with another.
	Grants []uuid.UUID
}

// Of is a task's restriction; the zero Restriction for a task that has none.
func Of(ctx context.Context, taskID uuid.UUID) (Restriction, error) {
	got, err := OfMany(ctx, []uuid.UUID{taskID})
	if err != nil {
		return Restriction{}, err
	}
	return got[taskID], nil
}

// OfMany is the restriction of each of the tasks that has one, with its
// grants. Two reads.
func OfMany(ctx context.Context, taskIDs []uuid.UUID) (map[uuid.UUID]Restriction, error) {
	out := map[uuid.UUID]Restriction{}
	if len(taskIDs) == 0 || !connected() {
		return out, nil
	}
	ids := make([]string, len(taskIDs))
	for i, id := range taskIDs {
		ids[i] = id.String()
	}
	type row struct {
		id uuid.UUID
		r  Restriction
	}
	rows, err := pgexec.Query(ctx, `
		SELECT task_id, project_id, restricted_by, restricted_at, via_task_id
		FROM task_restrictions WHERE task_id = ANY($1::uuid[]) AND `+softDelete.Live(""), func(r pgexec.Row) (row, error) {
		var (
			x       row
			by, via uuid.NullUUID
		)
		err := r.Scan(&x.id, &x.r.ProjectID, &by, &x.r.RestrictedAt, &via)
		x.r.Restricted, x.r.RestrictedBy, x.r.Via = true, by.UUID, via.UUID
		return x, err
	}, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	for _, x := range rows {
		out[x.id] = x.r
	}
	if len(out) == 0 {
		return out, nil
	}
	// A task restricted with another has that one's people.
	type grant struct{ task, user uuid.UUID }
	grants, err := pgexec.Query(ctx, `
		SELECT r.task_id, g.user_id
		FROM task_restrictions r
		JOIN task_restriction_grants g ON g.task_id = COALESCE(r.via_task_id, r.task_id) AND `+softDelete.Live("g")+`
		WHERE r.task_id = ANY($1::uuid[]) AND `+softDelete.Live("r")+` ORDER BY g.granted_at, g.user_id`, func(r pgexec.Row) (grant, error) {
		var g grant
		err := r.Scan(&g.task, &g.user)
		return g, err
	}, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	for _, g := range grants {
		r := out[g.task]
		r.Grants = append(r.Grants, g.user)
		out[g.task] = r
	}
	return out, nil
}

// Restrict keeps the task to its project's admins, assignee and creator and
// the people granted, and with it each of under (the tasks below it) that
// isn't restricted already: whoever may see the task sees those, and opening
// the task opens them again (Unrestrict). Those let in before and not now are
// taken off (their rows marked, by by); those let in before and now keep the
// grant they had; a person let in again, or a task restricted again, has its
// own row back. A task restricted with another before is restricted on its
// own from now. In one transaction, of five statements whatever the number of
// people.
func Restrict(ctx context.Context, taskID, projectID, by uuid.UUID, grants, under []uuid.UUID, now time.Time) error {
	if !connected() {
		return ErrNoDatabase
	}
	people := idStrings(grants, uuid.Nil)
	return pgexec.InTx(ctx, func(ctx context.Context) error {
		// Restricted already, it keeps who restricted it and when; lifted
		// before, it is restricted afresh, by by.
		if _, err := pgexec.Exec(ctx, `
			INSERT INTO task_restrictions (task_id, project_id, restricted_by, restricted_at)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4)
			ON CONFLICT (task_id) DO UPDATE SET project_id = EXCLUDED.project_id, via_task_id = NULL,
				restricted_by = CASE WHEN task_restrictions.deleted_at IS NULL THEN task_restrictions.restricted_by ELSE EXCLUDED.restricted_by END,
				restricted_at = CASE WHEN task_restrictions.deleted_at IS NULL THEN task_restrictions.restricted_at ELSE EXCLUDED.restricted_at END,
				deleted_at = NULL, deleted_by = NULL`,
			taskID, projectID, nullable(by), now); err != nil {
			return err
		}
		if _, err := pgexec.Exec(ctx, `
			UPDATE task_restriction_grants SET deleted_at = $3, deleted_by = $2::uuid
			WHERE task_id = $1::uuid AND `+softDelete.Live("")+` AND NOT (user_id = ANY($4::uuid[]))`,
			taskID, nullable(by), now, pq.Array(people)); err != nil {
			return err
		}
		if len(people) > 0 {
			if _, err := pgexec.Exec(ctx, `
				INSERT INTO task_restriction_grants (task_id, user_id, granted_by, granted_at)
				SELECT $1::uuid, u, $3::uuid, $4::timestamptz FROM unnest($2::uuid[]) AS u
				ON CONFLICT (task_id, user_id) DO UPDATE SET granted_by = EXCLUDED.granted_by, granted_at = EXCLUDED.granted_at,
					deleted_at = NULL, deleted_by = NULL
				WHERE task_restriction_grants.deleted_at IS NOT NULL`,
				taskID, pq.Array(people), nullable(by), now); err != nil {
				return err
			}
		}
		if ids := idStrings(under, taskID); len(ids) > 0 {
			// Only tasks Postgres has (every task made in the app); one already
			// restricted keeps its own people, or another's it is restricted with.
			if _, err := pgexec.Exec(ctx, `
				INSERT INTO task_restrictions (task_id, project_id, restricted_by, restricted_at, via_task_id)
				SELECT t.id, $2::uuid, $3::uuid, $4::timestamptz, $5::uuid FROM tasks t WHERE t.id = ANY($1::uuid[]) AND t.id <> $5::uuid
				`+revivedWith,
				pq.Array(ids), projectID, nullable(by), now, taskID); err != nil {
				return err
			}
		}
		return nil
	})
}

// revivedWith ends an insert of tasks restricted with another: one restricted
// already keeps its own row as it is, and one lifted before has its row back,
// restricted as the insert says.
const revivedWith = `ON CONFLICT (task_id) DO UPDATE SET project_id = EXCLUDED.project_id, restricted_by = EXCLUDED.restricted_by,
	restricted_at = EXCLUDED.restricted_at, via_task_id = EXCLUDED.via_task_id, deleted_at = NULL, deleted_by = NULL
	WHERE task_restrictions.deleted_at IS NOT NULL`

// Inherit restricts a task just made below a restricted one (from, its
// parent) with it: with the task from is restricted with, or with from
// itself. Nothing when from isn't restricted, or the task already is.
func Inherit(ctx context.Context, taskID, from, by uuid.UUID, now time.Time) error {
	if !connected() {
		return ErrNoDatabase
	}
	_, err := pgexec.Exec(ctx, `
		INSERT INTO task_restrictions (task_id, project_id, restricted_by, restricted_at, via_task_id)
		SELECT $1::uuid, r.project_id, $3::uuid, $4::timestamptz, COALESCE(r.via_task_id, r.task_id)
		FROM task_restrictions r WHERE r.task_id = $2::uuid AND `+softDelete.Live("r")+` AND COALESCE(r.via_task_id, r.task_id) <> $1::uuid
		`+revivedWith, taskID, from, nullable(by), now)
	return err
}

// idStrings is ids as strings, once each, without uuid.Nil or but.
func idStrings(ids []uuid.UUID, but uuid.UUID) []string {
	seen := map[uuid.UUID]bool{uuid.Nil: true, but: true}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id.String())
		}
	}
	return out
}

// Unrestrict opens the task to its project again, and the tasks restricted
// with it, by by: their rows and their grants are marked, not removed (V-179);
// false when it wasn't restricted. In one transaction.
func Unrestrict(ctx context.Context, taskID, by uuid.UUID) (bool, error) {
	if !connected() {
		return false, ErrNoDatabase
	}
	lifted := false
	err := pgexec.InTx(ctx, func(ctx context.Context) error {
		n, err := pgexec.Exec(ctx, `
			UPDATE task_restrictions SET deleted_at = NOW(), deleted_by = $2::uuid
			WHERE task_id = $1::uuid AND `+softDelete.Live(""), taskID, nullable(by))
		if err != nil || n == 0 {
			return err
		}
		lifted = true
		// The tasks restricted with it open with it, and whoever was let in goes too.
		if _, err := pgexec.Exec(ctx, `
			UPDATE task_restrictions SET deleted_at = NOW(), deleted_by = $2::uuid
			WHERE via_task_id = $1::uuid AND `+softDelete.Live(""), taskID, nullable(by)); err != nil {
			return err
		}
		_, err = pgexec.Exec(ctx, `
			UPDATE task_restriction_grants SET deleted_at = NOW(), deleted_by = $2::uuid
			WHERE task_id IN (SELECT task_id FROM task_restrictions WHERE task_id = $1::uuid OR via_task_id = $1::uuid)
			AND `+softDelete.Live(""), taskID, nullable(by))
		return err
	})
	return lifted && err == nil, err
}

// DropGrantsIn takes someone's grants on the projects' restricted tasks
// away, by by (uuid.Nil when no one person did it): they left the projects (a
// team takes all of its), and leaving a project leaves its work. The grants
// are marked, not removed (V-179). One statement.
func DropGrantsIn(ctx context.Context, projectIDs []uuid.UUID, userID, by uuid.UUID) error {
	if !connected() {
		return ErrNoDatabase
	}
	ids := idStrings(projectIDs, uuid.Nil)
	if len(ids) == 0 {
		return nil
	}
	_, err := pgexec.Exec(ctx, `
		UPDATE task_restriction_grants g SET deleted_at = NOW(), deleted_by = $3::uuid
		FROM task_restrictions r
		WHERE g.task_id = r.task_id AND r.project_id = ANY($1::uuid[]) AND g.user_id = $2::uuid AND `+softDelete.Live("g"),
		pq.Array(ids), userID, nullable(by))
	return err
}

func nullable(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}
