package models

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/models/pgexec"
	softDelete "github.com/akashc777/OneCamp/models/postgres/SoftDelete"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// Cycle is one time-boxed stretch of a project's work (migration 178).
type Cycle struct {
	Id           uuid.UUID  `json:"id"`
	ProjectUUID  uuid.UUID  `json:"project_uuid"`
	Number       int        `json:"number"`
	Name         string     `json:"name"`
	StartsAt     time.Time  `json:"starts_at"`
	EndsAt       time.Time  `json:"ends_at"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
	DoneCount    *int       `json:"done_count,omitempty"`
	CarriedCount *int       `json:"carried_count,omitempty"`
	// CapacityMinutes is the capacity a person set for this cycle; nil is the
	// project's own, what its recent cycles finished (business/Cycle/backlog.go).
	// CapacityPoints is the same in points, for a project that estimates in
	// them (migration 933).
	CapacityMinutes *int `json:"capacity_minutes,omitempty"`
	CapacityPoints  *int `json:"capacity_points,omitempty"`
}

// ErrOverlap is a cycle that would share days with another of the project's.
var ErrOverlap = errors.New("cycles overlap")

// OverlapError is ErrOverlap naming the live cycle In the way.
type OverlapError struct{ In Cycle }

func (e *OverlapError) Error() string { return ErrOverlap.Error() + ": " + e.In.Label() }

// Unwrap makes an OverlapError ErrOverlap to errors.Is.
func (e *OverlapError) Unwrap() error { return ErrOverlap }

// Label is how people know the cycle: its name, or "Cycle <number>". Pure.
func (c Cycle) Label() string {
	if c.Name != "" {
		return c.Name
	}
	return fmt.Sprintf("Cycle %d", c.Number)
}

func withTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
}

const columns = `id, project_uuid, number, name, starts_at, ends_at, completed_at, done_count, carried_count, capacity_minutes, capacity_points`

func scan(row interface{ Scan(...any) error }) (*Cycle, error) {
	var c Cycle
	err := row.Scan(&c.Id, &c.ProjectUUID, &c.Number, &c.Name, &c.StartsAt, &c.EndsAt, &c.CompletedAt, &c.DoneCount, &c.CarriedCount, &c.CapacityMinutes, &c.CapacityPoints)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &c, err
}

// scanRow is scan for pgexec.Query, which keeps cycles by value.
func scanRow(r pgexec.Row) (Cycle, error) {
	c, err := scan(r)
	if c == nil {
		return Cycle{}, err
	}
	return *c, err
}

// List is a project's cycles in order.
func List(project uuid.UUID) ([]Cycle, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx,
		`SELECT `+columns+` FROM project_cycles WHERE project_uuid = $1 AND `+softDelete.Live("")+` ORDER BY number`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Cycle{}
	for rows.Next() {
		c, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// Get is one cycle, or nil.
func Get(id uuid.UUID) (*Cycle, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	return scan(postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `SELECT `+columns+` FROM project_cycles WHERE id = $1 AND `+softDelete.Live(""), id))
}

// Create numbers the cycle after the project's last and refuses overlaps,
// under a per-project lock so two cycles made at once can't collide.
func Create(project uuid.UUID, name string, startsAt, endsAt time.Time, by uuid.UUID) (*Cycle, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	tx, err := postgresInit.DBConn.SqlDB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after Commit
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "cycles:"+project.String()); err != nil {
		return nil, err
	}
	var clash bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM project_cycles
		WHERE project_uuid = $1 AND starts_at < $3 AND ends_at > $2 AND `+softDelete.Live("")+`)`, project, startsAt, endsAt).Scan(&clash); err != nil {
		return nil, err
	}
	if clash {
		return nil, ErrOverlap
	}
	c, err := scan(tx.QueryRowContext(ctx, `
		INSERT INTO project_cycles (project_uuid, number, name, starts_at, ends_at, created_by)
		VALUES ($1, COALESCE((SELECT MAX(number) FROM project_cycles WHERE project_uuid = $1), 0) + 1, $2, $3, $4, $5)
		RETURNING `+columns, project, name, startsAt, endsAt, by))
	if err != nil {
		if helpers.IsUniqueViolation(err) {
			return nil, ErrOverlap
		}
		return nil, err
	}
	return c, tx.Commit()
}

