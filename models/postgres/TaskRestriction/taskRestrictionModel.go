// Package models (TaskRestriction) keeps restricted tasks and the people let
// into them (migration 472; design/projects-vs-asana-jira/WA2_PLAN.md).
// business/WorkAccess decides by these rows who may read a restricted task.
package models

import (
	"context"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/models/pgexec"
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
		       NOT ($1::uuid = '00000000-0000-0000-0000-000000000000'::uuid
		            OR (t.created_by IS DISTINCT FROM $1::uuid
		                AND (r.via_task_id IS NULL OR v.created_by IS DISTINCT FROM $1::uuid)
		                AND NOT EXISTS (SELECT 1 FROM task_restriction_grants g
		                                WHERE g.task_id = COALESCE(r.via_task_id, r.task_id) AND g.user_id = $1::uuid))) AS let_in
		FROM task_restrictions r
		JOIN tasks t ON t.id = r.task_id
		LEFT JOIN tasks v ON v.id = r.via_task_id`,
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
		FROM task_restrictions WHERE task_id = ANY($1::uuid[])`, func(r pgexec.Row) (row, error) {
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
		JOIN task_restriction_grants g ON g.task_id = COALESCE(r.via_task_id, r.task_id)
		WHERE r.task_id = ANY($1::uuid[]) ORDER BY g.granted_at, g.user_id`, func(r pgexec.Row) (grant, error) {
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
// the people granted, replacing any grants it had, and with it each of under
// (the tasks below it) that isn't restricted already: whoever may see the
// task sees those, and opening the task opens them again (Unrestrict). A task
// restricted with another before is restricted on its own from now. In one
// transaction, of four statements whatever the number of people.
func Restrict(ctx context.Context, taskID, projectID, by uuid.UUID, grants, under []uuid.UUID, now time.Time) error {
	if !connected() {
		return ErrNoDatabase
	}
	return pgexec.InTx(ctx, func(ctx context.Context) error {
		if _, err := pgexec.Exec(ctx, `
			INSERT INTO task_restrictions (task_id, project_id, restricted_by, restricted_at)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4)
			ON CONFLICT (task_id) DO UPDATE SET project_id = EXCLUDED.project_id, via_task_id = NULL`,
			taskID, projectID, nullable(by), now); err != nil {
			return err
		}
		if _, err := pgexec.Exec(ctx, `DELETE FROM task_restriction_grants WHERE task_id = $1::uuid`, taskID); err != nil {
			return err
		}
		if people := idStrings(grants, uuid.Nil); len(people) > 0 {
			if _, err := pgexec.Exec(ctx, `
				INSERT INTO task_restriction_grants (task_id, user_id, granted_by, granted_at)
				SELECT $1::uuid, u, $3::uuid, $4::timestamptz FROM unnest($2::uuid[]) AS u`,
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
				ON CONFLICT (task_id) DO NOTHING`,
				pq.Array(ids), projectID, nullable(by), now, taskID); err != nil {
				return err
			}
		}
		return nil
	})
}

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
		FROM task_restrictions r WHERE r.task_id = $2::uuid AND COALESCE(r.via_task_id, r.task_id) <> $1::uuid
		ON CONFLICT (task_id) DO NOTHING`, taskID, from, nullable(by), now)
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
// with it; false when it wasn't restricted.
func Unrestrict(ctx context.Context, taskID uuid.UUID) (bool, error) {
	if !connected() {
		return false, ErrNoDatabase
	}
	n, err := pgexec.Exec(ctx, `DELETE FROM task_restrictions WHERE task_id = $1::uuid`, taskID)
	return n > 0, err
}

// DropGrantsIn takes someone's grants on the projects' restricted tasks
// away: they left the projects (a team takes all of its), and leaving a
// project leaves its work. One statement.
func DropGrantsIn(ctx context.Context, projectIDs []uuid.UUID, userID uuid.UUID) error {
	if !connected() {
		return ErrNoDatabase
	}
	ids := idStrings(projectIDs, uuid.Nil)
	if len(ids) == 0 {
		return nil
	}
	_, err := pgexec.Exec(ctx, `
		DELETE FROM task_restriction_grants g
		USING task_restrictions r
		WHERE g.task_id = r.task_id AND r.project_id = ANY($1::uuid[]) AND g.user_id = $2::uuid`, pq.Array(ids), userID)
	return err
}

func nullable(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}