// Start moves an upcoming cycle to run from startsAt to endsAt (B3), under
// the project's lock: a completed cycle still on the calendar then ends at
// startsAt; one that isn't completed and overlaps is ErrOverlap.
func Start(ctx context.Context, project, id uuid.UUID, startsAt, endsAt time.Time) (*Cycle, error) {
	var started *Cycle
	err := pgexec.InTx(ctx, func(ctx context.Context) error {
		if err := lockProject(ctx, project); err != nil {
			return err
		}
		clash, err := pgexec.Scalar[bool](ctx, `SELECT EXISTS (SELECT 1 FROM project_cycles
			WHERE project_uuid = $1 AND id <> $4 AND completed_at IS NULL AND starts_at < $3 AND ends_at > $2 AND `+softDelete.Live("")+`)`, project, startsAt, endsAt, id)
		if err != nil {
			return err
		}
		if clash {
			return ErrOverlap
		}
		if _, err := pgexec.Exec(ctx, `UPDATE project_cycles SET ends_at = $2
			WHERE project_uuid = $1 AND completed_at IS NOT NULL AND ends_at > $2 AND starts_at < $2 AND `+softDelete.Live(""), project, startsAt); err != nil {
			return err
		}
		c, _, err := pgexec.QueryOne(ctx, `UPDATE project_cycles SET starts_at = $3, ends_at = $4
			WHERE id = $1 AND project_uuid = $2 AND `+softDelete.Live("")+` RETURNING `+columns, func(r pgexec.Row) (*Cycle, error) { return scan(r) }, id, project, startsAt, endsAt)
		started = c
		return err
	})
	if err != nil {
		return nil, err
	}
	return started, nil
}

// lockProject holds the project's cycles lock until the transaction ends, so
// two writes of its cycles at once can't collide.
func lockProject(ctx context.Context, project uuid.UUID) error {
	_, err := pgexec.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "cycles:"+project.String())
	return err
}

// Rename changes a cycle's name.
func Rename(id uuid.UUID, name string) error {
	ctx, cancel := withTimeout()
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `UPDATE project_cycles SET name = $2 WHERE id = $1 AND `+softDelete.Live(""), id, name)
	return err
}

// Delete removes a cycle; its tasks simply leave it.
func Delete(ctx context.Context, id, by uuid.UUID) error {
	_, err := softDelete.Delete(ctx, softDelete.Cycle, id, by, "")
	return err
}

// Restore brings a deleted cycle back with its tasks (their links stayed),
// and answers whether it was deleted, under the project's lock as Create and
// Start take it. A deleted cycle keeps its dates, so its restore makes the
// room Start would have made: a completed one ends where a live cycle that
// begins within its dates starts. An *OverlapError (ErrOverlap) names a live
// cycle that still covers some of its dates, as a new cycle may not either.
func Restore(ctx context.Context, id uuid.UUID) (bool, error) {
	var restored bool
	err := pgexec.InTx(ctx, func(ctx context.Context) error {
		project, found, err := pgexec.QueryOne(ctx, `SELECT project_uuid FROM project_cycles WHERE id = $1 AND deleted_at IS NOT NULL`,
			func(r pgexec.Row) (uuid.UUID, error) {
				var p uuid.UUID
				return p, r.Scan(&p)
			}, id)
		if err != nil || !found {
			return err
		}
		if err := lockProject(ctx, project); err != nil {
			return err
		}
		if _, err := pgexec.Exec(ctx, `UPDATE project_cycles d SET ends_at = COALESCE((SELECT MIN(c.starts_at) FROM project_cycles c
			WHERE c.project_uuid = d.project_uuid AND c.id <> d.id AND `+softDelete.Live("c")+` AND c.starts_at > d.starts_at AND c.starts_at < d.ends_at), d.ends_at)
			WHERE d.id = $1 AND d.completed_at IS NOT NULL AND d.deleted_at IS NOT NULL`, id); err != nil {
			return err
		}
		// scan answers no row as nil, so QueryOne's found is always true here.
		in, _, err := pgexec.QueryOne(ctx, `SELECT `+columns+` FROM project_cycles WHERE id = (SELECT c.id FROM project_cycles c JOIN project_cycles d ON d.id = $1
			WHERE c.project_uuid = d.project_uuid AND c.id <> d.id AND `+softDelete.Live("c")+` AND c.starts_at < d.ends_at AND c.ends_at > d.starts_at
			ORDER BY c.starts_at LIMIT 1)`, func(r pgexec.Row) (*Cycle, error) { return scan(r) }, id)
		if err != nil {
			return err
		}
		if in != nil {
			return &OverlapError{In: *in}
		}
		restored, err = softDelete.Restore(ctx, softDelete.Cycle, id)
		return err
	})
	return restored, err
}

// Members maps each cycle of the project to its tasks.
func Members(project uuid.UUID) (map[uuid.UUID][]string, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, `
		SELECT tc.cycle_id, tc.task_uuid FROM task_cycles tc
		  JOIN project_cycles c ON c.id = tc.cycle_id AND `+softDelete.Live("c")+` WHERE c.project_uuid = $1`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID][]string{}
	for rows.Next() {
		var c, t uuid.UUID
		if err := rows.Scan(&c, &t); err != nil {
			return nil, err
		}
		out[c] = append(out[c], t.String())
	}
	return out, rows.Err()
}

// TasksIn lists a cycle's tasks.
func TasksIn(cycle uuid.UUID) ([]string, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, `SELECT task_uuid FROM task_cycles WHERE cycle_id = $1`, cycle)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t uuid.UUID
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t.String())
	}
	return out, rows.Err()
}

// TasksInProjectCycles is the tasks of those of cycles that belong to
// project, in one statement (a list filtered by cycles; PR1, the BE audit's
// #15): a cycle of another project, or none, adds nothing.
func TasksInProjectCycles(project uuid.UUID, cycles []uuid.UUID) ([]string, error) {
	if len(cycles) == 0 {
		return nil, nil
	}
	ctx, cancel := withTimeout()
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, `SELECT tc.task_uuid FROM task_cycles tc
		JOIN project_cycles c ON c.id = tc.cycle_id AND `+softDelete.Live("c")+`
		WHERE c.project_uuid = $1 AND c.id = ANY($2::uuid[])`, project, pq.Array(cycles))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t uuid.UUID
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t.String())
	}
	return out, rows.Err()
}

// Member is a task of a cycle and when it joined. Unfinished is a task the
// cycle hadn't finished when it was completed: it has moved on since, but it
// was still to do here at the end (migration 191).
type Member struct {
	TaskUUID   string
	AddedAt    time.Time
	Unfinished bool
}

// MembersOf is a cycle's tasks with when each joined, the unfinished ones of
// a completed cycle included. A task back in the cycle after being carried
// out is listed once, as a current member.
func MembersOf(cycle uuid.UUID) ([]Member, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, `
		SELECT task_uuid, added_at, false FROM task_cycles WHERE cycle_id = $1
		UNION ALL
		SELECT u.task_uuid, u.added_at, true FROM cycle_unfinished_tasks u
		 WHERE u.cycle_id = $1 AND NOT EXISTS (SELECT 1 FROM task_cycles tc WHERE tc.task_uuid = u.task_uuid AND tc.cycle_id = $1)`, cycle)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Member
	for rows.Next() {
		var m Member
		var t uuid.UUID
		if err := rows.Scan(&t, &m.AddedAt, &m.Unfinished); err != nil {
			return nil, err
		}
		m.TaskUUID = t.String()
		out = append(out, m)
	}
	return out, rows.Err()
}

// CycleOf is the cycle a task is in, or nil.
func CycleOf(task uuid.UUID) (*Cycle, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	return scan(postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `
		SELECT `+prefixed+` FROM project_cycles c JOIN task_cycles tc ON tc.cycle_id = c.id WHERE tc.task_uuid = $1 AND `+softDelete.Live("c"), task))
}

const prefixed = `c.id, c.project_uuid, c.number, c.name, c.starts_at, c.ends_at, c.completed_at, c.done_count, c.carried_count, c.capacity_minutes, c.capacity_points`

// SetTask puts a task in a cycle, or takes it out (cycle nil).
func SetTask(task uuid.UUID, cycle *uuid.UUID) error {
	ctx, cancel := withTimeout()
	defer cancel()
	_, err := SetTasks(ctx, []uuid.UUID{task}, cycle)
	return err
}

// SetTasks puts tasks in a cycle, or takes them out of theirs (cycle nil), in
// one statement however many, and answers the ids it moved. A task already
// there keeps when it joined: the burndown reads its scope from added_at.
func SetTasks(ctx context.Context, tasks []uuid.UUID, cycle *uuid.UUID) ([]string, error) {
	if len(tasks) == 0 {
		return nil, nil
	}
	ids := make([]string, len(tasks))
	for i, t := range tasks {
		ids[i] = t.String()
	}
	if cycle == nil {
		return pgexec.Query(ctx, `DELETE FROM task_cycles WHERE task_uuid = ANY($1::uuid[]) RETURNING task_uuid::text`, pgexec.Text, pq.Array(ids))
	}
	return pgexec.Query(ctx, `
		INSERT INTO task_cycles (task_uuid, cycle_id) SELECT unnest($1::uuid[]), $2
		ON CONFLICT (task_uuid) DO UPDATE SET cycle_id = EXCLUDED.cycle_id, added_at = NOW()
		 WHERE task_cycles.cycle_id IS DISTINCT FROM EXCLUDED.cycle_id
		RETURNING task_uuid::text`, pgexec.Text, pq.Array(ids), *cycle)
}

// SetCapacity sets a cycle's own capacity in minutes, or clears it (nil) so
// the project's applies again.
func SetCapacity(ctx context.Context, id uuid.UUID, minutes *int) error {
	_, err := pgexec.Exec(ctx, `UPDATE project_cycles SET capacity_minutes = $2 WHERE id = $1 AND `+softDelete.Live(""), id, minutes)
	return err
}

// SetCapacityPoints sets a cycle's own capacity in points, or clears it (nil)
// so the project's applies again.
func SetCapacityPoints(ctx context.Context, id uuid.UUID, points *int) error {
	_, err := pgexec.Exec(ctx, `UPDATE project_cycles SET capacity_points = $2 WHERE id = $1 AND `+softDelete.Live(""), id, points)
	return err
}

// Complete closes a cycle and moves the listed tasks to the next cycle (or
// out of cycles when next is nil), in one transaction. False when the cycle
// was already complete.
func Complete(id uuid.UUID, done int, carry []string, next *uuid.UUID) (bool, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	tx, err := postgresInit.DBConn.SqlDB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after Commit
	res, err := tx.ExecContext(ctx, `UPDATE project_cycles SET completed_at = NOW(), done_count = $2, carried_count = $3
		WHERE id = $1 AND completed_at IS NULL AND `+softDelete.Live(""), id, done, len(carry))
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	for _, t := range carry {
		// Keep a trace in this cycle of what it left unfinished, before the
		// task moves on, so its burndown ends where the work really stood.
		if _, err = tx.ExecContext(ctx, `INSERT INTO cycle_unfinished_tasks (cycle_id, task_uuid, added_at)
			SELECT cycle_id, task_uuid, added_at FROM task_cycles WHERE task_uuid = $1 AND cycle_id = $2
			ON CONFLICT DO NOTHING`, t, id); err != nil {
			return false, err
		}
		if next == nil {
			_, err = tx.ExecContext(ctx, `DELETE FROM task_cycles WHERE task_uuid = $1 AND cycle_id = $2`, t, id)
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE task_cycles SET cycle_id = $3, added_at = NOW() WHERE task_uuid = $1 AND cycle_id = $2`, t, id, *next)
		}
		if err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

// ProjectsOf is each cycle's project, by the cycle's id; a cycle that isn't
// there is left out.
func ProjectsOf(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]uuid.UUID, error) {
	if len(ids) == 0 {
		return map[uuid.UUID]uuid.UUID{}, nil
	}
	return pgexec.Pairs[uuid.UUID, uuid.UUID](ctx, `SELECT id, project_uuid FROM project_cycles WHERE id = ANY($1) AND `+softDelete.Live(""), ids)
}

// RunningIn is the cycle running at at (begun, not ended, not completed) in
// each of the projects, soonest to end first: the cycles across a person's
// projects (business/Cycle/running.go), which names only projects they may view.
func RunningIn(ctx context.Context, projects []uuid.UUID, at time.Time) ([]Cycle, error) {
	if len(projects) == 0 {
		return []Cycle{}, nil
	}
	return pgexec.Query(ctx, `SELECT `+columns+` FROM project_cycles
		WHERE project_uuid = ANY($1::uuid[]) AND completed_at IS NULL AND starts_at <= $2 AND ends_at > $2 AND `+softDelete.Live("")+`
		ORDER BY ends_at, project_uuid`, scanRow, pq.Array(projects), at)
}

// MembersOfMany maps each of the cycles to its tasks, in one statement.
func MembersOfMany(ctx context.Context, cycles []uuid.UUID) (map[uuid.UUID][]string, error) {
	out := make(map[uuid.UUID][]string, len(cycles))
	if len(cycles) == 0 {
		return out, nil
	}
	type pair struct {
		cycle uuid.UUID
		task  uuid.UUID
	}
	rows, err := pgexec.Query(ctx, `SELECT cycle_id, task_uuid FROM task_cycles WHERE cycle_id = ANY($1::uuid[])`,
		func(r pgexec.Row) (pair, error) {
			var p pair
			err := r.Scan(&p.cycle, &p.task)
			return p, err
		}, pq.Array(cycles))
	if err != nil {
		return nil, err
	}
	for _, p := range rows {
		out[p.cycle] = append(out[p.cycle], p.task.String())
	}
	return out, nil
}
